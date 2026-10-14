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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CredentialLeaseSpec contains only non-secret request data. It is immutable
// after creation; the Broker is the only component that creates or mutates it.
type CredentialLeaseSpec struct {
	Target    Identity `json:"target"`
	Requester Identity `json:"requester"`

	// +kubebuilder:validation:Enum=base-readonly.v1;cluster-ops-write.v1
	Profile string `json:"profile"`

	// RequestedTTLSeconds is an upper-bounded request. The API server's actual
	// TokenRequest expiration is recorded in status.
	RequestedTTLSeconds int64 `json:"requestedTTLSeconds"`

	// ApprovalReference is an immutable external audit reference. It is not an
	// authentication factor.
	ApprovalReference string `json:"approvalReference,omitempty"`

	// ApprovalID is the immutable identifier from the external approval system.
	// It is used as the idempotency key and does not authorize redemption.
	ApprovalID string `json:"approvalID,omitempty"`

	// ApprovalReason is operator-supplied audit metadata. It must never contain
	// a token, kubeconfig, private key, or Secret data.
	ApprovalReason string `json:"approvalReason,omitempty"`
}

type CredentialLeasePhase string

const (
	CredentialLeaseAwaitingRedemption CredentialLeasePhase = "AwaitingRedemption"
	CredentialLeasePending            CredentialLeasePhase = "Pending"
	CredentialLeasePrepared           CredentialLeasePhase = "Prepared"
	CredentialLeaseIssued             CredentialLeasePhase = "Issued"
	CredentialLeaseFailed             CredentialLeasePhase = "Failed"
	CredentialLeaseRevoked            CredentialLeasePhase = "Revoked"
	CredentialLeaseExpired            CredentialLeasePhase = "Expired"
)

// CredentialLeaseStatus records lifecycle and resource references only. It
// must never contain a token, kubeconfig, private key, or Secret data.
type CredentialLeaseStatus struct {
	//+kubebuilder:validation:Enum=AwaitingRedemption;Pending;Prepared;Issued;Failed;Revoked;Expired
	Phase CredentialLeasePhase `json:"phase,omitempty"`

	ServiceAccountRef *corev1.ObjectReference `json:"serviceAccountRef,omitempty"`
	BoundSecretRef    *corev1.ObjectReference `json:"boundSecretRef,omitempty"`
	BindingRef        *corev1.ObjectReference `json:"bindingRef,omitempty"`

	// ConsumedAt is written with a resourceVersion compare-and-swap immediately
	// before the irreversible TokenRequest. A non-nil value prevents re-issue,
	// even if the response or the subsequent status update is lost.
	ConsumedAt *metav1.Time `json:"consumedAt,omitempty"`

	// ApprovalExpirationTimestamp bounds how long a generated reference can
	// remain unredeemed.
	ApprovalExpirationTimestamp *metav1.Time `json:"approvalExpirationTimestamp,omitempty"`
	RedeemedAt                  *metav1.Time `json:"redeemedAt,omitempty"`

	ExpirationTimestamp *metav1.Time       `json:"expirationTimestamp,omitempty"`
	Conditions          []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Profile",type="string",JSONPath=".spec.profile"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Expires",type="date",JSONPath=".status.expirationTimestamp"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// CredentialLease is the internal broker persistence record for one credential
// issuance attempt.
type CredentialLease struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CredentialLeaseSpec   `json:"spec,omitempty"`
	Status CredentialLeaseStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// CredentialLeaseList contains a list of CredentialLease objects.
type CredentialLeaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CredentialLease `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CredentialLease{}, &CredentialLeaseList{})
}
