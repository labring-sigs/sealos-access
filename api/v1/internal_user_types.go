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

const (
	InternalUserSystemNamespace = "access-system"
	CredentialBrokerNamespace   = "access-broker-system"

	BaseReadonlyProfile    = "base-readonly.v1"
	ClusterOpsWriteProfile = "cluster-ops-write.v1"

	InternalUserFinalizer    = "user.sealos.io/internal-user-finalizer"
	CredentialLeaseFinalizer = "user.sealos.io/credential-lease-finalizer"

	ManagedByLabel          = "user.sealos.io/managed-by"
	InternalUserLabel       = "user.sealos.io/internal-user"
	InternalUserUIDLabel    = "user.sealos.io/internal-user-uid"
	CredentialLeaseLabel    = "user.sealos.io/credential-lease"
	CredentialLeaseUIDLabel = "user.sealos.io/credential-lease-uid"
)

// Identity is an immutable OIDC issuer and subject pair.
type Identity struct {
	// Issuer is an HTTPS issuer from the server-side allowlist.
	Issuer string `json:"issuer"`
	// Subject is the opaque, immutable OIDC subject.
	Subject string `json:"subject"`
}

// InternalUserSpec defines one real internal person's identity and base profile.
type InternalUserSpec struct {
	Identity Identity `json:"identity"`
	// +kubebuilder:validation:Enum=base-readonly.v1
	RoleProfile string `json:"roleProfile"`
	Suspend     bool   `json:"suspend,omitempty"`
}

type InternalUserPhase string

const (
	InternalUserPending   InternalUserPhase = "Pending"
	InternalUserActive    InternalUserPhase = "Active"
	InternalUserSuspended InternalUserPhase = "Suspended"
	InternalUserFailed    InternalUserPhase = "Failed"
)

// InternalUserStatus records lifecycle state and non-secret resource references.
type InternalUserStatus struct {
	//+kubebuilder:validation:Enum=Pending;Active;Suspended;Failed
	Phase InternalUserPhase `json:"phase,omitempty"`

	DerivedName string `json:"derivedName,omitempty"`

	// StableServiceAccountRef points to the stable ServiceAccount in
	// access-system. It never contains credential data.
	StableServiceAccountRef *corev1.ObjectReference `json:"stableServiceAccountRef,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="DerivedName",type="string",JSONPath=".status.derivedName"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// InternalUser is the Schema for internal users.
type InternalUser struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   InternalUserSpec   `json:"spec,omitempty"`
	Status InternalUserStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// InternalUserList contains a list of InternalUser objects.
type InternalUserList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []InternalUser `json:"items"`
}

func init() {
	SchemeBuilder.Register(&InternalUser{}, &InternalUserList{})
}
