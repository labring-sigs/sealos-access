package internalcredentialstest

import (
	"context"
	"strings"
	"testing"
	"time"

	userv1 "github.com/labring-sigs/sealos-access/api/v1"
	"github.com/labring-sigs/sealos-access/controllers"
	"github.com/labring-sigs/sealos-access/pkg/internalcredentials"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func TestBaseLeaseUsesStableServiceAccount(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	user := activeUser()
	stable := &corev1.ServiceAccount{
		ObjectMeta:                   metav1.ObjectMeta{Name: user.Name, Namespace: userNS, Labels: internalUserLabels(user)},
		AutomountServiceAccountToken: ptr.To(false),
	}
	lease := baseLease()
	fakeClient := newFakeClient(t,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: userNS}},
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: baseRoleName}},
		user, stable, lease,
	)
	reconciler := &controllers.CredentialLeaseReconciler{Client: fakeClient, UserNamespace: userNS, BrokerNamespace: brokerNS, Now: func() time.Time { return now }}

	if _, err := reconciler.Reconcile(ctx, requestFor(brokerNS, lease.Name)); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, requestFor(brokerNS, lease.Name)); err != nil {
		t.Fatal(err)
	}

	current := &userv1.CredentialLease{}
	if err := fakeClient.Get(ctx, types.NamespacedName{Namespace: brokerNS, Name: lease.Name}, current); err != nil {
		t.Fatal(err)
	}
	if current.Status.Phase != userv1.CredentialLeasePrepared || current.Status.ServiceAccountRef == nil || current.Status.ServiceAccountRef.Name != user.Name || current.Status.BindingRef != nil || current.Status.BoundSecretRef == nil || current.Status.BoundSecretRef.UID == "" {
		t.Fatalf("base lease status = %#v", current.Status)
	}
	serviceAccounts := &corev1.ServiceAccountList{}
	if err := fakeClient.List(ctx, serviceAccounts, client.InNamespace(userNS)); err != nil {
		t.Fatal(err)
	}
	if len(serviceAccounts.Items) != 1 {
		t.Fatalf("service account count = %d, want stable account only", len(serviceAccounts.Items))
	}
	bindings := &rbacv1.ClusterRoleBindingList{}
	if err := fakeClient.List(ctx, bindings); err != nil {
		t.Fatal(err)
	}
	if len(bindings.Items) != 0 {
		t.Fatalf("base lease created %d temporary bindings", len(bindings.Items))
	}
}

func TestIssuedLeaseExpiresAndCleansResources(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	user := activeUser()
	expired := elevatedLease()
	expired.Name = "issued-expired"
	expired.Finalizers = []string{userv1.CredentialLeaseFinalizer}
	expired.Status.Phase = userv1.CredentialLeaseIssued
	expired.Status.ExpirationTimestamp = ptr.To(metav1.NewTime(now.Add(-time.Second)))
	expiredBinding, expiredSA, expiredSecret := temporaryLeaseResources(expired, user)
	future := elevatedLease()
	future.Name = "issued-future"
	future.Finalizers = []string{userv1.CredentialLeaseFinalizer}
	future.Status.Phase = userv1.CredentialLeaseIssued
	future.Status.ExpirationTimestamp = ptr.To(metav1.NewTime(now.Add(time.Hour)))
	fakeClient := newFakeClient(t, user, expired, expiredBinding, expiredSA, expiredSecret, future)
	reconciler := &controllers.CredentialLeaseReconciler{Client: fakeClient, UserNamespace: userNS, BrokerNamespace: brokerNS, Now: func() time.Time { return now }}

	result, err := reconciler.Reconcile(ctx, requestFor(brokerNS, expired.Name))
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != 0 {
		t.Fatalf("expired lease result = %#v, want no requeue", result)
	}
	current := &userv1.CredentialLease{}
	if err := fakeClient.Get(ctx, types.NamespacedName{Namespace: brokerNS, Name: expired.Name}, current); err != nil {
		t.Fatal(err)
	}
	if current.Status.Phase != userv1.CredentialLeaseExpired || controllerutil.ContainsFinalizer(current, userv1.CredentialLeaseFinalizer) {
		t.Fatalf("expired lease = %#v, finalizers = %#v", current.Status, current.Finalizers)
	}
	for _, object := range []client.Object{expiredBinding, expiredSA, expiredSecret} {
		key := client.ObjectKeyFromObject(object)
		if err := fakeClient.Get(ctx, key, object.DeepCopyObject().(client.Object)); !apierrors.IsNotFound(err) {
			t.Fatalf("resource %s/%s lookup error = %v, want NotFound", key.Namespace, key.Name, err)
		}
	}

	result, err = reconciler.Reconcile(ctx, requestFor(brokerNS, future.Name))
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter <= 0 || result.RequeueAfter > 30*time.Second {
		t.Fatalf("future lease result = %#v, want bounded requeue", result)
	}
	if err := fakeClient.Get(ctx, types.NamespacedName{Namespace: brokerNS, Name: future.Name}, current); err != nil {
		t.Fatal(err)
	}
	if current.Status.Phase != userv1.CredentialLeaseIssued || !controllerutil.ContainsFinalizer(current, userv1.CredentialLeaseFinalizer) {
		t.Fatalf("future lease changed unexpectedly: %#v finalizers=%#v", current.Status, current.Finalizers)
	}
}

func TestConsumedLeaseExpiresAfterResponseLoss(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	user := activeUser()
	lease := elevatedLease()
	lease.Name = "consumed-response-lost"
	lease.Finalizers = []string{userv1.CredentialLeaseFinalizer}
	lease.Status.Phase = userv1.CredentialLeasePrepared
	lease.Status.ConsumedAt = ptr.To(metav1.NewTime(now.Add(-time.Hour - time.Second)))
	binding, sa, secret := temporaryLeaseResources(lease, user)
	fakeClient := newFakeClient(t, user, lease, binding, sa, secret)
	reconciler := &controllers.CredentialLeaseReconciler{Client: fakeClient, UserNamespace: userNS, BrokerNamespace: brokerNS, Now: func() time.Time { return now }}

	result, err := reconciler.Reconcile(ctx, requestFor(brokerNS, lease.Name))
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != 0 {
		t.Fatalf("response-loss result = %#v, want no requeue", result)
	}
	current := &userv1.CredentialLease{}
	if err := fakeClient.Get(ctx, types.NamespacedName{Namespace: brokerNS, Name: lease.Name}, current); err != nil {
		t.Fatal(err)
	}
	if current.Status.Phase != userv1.CredentialLeaseExpired || controllerutil.ContainsFinalizer(current, userv1.CredentialLeaseFinalizer) {
		t.Fatalf("response-loss lease = %#v, finalizers = %#v", current.Status, current.Finalizers)
	}
	for _, object := range []client.Object{binding, sa, secret} {
		key := client.ObjectKeyFromObject(object)
		if err := fakeClient.Get(ctx, key, object.DeepCopyObject().(client.Object)); !apierrors.IsNotFound(err) {
			t.Fatalf("resource %s/%s lookup error = %v, want NotFound", key.Namespace, key.Name, err)
		}
	}

	future := elevatedLease()
	future.Name = "consumed-before-deadline"
	future.Finalizers = []string{userv1.CredentialLeaseFinalizer}
	future.Status.Phase = userv1.CredentialLeasePrepared
	future.Status.ConsumedAt = ptr.To(metav1.NewTime(now.Add(-10 * time.Minute)))
	if err := fakeClient.Create(ctx, future); err != nil {
		t.Fatal(err)
	}
	result, err = reconciler.Reconcile(ctx, requestFor(brokerNS, future.Name))
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter <= 0 || result.RequeueAfter > 30*time.Second {
		t.Fatalf("future consumed result = %#v, want bounded requeue", result)
	}
}

func TestApprovedLeaseWithEmptyPhaseInitializesAwaitingRedemption(t *testing.T) {
	ctx := context.Background()
	lease := elevatedLease()
	lease.Status = userv1.CredentialLeaseStatus{}
	fakeClient := newFakeClient(t, lease)
	reconciler := &controllers.CredentialLeaseReconciler{Client: fakeClient, UserNamespace: userNS, BrokerNamespace: brokerNS}

	if _, err := reconciler.Reconcile(ctx, requestFor(brokerNS, lease.Name)); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, requestFor(brokerNS, lease.Name)); err != nil {
		t.Fatal(err)
	}
	current := &userv1.CredentialLease{}
	if err := fakeClient.Get(ctx, types.NamespacedName{Namespace: brokerNS, Name: lease.Name}, current); err != nil {
		t.Fatal(err)
	}
	if current.Status.Phase != userv1.CredentialLeaseAwaitingRedemption || current.Status.ServiceAccountRef != nil || current.Status.BoundSecretRef != nil || current.Status.BindingRef != nil {
		t.Fatalf("initialized approval lease = %#v", current.Status)
	}
	serviceAccounts := &corev1.ServiceAccountList{}
	if err := fakeClient.List(ctx, serviceAccounts); err != nil {
		t.Fatal(err)
	}
	if len(serviceAccounts.Items) != 0 {
		t.Fatalf("empty-phase lease created %d ServiceAccounts", len(serviceAccounts.Items))
	}
}

func TestLeaseRejectsInvalidTargetAndProfile(t *testing.T) {
	tests := []struct {
		name        string
		user        *userv1.InternalUser
		mutateLease func(*userv1.CredentialLease)
		wantResult  bool
		wantPhase   userv1.CredentialLeasePhase
	}{
		{name: "missing target", mutateLease: func(_ *userv1.CredentialLease) {}, wantResult: true},
		{name: "inactive target", user: func() *userv1.InternalUser {
			user := activeUser()
			user.Status.Phase = userv1.InternalUserPending
			return user
		}()},
		{name: "identity mismatch", user: func() *userv1.InternalUser {
			user := activeUser()
			user.Spec.Identity.Subject = "different"
			return user
		}()},
		{name: "suspended target", user: func() *userv1.InternalUser { user := activeUser(); user.Spec.Suspend = true; return user }(), wantPhase: userv1.CredentialLeaseRevoked},
		{name: "invalid requester", mutateLease: func(lease *userv1.CredentialLease) { lease.Spec.Requester.Issuer = "http://invalid.example" }, wantPhase: userv1.CredentialLeaseFailed},
		{name: "invalid target", mutateLease: func(lease *userv1.CredentialLease) { lease.Spec.Target.Issuer = "http://invalid.example" }, wantPhase: userv1.CredentialLeaseFailed},
		{name: "invalid TTL", mutateLease: func(lease *userv1.CredentialLease) { lease.Spec.RequestedTTLSeconds = 60 }, wantPhase: userv1.CredentialLeaseFailed},
		{name: "missing approval ID", mutateLease: func(lease *userv1.CredentialLease) { lease.Spec.ApprovalID = "" }, wantPhase: userv1.CredentialLeaseFailed},
		{name: "missing approval reference", mutateLease: func(lease *userv1.CredentialLease) { lease.Spec.ApprovalReference = "" }, wantPhase: userv1.CredentialLeaseFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			lease := elevatedLease()
			if tt.mutateLease != nil {
				tt.mutateLease(lease)
			}
			objects := []client.Object{lease}
			if tt.user != nil {
				objects = append(objects, tt.user)
			}
			fakeClient := newFakeClient(t, objects...)
			reconciler := &controllers.CredentialLeaseReconciler{Client: fakeClient, UserNamespace: userNS, BrokerNamespace: brokerNS}
			if _, err := reconciler.Reconcile(ctx, requestFor(brokerNS, lease.Name)); err != nil {
				t.Fatal(err)
			}
			result, err := reconciler.Reconcile(ctx, requestFor(brokerNS, lease.Name))
			if tt.wantResult && (err != nil || result.RequeueAfter <= 0) {
				t.Fatalf("missing-target result=%#v error=%v, want positive requeue", result, err)
			}
			current := &userv1.CredentialLease{}
			if getErr := fakeClient.Get(ctx, types.NamespacedName{Namespace: brokerNS, Name: lease.Name}, current); getErr != nil {
				t.Fatal(getErr)
			}
			if tt.wantPhase != "" && current.Status.Phase != tt.wantPhase {
				t.Fatalf("lease phase = %q, want %q", current.Status.Phase, tt.wantPhase)
			}
			if tt.wantPhase == userv1.CredentialLeaseFailed || tt.wantPhase == userv1.CredentialLeaseRevoked {
				if controllerutil.ContainsFinalizer(current, userv1.CredentialLeaseFinalizer) {
					t.Fatalf("terminal lease retained finalizer: %#v", current.Finalizers)
				}
			}
		})
	}
}

func TestLeaseRejectsExistingResourceShapeMismatch(t *testing.T) {
	tests := []struct {
		name      string
		profile   string
		resources func(*userv1.CredentialLease, *userv1.InternalUser) []client.Object
	}{
		{name: "temporary binding role", resources: func(lease *userv1.CredentialLease, user *userv1.InternalUser) []client.Object {
			binding, sa, secret := temporaryLeaseResources(lease, user)
			binding.RoleRef.Name = "unexpected-role"
			return []client.Object{sa, binding, secret}
		}},
		{name: "temporary binding subject", resources: func(lease *userv1.CredentialLease, user *userv1.InternalUser) []client.Object {
			binding, sa, secret := temporaryLeaseResources(lease, user)
			binding.Subjects[0].Name = "unexpected-subject"
			return []client.Object{sa, binding, secret}
		}},
		{name: "temporary ServiceAccount legacy token", resources: func(lease *userv1.CredentialLease, user *userv1.InternalUser) []client.Object {
			_, sa, _ := temporaryLeaseResources(lease, user)
			sa.Secrets = []corev1.ObjectReference{{Name: "legacy-token"}}
			return []client.Object{sa}
		}},
		{name: "bound Secret data", resources: func(lease *userv1.CredentialLease, user *userv1.InternalUser) []client.Object {
			binding, sa, secret := temporaryLeaseResources(lease, user)
			secret.Data = map[string][]byte{"unexpected": []byte("credential")}
			return []client.Object{sa, binding, secret}
		}},
		{name: "bound Secret UID", resources: func(lease *userv1.CredentialLease, user *userv1.InternalUser) []client.Object {
			binding, sa, secret := temporaryLeaseResources(lease, user)
			secret.UID = ""
			return []client.Object{sa, binding, secret}
		}},
		{name: "stable ServiceAccount automount", profile: userv1.BaseReadonlyProfile, resources: func(lease *userv1.CredentialLease, user *userv1.InternalUser) []client.Object {
			stable := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: user.Name, Namespace: userNS, Labels: internalUserLabels(user)}, AutomountServiceAccountToken: ptr.To(true)}
			return []client.Object{stable}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			user := activeUser()
			lease := elevatedLease()
			if tt.profile != "" {
				lease = baseLease()
			}
			objects := []client.Object{user, lease}
			if tt.profile == userv1.BaseReadonlyProfile {
				objects = append(objects, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: userNS}}, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: baseRoleName}})
			} else {
				objects = append(objects, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: internalcredentials.ClusterOpsWriteRoleName}})
			}
			objects = append(objects, tt.resources(lease, user)...)
			fakeClient := newFakeClient(t, objects...)
			events := record.NewFakeRecorder(2)
			reconciler := &controllers.CredentialLeaseReconciler{Client: fakeClient, UserNamespace: userNS, BrokerNamespace: brokerNS, Recorder: events}
			if _, err := reconciler.Reconcile(ctx, requestFor(brokerNS, lease.Name)); err != nil {
				t.Fatal(err)
			}
			if _, err := reconciler.Reconcile(ctx, requestFor(brokerNS, lease.Name)); err == nil {
				t.Fatal("resource shape mismatch was accepted")
			}
			select {
			case event := <-events.Events:
				if !strings.Contains(event, "CredentialLeaseResourceRejected") {
					t.Fatalf("event = %q, want resource rejection", event)
				}
			default:
				t.Fatal("resource mismatch did not emit a Warning Event")
			}
		})
	}
}

func baseLease() *userv1.CredentialLease {
	return &userv1.CredentialLease{
		ObjectMeta: metav1.ObjectMeta{Name: "base-lease", Namespace: brokerNS, UID: "base-lease-uid"},
		Spec: userv1.CredentialLeaseSpec{
			Target:              userv1.Identity{Issuer: issuer, Subject: subject},
			Requester:           userv1.Identity{Issuer: issuer, Subject: subject},
			Profile:             userv1.BaseReadonlyProfile,
			RequestedTTLSeconds: int64((30 * time.Minute) / time.Second),
		},
		Status: userv1.CredentialLeaseStatus{Phase: userv1.CredentialLeasePending},
	}
}

func temporaryLeaseResources(lease *userv1.CredentialLease, user *userv1.InternalUser) (*rbacv1.ClusterRoleBinding, *corev1.ServiceAccount, *corev1.Secret) {
	labels := map[string]string{
		userv1.ManagedByLabel:          "credential-lease-controller",
		userv1.CredentialLeaseLabel:    lease.Name,
		userv1.CredentialLeaseUIDLabel: string(lease.UID),
		userv1.InternalUserLabel:       user.Name,
	}
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: internalcredentials.ResourceName("internal-lease", brokerNS, lease.Name), Namespace: userNS, Labels: labels}, AutomountServiceAccountToken: ptr.To(false)}
	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: internalcredentials.ResourceName("internal-lease-binding", brokerNS, lease.Name), Labels: labels},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: internalcredentials.ClusterOpsWriteRoleName},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: sa.Name, Namespace: userNS}},
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: internalcredentials.ResourceName("internal-lease-secret", brokerNS, lease.Name), Namespace: userNS, Labels: labels, UID: "secret-uid"}, Type: corev1.SecretTypeOpaque}
	return binding, sa, secret
}
