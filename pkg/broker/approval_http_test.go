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

package broker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	userv1 "github.com/labring-sigs/sealos-access/api/v1"
	"github.com/labring-sigs/sealos-access/pkg/internalcredentials"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestInternalApproveRejectsUnauthenticatedOrNonAdminClients(t *testing.T) {
	now := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	adminPrincipal := Principal{Issuer: issuerForP0, Subject: "admin", Groups: []string{"sealos.internal.admin"}}

	t.Run("missing authorization header", func(t *testing.T) {
		server := p0ApprovalServer(t, now, fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(activeP0User()).Build())
		request := httptest.NewRequest(http.MethodPost, testTargetPath("/v1/internal/credentials/approve"), strings.NewReader(validApprovalBody()))
		request.RemoteAddr = "127.0.0.1:443"
		request.Header.Set("X-Trusted-Client-IP", "10.1.2.3")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, body = %s, want %d", response.Code, response.Body.String(), http.StatusUnauthorized)
		}
		leases := &userv1.CredentialLeaseList{}
		if err := testLeaseClient(server).List(context.Background(), leases); err != nil {
			t.Fatal(err)
		}
		if len(leases.Items) != 0 {
			t.Fatalf("invalid client created %d leases", len(leases.Items))
		}
	})

	t.Run("empty bearer token", func(t *testing.T) {
		server := p0ApprovalServer(t, now, fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(activeP0User()).Build())
		request := httptest.NewRequest(http.MethodPost, testTargetPath("/v1/internal/credentials/approve"), strings.NewReader(validApprovalBody()))
		request.RemoteAddr = "127.0.0.1:443"
		request.Header.Set("X-Trusted-Client-IP", "10.1.2.3")
		request.Header.Set("Authorization", "Bearer ")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, body = %s, want %d", response.Code, response.Body.String(), http.StatusUnauthorized)
		}
		leases := &userv1.CredentialLeaseList{}
		if err := testLeaseClient(server).List(context.Background(), leases); err != nil {
			t.Fatal(err)
		}
		if len(leases.Items) != 0 {
			t.Fatalf("invalid client created %d leases", len(leases.Items))
		}
	})

	t.Run("non-admin group", func(t *testing.T) {
		server := p0ApprovalServer(t, now, fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(activeP0User()).Build())
		// Use a non-admin principal
		server.Authenticator = staticAuthenticator{principal: Principal{Issuer: issuerForP0, Subject: "alice", Groups: []string{"users"}}}
		request := httptest.NewRequest(http.MethodPost, testTargetPath("/v1/internal/credentials/approve"), strings.NewReader(validApprovalBody()))
		request.RemoteAddr = "127.0.0.1:443"
		request.Header.Set("X-Trusted-Client-IP", "10.1.2.3")
		request.Header.Set("Authorization", "Bearer alice")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Fatalf("status = %d, body = %s, want %d", response.Code, response.Body.String(), http.StatusForbidden)
		}
	})

	t.Run("admin group succeeds", func(t *testing.T) {
		server := p0ApprovalServer(t, now, fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(&userv1.CredentialLease{}).WithObjects(activeP0User()).Build())
		server.Authenticator = staticAuthenticator{principal: adminPrincipal}
		request := httptest.NewRequest(http.MethodPost, testTargetPath("/v1/internal/credentials/approve"), strings.NewReader(validApprovalBody()))
		request.RemoteAddr = "127.0.0.1:443"
		request.Header.Set("X-Trusted-Client-IP", "10.1.2.3")
		request.Header.Set("Authorization", "Bearer admin")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusCreated {
			t.Fatalf("status = %d, body = %s, want %d", response.Code, response.Body.String(), http.StatusCreated)
		}
	})
}

func TestInternalApproveValidatesAndPersistsRequest(t *testing.T) {
	now := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	adminPrincipal := Principal{Issuer: issuerForP0, Subject: "admin", Groups: []string{"sealos.internal.admin"}}
	valid := func(t *testing.T, server *Server, body string, method string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, testTargetPath("/v1/internal/credentials/approve"), strings.NewReader(body))
		request.RemoteAddr = "127.0.0.1:443"
		request.Header.Set("X-Trusted-Client-IP", "10.1.2.3")
		request.Header.Set("Authorization", "Bearer admin")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		return response
	}
	tests := []struct {
		name   string
		body   string
		method string
	}{
		{name: "malformed JSON", body: "{"},
		{name: "unknown field", body: validApprovalBody()[:len(validApprovalBody())-1] + `,"unknown":"field"}`},
		{name: "multiple JSON values", body: validApprovalBody() + validApprovalBody()},
		{name: "invalid approval ID", body: strings.Replace(validApprovalBody(), "approval-instance", "approval instance", 1)},
		{name: "invalid reason", body: strings.Replace(validApprovalBody(), "approval reason", "", 1)},
		{name: "invalid TTL", body: strings.Replace(validApprovalBody(), "1800", "60", 1)},
		{name: "invalid identity", body: strings.Replace(validApprovalBody(), "https://issuer.example", "http://issuer.example", 1)},
		{name: "missing target", body: `{"approvalID":"approval-instance-1","requestedTTLSeconds":1800,"approvalReason":"approval reason"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := p0ApprovalServer(t, now, fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(activeP0User()).Build())
			server.Authenticator = staticAuthenticator{principal: adminPrincipal}
			response := valid(t, server, tt.body, http.MethodPost)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s, want %d", response.Code, response.Body.String(), http.StatusBadRequest)
			}
			if strings.Contains(response.Body.String(), "clusterroles") || strings.Contains(response.Body.String(), "internal-credential") {
				t.Fatalf("response leaks internal error: %s", response.Body.String())
			}
		})
	}

	t.Run("invalid method", func(t *testing.T) {
		server := p0ApprovalServer(t, now, fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(activeP0User()).Build())
		server.Authenticator = staticAuthenticator{principal: adminPrincipal}
		response := valid(t, server, "", http.MethodGet)
		if response.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want %d", response.Code, http.StatusMethodNotAllowed)
		}
	})

	t.Run("suspended target", func(t *testing.T) {
		user := activeP0User()
		user.Status.Phase = userv1.InternalUserSuspended
		server := p0ApprovalServer(t, now, fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(user).Build())
		server.Authenticator = staticAuthenticator{principal: adminPrincipal}
		response := valid(t, server, validApprovalBody(), http.MethodPost)
		if response.Code != http.StatusForbidden {
			t.Fatalf("status = %d, body = %s, want %d", response.Code, response.Body.String(), http.StatusForbidden)
		}
	})

	t.Run("persisted approval exists and is idempotent", func(t *testing.T) {
		server := p0ApprovalServer(t, now, fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(&userv1.CredentialLease{}).WithObjects(activeP0User()).Build())
		server.Authenticator = staticAuthenticator{principal: adminPrincipal}
		body := validApprovalBody()
		first := valid(t, server, body, http.MethodPost)
		if first.Code != http.StatusCreated {
			t.Fatalf("first approval status = %d, body = %s", first.Code, first.Body.String())
		}
		second := valid(t, server, body, http.MethodPost)
		if second.Code != http.StatusOK {
			t.Fatalf("idempotent approval status = %d, body = %s", second.Code, second.Body.String())
		}
		var firstResp, secondResp approvalResponse
		if err := json.Unmarshal(first.Body.Bytes(), &firstResp); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(second.Body.Bytes(), &secondResp); err != nil {
			t.Fatal(err)
		}
		if secondResp.ApprovalReference != firstResp.ApprovalReference || secondResp.ApprovalID != firstResp.ApprovalID {
			t.Fatalf("idempotent response = %#v, first = %#v", secondResp, firstResp)
		}
		conflict := strings.Replace(body, "approval reason", "different reason", 1)
		conflictResp := valid(t, server, conflict, http.MethodPost)
		if conflictResp.Code != http.StatusConflict {
			t.Fatalf("approval ID mismatch status = %d, want %d", conflictResp.Code, http.StatusConflict)
		}
	})
}

func TestInternalApproveLeaseNotCreatedForInvalidTarget(t *testing.T) {
	now := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	adminPrincipal := Principal{Issuer: issuerForP0, Subject: "admin", Groups: []string{"sealos.internal.admin"}}

	t.Run("non-existent user", func(t *testing.T) {
		// No InternalUser exists for this target
		server := p0ApprovalServer(t, now, fake.NewClientBuilder().WithScheme(testScheme()).Build())
		server.Authenticator = staticAuthenticator{principal: adminPrincipal}
		request := httptest.NewRequest(http.MethodPost, testTargetPath("/v1/internal/credentials/approve"), strings.NewReader(validApprovalBody()))
		request.RemoteAddr = "127.0.0.1:443"
		request.Header.Set("X-Trusted-Client-IP", "10.1.2.3")
		request.Header.Set("Authorization", "Bearer admin")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatalf("status = %d, body = %s, want %d", response.Code, response.Body.String(), http.StatusNotFound)
		}
	})

	t.Run("disallowed issuer", func(t *testing.T) {
		server := p0ApprovalServer(t, now, fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(activeP0User()).Build())
		server.Authenticator = staticAuthenticator{principal: adminPrincipal}
		body := `{"approvalID":"approval-instance-1","target":{"issuer":"https://disallowed.example","subject":"alice"},"requestedTTLSeconds":1800,"approvalReason":"approval reason"}`
		request := httptest.NewRequest(http.MethodPost, testTargetPath("/v1/internal/credentials/approve"), strings.NewReader(body))
		request.RemoteAddr = "127.0.0.1:443"
		request.Header.Set("X-Trusted-Client-IP", "10.1.2.3")
		request.Header.Set("Authorization", "Bearer admin")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, body = %s, want %d", response.Code, response.Body.String(), http.StatusBadRequest)
		}
	})
}

func TestInternalApproveSourceIPPolicy(t *testing.T) {
	now := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	adminPrincipal := Principal{Issuer: issuerForP0, Subject: "admin", Groups: []string{"sealos.internal.admin"}}

	t.Run("forbidden source IP", func(t *testing.T) {
		server := p0ApprovalServer(t, now, fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(activeP0User()).Build())
		server.Authenticator = staticAuthenticator{principal: adminPrincipal}
		// Use an IP that is not in the allowed CIDR (10.0.0.0/8) - test default allows 10.0.0.0/8
		// Request comes from 192.168.1.1 which should be forbidden
		request := httptest.NewRequest(http.MethodPost, testTargetPath("/v1/internal/credentials/approve"), strings.NewReader(validApprovalBody()))
		request.RemoteAddr = "192.168.1.1:443"
		request.Header.Set("X-Trusted-Client-IP", "192.168.1.1")
		request.Header.Set("Authorization", "Bearer admin")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Fatalf("status = %d, body = %s, want %d", response.Code, response.Body.String(), http.StatusForbidden)
		}
	})

	t.Run("allowed source IP succeeds with admin", func(t *testing.T) {
		server := p0ApprovalServer(t, now, fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(&userv1.CredentialLease{}).WithObjects(activeP0User()).Build())
		server.Authenticator = staticAuthenticator{principal: adminPrincipal}
		request := httptest.NewRequest(http.MethodPost, testTargetPath("/v1/internal/credentials/approve"), strings.NewReader(validApprovalBody()))
		request.RemoteAddr = "127.0.0.1:443"
		request.Header.Set("X-Trusted-Client-IP", "10.1.2.3")
		request.Header.Set("Authorization", "Bearer admin")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusCreated {
			t.Fatalf("status = %d, body = %s, want %d", response.Code, response.Body.String(), http.StatusCreated)
		}
	})
}

func TestInternalApprovePersistsAwaitingLeaseAndIsIdempotent(t *testing.T) {
	now := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	identity := userv1.Identity{Issuer: "https://issuer.example", Subject: "alice"}
	adminIdentity := userv1.Identity{Issuer: "https://issuer.example", Subject: "admin"}
	user := &userv1.InternalUser{
		ObjectMeta: metav1.ObjectMeta{Name: internalcredentials.DeriveInternalUserName(identity.Issuer, identity.Subject)},
		Spec:       userv1.InternalUserSpec{Identity: identity, RoleProfile: userv1.BaseReadonlyProfile},
		Status:     userv1.InternalUserStatus{Phase: userv1.InternalUserActive},
	}
	adminPrincipal := Principal{Issuer: adminIdentity.Issuer, Subject: adminIdentity.Subject, Groups: []string{"sealos.internal.admin"}}
	server := newTestServer(t, fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(&userv1.CredentialLease{}).WithObjects(user).Build(), staticAuthenticator{principal: adminPrincipal}, &recordingTokenIssuer{}, now)
	body := `{"approvalID":"approval-instance-1","target":{"issuer":"https://issuer.example","subject":"alice"},"requestedTTLSeconds":1800,"approvalReason":"deploy incident remediation"}`
	serve := func(value string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, testTargetPath("/v1/internal/credentials/approve"), strings.NewReader(value))
		req.RemoteAddr = "127.0.0.1:443"
		req.Header.Set("X-Trusted-Client-IP", "10.1.2.3")
		req.Header.Set("Authorization", "Bearer admin")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, req)
		return response
	}
	first := serve(body)
	if first.Code != http.StatusCreated {
		t.Fatalf("first approval status = %d, body = %s", first.Code, first.Body.String())
	}
	var approval approvalResponse
	if err := json.Unmarshal(first.Body.Bytes(), &approval); err != nil {
		t.Fatal(err)
	}
	if approval.ApprovalID != "approval-instance-1" || approval.Profile != userv1.ClusterOpsWriteProfile || approval.RequestedTTLSeconds != 1800 || approval.ApprovalReference == "" {
		t.Fatalf("approval response = %#v", approval)
	}
	lease := &userv1.CredentialLease{}
	if err := testLeaseClient(server).Get(context.Background(), types.NamespacedName{Namespace: userv1.CredentialBrokerNamespace, Name: approvalLeaseName("approval-instance-1")}, lease); err != nil {
		t.Fatal(err)
	}
	if lease.Status.Phase != userv1.CredentialLeaseAwaitingRedemption || lease.Status.ApprovalExpirationTimestamp == nil || len(lease.Status.Conditions) != 0 {
		t.Fatalf("approval lease status = %#v", lease.Status)
	}
	serviceAccounts := &corev1.ServiceAccountList{}
	if err := testLeaseClient(server).List(context.Background(), serviceAccounts, client.InNamespace(userv1.InternalUserSystemNamespace)); err != nil {
		t.Fatal(err)
	}
	if len(serviceAccounts.Items) != 0 {
		t.Fatalf("approval created %d ServiceAccounts before redemption", len(serviceAccounts.Items))
	}
	second := serve(body)
	if second.Code != http.StatusOK {
		t.Fatalf("idempotent approval status = %d, body = %s", second.Code, second.Body.String())
	}
	var retry approvalResponse
	if err := json.Unmarshal(second.Body.Bytes(), &retry); err != nil {
		t.Fatal(err)
	}
	if retry.ApprovalReference != approval.ApprovalReference || retry.ApprovalID != approval.ApprovalID {
		t.Fatalf("idempotent response = %#v, first = %#v", retry, approval)
	}
	conflict := serve(strings.Replace(body, "deploy incident remediation", "different target operation", 1))
	if conflict.Code != http.StatusConflict {
		t.Fatalf("approval ID mismatch status = %d, want %d", conflict.Code, http.StatusConflict)
	}
}

func TestInternalApproveRequiresOIDCAdminAuth(t *testing.T) {
	identity := userv1.Identity{Issuer: "https://issuer.example", Subject: "alice"}
	user := &userv1.InternalUser{ObjectMeta: metav1.ObjectMeta{Name: internalcredentials.DeriveInternalUserName(identity.Issuer, identity.Subject)}, Spec: userv1.InternalUserSpec{Identity: identity}, Status: userv1.InternalUserStatus{Phase: userv1.InternalUserActive}}
	server := newTestServer(t, fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(user).Build(), staticAuthenticator{principal: Principal{Issuer: identity.Issuer, Subject: identity.Subject}}, &recordingTokenIssuer{}, time.Now())
	request := httptest.NewRequest(http.MethodPost, testTargetPath("/v1/internal/credentials/approve"), strings.NewReader(`{"approvalID":"id","target":{"issuer":"https://issuer.example","subject":"alice"},"requestedTTLSeconds":1800,"approvalReason":"reason"}`))
	request.RemoteAddr = "127.0.0.1:443"
	request.Header.Set("X-Trusted-Client-IP", "10.1.2.3")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	// Should be 401 because there's no Authorization header
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
}

const (
	issuerForP0   = "https://issuer.example"
	userNSForP0   = userv1.InternalUserSystemNamespace
	brokerNSForP0 = userv1.CredentialBrokerNamespace
)

func p0ApprovalServer(t *testing.T, now time.Time, leaseClient client.Client) *Server {
	t.Helper()
	t.Helper()
	return newTestServer(t, leaseClient, staticAuthenticator{principal: Principal{Issuer: issuerForP0, Subject: "alice"}}, &recordingTokenIssuer{}, now)
}

func validApprovalBody() string {
	return `{"approvalID":"approval-instance-1","target":{"issuer":"https://issuer.example","subject":"alice"},"requestedTTLSeconds":1800,"approvalReason":"approval reason"}`
}

func activeP0User() *userv1.InternalUser {
	identity := userv1.Identity{Issuer: issuerForP0, Subject: "alice"}
	return &userv1.InternalUser{
		ObjectMeta: metav1.ObjectMeta{Name: internalcredentials.DeriveInternalUserName(identity.Issuer, identity.Subject)},
		Spec:       userv1.InternalUserSpec{Identity: identity, RoleProfile: userv1.BaseReadonlyProfile},
		Status:     userv1.InternalUserStatus{Phase: userv1.InternalUserActive},
	}
}
