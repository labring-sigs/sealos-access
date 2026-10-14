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
	"strings"
	"testing"

	"github.com/labring-sigs/sealos-access/pkg/internalcredentials"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestInternalUserWebhookValidateUpdateDeletingSkipsAllowlist(t *testing.T) {
	const issuer = "https://issuer.example.invalid"
	wh := &InternalUserWebhook{AllowedIssuers: map[string]struct{}{}}
	oldUser := &InternalUser{
		Spec: InternalUserSpec{
			Identity:    Identity{Issuer: issuer, Subject: "test-user"},
			RoleProfile: BaseReadonlyProfile,
		},
	}
	newUser := oldUser.DeepCopy()
	now := metav1.Now()
	newUser.DeletionTimestamp = &now
	// Simulate the controller removing the finalizer while deleting the object.
	newUser.Finalizers = nil

	if _, err := wh.ValidateUpdate(nil, oldUser, newUser); err != nil {
		t.Fatalf("ValidateUpdate must allow finalizer removal during deletion: %v", err)
	}
}

func TestInternalUserWebhookValidateUpdateStillEnforcesAllowlist(t *testing.T) {
	const issuer = "https://issuer.example.invalid"
	wh := &InternalUserWebhook{AllowedIssuers: map[string]struct{}{"https://allowed.example": {}}}
	user := &InternalUser{
		ObjectMeta: metav1.ObjectMeta{
			Name: internalcredentials.DeriveInternalUserName(issuer, "test-user"),
		},
		Spec: InternalUserSpec{
			Identity:    Identity{Issuer: issuer, Subject: "test-user"},
			RoleProfile: BaseReadonlyProfile,
		},
	}

	_, err := wh.ValidateUpdate(nil, user.DeepCopy(), user.DeepCopy())
	if err == nil || !strings.Contains(err.Error(), "is not allowed") {
		t.Fatalf("ValidateUpdate must reject a disallowed issuer, got: %v", err)
	}
}
