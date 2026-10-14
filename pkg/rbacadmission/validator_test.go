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

package rbacadmission

import (
	"context"
	"encoding/json"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	userv1 "github.com/labring-sigs/sealos-access/api/v1"
	"github.com/labring-sigs/sealos-access/pkg/internalcredentials"
)

const (
	testIssuer  = "https://issuer.example"
	testSubject = "alice"
)

func TestValidatorAllowsUnrelatedCaller(t *testing.T) {
	validator := newValidator(t, testUser(t, testSubject), nil)
	resp := validator.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UserInfo:  authenticationInfo("system:admin"),
		Operation: admissionv1.Create,
		Resource:  clusterRoleBindingResource,
		Object:    rawBinding(t, arbitraryBinding()),
	}})
	if !resp.Allowed {
		t.Fatalf("unrelated caller was denied: %#v", resp.Result)
	}
}

func TestValidatorAllowsOwnedBaseBinding(t *testing.T) {
	user := testUser(t, testSubject)
	validator := newValidator(t, user, nil)
	binding := baseBinding(user)
	if err := validator.Validate(context.Background(), admissionv1.Create, binding); err != nil {
		t.Fatalf("owned base binding was denied: %v", err)
	}
}

func TestValidatorAllowsOwnedElevatedBinding(t *testing.T) {
	user := testUser(t, testSubject)
	lease := testLease(t, "lease-a", user)
	validator := newValidator(t, user, lease)
	if err := validator.Validate(context.Background(), admissionv1.Create, elevatedBinding(lease, user)); err != nil {
		t.Fatalf("owned elevated binding was denied: %v", err)
	}
}

func TestValidatorRejectsBindingMutations(t *testing.T) {
	user := testUser(t, testSubject)
	lease := testLease(t, "lease-a", user)
	validator := newValidator(t, user, lease)

	tests := []struct {
		name   string
		object *rbacv1.ClusterRoleBinding
	}{
		{name: "arbitrary role", object: func() *rbacv1.ClusterRoleBinding {
			b := elevatedBinding(lease, user)
			b.RoleRef.Name = "cluster-admin"
			return b
		}()},
		{name: "arbitrary subject", object: func() *rbacv1.ClusterRoleBinding {
			b := elevatedBinding(lease, user)
			b.Subjects[0].Name = "other"
			return b
		}()},
		{name: "wrong ownership uid", object: func() *rbacv1.ClusterRoleBinding {
			b := elevatedBinding(lease, user)
			b.Labels[userv1.CredentialLeaseUIDLabel] = "other"
			return b
		}()},
		{name: "wrong binding name", object: func() *rbacv1.ClusterRoleBinding { b := elevatedBinding(lease, user); b.Name = "arbitrary"; return b }()},
		{name: "stable binding with lease labels", object: func() *rbacv1.ClusterRoleBinding {
			b := baseBinding(user)
			b.Labels[userv1.CredentialLeaseLabel] = lease.Name
			return b
		}()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validator.Validate(context.Background(), admissionv1.Create, tt.object); err == nil {
				t.Fatal("malformed binding was accepted")
			}
		})
	}
}

func TestValidatorRejectsCrossLeaseUpdateAndWrongDelete(t *testing.T) {
	user := testUser(t, testSubject)
	otherUser := testUser(t, "bob")
	lease := testLease(t, "lease-a", user)
	otherLease := testLease(t, "lease-b", otherUser)
	validator := newValidator(t, user, lease, otherUser, otherLease)

	crossLease := elevatedBinding(lease, user)
	crossLease.Labels = elevatedBinding(otherLease, otherUser).Labels
	if err := validator.Validate(context.Background(), admissionv1.Update, crossLease); err == nil {
		t.Fatal("binding was changed to another lease's ownership")
	}

	wrongDelete := elevatedBinding(lease, user)
	wrongDelete.Labels[userv1.CredentialLeaseUIDLabel] = "wrong-uid"
	if err := validator.Validate(context.Background(), admissionv1.Delete, wrongDelete); err == nil {
		t.Fatal("wrongly owned binding deletion was accepted")
	}

	if err := validator.Validate(context.Background(), admissionv1.Delete, elevatedBinding(lease, user)); err != nil {
		t.Fatalf("owned cleanup deletion was denied: %v", err)
	}
}

func TestValidatorFailsClosedWhenOwnerLookupFails(t *testing.T) {
	validator := newValidator(t)
	binding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{
		Name:   internalcredentials.ResourceName("internal-user-base", "", "missing"),
		Labels: map[string]string{userv1.ManagedByLabel: "internal-user-controller", userv1.InternalUserLabel: "missing"},
	}}
	resp := validator.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UserInfo:  authenticationInfo(InternalUserControllerUsername),
		Operation: admissionv1.Create,
		Resource:  clusterRoleBindingResource,
		Object:    rawBinding(t, binding),
	}})
	if resp.Allowed {
		t.Fatal("binding with a missing owner was accepted")
	}
}

func newValidator(t *testing.T, objects ...client.Object) *Validator {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := userv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := rbacv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	clientObjects := make([]client.Object, 0, len(objects))
	for _, object := range objects {
		if object != nil {
			clientObjects = append(clientObjects, object)
		}
	}
	return NewValidator(fake.NewClientBuilder().WithScheme(scheme).WithObjects(clientObjects...).Build(), scheme)
}

func testUser(t *testing.T, subject string) *userv1.InternalUser {
	t.Helper()
	return &userv1.InternalUser{
		ObjectMeta: metav1.ObjectMeta{
			Name: internalcredentials.DeriveInternalUserName(testIssuer, subject),
			UID:  types.UID("uid-" + subject),
		},
		Spec: userv1.InternalUserSpec{
			Identity:    userv1.Identity{Issuer: testIssuer, Subject: subject},
			RoleProfile: userv1.BaseReadonlyProfile,
		},
	}
}

func testLease(t *testing.T, name string, user *userv1.InternalUser) *userv1.CredentialLease {
	t.Helper()
	return &userv1.CredentialLease{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: userv1.CredentialBrokerNamespace, UID: types.UID("uid-" + name)},
		Spec: userv1.CredentialLeaseSpec{
			Target:  user.Spec.Identity,
			Profile: userv1.ClusterOpsWriteProfile,
		},
	}
}

func baseBinding(user *userv1.InternalUser) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: internalcredentials.ResourceName("internal-user-base", "", user.Name),
			Labels: map[string]string{
				userv1.ManagedByLabel:       internalUserControllerName,
				userv1.InternalUserLabel:    user.Name,
				userv1.InternalUserUIDLabel: string(user.UID),
			},
		},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: internalcredentials.BaseReadonlyRoleName},
		Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: user.Name, Namespace: userv1.InternalUserSystemNamespace}},
	}
}

func elevatedBinding(lease *userv1.CredentialLease, user *userv1.InternalUser) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: internalcredentials.ResourceName("internal-lease-binding", lease.Namespace, lease.Name),
			Labels: map[string]string{
				userv1.ManagedByLabel:          credentialLeaseControllerName,
				userv1.CredentialLeaseLabel:    lease.Name,
				userv1.CredentialLeaseUIDLabel: string(lease.UID),
				userv1.InternalUserLabel:       user.Name,
			},
		},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: internalcredentials.ClusterOpsWriteRoleName},
		Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: internalcredentials.ResourceName("internal-lease", lease.Namespace, lease.Name), Namespace: userv1.InternalUserSystemNamespace}},
	}
}

func arbitraryBinding() *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "arbitrary"}}
}

func authenticationInfo(username string) authenticationv1.UserInfo {
	return authenticationv1.UserInfo{Username: username}
}

func rawBinding(t *testing.T, binding *rbacv1.ClusterRoleBinding) runtime.RawExtension {
	t.Helper()
	raw, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	return runtime.RawExtension{Raw: raw}
}
