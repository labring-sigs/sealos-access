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
	"strings"
	"unicode"

	"github.com/labring-sigs/sealos-access/pkg/internalcredentials"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

type CredentialLeaseWebhook struct{}

func (r *CredentialLeaseWebhook) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&CredentialLease{}).
		WithDefaulter(r).
		WithValidator(r).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-user-sealos-io-v1-credentiallease,mutating=true,failurePolicy=fail,sideEffects=None,groups=user.sealos.io,resources=credentialleases,verbs=create;update,versions=v1,name=mcredentiallease.kb.io,admissionReviewVersions=v1

var _ webhook.CustomDefaulter = &CredentialLeaseWebhook{}

func (r *CredentialLeaseWebhook) Default(_ context.Context, obj runtime.Object) error {
	lease, ok := obj.(*CredentialLease)
	if !ok {
		return errors.New("object is not a CredentialLease")
	}
	if lease.Namespace == "" {
		lease.Namespace = CredentialBrokerNamespace
	}
	return nil
}

// +kubebuilder:webhook:path=/validate-user-sealos-io-v1-credentiallease,mutating=false,failurePolicy=fail,sideEffects=None,groups=user.sealos.io,resources=credentialleases,verbs=create;update;delete,versions=v1,name=vcredentiallease.kb.io,admissionReviewVersions=v1

var _ webhook.CustomValidator = &CredentialLeaseWebhook{}

func (r *CredentialLeaseWebhook) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	lease, ok := obj.(*CredentialLease)
	if !ok {
		return nil, errors.New("object is not a CredentialLease")
	}
	if err := validateCredentialLease(lease); err != nil {
		return nil, err
	}
	return nil, nil
}

func (r *CredentialLeaseWebhook) ValidateUpdate(_ context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	oldLease, ok := oldObj.(*CredentialLease)
	if !ok {
		return nil, errors.New("old object is not a CredentialLease")
	}
	newLease, ok := newObj.(*CredentialLease)
	if !ok {
		return nil, errors.New("new object is not a CredentialLease")
	}
	if oldLease.Spec != newLease.Spec {
		return nil, errors.New("CredentialLease spec is immutable")
	}
	if err := validateCredentialLease(newLease); err != nil {
		return nil, err
	}
	return nil, nil
}

func (r *CredentialLeaseWebhook) ValidateDelete(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	if _, ok := obj.(*CredentialLease); !ok {
		return nil, errors.New("object is not a CredentialLease")
	}
	return nil, nil
}

func validateCredentialLease(lease *CredentialLease) error {
	if lease.Namespace != CredentialBrokerNamespace {
		return fmt.Errorf("CredentialLease must be created in namespace %q", CredentialBrokerNamespace)
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
	if lease.Spec.Profile == ClusterOpsWriteProfile && lease.Spec.ApprovalReference == "" {
		return errors.New("elevated CredentialLease requires an approval reference")
	}
	if lease.Spec.Profile == ClusterOpsWriteProfile && lease.Spec.ApprovalID == "" {
		return errors.New("elevated CredentialLease requires an approval ID")
	}
	if len(lease.Spec.ApprovalReference) > 512 {
		return errors.New("approval reference must be at most 512 bytes")
	}
	if len(lease.Spec.ApprovalID) > 512 || strings.TrimSpace(lease.Spec.ApprovalID) != lease.Spec.ApprovalID || strings.IndexFunc(lease.Spec.ApprovalID, unicode.IsSpace) >= 0 {
		return errors.New("approval ID is invalid")
	}
	if len(lease.Spec.ApprovalReason) > 2048 {
		return errors.New("approval reason must be at most 2048 bytes")
	}
	return nil
}
