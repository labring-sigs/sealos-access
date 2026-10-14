/*
Copyright 2026 labring.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	userv1 "github.com/labring-sigs/sealos-access/api/v1"
	"github.com/labring-sigs/sealos-access/pkg/internalcredentials"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	internalUserControllerName = "internal-user-controller"
	internalUserRequeueAfter   = 30 * time.Second
)

// InternalUserReconciler owns stable ServiceAccounts and the base profile
// binding. It deliberately has no TokenRequest capability.
type InternalUserReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Logger   logr.Logger
	Recorder record.EventRecorder

	UserNamespace string
	BaseRoleName  string
}

// +kubebuilder:rbac:groups=access.sealos.io,resources=internalusers,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=access.sealos.io,resources=internalusers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=access.sealos.io,resources=internalusers/finalizers,verbs=update
// +kubebuilder:rbac:groups=access.sealos.io,resources=credentialleases,verbs=get;list;watch
// +kubebuilder:rbac:groups=access.sealos.io,resources=credentialleases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *InternalUserReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Client == nil {
		r.Client = mgr.GetClient()
	}
	if r.Scheme == nil {
		r.Scheme = mgr.GetScheme()
	}
	r.Logger = ctrl.Log.WithName(internalUserControllerName)
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor(internalUserControllerName)
	}
	if r.UserNamespace == "" {
		r.UserNamespace = userv1.InternalUserSystemNamespace
	}
	if r.BaseRoleName == "" {
		r.BaseRoleName = internalcredentials.BaseReadonlyRoleName
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&userv1.InternalUser{}).
		Watches(&corev1.ServiceAccount{}, handler.EnqueueRequestsFromMapFunc(r.mapManagedInternalUser), builder.WithPredicates(internalUserManagedResourcePredicate())).
		Watches(&rbacv1.ClusterRoleBinding{}, handler.EnqueueRequestsFromMapFunc(r.mapManagedInternalUser), builder.WithPredicates(internalUserManagedResourcePredicate())).
		Complete(r)
}

func internalUserManagedResourcePredicate() predicate.Predicate {
	return predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj.GetLabels()[userv1.ManagedByLabel] == internalUserControllerName
	})
}

func (r *InternalUserReconciler) mapManagedInternalUser(_ context.Context, obj client.Object) []reconcile.Request {
	switch obj.(type) {
	case *corev1.ServiceAccount, *rbacv1.ClusterRoleBinding:
	default:
		return nil
	}
	labels := obj.GetLabels()
	if labels[userv1.ManagedByLabel] != internalUserControllerName {
		return nil
	}
	name := labels[userv1.InternalUserLabel]
	if name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: name}}}
}

func (r *InternalUserReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	user := &userv1.InternalUser{}
	if err := r.Get(ctx, req.NamespacedName, user); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !controllerutil.ContainsFinalizer(user, userv1.InternalUserFinalizer) {
		if err := r.addFinalizer(ctx, user); err != nil {
			return ctrl.Result{}, err
		}
	}

	if !user.DeletionTimestamp.IsZero() {
		return r.reconcileDeletion(ctx, user)
	}

	if err := r.validateUser(user); err != nil {
		_ = r.setStatus(ctx, user, userv1.InternalUserFailed, err.Error(), false)
		return ctrl.Result{}, err
	}

	if err := r.ensureNamespace(ctx); err != nil {
		_ = r.setStatus(ctx, user, userv1.InternalUserFailed, err.Error(), false)
		return ctrl.Result{}, err
	}

	if err := r.ensureStableServiceAccount(ctx, user); err != nil {
		_ = r.setStatus(ctx, user, userv1.InternalUserFailed, err.Error(), false)
		return ctrl.Result{}, err
	}

	if user.Spec.Suspend {
		if err := r.revokeLeases(ctx, user); err != nil {
			_ = r.setStatus(ctx, user, userv1.InternalUserFailed, err.Error(), false)
			return ctrl.Result{}, err
		}
		if err := r.deleteBaseBinding(ctx, user); err != nil {
			_ = r.setStatus(ctx, user, userv1.InternalUserFailed, err.Error(), false)
			return ctrl.Result{}, err
		}
		if err := r.setStatus(ctx, user, userv1.InternalUserSuspended, "internal user is suspended", true); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: internalUserRequeueAfter}, nil
	}

	if err := r.ensureBaseRole(ctx); err != nil {
		_ = r.setStatus(ctx, user, userv1.InternalUserFailed, err.Error(), false)
		return ctrl.Result{}, err
	}
	if err := r.ensureBaseBinding(ctx, user); err != nil {
		_ = r.setStatus(ctx, user, userv1.InternalUserFailed, err.Error(), false)
		return ctrl.Result{}, err
	}
	if err := r.setStatus(ctx, user, userv1.InternalUserActive, "stable ServiceAccount and base binding are ready", true); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: internalUserRequeueAfter}, nil
}

func (r *InternalUserReconciler) validateUser(user *userv1.InternalUser) error {
	if user.Spec.RoleProfile != userv1.BaseReadonlyProfile {
		return fmt.Errorf("roleProfile must be %q", userv1.BaseReadonlyProfile)
	}
	if err := internalcredentials.ValidateIdentity(user.Spec.Identity.Issuer, user.Spec.Identity.Subject); err != nil {
		return err
	}
	expectedName := internalcredentials.DeriveInternalUserName(user.Spec.Identity.Issuer, user.Spec.Identity.Subject)
	if user.Name != expectedName {
		return fmt.Errorf("InternalUser name must be %q", expectedName)
	}
	return nil
}

func (r *InternalUserReconciler) ensureNamespace(ctx context.Context) error {
	ns := &corev1.Namespace{}
	if err := r.Get(ctx, types.NamespacedName{Name: r.UserNamespace}, ns); err != nil {
		return fmt.Errorf("get internal user namespace: %w", err)
	}
	return nil
}

func (r *InternalUserReconciler) ensureStableServiceAccount(ctx context.Context, user *userv1.InternalUser) error {
	name := internalcredentials.DeriveInternalUserName(user.Spec.Identity.Issuer, user.Spec.Identity.Subject)
	sa := &corev1.ServiceAccount{}
	key := types.NamespacedName{Name: name, Namespace: r.UserNamespace}
	err := r.Get(ctx, key, sa)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("get stable ServiceAccount: %w", err)
	}
	if err == nil {
		if err := verifyInternalUserOwnership(sa, user); err != nil {
			return err
		}
		if len(sa.Secrets) != 0 {
			return fmt.Errorf("stable ServiceAccount %s/%s has legacy token references", sa.Namespace, sa.Name)
		}
	}
	if err := r.createOrUpdateStableServiceAccount(ctx, sa, user, err == nil); err != nil {
		return err
	}
	return nil
}

func (r *InternalUserReconciler) createOrUpdateStableServiceAccount(ctx context.Context, sa *corev1.ServiceAccount, user *userv1.InternalUser, exists bool) error {
	if !exists {
		sa = &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
			Name:      internalcredentials.DeriveInternalUserName(user.Spec.Identity.Issuer, user.Spec.Identity.Subject),
			Namespace: r.UserNamespace,
		}}
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sa, func() error {
		labels := internalUserLabels(user)
		if sa.Labels == nil {
			sa.Labels = map[string]string{}
		}
		for key, value := range labels {
			sa.Labels[key] = value
		}
		sa.AutomountServiceAccountToken = ptr.To(false)
		return nil
	})
	if err != nil {
		return fmt.Errorf("ensure stable ServiceAccount: %w", err)
	}
	return nil
}

func (r *InternalUserReconciler) ensureBaseRole(ctx context.Context) error {
	role := &rbacv1.ClusterRole{}
	if err := r.Get(ctx, types.NamespacedName{Name: r.BaseRoleName}, role); err != nil {
		return fmt.Errorf("get base profile ClusterRole %q: %w", r.BaseRoleName, err)
	}
	return nil
}

func (r *InternalUserReconciler) ensureBaseBinding(ctx context.Context, user *userv1.InternalUser) error {
	name := internalcredentials.ResourceName("internal-user-base", "", user.Name)
	binding := &rbacv1.ClusterRoleBinding{}
	err := r.Get(ctx, types.NamespacedName{Name: name}, binding)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("get base ClusterRoleBinding: %w", err)
	}
	if err == nil {
		if err := verifyInternalUserOwnership(binding, user); err != nil {
			return err
		}
		if binding.RoleRef.Name != r.BaseRoleName || binding.RoleRef.Kind != "ClusterRole" || binding.RoleRef.APIGroup != rbacv1.GroupName {
			return fmt.Errorf("base ClusterRoleBinding %s has an unexpected roleRef", binding.Name)
		}
		if len(binding.Subjects) != 1 || binding.Subjects[0].Kind != "ServiceAccount" ||
			binding.Subjects[0].Name != user.Name || binding.Subjects[0].Namespace != r.UserNamespace {
			return fmt.Errorf("base ClusterRoleBinding %s has an unexpected subject", binding.Name)
		}
		return nil
	}

	binding = &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, binding, func() error {
		binding.Labels = internalUserLabels(user)
		binding.RoleRef = rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: r.BaseRoleName}
		binding.Subjects = []rbacv1.Subject{{Kind: "ServiceAccount", Name: user.Name, Namespace: r.UserNamespace}}
		return nil
	})
	if err != nil {
		return fmt.Errorf("ensure base ClusterRoleBinding: %w", err)
	}
	return nil
}

func (r *InternalUserReconciler) deleteBaseBinding(ctx context.Context, user *userv1.InternalUser) error {
	name := internalcredentials.ResourceName("internal-user-base", "", user.Name)
	binding := &rbacv1.ClusterRoleBinding{}
	if err := r.Get(ctx, types.NamespacedName{Name: name}, binding); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get base ClusterRoleBinding for cleanup: %w", err)
	}
	if err := verifyInternalUserOwnership(binding, user); err != nil {
		return err
	}
	if err := r.Delete(ctx, binding); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete base ClusterRoleBinding: %w", err)
	}
	return nil
}

func (r *InternalUserReconciler) revokeLeases(ctx context.Context, user *userv1.InternalUser) error {
	leases := &userv1.CredentialLeaseList{}
	if err := r.List(ctx, leases, client.InNamespace(userv1.CredentialBrokerNamespace)); err != nil {
		return fmt.Errorf("list CredentialLeases for revocation: %w", err)
	}
	for i := range leases.Items {
		lease := &leases.Items[i]
		if lease.Spec.Target != user.Spec.Identity || isTerminalLease(lease.Status.Phase) {
			continue
		}
		lease.Status.Phase = userv1.CredentialLeaseRevoked
		setLeaseCondition(&lease.Status.Conditions, "Ready", metav1.ConditionFalse, "InternalUserSuspended", "target InternalUser is suspended")
		if err := r.Status().Update(ctx, lease); err != nil {
			return fmt.Errorf("revoke CredentialLease %s/%s: %w", lease.Namespace, lease.Name, err)
		}
	}
	return nil
}

func (r *InternalUserReconciler) reconcileDeletion(ctx context.Context, user *userv1.InternalUser) (ctrl.Result, error) {
	if err := r.revokeLeases(ctx, user); err != nil {
		return ctrl.Result{}, err
	}
	leases := &userv1.CredentialLeaseList{}
	if err := r.List(ctx, leases, client.InNamespace(userv1.CredentialBrokerNamespace)); err != nil {
		return ctrl.Result{}, fmt.Errorf("list CredentialLeases during deletion: %w", err)
	}
	for _, lease := range leases.Items {
		if lease.Spec.Target == user.Spec.Identity && controllerutil.ContainsFinalizer(&lease, userv1.CredentialLeaseFinalizer) {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
	}
	if err := r.deleteBaseBinding(ctx, user); err != nil {
		return ctrl.Result{}, err
	}
	name := internalcredentials.DeriveInternalUserName(user.Spec.Identity.Issuer, user.Spec.Identity.Subject)
	sa := &corev1.ServiceAccount{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: r.UserNamespace}, sa); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("get stable ServiceAccount for deletion: %w", err)
		}
	} else {
		if err := verifyInternalUserOwnership(sa, user); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Delete(ctx, sa); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("delete stable ServiceAccount: %w", err)
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if err := r.removeFinalizer(ctx, user); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *InternalUserReconciler) addFinalizer(ctx context.Context, user *userv1.InternalUser) error {
	controllerutil.AddFinalizer(user, userv1.InternalUserFinalizer)
	if err := r.Update(ctx, user); err != nil {
		return fmt.Errorf("add InternalUser finalizer: %w", err)
	}
	return nil
}

func (r *InternalUserReconciler) removeFinalizer(ctx context.Context, user *userv1.InternalUser) error {
	controllerutil.RemoveFinalizer(user, userv1.InternalUserFinalizer)
	if err := r.Update(ctx, user); err != nil {
		return fmt.Errorf("remove InternalUser finalizer: %w", err)
	}
	return nil
}

func (r *InternalUserReconciler) setStatus(ctx context.Context, user *userv1.InternalUser, phase userv1.InternalUserPhase, message string, ready bool) error {
	changed := user.Status.Phase != phase || user.Status.DerivedName != user.Name || user.Status.StableServiceAccountRef == nil ||
		user.Status.StableServiceAccountRef.Name != user.Name || user.Status.StableServiceAccountRef.Namespace != r.UserNamespace
	user.Status.Phase = phase
	user.Status.DerivedName = user.Name
	user.Status.StableServiceAccountRef = &corev1.ObjectReference{
		APIVersion: "v1", Kind: "ServiceAccount", Namespace: r.UserNamespace, Name: user.Name,
	}
	conditionStatus := metav1.ConditionFalse
	if ready {
		conditionStatus = metav1.ConditionTrue
	}
	oldCondition := findCondition(user.Status.Conditions, "Ready")
	setCondition(&user.Status.Conditions, "Ready", conditionStatus, string(phase), message)
	if oldCondition == nil || oldCondition.Status != conditionStatus || oldCondition.Reason != string(phase) || oldCondition.Message != message {
		changed = true
	}
	if !changed {
		return nil
	}
	if err := r.Status().Update(ctx, user); err != nil {
		return fmt.Errorf("update InternalUser status: %w", err)
	}
	return nil
}

func internalUserLabels(user *userv1.InternalUser) map[string]string {
	labels := map[string]string{
		userv1.ManagedByLabel:    internalUserControllerName,
		userv1.InternalUserLabel: user.Name,
	}
	if user.UID != "" {
		labels[userv1.InternalUserUIDLabel] = string(user.UID)
	}
	return labels
}

func verifyInternalUserOwnership(obj metav1.Object, user *userv1.InternalUser) error {
	labels := obj.GetLabels()
	if labels[userv1.ManagedByLabel] != internalUserControllerName || labels[userv1.InternalUserLabel] != user.Name {
		return fmt.Errorf("refusing to adopt or delete %s/%s: InternalUser ownership labels do not match", obj.GetNamespace(), obj.GetName())
	}
	if user.UID != "" && labels[userv1.InternalUserUIDLabel] != string(user.UID) {
		return fmt.Errorf("refusing to adopt or delete %s/%s: InternalUser UID label does not match", obj.GetNamespace(), obj.GetName())
	}
	return nil
}

func isTerminalLease(phase userv1.CredentialLeasePhase) bool {
	switch phase {
	case userv1.CredentialLeaseFailed, userv1.CredentialLeaseRevoked, userv1.CredentialLeaseExpired:
		return true
	default:
		return false
	}
}

func findCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}

func setCondition(conditions *[]metav1.Condition, conditionType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(conditions, metav1.Condition{
		Type: conditionType, Status: status, Reason: reason, Message: message,
	})
}

func setLeaseCondition(conditions *[]metav1.Condition, conditionType string, status metav1.ConditionStatus, reason, message string) {
	setCondition(conditions, conditionType, status, reason, message)
}
