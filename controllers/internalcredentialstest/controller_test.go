package internalcredentialstest

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	userv1 "github.com/labring-sigs/sealos-access/api/v1"
	"github.com/labring-sigs/sealos-access/controllers"
	"github.com/labring-sigs/sealos-access/pkg/internalcredentials"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	issuer       = "https://issuer.example"
	subject      = "alice"
	userUID      = types.UID("user-uid")
	brokerNS     = userv1.CredentialBrokerNamespace
	userNS       = userv1.InternalUserSystemNamespace
	baseRoleName = internalcredentials.BaseReadonlyRoleName
)

func TestStableAndElevatedResources(t *testing.T) {
	ctx := context.Background()
	user := activeUser()
	baseBinding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: internalcredentials.ResourceName("internal-user-base", "", user.Name), Labels: internalUserLabels(user)},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: baseRoleName},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: user.Name, Namespace: userNS}},
	}
	client := newFakeClient(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: userNS}},
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: baseRoleName}},
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: internalcredentials.ClusterOpsWriteRoleName}}, user, baseBinding)
	userReconciler := &controllers.InternalUserReconciler{Client: client, UserNamespace: userNS, BaseRoleName: baseRoleName}
	if _, err := userReconciler.Reconcile(ctx, request(user.Name)); err != nil {
		t.Fatal(err)
	}
	if _, err := userReconciler.Reconcile(ctx, request(user.Name)); err != nil {
		t.Fatal(err)
	}
	stable := &corev1.ServiceAccount{}
	if err := client.Get(ctx, types.NamespacedName{Name: user.Name, Namespace: userNS}, stable); err != nil {
		t.Fatal(err)
	}
	if stable.AutomountServiceAccountToken == nil || *stable.AutomountServiceAccountToken {
		t.Fatal("stable ServiceAccount automount is enabled")
	}

	lease := elevatedLease()
	if err := client.Create(ctx, lease); err != nil {
		t.Fatal(err)
	}
	leaseReconciler := &controllers.CredentialLeaseReconciler{Client: client, UserNamespace: userNS, BrokerNamespace: brokerNS, Now: func() time.Time { return time.Now() }}
	if _, err := leaseReconciler.Reconcile(ctx, requestFor(brokerNS, lease.Name)); err != nil {
		t.Fatal(err)
	}
	if _, err := leaseReconciler.Reconcile(ctx, requestFor(brokerNS, lease.Name)); err != nil {
		t.Fatal(err)
	}
	prepared := &userv1.CredentialLease{}
	if err := client.Get(ctx, types.NamespacedName{Name: lease.Name, Namespace: brokerNS}, prepared); err != nil {
		t.Fatal(err)
	}
	if prepared.Status.Phase != userv1.CredentialLeasePrepared {
		t.Fatalf("lease phase = %q, want Prepared", prepared.Status.Phase)
	}
	if prepared.Status.ServiceAccountRef == nil || prepared.Status.BindingRef == nil || prepared.Status.BoundSecretRef == nil {
		t.Fatal("prepared lease is missing resource references")
	}
	if prepared.Status.ServiceAccountRef.Name == user.Name {
		t.Fatal("elevated lease reused the stable ServiceAccount")
	}
	if prepared.Status.BoundSecretRef.UID == "" {
		t.Fatal("bound Secret UID was not recorded")
	}
	remainingBinding := &rbacv1.ClusterRoleBinding{}
	if err := client.Get(ctx, types.NamespacedName{Name: baseBinding.Name}, remainingBinding); err != nil {
		t.Fatal("elevated preparation removed the stable base binding")
	}
}

func TestAwaitingRedemptionDoesNotPrepareResourcesAndExpires(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	user := activeUser()
	expires := metav1.NewTime(now.Add(time.Hour))
	lease := elevatedLease()
	lease.Status = userv1.CredentialLeaseStatus{Phase: userv1.CredentialLeaseAwaitingRedemption, ApprovalExpirationTimestamp: &expires}
	fakeClient := newFakeClient(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: userNS}}, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: internalcredentials.ClusterOpsWriteRoleName}}, user, lease)
	reconciler := &controllers.CredentialLeaseReconciler{Client: fakeClient, UserNamespace: userNS, BrokerNamespace: brokerNS, Now: func() time.Time { return now }}
	if _, err := reconciler.Reconcile(ctx, requestFor(brokerNS, lease.Name)); err != nil {
		t.Fatal(err)
	}
	result, err := reconciler.Reconcile(ctx, requestFor(brokerNS, lease.Name))
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("waiting result = %#v, want positive requeue", result)
	}
	prepared := &userv1.CredentialLease{}
	if err := fakeClient.Get(ctx, types.NamespacedName{Namespace: brokerNS, Name: lease.Name}, prepared); err != nil {
		t.Fatal(err)
	}
	if prepared.Status.Phase != userv1.CredentialLeaseAwaitingRedemption || prepared.Status.ServiceAccountRef != nil || prepared.Status.BoundSecretRef != nil || prepared.Status.BindingRef != nil {
		t.Fatalf("awaiting lease was prepared: %#v", prepared.Status)
	}
	serviceAccounts := &corev1.ServiceAccountList{}
	if err := fakeClient.List(ctx, serviceAccounts, client.InNamespace(userNS)); err != nil {
		t.Fatal(err)
	}
	secrets := &corev1.SecretList{}
	if err := fakeClient.List(ctx, secrets, client.InNamespace(userNS)); err != nil {
		t.Fatal(err)
	}
	bindings := &rbacv1.ClusterRoleBindingList{}
	if err := fakeClient.List(ctx, bindings); err != nil {
		t.Fatal(err)
	}
	if len(serviceAccounts.Items) != 0 || len(secrets.Items) != 0 || len(bindings.Items) != 0 {
		t.Fatalf("awaiting lease created resources: serviceaccounts=%d secrets=%d bindings=%d", len(serviceAccounts.Items), len(secrets.Items), len(bindings.Items))
	}
	prepared.Status.ApprovalExpirationTimestamp = ptr.To(metav1.NewTime(now.Add(-time.Second)))
	if err := fakeClient.Status().Update(ctx, prepared); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, requestFor(brokerNS, lease.Name)); err != nil {
		t.Fatal(err)
	}
	if err := fakeClient.Get(ctx, types.NamespacedName{Namespace: brokerNS, Name: lease.Name}, prepared); err != nil {
		t.Fatal(err)
	}
	if prepared.Status.Phase != userv1.CredentialLeaseExpired || controllerutil.ContainsFinalizer(prepared, userv1.CredentialLeaseFinalizer) {
		t.Fatalf("expired awaiting lease = %#v, finalizers = %#v", prepared.Status, prepared.Finalizers)
	}
}

func TestOwnerMismatchFailsClosed(t *testing.T) {
	ctx := context.Background()
	user := activeUser()
	lease := elevatedLease()
	wrongBinding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:   internalcredentials.ResourceName("internal-lease-binding", brokerNS, lease.Name),
			Labels: map[string]string{userv1.ManagedByLabel: "someone-else"},
		},
	}
	client := newFakeClient(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: userNS}},
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: internalcredentials.ClusterOpsWriteRoleName}}, user, lease, wrongBinding)
	events := record.NewFakeRecorder(2)
	r := &controllers.CredentialLeaseReconciler{Client: client, UserNamespace: userNS, BrokerNamespace: brokerNS, Recorder: events}
	if _, err := r.Reconcile(ctx, requestFor(brokerNS, lease.Name)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, requestFor(brokerNS, lease.Name)); err == nil {
		t.Fatal("owner mismatch was adopted")
	}
	unchanged := &rbacv1.ClusterRoleBinding{}
	if err := client.Get(ctx, types.NamespacedName{Name: wrongBinding.Name}, unchanged); err != nil {
		t.Fatal(err)
	}
	if unchanged.Labels[userv1.ManagedByLabel] != "someone-else" {
		t.Fatal("owner-mismatched binding was modified")
	}
	select {
	case event := <-events.Events:
		if !strings.Contains(event, "CredentialLeaseResourceRejected") || !strings.Contains(event, wrongBinding.Name) {
			t.Fatalf("ownership event = %q, want actionable resource rejection", event)
		}
	default:
		t.Fatal("ownership mismatch did not emit an event")
	}
}

func TestServiceAccountAndSecretOwnerMismatchFailClosed(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(map[string]string)
		withOthers bool
	}{
		{name: "service account", mutate: func(labels map[string]string) { labels[userv1.ManagedByLabel] = "someone-else" }},
		{name: "secret", mutate: func(labels map[string]string) { labels[userv1.CredentialLeaseUIDLabel] = "wrong-uid" }, withOthers: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			user := activeUser()
			lease := elevatedLease()
			lease.UID = types.UID("lease-uid")
			labels := map[string]string{
				userv1.ManagedByLabel:          "credential-lease-controller",
				userv1.CredentialLeaseLabel:    lease.Name,
				userv1.CredentialLeaseUIDLabel: string(lease.UID),
				userv1.InternalUserLabel:       user.Name,
			}
			mutated := map[string]string{}
			for key, value := range labels {
				mutated[key] = value
			}
			tt.mutate(mutated)
			sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
				Name:      internalcredentials.ResourceName("internal-lease", brokerNS, lease.Name),
				Namespace: userNS,
				Labels:    labels,
			}, AutomountServiceAccountToken: ptr.To(false)}
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Name:      internalcredentials.ResourceName("internal-lease-secret", brokerNS, lease.Name),
				Namespace: userNS,
				Labels:    labels,
			}, Type: corev1.SecretTypeOpaque}
			binding := &rbacv1.ClusterRoleBinding{
				ObjectMeta: metav1.ObjectMeta{
					Name:   internalcredentials.ResourceName("internal-lease-binding", brokerNS, lease.Name),
					Labels: labels,
				},
				RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: internalcredentials.ClusterOpsWriteRoleName},
				Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: sa.Name, Namespace: userNS}},
			}
			objects := []client.Object{&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: userNS}}, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: internalcredentials.ClusterOpsWriteRoleName}}, user, lease}
			if tt.name == "service account" {
				sa.Labels = mutated
				objects = append(objects, sa)
			} else {
				secret.Labels = mutated
				objects = append(objects, sa, binding, secret)
			}
			client := newFakeClient(t, objects...)
			events := record.NewFakeRecorder(2)
			r := &controllers.CredentialLeaseReconciler{Client: client, UserNamespace: userNS, BrokerNamespace: brokerNS, Recorder: events}
			if _, err := r.Reconcile(ctx, requestFor(brokerNS, lease.Name)); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Reconcile(ctx, requestFor(brokerNS, lease.Name)); err == nil {
				t.Fatal("owner mismatch was accepted")
			}
			select {
			case event := <-events.Events:
				if !strings.Contains(event, "CredentialLeaseResourceRejected") {
					t.Fatalf("ownership event = %q, want resource rejection", event)
				}
			default:
				t.Fatal("ownership mismatch did not emit an event")
			}
		})
	}
}

func TestCleanupOrder(t *testing.T) {
	ctx := context.Background()
	user := activeUser()
	lease := elevatedLease()
	lease.Finalizers = []string{userv1.CredentialLeaseFinalizer}
	lease.Status.Phase = userv1.CredentialLeaseRevoked
	leaseUID := types.UID(uuid.NewUUID())
	lease.UID = leaseUID
	labels := map[string]string{
		userv1.ManagedByLabel:          "credential-lease-controller",
		userv1.CredentialLeaseLabel:    lease.Name,
		userv1.CredentialLeaseUIDLabel: string(leaseUID),
		userv1.InternalUserLabel:       user.Name,
	}
	binding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: internalcredentials.ResourceName("internal-lease-binding", brokerNS, lease.Name), Labels: labels}}
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: internalcredentials.ResourceName("internal-lease", brokerNS, lease.Name), Namespace: userNS, Labels: labels}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: internalcredentials.ResourceName("internal-lease-secret", brokerNS, lease.Name), Namespace: userNS, Labels: labels}}
	var deleted []string
	base := fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(lease, user).WithObjects(user, lease, binding, sa, secret).WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			deleted = append(deleted, objectKind(obj)+"/"+obj.GetName())
			return c.Delete(ctx, obj, opts...)
		},
	}).Build()
	r := &controllers.CredentialLeaseReconciler{Client: base, UserNamespace: userNS, BrokerNamespace: brokerNS}
	if _, err := r.Reconcile(ctx, requestFor(brokerNS, lease.Name)); err != nil {
		t.Fatal(err)
	}
	want := []string{"ClusterRoleBinding/" + binding.Name, "Secret/" + secret.Name, "ServiceAccount/" + sa.Name}
	if !reflect.DeepEqual(deleted, want) {
		t.Fatalf("delete order = %#v, want %#v", deleted, want)
	}
}

func TestCleanupFailureRetainsFinalizer(t *testing.T) {
	ctx := context.Background()
	user := activeUser()
	lease := elevatedLease()
	lease.Finalizers = []string{userv1.CredentialLeaseFinalizer}
	lease.Status.Phase = userv1.CredentialLeaseRevoked
	lease.UID = types.UID("lease-uid")
	labels := map[string]string{
		userv1.ManagedByLabel:          "credential-lease-controller",
		userv1.CredentialLeaseLabel:    lease.Name,
		userv1.CredentialLeaseUIDLabel: string(lease.UID),
		userv1.InternalUserLabel:       user.Name,
	}
	binding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: internalcredentials.ResourceName("internal-lease-binding", brokerNS, lease.Name), Labels: labels}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: internalcredentials.ResourceName("internal-lease-secret", brokerNS, lease.Name), Namespace: userNS, Labels: labels}}
	client := fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(lease, user).WithObjects(user, lease, binding, secret).WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.DeleteOption) error {
			if _, ok := obj.(*rbacv1.ClusterRoleBinding); ok {
				return errors.New("simulated cleanup failure")
			}
			return nil
		},
	}).Build()
	r := &controllers.CredentialLeaseReconciler{Client: client, UserNamespace: userNS, BrokerNamespace: brokerNS}
	result, err := r.Reconcile(ctx, requestFor(brokerNS, lease.Name))
	if err == nil || !strings.Contains(err.Error(), "simulated cleanup failure") {
		t.Fatalf("cleanup error = %v, want simulated failure", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("requeueAfter = %s, want positive retry delay", result.RequeueAfter)
	}
	current := &userv1.CredentialLease{}
	if err := client.Get(ctx, types.NamespacedName{Namespace: brokerNS, Name: lease.Name}, current); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(current, userv1.CredentialLeaseFinalizer) {
		t.Fatalf("finalizers = %v, want cleanup finalizer retained", current.Finalizers)
	}
}

func TestTerminalLeaseWithoutFinalizerIsNotRecreated(t *testing.T) {
	ctx := context.Background()
	lease := elevatedLease()
	lease.Status.Phase = userv1.CredentialLeaseRevoked
	client := newFakeClient(t, lease)
	r := &controllers.CredentialLeaseReconciler{Client: client, UserNamespace: userNS, BrokerNamespace: brokerNS}

	if _, err := r.Reconcile(ctx, requestFor(brokerNS, lease.Name)); err != nil {
		t.Fatal(err)
	}
	current := &userv1.CredentialLease{}
	if err := client.Get(ctx, types.NamespacedName{Namespace: brokerNS, Name: lease.Name}, current); err != nil {
		t.Fatal(err)
	}
	if len(current.Finalizers) != 0 {
		t.Fatalf("terminal lease finalizers = %v, want none", current.Finalizers)
	}
}

func activeUser() *userv1.InternalUser {
	return &userv1.InternalUser{
		ObjectMeta: metav1.ObjectMeta{Name: internalcredentials.DeriveInternalUserName(issuer, subject), UID: userUID},
		Spec:       userv1.InternalUserSpec{Identity: userv1.Identity{Issuer: issuer, Subject: subject}, RoleProfile: userv1.BaseReadonlyProfile},
		Status:     userv1.InternalUserStatus{Phase: userv1.InternalUserActive},
	}
}

func elevatedLease() *userv1.CredentialLease {
	return &userv1.CredentialLease{ObjectMeta: metav1.ObjectMeta{Name: "lease-test", Namespace: brokerNS, UID: "lease-uid"}, Spec: userv1.CredentialLeaseSpec{
		Target: userv1.Identity{Issuer: issuer, Subject: subject}, Requester: userv1.Identity{Issuer: issuer, Subject: "admin"}, Profile: userv1.ClusterOpsWriteProfile, RequestedTTLSeconds: int64((30 * time.Minute) / time.Second), ApprovalReference: "r1.approval-00000000000000000000000000000000.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", ApprovalID: "approval-1", ApprovalReason: "test approval",
	}, Status: userv1.CredentialLeaseStatus{Phase: userv1.CredentialLeasePending}}
}

func newFakeClient(t *testing.T, objects ...client.Object) client.WithWatch {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(&userv1.InternalUser{}, &userv1.CredentialLease{}).WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if secret, ok := obj.(*corev1.Secret); ok && secret.UID == "" {
				secret.UID = types.UID("secret-uid")
			}
			return c.Create(ctx, obj, opts...)
		},
	}).Build()
}

func testScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = rbacv1.AddToScheme(scheme)
	_ = userv1.AddToScheme(scheme)
	return scheme
}

func request(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}
}

func requestFor(namespace, name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: name}}
}

func internalUserLabels(user *userv1.InternalUser) map[string]string {
	return map[string]string{userv1.ManagedByLabel: "internal-user-controller", userv1.InternalUserLabel: user.Name, userv1.InternalUserUIDLabel: string(user.UID)}
}

func objectKind(obj client.Object) string {
	switch obj.(type) {
	case *rbacv1.ClusterRoleBinding:
		return "ClusterRoleBinding"
	case *corev1.Secret:
		return "Secret"
	case *corev1.ServiceAccount:
		return "ServiceAccount"
	default:
		return "Unknown"
	}
}
