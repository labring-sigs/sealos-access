package internalcredentialstest

import (
	"context"
	"errors"
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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func TestInternalUserReconcileCreatesStableResources(t *testing.T) {
	ctx := context.Background()
	user := activeUser()
	fakeClient := newFakeClient(t,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: userNS}},
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: baseRoleName}},
		user,
	)
	reconciler := &controllers.InternalUserReconciler{Client: fakeClient, UserNamespace: userNS, BaseRoleName: baseRoleName}

	if _, err := reconciler.Reconcile(ctx, request(user.Name)); err != nil {
		t.Fatal(err)
	}

	stable := &corev1.ServiceAccount{}
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: user.Name, Namespace: userNS}, stable); err != nil {
		t.Fatal(err)
	}
	if stable.AutomountServiceAccountToken == nil || *stable.AutomountServiceAccountToken {
		t.Fatal("stable ServiceAccount automount is enabled")
	}
	if stable.Labels[userv1.ManagedByLabel] != "internal-user-controller" || stable.Labels[userv1.InternalUserLabel] != user.Name || stable.Labels[userv1.InternalUserUIDLabel] != string(user.UID) {
		t.Fatalf("stable ServiceAccount labels = %#v", stable.Labels)
	}

	binding := &rbacv1.ClusterRoleBinding{}
	bindingName := internalcredentials.ResourceName("internal-user-base", "", user.Name)
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: bindingName}, binding); err != nil {
		t.Fatal(err)
	}
	if binding.RoleRef != (rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: baseRoleName}) {
		t.Fatalf("base binding roleRef = %#v", binding.RoleRef)
	}
	if len(binding.Subjects) != 1 || binding.Subjects[0].Name != user.Name || binding.Subjects[0].Namespace != userNS || binding.Subjects[0].Kind != "ServiceAccount" {
		t.Fatalf("base binding subjects = %#v", binding.Subjects)
	}
	if binding.Labels[userv1.InternalUserUIDLabel] != string(user.UID) {
		t.Fatalf("base binding labels = %#v", binding.Labels)
	}

	current := &userv1.InternalUser{}
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: user.Name}, current); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(current, userv1.InternalUserFinalizer) || current.Status.Phase != userv1.InternalUserActive || current.Status.StableServiceAccountRef == nil || current.Status.StableServiceAccountRef.Name != user.Name {
		t.Fatalf("InternalUser state = %#v, finalizers = %#v", current.Status, current.Finalizers)
	}
	if condition := internalUserCondition(current.Status.Conditions, "Ready"); condition == nil || condition.Status != metav1.ConditionTrue {
		t.Fatalf("Ready condition = %#v", condition)
	}

	if _, err := reconciler.Reconcile(ctx, request(user.Name)); err != nil {
		t.Fatal(err)
	}
	serviceAccounts := &corev1.ServiceAccountList{}
	if err := fakeClient.List(ctx, serviceAccounts, client.InNamespace(userNS)); err != nil {
		t.Fatal(err)
	}
	bindings := &rbacv1.ClusterRoleBindingList{}
	if err := fakeClient.List(ctx, bindings); err != nil {
		t.Fatal(err)
	}
	if len(serviceAccounts.Items) != 1 || len(bindings.Items) != 1 {
		t.Fatalf("second reconcile created duplicates: serviceaccounts=%d bindings=%d", len(serviceAccounts.Items), len(bindings.Items))
	}
}

func TestInternalUserSuspendRevokesLeasesAndRemovesBaseBinding(t *testing.T) {
	ctx := context.Background()
	user := activeUser()
	user.Spec.Suspend = true
	user.Finalizers = []string{userv1.InternalUserFinalizer}
	baseBinding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: internalcredentials.ResourceName("internal-user-base", "", user.Name), Labels: internalUserLabels(user)},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: baseRoleName},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: user.Name, Namespace: userNS}},
	}
	inFlight := elevatedLease()
	inFlight.Status.Phase = userv1.CredentialLeasePrepared
	terminal := elevatedLease()
	terminal.Name = "terminal-lease"
	terminal.Status.Phase = userv1.CredentialLeaseExpired
	fakeClient := newFakeClient(t,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: userNS}},
		user, baseBinding, inFlight, terminal,
	)
	reconciler := &controllers.InternalUserReconciler{Client: fakeClient, UserNamespace: userNS, BaseRoleName: baseRoleName}

	result, err := reconciler.Reconcile(ctx, request(user.Name))
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("suspend result = %#v, want requeue", result)
	}

	currentLease := &userv1.CredentialLease{}
	if err := fakeClient.Get(ctx, types.NamespacedName{Namespace: brokerNS, Name: inFlight.Name}, currentLease); err != nil {
		t.Fatal(err)
	}
	if currentLease.Status.Phase != userv1.CredentialLeaseRevoked {
		t.Fatalf("in-flight lease phase = %q, want Revoked", currentLease.Status.Phase)
	}
	if condition := internalUserCondition(currentLease.Status.Conditions, "Ready"); condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "InternalUserSuspended" {
		t.Fatalf("revoked lease condition = %#v", currentLease.Status.Conditions)
	}
	if err := fakeClient.Get(ctx, types.NamespacedName{Namespace: brokerNS, Name: terminal.Name}, currentLease); err != nil {
		t.Fatal(err)
	}
	if currentLease.Status.Phase != userv1.CredentialLeaseExpired {
		t.Fatalf("terminal lease phase = %q, want unchanged Expired", currentLease.Status.Phase)
	}

	deletedBinding := &rbacv1.ClusterRoleBinding{}
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: baseBinding.Name}, deletedBinding); !apierrors.IsNotFound(err) {
		t.Fatalf("base binding lookup error = %v, want NotFound", err)
	}
	currentUser := &userv1.InternalUser{}
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: user.Name}, currentUser); err != nil {
		t.Fatal(err)
	}
	if currentUser.Status.Phase != userv1.InternalUserSuspended {
		t.Fatalf("user phase = %q, want Suspended", currentUser.Status.Phase)
	}
}

func TestInternalUserDeletionWaitsForLeaseFinalizers(t *testing.T) {
	ctx := context.Background()
	user := activeUser()
	user.Finalizers = []string{userv1.InternalUserFinalizer}
	deletionTime := metav1.NewTime(time.Now().UTC())
	user.DeletionTimestamp = &deletionTime
	baseBinding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: internalcredentials.ResourceName("internal-user-base", "", user.Name), Labels: internalUserLabels(user)},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: baseRoleName},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: user.Name, Namespace: userNS}},
	}
	stable := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: user.Name, Namespace: userNS, Labels: internalUserLabels(user)}}
	lease := elevatedLease()
	lease.Finalizers = []string{userv1.CredentialLeaseFinalizer}
	removedFinalizer := false
	fakeClient := fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(&userv1.InternalUser{}, &userv1.CredentialLease{}).WithObjects(user, baseBinding, stable, lease).WithInterceptorFuncs(interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.UpdateOption) error {
			if current, ok := object.(*userv1.InternalUser); ok && !controllerutil.ContainsFinalizer(current, userv1.InternalUserFinalizer) {
				removedFinalizer = true
			}
			return c.Update(ctx, object, opts...)
		},
	}).Build()
	reconciler := &controllers.InternalUserReconciler{Client: fakeClient, UserNamespace: userNS, BaseRoleName: baseRoleName}

	result, err := reconciler.Reconcile(ctx, request(user.Name))
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != time.Second {
		t.Fatalf("deletion wait result = %#v, want one-second requeue", result)
	}
	currentLease := &userv1.CredentialLease{}
	if err := fakeClient.Get(ctx, types.NamespacedName{Namespace: brokerNS, Name: lease.Name}, currentLease); err != nil {
		t.Fatal(err)
	}
	currentLease.Finalizers = nil
	if err := fakeClient.Update(ctx, currentLease); err != nil {
		t.Fatal(err)
	}

	if _, err := reconciler.Reconcile(ctx, request(user.Name)); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request(user.Name)); err != nil {
		t.Fatal(err)
	}
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: baseBinding.Name}, &rbacv1.ClusterRoleBinding{}); !apierrors.IsNotFound(err) {
		t.Fatalf("base binding lookup error = %v, want NotFound", err)
	}
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: stable.Name, Namespace: stable.Namespace}, &corev1.ServiceAccount{}); !apierrors.IsNotFound(err) {
		t.Fatalf("stable ServiceAccount lookup error = %v, want NotFound", err)
	}
	if !removedFinalizer {
		t.Fatal("InternalUser finalizer was not removed")
	}
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: user.Name}, &userv1.InternalUser{}); !apierrors.IsNotFound(err) {
		t.Fatalf("InternalUser lookup error = %v, want deletion after finalizer removal", err)
	}
}

func TestInternalUserRejectsInvalidIdentityAndOwnership(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*userv1.InternalUser)
		want   string
	}{
		{name: "invalid profile", mutate: func(user *userv1.InternalUser) { user.Spec.RoleProfile = "cluster-ops-write.v1" }, want: "roleProfile"},
		{name: "invalid issuer", mutate: func(user *userv1.InternalUser) { user.Spec.Identity.Issuer = "http://issuer.example" }, want: "issuer"},
		{name: "invalid derived name", mutate: func(user *userv1.InternalUser) { user.Name = "not-the-derived-name" }, want: "InternalUser name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			user := activeUser()
			tt.mutate(user)
			fakeClient := newFakeClient(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: userNS}}, user)
			reconciler := &controllers.InternalUserReconciler{Client: fakeClient, UserNamespace: userNS, BaseRoleName: baseRoleName}
			_, err := reconciler.Reconcile(ctx, request(user.Name))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("reconcile error = %v, want %q", err, tt.want)
			}
			current := &userv1.InternalUser{}
			if getErr := fakeClient.Get(ctx, types.NamespacedName{Name: user.Name}, current); getErr != nil {
				t.Fatal(getErr)
			}
			if current.Status.Phase != userv1.InternalUserFailed {
				t.Fatalf("status phase = %q, want Failed", current.Status.Phase)
			}
			serviceAccounts := &corev1.ServiceAccountList{}
			if err := fakeClient.List(ctx, serviceAccounts, client.InNamespace(userNS)); err != nil {
				t.Fatal(err)
			}
			if len(serviceAccounts.Items) != 0 {
				t.Fatalf("invalid user created %d ServiceAccounts", len(serviceAccounts.Items))
			}
		})
	}

	for _, object := range []struct {
		name string
		obj  client.Object
	}{
		{name: "service account", obj: &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: activeUser().Name, Namespace: userNS, Labels: map[string]string{userv1.ManagedByLabel: "another-controller", userv1.InternalUserLabel: activeUser().Name}}}},
		{name: "binding", obj: &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: internalcredentials.ResourceName("internal-user-base", "", activeUser().Name), Labels: map[string]string{userv1.ManagedByLabel: "internal-user-controller", userv1.InternalUserLabel: activeUser().Name, userv1.InternalUserUIDLabel: "wrong-uid"}}}},
	} {
		t.Run("ownership "+object.name, func(t *testing.T) {
			ctx := context.Background()
			user := activeUser()
			fakeClient := newFakeClient(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: userNS}}, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: baseRoleName}}, user, object.obj)
			reconciler := &controllers.InternalUserReconciler{Client: fakeClient, UserNamespace: userNS, BaseRoleName: baseRoleName}
			if _, err := reconciler.Reconcile(ctx, request(user.Name)); err != nil {
				if !strings.Contains(err.Error(), "ownership") && !strings.Contains(err.Error(), "UID") {
					t.Fatalf("reconcile error = %v", err)
				}
			} else {
				t.Fatal("ownership mismatch was adopted")
			}
		})
	}
}

func TestInternalUserRejectsUnsafeStableAccount(t *testing.T) {
	tests := []struct {
		name string
		obj  client.Object
	}{
		{name: "legacy token reference", obj: func() client.Object {
			user := activeUser()
			return &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: user.Name, Namespace: userNS, Labels: internalUserLabels(user)}, Secrets: []corev1.ObjectReference{{Name: "legacy-token"}}}
		}()},
		{name: "unexpected base role", obj: func() client.Object {
			user := activeUser()
			return &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: internalcredentials.ResourceName("internal-user-base", "", user.Name), Labels: internalUserLabels(user)}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "another-role"}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: user.Name, Namespace: userNS}}}
		}()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			user := activeUser()
			fakeClient := newFakeClient(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: userNS}}, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: baseRoleName}}, user, tt.obj)
			reconciler := &controllers.InternalUserReconciler{Client: fakeClient, UserNamespace: userNS, BaseRoleName: baseRoleName}
			if _, err := reconciler.Reconcile(ctx, request(user.Name)); err == nil {
				t.Fatal("unsafe stable resource was accepted")
			}
		})
	}
}

func TestInternalUserErrorPaths(t *testing.T) {
	t.Run("missing namespace", func(t *testing.T) {
		ctx := context.Background()
		user := activeUser()
		fakeClient := newFakeClient(t, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: baseRoleName}}, user)
		reconciler := &controllers.InternalUserReconciler{Client: fakeClient, UserNamespace: userNS, BaseRoleName: baseRoleName}
		if _, err := reconciler.Reconcile(ctx, request(user.Name)); err == nil || !strings.Contains(err.Error(), "namespace") {
			t.Fatalf("reconcile error = %v, want namespace error", err)
		}
	})

	t.Run("missing base role", func(t *testing.T) {
		ctx := context.Background()
		user := activeUser()
		fakeClient := newFakeClient(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: userNS}}, user)
		reconciler := &controllers.InternalUserReconciler{Client: fakeClient, UserNamespace: userNS, BaseRoleName: baseRoleName}
		if _, err := reconciler.Reconcile(ctx, request(user.Name)); err == nil || !strings.Contains(err.Error(), "ClusterRole") {
			t.Fatalf("reconcile error = %v, want ClusterRole error", err)
		}
	})

	t.Run("lease list failure", func(t *testing.T) {
		ctx := context.Background()
		user := activeUser()
		user.Spec.Suspend = true
		fakeClient := fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(&userv1.InternalUser{}, &userv1.CredentialLease{}).WithObjects(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: userNS}}, user).WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, _ ...client.ListOption) error {
				if _, ok := list.(*userv1.CredentialLeaseList); ok {
					return errors.New("simulated lease list failure")
				}
				return nil
			},
		}).Build()
		reconciler := &controllers.InternalUserReconciler{Client: fakeClient, UserNamespace: userNS, BaseRoleName: baseRoleName}
		if _, err := reconciler.Reconcile(ctx, request(user.Name)); err == nil || !strings.Contains(err.Error(), "lease list failure") {
			t.Fatalf("reconcile error = %v, want lease list failure", err)
		}
	})

	t.Run("finalizer update failure", func(t *testing.T) {
		ctx := context.Background()
		user := activeUser()
		fakeClient := fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(&userv1.InternalUser{}).WithObjects(user).WithInterceptorFuncs(interceptor.Funcs{
			Update: func(_ context.Context, _ client.WithWatch, object client.Object, _ ...client.UpdateOption) error {
				if _, ok := object.(*userv1.InternalUser); ok && controllerutil.ContainsFinalizer(object, userv1.InternalUserFinalizer) {
					return errors.New("simulated finalizer update failure")
				}
				return nil
			},
		}).Build()
		reconciler := &controllers.InternalUserReconciler{Client: fakeClient, UserNamespace: userNS, BaseRoleName: baseRoleName}
		if _, err := reconciler.Reconcile(ctx, request(user.Name)); err == nil || !strings.Contains(err.Error(), "finalizer update failure") {
			t.Fatalf("reconcile error = %v, want finalizer update failure", err)
		}
	})

	t.Run("status update failure is returned on success", func(t *testing.T) {
		ctx := context.Background()
		user := activeUser()
		fakeClient := fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(&userv1.InternalUser{}).WithObjects(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: userNS}}, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: baseRoleName}}, user).WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(_ context.Context, _ client.Client, subResourceName string, _ client.Object, _ ...client.SubResourceUpdateOption) error {
				if subResourceName == "status" {
					return errors.New("simulated status update failure")
				}
				return nil
			},
		}).Build()
		reconciler := &controllers.InternalUserReconciler{Client: fakeClient, UserNamespace: userNS, BaseRoleName: baseRoleName}
		if _, err := reconciler.Reconcile(ctx, request(user.Name)); err == nil || !strings.Contains(err.Error(), "status update failure") {
			t.Fatalf("reconcile error = %v, want status update failure", err)
		}
	})

	t.Run("failure status update does not mask validation error", func(t *testing.T) {
		ctx := context.Background()
		user := activeUser()
		user.Spec.RoleProfile = "cluster-ops-write.v1"
		fakeClient := fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(&userv1.InternalUser{}).WithObjects(user).WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(_ context.Context, _ client.Client, subResourceName string, _ client.Object, _ ...client.SubResourceUpdateOption) error {
				if subResourceName == "status" {
					return errors.New("simulated failure status update")
				}
				return nil
			},
		}).Build()
		reconciler := &controllers.InternalUserReconciler{Client: fakeClient, UserNamespace: userNS, BaseRoleName: baseRoleName}
		if _, err := reconciler.Reconcile(ctx, request(user.Name)); err == nil || !strings.Contains(err.Error(), "roleProfile") || strings.Contains(err.Error(), "failure status update") {
			t.Fatalf("reconcile error = %v, want original validation error", err)
		}
	})

	t.Run("delete failure", func(t *testing.T) {
		ctx := context.Background()
		user := activeUser()
		user.Finalizers = []string{userv1.InternalUserFinalizer}
		deletionTime := metav1.NewTime(time.Now().UTC())
		user.DeletionTimestamp = &deletionTime
		binding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: internalcredentials.ResourceName("internal-user-base", "", user.Name), Labels: internalUserLabels(user)}}
		fakeClient := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(user, binding).WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(_ context.Context, _ client.WithWatch, object client.Object, _ ...client.DeleteOption) error {
				if _, ok := object.(*rbacv1.ClusterRoleBinding); ok {
					return errors.New("simulated binding delete failure")
				}
				return nil
			},
		}).Build()
		reconciler := &controllers.InternalUserReconciler{Client: fakeClient, UserNamespace: userNS, BaseRoleName: baseRoleName}
		if _, err := reconciler.Reconcile(ctx, request(user.Name)); err == nil || !strings.Contains(err.Error(), "binding delete failure") {
			t.Fatalf("reconcile error = %v, want binding delete failure", err)
		}
	})
}

func internalUserCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}
