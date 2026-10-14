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

package v1

import (
	"context"
	"errors"
	"fmt"

	"github.com/labring-sigs/sealos-access/pkg/internalcredentials"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

var internalUserLog = logf.Log.WithName("internal-user-webhook")

// InternalUserWebhook validates immutable identity inputs and the server-side
// issuer allowlist. The allowlist is supplied by the deployment, never by the
// request body.
type InternalUserWebhook struct {
	AllowedIssuers map[string]struct{}
}

func (r *InternalUserWebhook) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&InternalUser{}).
		WithDefaulter(r).
		WithValidator(r).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-user-sealos-io-v1-internaluser,mutating=true,failurePolicy=fail,sideEffects=None,groups=user.sealos.io,resources=internalusers,verbs=create;update,versions=v1,name=minternaluser.kb.io,admissionReviewVersions=v1

var _ webhook.CustomDefaulter = &InternalUserWebhook{}

func (r *InternalUserWebhook) Default(_ context.Context, obj runtime.Object) error {
	user, ok := obj.(*InternalUser)
	if !ok {
		return errors.New("object is not an InternalUser")
	}
	if user.Spec.RoleProfile == "" {
		user.Spec.RoleProfile = BaseReadonlyProfile
	}
	return nil
}

// +kubebuilder:webhook:path=/validate-user-sealos-io-v1-internaluser,mutating=false,failurePolicy=fail,sideEffects=None,groups=user.sealos.io,resources=internalusers,verbs=create;update;delete,versions=v1,name=vinternaluser.kb.io,admissionReviewVersions=v1

var _ webhook.CustomValidator = &InternalUserWebhook{}

func (r *InternalUserWebhook) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	user, ok := obj.(*InternalUser)
	if !ok {
		return nil, errors.New("object is not an InternalUser")
	}
	if err := r.validate(user); err != nil {
		return nil, err
	}
	internalUserLog.V(1).Info("validated InternalUser create", "name", user.Name)
	return nil, nil
}

func (r *InternalUserWebhook) ValidateUpdate(_ context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	oldUser, ok := oldObj.(*InternalUser)
	if !ok {
		return nil, errors.New("old object is not an InternalUser")
	}
	newUser, ok := newObj.(*InternalUser)
	if !ok {
		return nil, errors.New("new object is not an InternalUser")
	}
	// Finalizer removal during deletion is an update that must not re-run
	// identity or issuer allowlist validation. Otherwise removing an issuer
	// from the allowlist would make its existing InternalUsers impossible to
	// delete, because cleanup would be blocked by the webhook itself.
	if newUser.DeletionTimestamp != nil {
		return nil, nil
	}
	if oldUser.Spec.Identity != newUser.Spec.Identity {
		return nil, errors.New("InternalUser identity is immutable")
	}
	if oldUser.Spec.RoleProfile != newUser.Spec.RoleProfile {
		return nil, errors.New("InternalUser roleProfile is immutable")
	}
	if err := r.validate(newUser); err != nil {
		return nil, err
	}
	return nil, nil
}

func (r *InternalUserWebhook) ValidateDelete(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	if _, ok := obj.(*InternalUser); !ok {
		return nil, errors.New("object is not an InternalUser")
	}
	return nil, nil
}

func (r *InternalUserWebhook) validate(user *InternalUser) error {
	if user.Name == "" {
		return errors.New("InternalUser name is required")
	}
	if user.Spec.RoleProfile != BaseReadonlyProfile {
		return fmt.Errorf("InternalUser roleProfile must be %q", BaseReadonlyProfile)
	}
	if err := internalcredentials.ValidateIdentity(user.Spec.Identity.Issuer, user.Spec.Identity.Subject); err != nil {
		return fmt.Errorf("invalid InternalUser identity: %w", err)
	}
	if len(r.AllowedIssuers) == 0 {
		return errors.New("InternalUser OIDC issuer allowlist is not configured")
	}
	if _, ok := r.AllowedIssuers[user.Spec.Identity.Issuer]; !ok {
		return fmt.Errorf("OIDC issuer %q is not allowed", user.Spec.Identity.Issuer)
	}
	expectedName := internalcredentials.DeriveInternalUserName(user.Spec.Identity.Issuer, user.Spec.Identity.Subject)
	if user.Name != expectedName {
		return fmt.Errorf("InternalUser name must be %q for the supplied identity", expectedName)
	}
	return nil
}

var _ client.Object = &InternalUser{}
