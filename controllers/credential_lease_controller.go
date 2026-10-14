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
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	userv1 "github.com/labring-sigs/sealos-access/api/v1"
	"github.com/labring-sigs/sealos-access/pkg/internalcredentials"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const (
	credentialLeaseControllerName = "credential-lease-controller"
	credentialLeaseRequeueAfter   = 30 * time.Second
	leaseCleanupRequeueAfter      = time.Second
	credentialLeaseScanInterval   = 30 * time.Second
)

var errTargetSuspended = errors.New("target InternalUser is suspended")

// CredentialLeaseReconciler prepares and cleans the non-secret objects used by
// a Broker TokenRequest. It never calls TokenRequest and never reads Secret
// data.
type CredentialLeaseReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Logger   logr.Logger
	Recorder record.EventRecorder

	UserNamespace    string
	BrokerNamespace  string
	BaseRoleName     string
	ElevatedRoleName string
	Now              func() time.Time
}

// +kubebuilder:rbac:groups=user.sealos.io,resources=credentialleases,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=user.sealos.io,resources=credentialleases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=user.sealos.io,resources=credentialleases/finalizers,verbs=update
// +kubebuilder:rbac:groups=user.sealos.io,resources=internalusers,verbs=get
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *CredentialLeaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Client == nil {
		r.Client = mgr.GetClient()
	}
	if r.Scheme == nil {
		r.Scheme = mgr.GetScheme()
	}
	r.Logger = ctrl.Log.WithName(credentialLeaseControllerName)
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor(credentialLeaseControllerName)
	}
	if r.UserNamespace == "" {
		r.UserNamespace = userv1.InternalUserSystemNamespace
	}
	if r.BrokerNamespace == "" {
		r.BrokerNamespace = userv1.CredentialBrokerNamespace
	}
	if r.BaseRoleName == "" {
		r.BaseRoleName = internalcredentials.BaseReadonlyRoleName
	}
	if r.ElevatedRoleName == "" {
		r.ElevatedRoleName = internalcredentials.ClusterOpsWriteRoleName
	}
	if r.Now == nil {
		r.Now = time.Now
	}

	scanEvents := make(chan event.GenericEvent, 1024)
	if err := mgr.Add(&credentialLeaseScanner{
		Client:          r.Client,
		BrokerNamespace: r.BrokerNamespace,
		Events:          scanEvents,
		Interval:        credentialLeaseScanInterval,
		Logger:          r.Logger,
	}); err != nil {
		return fmt.Errorf("add CredentialLease periodic scanner: %w", err)
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&userv1.CredentialLease{}, builder.WithPredicates(credentialLeaseNamespacePredicate(r.BrokerNamespace))).
		Watches(&userv1.InternalUser{}, handler.EnqueueRequestsFromMapFunc(r.mapInternalUserToLeases)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.mapManagedLease), builder.WithPredicates(credentialLeaseManagedResourcePredicate())).
		Watches(&corev1.ServiceAccount{}, handler.EnqueueRequestsFromMapFunc(r.mapManagedLease), builder.WithPredicates(credentialLeaseManagedResourcePredicate())).
		Watches(&rbacv1.ClusterRoleBinding{}, handler.EnqueueRequestsFromMapFunc(r.mapManagedLease), builder.WithPredicates(credentialLeaseManagedResourcePredicate())).
		WatchesRawSource(source.Channel(scanEvents, handler.EnqueueRequestsFromMapFunc(credentialLeaseScanEventMapper(r.BrokerNamespace)))).
		Complete(r)
}

func credentialLeaseNamespacePredicate(namespace string) predicate.Predicate {
	return predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj.GetNamespace() == namespace
	})
}

func credentialLeaseManagedResourcePredicate() predicate.Predicate {
	return predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj.GetLabels()[userv1.ManagedByLabel] == credentialLeaseControllerName
	})
}

func credentialLeaseScanEventMapper(namespace string) handler.MapFunc {
	return func(_ context.Context, obj client.Object) []reconcile.Request {
		lease, ok := obj.(*userv1.CredentialLease)
		if !ok || lease.Namespace != namespace {
			return nil
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: lease.Namespace, Name: lease.Name}}}
	}
}

type credentialLeaseScanner struct {
	Client          client.Reader
	BrokerNamespace string
	Events          chan<- event.GenericEvent
	Interval        time.Duration
	Logger          logr.Logger
}

func (s *credentialLeaseScanner) NeedLeaderElection() bool { return true }

func (s *credentialLeaseScanner) Start(ctx context.Context) error {
	interval := s.Interval
	if interval <= 0 {
		interval = credentialLeaseScanInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	s.scan(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.scan(ctx)
		}
	}
}

func (s *credentialLeaseScanner) scan(ctx context.Context) {
	if s.Client == nil || s.Events == nil {
		return
	}
	leases := &userv1.CredentialLeaseList{}
	if err := s.Client.List(ctx, leases, client.InNamespace(s.BrokerNamespace)); err != nil {
		s.Logger.Error(err, "periodic CredentialLease scan failed")
		return
	}
	for i := range leases.Items {
		lease := leases.Items[i].DeepCopy()
		select {
		case s.Events <- event.GenericEvent{Object: lease}:
		case <-ctx.Done():
			return
		}
	}
}

func (r *CredentialLeaseReconciler) mapManagedLease(_ context.Context, obj client.Object) []reconcile.Request {
	switch obj.(type) {
	case *corev1.Secret, *corev1.ServiceAccount, *rbacv1.ClusterRoleBinding:
	default:
		return nil
	}
	labels := obj.GetLabels()
	if labels[userv1.ManagedByLabel] != credentialLeaseControllerName {
		return nil
	}
	name := labels[userv1.CredentialLeaseLabel]
	if name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{
		Namespace: r.BrokerNamespace,
		Name:      name,
	}}}
}

func (r *CredentialLeaseReconciler) mapInternalUserToLeases(ctx context.Context, obj client.Object) []reconcile.Request {
	user, ok := obj.(*userv1.InternalUser)
	if !ok {
		return nil
	}
	leases := &userv1.CredentialLeaseList{}
	if err := r.List(ctx, leases, client.InNamespace(r.BrokerNamespace)); err != nil {
		r.Logger.Error(err, "list CredentialLeases for InternalUser event", "user", user.Name)
		return nil
	}
	requests := make([]reconcile.Request, 0)
	for i := range leases.Items {
		lease := &leases.Items[i]
		if lease.Spec.Target == user.Spec.Identity {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{
				Namespace: lease.Namespace,
				Name:      lease.Name,
			}})
		}
	}
	return requests
}

func (r *CredentialLeaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	lease := &userv1.CredentialLease{}
	if err := r.Get(ctx, req.NamespacedName, lease); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if lease.Namespace != r.BrokerNamespace {
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(lease, userv1.CredentialLeaseFinalizer) {
		// Terminal leases without a finalizer have already completed cleanup.
		// Cache resyncs must not re-add the finalizer and create a cleanup loop.
		if isTerminalLease(lease.Status.Phase) {
			return ctrl.Result{}, nil
		}
		controllerutil.AddFinalizer(lease, userv1.CredentialLeaseFinalizer)
		if err := r.Update(ctx, lease); err != nil {
			return ctrl.Result{}, fmt.Errorf("add CredentialLease finalizer: %w", err)
		}
		return ctrl.Result{}, nil
	}

	if !lease.DeletionTimestamp.IsZero() {
		return r.cleanupAndFinalize(ctx, lease)
	}

	if isTerminalLease(lease.Status.Phase) {
		return r.cleanupAndFinalize(ctx, lease)
	}

	// CRD status subresources may discard status on create. An elevated
	// approval record is nevertheless identifiable from its immutable spec and
	// must be normalized to the waiting phase before any resources are created.
	if lease.Status.Phase == "" && lease.Spec.Profile == userv1.ClusterOpsWriteProfile && lease.Spec.ApprovalID != "" {
		lease.Status.Phase = userv1.CredentialLeaseAwaitingRedemption
		if err := r.Status().Update(ctx, lease); err != nil {
			return ctrl.Result{}, fmt.Errorf("initialize approved CredentialLease phase: %w", err)
		}
		return ctrl.Result{}, nil
	}

	if lease.Status.Phase == userv1.CredentialLeaseAwaitingRedemption {
		return r.reconcileAwaitingRedemption(ctx, lease)
	}

	if lease.Status.Phase == userv1.CredentialLeaseIssued {
		return r.reconcileIssued(ctx, lease)
	}

	if lease.Status.ConsumedAt != nil {
		return r.reconcileConsumed(ctx, lease)
	}

	if err := r.validateLease(lease); err != nil {
		if statusErr := r.markTerminal(ctx, lease, userv1.CredentialLeaseFailed, "InvalidLease", err.Error()); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return r.cleanupAndFinalize(ctx, lease)
	}

	profile, _ := internalcredentials.ProfileFor(lease.Spec.Profile)
	if err := r.ensureTarget(ctx, lease); err != nil {
		switch {
		case errors.Is(err, errTargetSuspended):
			if statusErr := r.markTerminal(ctx, lease, userv1.CredentialLeaseRevoked, "TargetSuspended", err.Error()); statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			return r.cleanupAndFinalize(ctx, lease)
		case apierrors.IsNotFound(err):
			return ctrl.Result{RequeueAfter: credentialLeaseRequeueAfter}, nil
		default:
			return ctrl.Result{}, err
		}
	}
	if err := r.ensureRole(ctx, profile.RoleName); err != nil {
		r.recordWarning(lease, "CredentialLeasePreparationBlocked", err)
		return ctrl.Result{}, err
	}
	if err := r.ensurePreparedResources(ctx, lease, profile); err != nil {
		r.recordWarning(lease, "CredentialLeaseResourceRejected", err)
		return ctrl.Result{}, err
	}
	if err := r.markPrepared(ctx, lease); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: credentialLeaseRequeueAfter}, nil
}

func (r *CredentialLeaseReconciler) validateLease(lease *userv1.CredentialLease) error {
	if lease.Namespace != r.BrokerNamespace {
		return fmt.Errorf("CredentialLease must be in namespace %q", r.BrokerNamespace)
	}
	if err := internalcredentials.ValidateIdentity(lease.Spec.Target.Issuer, lease.Spec.Target.Subject); err != nil {
		return fmt.Errorf("invalid target identity: %w", err)
	}
	if err := internalcredentials.ValidateIdentity(lease.Spec.Requester.Issuer, lease.Spec.Requester.Subject); err != nil {
		return fmt.Errorf("invalid requester identity: %w", err)
	}
	if _, err := internalcredentials.ValidateRequestedTTL(lease.Spec.Profile, lease.Spec.RequestedTTLSeconds); err != nil {
		return err
	}
	if lease.Spec.Profile == userv1.ClusterOpsWriteProfile && lease.Spec.ApprovalReference == "" {
		return errors.New("elevated CredentialLease requires an approval reference")
	}
	if lease.Spec.Profile == userv1.ClusterOpsWriteProfile && lease.Spec.ApprovalID == "" {
		return errors.New("elevated CredentialLease requires an approval ID")
	}
	return nil
}

func (r *CredentialLeaseReconciler) reconcileAwaitingRedemption(ctx context.Context, lease *userv1.CredentialLease) (ctrl.Result, error) {
	if err := r.validateLease(lease); err != nil {
		if statusErr := r.markTerminal(ctx, lease, userv1.CredentialLeaseFailed, "InvalidApproval", err.Error()); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return r.cleanupAndFinalize(ctx, lease)
	}
	if err := r.ensureTarget(ctx, lease); err != nil {
		switch {
		case errors.Is(err, errTargetSuspended):
			if statusErr := r.markTerminal(ctx, lease, userv1.CredentialLeaseRevoked, "TargetSuspended", err.Error()); statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			return r.cleanupAndFinalize(ctx, lease)
		case apierrors.IsNotFound(err):
			return ctrl.Result{RequeueAfter: credentialLeaseRequeueAfter}, nil
		default:
			return ctrl.Result{}, err
		}
	}
	if lease.Status.ApprovalExpirationTimestamp == nil {
		return ctrl.Result{RequeueAfter: credentialLeaseRequeueAfter}, nil
	}
	remaining := lease.Status.ApprovalExpirationTimestamp.Time.Sub(r.now())
	if remaining <= 0 {
		if err := r.markTerminal(ctx, lease, userv1.CredentialLeaseExpired, "ApprovalExpired", "approval reference expired before redemption"); err != nil {
			return ctrl.Result{}, err
		}
		return r.cleanupAndFinalize(ctx, lease)
	}
	if remaining > credentialLeaseRequeueAfter {
		remaining = credentialLeaseRequeueAfter
	}
	return ctrl.Result{RequeueAfter: remaining}, nil
}

func (r *CredentialLeaseReconciler) ensureTarget(ctx context.Context, lease *userv1.CredentialLease) error {
	name := internalcredentials.DeriveInternalUserName(lease.Spec.Target.Issuer, lease.Spec.Target.Subject)
	user := &userv1.InternalUser{}
	if err := r.Get(ctx, types.NamespacedName{Name: name}, user); err != nil {
		return fmt.Errorf("get target InternalUser %q: %w", name, err)
	}
	if user.Spec.Identity != lease.Spec.Target {
		return fmt.Errorf("target InternalUser %q identity does not match Lease", name)
	}
	if user.Spec.Suspend {
		return errTargetSuspended
	}
	if user.Status.Phase != userv1.InternalUserActive {
		return fmt.Errorf("target InternalUser %q is not active", name)
	}
	return nil
}

func (r *CredentialLeaseReconciler) ensureRole(ctx context.Context, roleName string) error {
	role := &rbacv1.ClusterRole{}
	if err := r.Get(ctx, types.NamespacedName{Name: roleName}, role); err != nil {
		return fmt.Errorf("get profile ClusterRole %q: %w", roleName, err)
	}
	return nil
}

func (r *CredentialLeaseReconciler) ensurePreparedResources(ctx context.Context, lease *userv1.CredentialLease, profile internalcredentials.Profile) error {
	if profile.UsesStableAccount {
		name := internalcredentials.DeriveInternalUserName(lease.Spec.Target.Issuer, lease.Spec.Target.Subject)
		sa := &corev1.ServiceAccount{}
		if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: r.UserNamespace}, sa); err != nil {
			return fmt.Errorf("get stable ServiceAccount: %w", err)
		}
		if err := verifyLeaseTargetServiceAccount(sa, lease); err != nil {
			return err
		}
		lease.Status.ServiceAccountRef = objectReference(sa)
	} else {
		name := internalcredentials.ResourceName("internal-lease", lease.Namespace, lease.Name)
		sa, err := r.ensureTemporaryServiceAccount(ctx, lease, name)
		if err != nil {
			return err
		}
		binding, err := r.ensureTemporaryBinding(ctx, lease, profile.RoleName, name)
		if err != nil {
			return err
		}
		lease.Status.ServiceAccountRef = objectReference(sa)
		lease.Status.BindingRef = objectReference(binding)
	}

	secret, err := r.ensureBoundSecret(ctx, lease)
	if err != nil {
		return err
	}
	lease.Status.BoundSecretRef = objectReference(secret)
	return nil
}

func (r *CredentialLeaseReconciler) ensureTemporaryServiceAccount(ctx context.Context, lease *userv1.CredentialLease, name string) (*corev1.ServiceAccount, error) {
	sa := &corev1.ServiceAccount{}
	key := types.NamespacedName{Name: name, Namespace: r.UserNamespace}
	err := r.Get(ctx, key, sa)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get temporary ServiceAccount: %w", err)
	}
	if err == nil {
		if ownershipErr := verifyLeaseOwnership(sa, lease); ownershipErr != nil {
			return nil, ownershipErr
		}
		if len(sa.Secrets) != 0 {
			return nil, fmt.Errorf("temporary ServiceAccount %s/%s has legacy token references", sa.Namespace, sa.Name)
		}
	} else {
		sa = &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.UserNamespace}}
	}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, sa, func() error {
		setLeaseLabels(sa, lease)
		sa.AutomountServiceAccountToken = ptr.To(false)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("ensure temporary ServiceAccount: %w", err)
	}
	return sa, nil
}

func (r *CredentialLeaseReconciler) ensureTemporaryBinding(ctx context.Context, lease *userv1.CredentialLease, roleName, serviceAccountName string) (*rbacv1.ClusterRoleBinding, error) {
	name := internalcredentials.ResourceName("internal-lease-binding", lease.Namespace, lease.Name)
	binding := &rbacv1.ClusterRoleBinding{}
	err := r.Get(ctx, types.NamespacedName{Name: name}, binding)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get temporary ClusterRoleBinding: %w", err)
	}
	if err == nil {
		if ownershipErr := verifyLeaseOwnership(binding, lease); ownershipErr != nil {
			return nil, ownershipErr
		}
		if binding.RoleRef != (rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: roleName}) ||
			len(binding.Subjects) != 1 || binding.Subjects[0].Kind != "ServiceAccount" ||
			binding.Subjects[0].Name != serviceAccountName || binding.Subjects[0].Namespace != r.UserNamespace {
			return nil, fmt.Errorf("temporary ClusterRoleBinding %s has unexpected roleRef or subject", name)
		}
		return binding, nil
	}

	binding = &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name}}
	setLeaseLabels(binding, lease)
	binding.RoleRef = rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: roleName}
	binding.Subjects = []rbacv1.Subject{{Kind: "ServiceAccount", Name: serviceAccountName, Namespace: r.UserNamespace}}
	if err := r.Create(ctx, binding); err != nil {
		return nil, fmt.Errorf("create temporary ClusterRoleBinding: %w", err)
	}
	return binding, nil
}

func (r *CredentialLeaseReconciler) ensureBoundSecret(ctx context.Context, lease *userv1.CredentialLease) (*corev1.Secret, error) {
	name := internalcredentials.ResourceName("internal-lease-secret", lease.Namespace, lease.Name)
	secret := &corev1.Secret{}
	key := types.NamespacedName{Name: name, Namespace: r.UserNamespace}
	err := r.Get(ctx, key, secret)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get bound Secret: %w", err)
	}
	if err == nil {
		if ownershipErr := verifyLeaseOwnership(secret, lease); ownershipErr != nil {
			return nil, ownershipErr
		}
		if len(secret.Data) != 0 || len(secret.StringData) != 0 {
			return nil, fmt.Errorf("bound Secret %s/%s contains data", secret.Namespace, secret.Name)
		}
	} else {
		secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.UserNamespace}}
	}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		setLeaseLabels(secret, lease)
		secret.Type = corev1.SecretTypeOpaque
		secret.Data = nil
		secret.StringData = nil
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("ensure bound Secret: %w", err)
	}
	if secret.UID == "" {
		return nil, fmt.Errorf("bound Secret %s/%s has no UID", secret.Namespace, secret.Name)
	}
	return secret, nil
}

func (r *CredentialLeaseReconciler) markPrepared(ctx context.Context, lease *userv1.CredentialLease) error {
	lease.Status.Phase = userv1.CredentialLeasePrepared
	setLeaseCondition(&lease.Status.Conditions, "Prepared", metav1.ConditionTrue, "ResourcesReady", "lease resources are ready for TokenRequest")
	if err := r.Status().Update(ctx, lease); err != nil {
		return fmt.Errorf("mark CredentialLease Prepared: %w", err)
	}
	return nil
}

func (r *CredentialLeaseReconciler) markTerminal(ctx context.Context, lease *userv1.CredentialLease, phase userv1.CredentialLeasePhase, reason, message string) error {
	lease.Status.Phase = phase
	setLeaseCondition(&lease.Status.Conditions, "Ready", metav1.ConditionFalse, reason, message)
	if err := r.Status().Update(ctx, lease); err != nil {
		return fmt.Errorf("mark CredentialLease %s: %w", phase, err)
	}
	return nil
}

func (r *CredentialLeaseReconciler) reconcileIssued(ctx context.Context, lease *userv1.CredentialLease) (ctrl.Result, error) {
	if lease.Status.ExpirationTimestamp == nil {
		return ctrl.Result{RequeueAfter: credentialLeaseRequeueAfter}, nil
	}
	remaining := time.Until(lease.Status.ExpirationTimestamp.Time)
	if r.Now != nil {
		remaining = lease.Status.ExpirationTimestamp.Time.Sub(r.Now())
	}
	if remaining > 0 {
		if remaining > credentialLeaseRequeueAfter {
			remaining = credentialLeaseRequeueAfter
		}
		return ctrl.Result{RequeueAfter: remaining}, nil
	}
	if err := r.markTerminal(ctx, lease, userv1.CredentialLeaseExpired, "Expired", "credential expiration timestamp has passed"); err != nil {
		return ctrl.Result{}, err
	}
	return r.cleanupAndFinalize(ctx, lease)
}

func (r *CredentialLeaseReconciler) reconcileConsumed(ctx context.Context, lease *userv1.CredentialLease) (ctrl.Result, error) {
	maxTTL, err := internalcredentials.ValidateRequestedTTL(lease.Spec.Profile, lease.Spec.RequestedTTLSeconds)
	if err != nil {
		return ctrl.Result{}, err
	}
	deadline := lease.Status.ConsumedAt.Add(maxTTL)
	remaining := deadline.Sub(r.now())
	if remaining > 0 {
		if remaining > credentialLeaseRequeueAfter {
			remaining = credentialLeaseRequeueAfter
		}
		return ctrl.Result{RequeueAfter: remaining}, nil
	}
	if err := r.markTerminal(ctx, lease, userv1.CredentialLeaseExpired, "ConsumedResponseLost", "lease was consumed but issuance status was not recorded"); err != nil {
		return ctrl.Result{}, err
	}
	return r.cleanupAndFinalize(ctx, lease)
}

func (r *CredentialLeaseReconciler) cleanupAndFinalize(ctx context.Context, lease *userv1.CredentialLease) (ctrl.Result, error) {
	if err := r.cleanupLeaseResources(ctx, lease); err != nil {
		r.recordWarning(lease, "CredentialLeaseCleanupBlocked", err)
		return ctrl.Result{RequeueAfter: leaseCleanupRequeueAfter}, err
	}
	if controllerutil.ContainsFinalizer(lease, userv1.CredentialLeaseFinalizer) {
		controllerutil.RemoveFinalizer(lease, userv1.CredentialLeaseFinalizer)
		if err := r.Update(ctx, lease); err != nil {
			return ctrl.Result{}, fmt.Errorf("remove CredentialLease finalizer: %w", err)
		}
	}
	return ctrl.Result{}, nil
}

func (r *CredentialLeaseReconciler) recordWarning(lease *userv1.CredentialLease, reason string, err error) {
	if r.Recorder == nil || lease == nil || err == nil {
		return
	}
	r.Recorder.Event(lease, corev1.EventTypeWarning, reason, err.Error())
}

func (r *CredentialLeaseReconciler) cleanupLeaseResources(ctx context.Context, lease *userv1.CredentialLease) error {
	profile, ok := internalcredentials.ProfileFor(lease.Spec.Profile)
	if !ok {
		return fmt.Errorf("unsupported credential profile %q during cleanup", lease.Spec.Profile)
	}

	// Remove the binding before deleting the bound object or temporary account.
	if !profile.UsesStableAccount {
		name := internalcredentials.ResourceName("internal-lease-binding", lease.Namespace, lease.Name)
		binding := &rbacv1.ClusterRoleBinding{}
		if err := r.Get(ctx, types.NamespacedName{Name: name}, binding); err != nil {
			if !apierrors.IsNotFound(err) {
				return fmt.Errorf("get temporary ClusterRoleBinding for cleanup: %w", err)
			}
		} else {
			if err := verifyLeaseOwnership(binding, lease); err != nil {
				return err
			}
			if err := r.Delete(ctx, binding); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("delete temporary ClusterRoleBinding: %w", err)
			}
		}
	}

	secretName := internalcredentials.ResourceName("internal-lease-secret", lease.Namespace, lease.Name)
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: r.UserNamespace}, secret); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get bound Secret for cleanup: %w", err)
		}
	} else {
		if err := verifyLeaseOwnership(secret, lease); err != nil {
			return err
		}
		if err := r.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete bound Secret: %w", err)
		}
	}

	if !profile.UsesStableAccount {
		name := internalcredentials.ResourceName("internal-lease", lease.Namespace, lease.Name)
		sa := &corev1.ServiceAccount{}
		if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: r.UserNamespace}, sa); err != nil {
			if !apierrors.IsNotFound(err) {
				return fmt.Errorf("get temporary ServiceAccount for cleanup: %w", err)
			}
		} else {
			if err := verifyLeaseOwnership(sa, lease); err != nil {
				return err
			}
			if err := r.Delete(ctx, sa); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("delete temporary ServiceAccount: %w", err)
			}
		}
	}
	return nil
}

func setLeaseLabels(obj metav1.Object, lease *userv1.CredentialLease) {
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[userv1.ManagedByLabel] = credentialLeaseControllerName
	labels[userv1.CredentialLeaseLabel] = lease.Name
	labels[userv1.InternalUserLabel] = internalcredentials.DeriveInternalUserName(lease.Spec.Target.Issuer, lease.Spec.Target.Subject)
	if lease.UID != "" {
		labels[userv1.CredentialLeaseUIDLabel] = string(lease.UID)
	}
	obj.SetLabels(labels)
}

func verifyLeaseOwnership(obj metav1.Object, lease *userv1.CredentialLease) error {
	labels := obj.GetLabels()
	if labels[userv1.ManagedByLabel] != credentialLeaseControllerName || labels[userv1.CredentialLeaseLabel] != lease.Name {
		return fmt.Errorf("refusing to adopt or delete %s/%s: CredentialLease ownership labels do not match", obj.GetNamespace(), obj.GetName())
	}
	if lease.UID != "" && labels[userv1.CredentialLeaseUIDLabel] != string(lease.UID) {
		return fmt.Errorf("refusing to adopt or delete %s/%s: CredentialLease UID label does not match", obj.GetNamespace(), obj.GetName())
	}
	wantTarget := internalcredentials.DeriveInternalUserName(lease.Spec.Target.Issuer, lease.Spec.Target.Subject)
	if labels[userv1.InternalUserLabel] != wantTarget {
		return fmt.Errorf("refusing to adopt or delete %s/%s: target ownership label does not match", obj.GetNamespace(), obj.GetName())
	}
	return nil
}

func verifyLeaseTargetServiceAccount(sa *corev1.ServiceAccount, lease *userv1.CredentialLease) error {
	if sa.Labels[userv1.ManagedByLabel] != internalUserControllerName ||
		sa.Labels[userv1.InternalUserLabel] != internalcredentials.DeriveInternalUserName(lease.Spec.Target.Issuer, lease.Spec.Target.Subject) {
		return fmt.Errorf("stable ServiceAccount %s/%s is not owned by the target InternalUser", sa.Namespace, sa.Name)
	}
	if sa.AutomountServiceAccountToken != nil && *sa.AutomountServiceAccountToken {
		return fmt.Errorf("stable ServiceAccount %s/%s has automount enabled", sa.Namespace, sa.Name)
	}
	return nil
}

func objectReference(obj client.Object) *corev1.ObjectReference {
	ref := &corev1.ObjectReference{
		Namespace: obj.GetNamespace(),
		Name:      obj.GetName(),
		UID:       obj.GetUID(),
	}
	switch obj.(type) {
	case *corev1.ServiceAccount:
		ref.APIVersion = "v1"
		ref.Kind = "ServiceAccount"
	case *corev1.Secret:
		ref.APIVersion = "v1"
		ref.Kind = "Secret"
	case *rbacv1.ClusterRoleBinding:
		ref.APIVersion = rbacv1.SchemeGroupVersion.String()
		ref.Kind = "ClusterRoleBinding"
	default:
		gvk := obj.GetObjectKind().GroupVersionKind()
		ref.APIVersion = gvk.GroupVersion().String()
		ref.Kind = gvk.Kind
	}
	return ref
}

func (r *CredentialLeaseReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}
