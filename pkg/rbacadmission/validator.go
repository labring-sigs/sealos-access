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

// Package rbacadmission validates the small, server-owned set of
// ClusterRoleBindings that the internal-user Controller may create or remove.
// It is intentionally separate from the Controller process.
package rbacadmission

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	userv1 "github.com/labring-sigs/sealos-access/api/v1"
	"github.com/labring-sigs/sealos-access/pkg/internalcredentials"
)

const (
	InternalUserControllerUsername = "system:serviceaccount:access-controller-system:internal-user-controller"
	internalUserControllerName     = "internal-user-controller"
	credentialLeaseControllerName  = "credential-lease-controller"
)

var clusterRoleBindingResource = metav1.GroupVersionResource{
	Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings",
}

type dependencyError struct {
	err error
}

func (e *dependencyError) Error() string {
	return e.err.Error()
}

func (e *dependencyError) Unwrap() error {
	return e.err
}

// Validator is the admission handler for Controller-owned ClusterRoleBindings.
// Client is used only to read the corresponding InternalUser/CredentialLease.
type Validator struct {
	Client  client.Reader
	Decoder admission.Decoder

	UserNamespace      string
	BrokerNamespace    string
	BaseRoleName       string
	ElevatedRoleName   string
	ControllerUsername string
}

// NewValidator constructs a validator with the platform defaults.
func NewValidator(reader client.Reader, scheme *runtime.Scheme) *Validator {
	return &Validator{
		Client:             reader,
		Decoder:            admission.NewDecoder(scheme),
		UserNamespace:      userv1.InternalUserSystemNamespace,
		BrokerNamespace:    userv1.CredentialBrokerNamespace,
		BaseRoleName:       internalcredentials.BaseReadonlyRoleName,
		ElevatedRoleName:   internalcredentials.ClusterOpsWriteRoleName,
		ControllerUsername: InternalUserControllerUsername,
	}
}

// Handle allows unrelated callers through, and validates every binding
// mutation issued by the Controller identity. API lookup failures are returned
// as errors so the ValidatingWebhookConfiguration's failurePolicy: Fail takes
// effect when the validator cannot establish ownership.
func (v *Validator) Handle(ctx context.Context, req admission.Request) admission.Response {
	if req.UserInfo.Username != v.ControllerUsername || req.Resource != clusterRoleBindingResource || req.SubResource != "" {
		return admission.Allowed("")
	}

	raw := req.Object
	if req.Operation == admissionv1.Delete {
		raw = req.OldObject
	}
	if len(raw.Raw) == 0 {
		return admission.Denied("ClusterRoleBinding object is required")
	}

	binding := &rbacv1.ClusterRoleBinding{}
	if err := v.Decoder.DecodeRaw(raw, binding); err != nil {
		return admission.Denied("ClusterRoleBinding could not be decoded")
	}
	if err := v.Validate(ctx, req.Operation, binding); err != nil {
		var dependencyErr *dependencyError
		if errors.As(err, &dependencyErr) {
			return admission.Errored(http.StatusInternalServerError, errors.New("RBAC ownership validation is unavailable"))
		}
		return admission.Denied("ClusterRoleBinding is not an authorized internal-user binding")
	}
	return admission.Allowed("")
}

// Validate checks a decoded ClusterRoleBinding against the live InternalUser
// or CredentialLease that owns it. The operation matters because deletion is
// needed for cleanup even after the owner has entered a terminal state.
func (v *Validator) Validate(ctx context.Context, operation admissionv1.Operation, binding *rbacv1.ClusterRoleBinding) error {
	if v.Client == nil {
		return &dependencyError{err: errors.New("RBAC admission client is nil")}
	}
	if binding.Namespace != "" {
		return errors.New("ClusterRoleBinding must be cluster-scoped")
	}
	if binding.RoleRef.APIGroup != rbacv1.GroupName || binding.RoleRef.Kind != "ClusterRole" {
		return errors.New("ClusterRoleBinding roleRef is not a ClusterRole")
	}

	labels := binding.Labels
	if leaseName, hasLeaseLabel := labels[userv1.CredentialLeaseLabel]; hasLeaseLabel {
		return v.validateLeaseBinding(ctx, operation, binding, leaseName)
	}
	return v.validateBaseBinding(ctx, operation, binding)
}

func (v *Validator) validateBaseBinding(ctx context.Context, operation admissionv1.Operation, binding *rbacv1.ClusterRoleBinding) error {
	labels := binding.Labels
	userName := labels[userv1.InternalUserLabel]
	if userName == "" {
		return errors.New("base binding is missing the InternalUser label")
	}
	if labels[userv1.CredentialLeaseUIDLabel] != "" {
		return errors.New("base binding has a CredentialLease UID label")
	}

	user := &userv1.InternalUser{}
	if err := v.Client.Get(ctx, types.NamespacedName{Name: userName}, user); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("InternalUser %q does not exist", userName)
		}
		return &dependencyError{err: fmt.Errorf("get InternalUser %q: %w", userName, err)}
	}
	if err := validateInternalUser(user); err != nil {
		return err
	}
	if operation != admissionv1.Delete && (user.Spec.Suspend || !user.DeletionTimestamp.IsZero()) {
		return errors.New("base binding cannot be created for a suspended or deleting InternalUser")
	}

	wantLabels := map[string]string{
		userv1.ManagedByLabel:       internalUserControllerName,
		userv1.InternalUserLabel:    user.Name,
		userv1.InternalUserUIDLabel: string(user.UID),
	}
	if err := validateOwnershipLabels(labels, wantLabels); err != nil {
		return err
	}
	wantName := internalcredentials.ResourceName("internal-user-base", "", user.Name)
	if binding.Name != wantName {
		return fmt.Errorf("base binding name must be %q", wantName)
	}
	if binding.RoleRef.Name != v.BaseRoleName {
		return fmt.Errorf("base binding roleRef must be %q", v.BaseRoleName)
	}
	if !hasSingleServiceAccountSubject(binding, user.Name, v.UserNamespace) {
		return errors.New("base binding subject is not the target stable ServiceAccount")
	}
	return nil
}

func (v *Validator) validateLeaseBinding(ctx context.Context, operation admissionv1.Operation, binding *rbacv1.ClusterRoleBinding, leaseName string) error {
	if leaseName == "" {
		return errors.New("elevated binding has an empty CredentialLease label")
	}
	lease := &userv1.CredentialLease{}
	if err := v.Client.Get(ctx, types.NamespacedName{Namespace: v.BrokerNamespace, Name: leaseName}, lease); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("CredentialLease %q does not exist", leaseName)
		}
		return &dependencyError{err: fmt.Errorf("get CredentialLease %q: %w", leaseName, err)}
	}
	if lease.UID == "" {
		return errors.New("CredentialLease has no UID")
	}
	if lease.Spec.Profile != userv1.ClusterOpsWriteProfile {
		return errors.New("only the cluster-ops-write profile may own an elevated binding")
	}
	if err := internalcredentials.ValidateIdentity(lease.Spec.Target.Issuer, lease.Spec.Target.Subject); err != nil {
		return fmt.Errorf("CredentialLease target identity is invalid: %w", err)
	}
	userName := internalcredentials.DeriveInternalUserName(lease.Spec.Target.Issuer, lease.Spec.Target.Subject)
	user := &userv1.InternalUser{}
	if err := v.Client.Get(ctx, types.NamespacedName{Name: userName}, user); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("target InternalUser %q does not exist", userName)
		}
		return &dependencyError{err: fmt.Errorf("get target InternalUser %q: %w", userName, err)}
	}
	if err := validateInternalUser(user); err != nil {
		return err
	}
	if user.Spec.Identity != lease.Spec.Target {
		return errors.New("CredentialLease target does not match InternalUser identity")
	}
	if operation != admissionv1.Delete && (user.Spec.Suspend || !user.DeletionTimestamp.IsZero() || !lease.DeletionTimestamp.IsZero() || terminalLease(lease.Status.Phase)) {
		return errors.New("elevated binding cannot be created for a suspended, deleting, or terminal owner")
	}

	wantLabels := map[string]string{
		userv1.ManagedByLabel:          credentialLeaseControllerName,
		userv1.CredentialLeaseLabel:    lease.Name,
		userv1.CredentialLeaseUIDLabel: string(lease.UID),
		userv1.InternalUserLabel:       user.Name,
	}
	if err := validateOwnershipLabels(binding.Labels, wantLabels); err != nil {
		return err
	}
	wantName := internalcredentials.ResourceName("internal-lease-binding", lease.Namespace, lease.Name)
	if binding.Name != wantName {
		return fmt.Errorf("elevated binding name must be %q", wantName)
	}
	if binding.RoleRef.Name != v.ElevatedRoleName {
		return fmt.Errorf("elevated binding roleRef must be %q", v.ElevatedRoleName)
	}
	wantSubject := internalcredentials.ResourceName("internal-lease", lease.Namespace, lease.Name)
	if !hasSingleServiceAccountSubject(binding, wantSubject, v.UserNamespace) {
		return errors.New("elevated binding subject is not the lease ServiceAccount")
	}
	return nil
}

func validateInternalUser(user *userv1.InternalUser) error {
	if user.UID == "" {
		return errors.New("InternalUser has no UID")
	}
	if user.Spec.RoleProfile != userv1.BaseReadonlyProfile {
		return errors.New("InternalUser does not use the base profile")
	}
	if err := internalcredentials.ValidateIdentity(user.Spec.Identity.Issuer, user.Spec.Identity.Subject); err != nil {
		return fmt.Errorf("InternalUser identity is invalid: %w", err)
	}
	if want := internalcredentials.DeriveInternalUserName(user.Spec.Identity.Issuer, user.Spec.Identity.Subject); user.Name != want {
		return fmt.Errorf("InternalUser name must be %q", want)
	}
	return nil
}

func validateOwnershipLabels(got, want map[string]string) error {
	for key, value := range want {
		if got[key] != value {
			return fmt.Errorf("ownership label %q is invalid", key)
		}
	}
	for key := range got {
		if strings.HasPrefix(key, "user.sealos.io/") {
			if _, expected := want[key]; !expected {
				return fmt.Errorf("unexpected ownership label %q", key)
			}
		}
	}
	return nil
}

func hasSingleServiceAccountSubject(binding *rbacv1.ClusterRoleBinding, name, namespace string) bool {
	if len(binding.Subjects) != 1 {
		return false
	}
	subject := binding.Subjects[0]
	return subject.APIGroup == "" && subject.Kind == "ServiceAccount" && subject.Name == name && subject.Namespace == namespace
}

func terminalLease(phase userv1.CredentialLeasePhase) bool {
	switch phase {
	case userv1.CredentialLeaseFailed, userv1.CredentialLeaseRevoked, userv1.CredentialLeaseExpired:
		return true
	default:
		return false
	}
}
