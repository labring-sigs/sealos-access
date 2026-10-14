package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	userv1 "github.com/labring-sigs/sealos-access/api/v1"
	"github.com/labring-sigs/sealos-access/pkg/internalcredentials"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestMultiTargetServerKeepsCredentialIssuanceOnSelectedTarget(t *testing.T) {
	now := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	identity := userv1.Identity{Issuer: "https://issuer.example", Subject: "alice"}
	clientA, issuerA := multiTargetClient(t, identity, "cluster-a", now)
	clientB, issuerB := multiTargetClient(t, identity, "cluster-b", now)
	backendA, err := NewStaticTargetBackend(TargetSettings{
		ID:                 "cluster-a",
		APIServer:          "https://api.cluster-a.example",
		ClusterServer:      "https://api.cluster-a.example",
		ClusterCAData:      []byte("ca-a"),
		KubernetesAudience: "audience-a",
		UserNamespace:      userv1.InternalUserSystemNamespace,
		BrokerNamespace:    userv1.CredentialBrokerNamespace,
		ClusterName:        "cluster-a",
		Enabled:            true,
	}, clientA, issuerA)
	if err != nil {
		t.Fatal(err)
	}
	backendB, err := NewStaticTargetBackend(TargetSettings{
		ID:                 "cluster-b",
		APIServer:          "https://api.cluster-b.example",
		ClusterServer:      "https://api.cluster-b.example",
		ClusterCAData:      []byte("ca-b"),
		KubernetesAudience: "audience-b",
		UserNamespace:      userv1.InternalUserSystemNamespace,
		BrokerNamespace:    userv1.CredentialBrokerNamespace,
		ClusterName:        "cluster-b",
		Enabled:            true,
	}, clientB, issuerB)
	if err != nil {
		t.Fatal(err)
	}
	trusted, err := ParseCIDRs([]string{"127.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	allowed, err := ParseCIDRs([]string{"10.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewMultiTargetServer(map[string]TargetBackend{"cluster-a": backendA, "cluster-b": backendB}, staticAuthenticator{principal: Principal{Issuer: identity.Issuer, Subject: identity.Subject}}, Config{
		AllowedIssuers: map[string]struct{}{identity.Issuer: {}},
		AdminGroup:     "sealos.internal.admin",
		SourceIP:       SourceIPPolicy{TrustedProxyCIDRs: trusted, AllowedClientCIDRs: allowed, TrustedClientIPHeader: "X-Trusted-Client-IP"},
		Clock:          func() time.Time { return now },
		PrepareTimeout: time.Second,
		PollInterval:   time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := func(clusterID string) issueResponse {
		httpRequest := httptest.NewRequest(http.MethodPost, "/v1/clusters/"+clusterID+"/credentials/base", bytes.NewBufferString(`{"requestedTTLSeconds":1800}`))
		httpRequest.RemoteAddr = "127.0.0.1:443"
		httpRequest.Header.Set("X-Trusted-Client-IP", "10.1.2.3")
		httpRequest.Header.Set("Authorization", "Bearer valid")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, httpRequest)
		if response.Code != http.StatusOK {
			t.Fatalf("target %s response = %d %s", clusterID, response.Code, response.Body.String())
		}
		var result issueResponse
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	resultA := request("cluster-a")
	resultB := request("cluster-b")
	if resultA.ClusterID != "cluster-a" || resultB.ClusterID != "cluster-b" {
		t.Fatalf("target IDs = %q, %q", resultA.ClusterID, resultB.ClusterID)
	}
	if !strings.Contains(resultA.Kubeconfig, "api.cluster-a.example") || !strings.Contains(resultB.Kubeconfig, "api.cluster-b.example") {
		t.Fatalf("kubeconfigs were not target-specific: %q / %q", resultA.Kubeconfig, resultB.Kubeconfig)
	}
	if issuerA.calls != 1 || issuerB.calls != 1 {
		t.Fatalf("TokenIssuer calls = %d, %d", issuerA.calls, issuerB.calls)
	}
	leaseA := &userv1.CredentialLease{}
	if err := clientA.Get(context.Background(), client.ObjectKey{Namespace: userv1.CredentialBrokerNamespace, Name: resultA.LeaseID}, leaseA); err != nil {
		t.Fatal(err)
	}
	leaseB := &userv1.CredentialLease{}
	if err := clientB.Get(context.Background(), client.ObjectKey{Namespace: userv1.CredentialBrokerNamespace, Name: resultB.LeaseID}, leaseB); err != nil {
		t.Fatal(err)
	}
	if leaseA.Spec.Target != identity || leaseB.Spec.Target != identity {
		t.Fatalf("target lease identities = %#v / %#v", leaseA.Spec.Target, leaseB.Spec.Target)
	}
}

func multiTargetClient(t *testing.T, identity userv1.Identity, targetID string, now time.Time) (client.Client, *recordingTokenIssuer) {
	t.Helper()
	user := &userv1.InternalUser{
		ObjectMeta: metav1.ObjectMeta{Name: internalcredentials.DeriveInternalUserName(identity.Issuer, identity.Subject)},
		Spec:       userv1.InternalUserSpec{Identity: identity, RoleProfile: userv1.BaseReadonlyProfile},
		Status:     userv1.InternalUserStatus{Phase: userv1.InternalUserActive},
	}
	audience := "audience-a"
	if targetID == "cluster-b" {
		audience = "audience-b"
	}
	issuer := &recordingTokenIssuer{result: TokenResult{Token: "token-" + targetID, Expiration: now.Add(20 * time.Minute), Audiences: []string{audience}}}
	client := fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(&userv1.CredentialLease{}).WithObjects(user).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
			lease, ok := object.(*userv1.CredentialLease)
			if !ok {
				return c.Create(ctx, object, opts...)
			}
			if lease.Spec.Profile != userv1.BaseReadonlyProfile {
				return c.Create(ctx, object, opts...)
			}
			lease.Name = "lease-" + targetID
			lease.GenerateName = ""
			lease.UID = types.UID("uid-" + targetID)
			lease.Status.Phase = userv1.CredentialLeasePrepared
			lease.Status.ServiceAccountRef = &v1.ObjectReference{APIVersion: "v1", Kind: "ServiceAccount", Namespace: userv1.InternalUserSystemNamespace, Name: internalcredentials.DeriveInternalUserName(identity.Issuer, identity.Subject)}
			lease.Status.BoundSecretRef = &v1.ObjectReference{APIVersion: "v1", Kind: "Secret", Namespace: userv1.InternalUserSystemNamespace, Name: "secret-" + targetID, UID: types.UID("secret-" + targetID)}
			return c.Create(ctx, object, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, resource string, object client.Object, opts ...client.SubResourceUpdateOption) error {
			if resource == "status" {
				if lease, ok := object.(*userv1.CredentialLease); ok && lease.Spec.Profile == userv1.ClusterOpsWriteProfile && lease.Status.Phase == userv1.CredentialLeasePending {
					prepareMultiTargetElevatedLeaseObject(lease)
				}
			}
			return c.Status().Update(ctx, object, opts...)
		},
	}).Build()
	return client, issuer
}

func TestMultiTargetElevatedApprovalAndRedeemStaysOnOneTarget(t *testing.T) {
	now := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	server, clientA, clientB, issuerA, issuerB := newP1MultiTargetServer(t, now)
	approvalBody := `{"clusterID":"cluster-a","approvalID":"approval-a","target":{"issuer":"https://issuer.example","subject":"alice"},"requestedTTLSeconds":1800,"approvalReason":"cluster-a operation"}`
	approvalResponseRecorder := p1MultiTargetRequest(server, "admin-token", "/v1/clusters/cluster-a/internal/credentials/approve", approvalBody)
	if approvalResponseRecorder.Code != http.StatusCreated {
		t.Fatalf("approval status = %d, body = %s", approvalResponseRecorder.Code, approvalResponseRecorder.Body.String())
	}
	var approval approvalResponse
	if err := json.Unmarshal(approvalResponseRecorder.Body.Bytes(), &approval); err != nil {
		t.Fatal(err)
	}
	if approval.ClusterID != "cluster-a" || approval.ApprovalReference == "" {
		t.Fatalf("approval response = %#v", approval)
	}

	lease := &userv1.CredentialLease{}
	if err := clientA.Get(context.Background(), client.ObjectKey{Namespace: userv1.CredentialBrokerNamespace, Name: approvalLeaseName("approval-a")}, lease); err != nil {
		t.Fatal(err)
	}
	redeem := p1MultiTargetRequest(server, "alice-token", "/v1/clusters/cluster-a/credentials/elevated/redeem", `{"approvalReference":"`+approval.ApprovalReference+`"}`)
	if redeem.Code != http.StatusOK {
		t.Fatalf("redeem status = %d, body = %s", redeem.Code, redeem.Body.String())
	}
	var issued issueResponse
	if err := json.Unmarshal(redeem.Body.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}
	if issued.ClusterID != "cluster-a" || !strings.Contains(issued.Kubeconfig, "api.cluster-a.example") {
		t.Fatalf("redeem response = %#v", issued)
	}
	if issuerA.calls != 1 || issuerB.calls != 0 {
		t.Fatalf("TokenRequest calls after redeem = %d, %d", issuerA.calls, issuerB.calls)
	}

	crossTarget := p1MultiTargetRequest(server, "alice-token", "/v1/clusters/cluster-b/credentials/elevated/redeem", `{"approvalReference":"`+approval.ApprovalReference+`"}`)
	if crossTarget.Code != http.StatusForbidden {
		t.Fatalf("cross-target redeem status = %d, body = %s", crossTarget.Code, crossTarget.Body.String())
	}
	if issuerB.calls != 0 {
		t.Fatalf("cross-target redeem reached target B TokenRequest: %d", issuerB.calls)
	}
	if err := clientB.Get(context.Background(), client.ObjectKey{Namespace: userv1.CredentialBrokerNamespace, Name: approvalLeaseName("approval-a")}, &userv1.CredentialLease{}); err == nil {
		t.Fatal("target A approval lease was visible in target B")
	}
}

func TestMultiTargetFailureAndRevokeAreIndependent(t *testing.T) {
	now := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	server, clientA, clientB, issuerA, issuerB := newP1MultiTargetServer(t, now)

	server.Targets["cluster-a"] = mustStaticTargetBackend(t, TargetSettings{
		ID:                 "cluster-a",
		APIServer:          "https://api.cluster-a.example",
		ClusterServer:      "https://api.cluster-a.example",
		ClusterCAData:      []byte("ca-a"),
		KubernetesAudience: "audience-a",
		UserNamespace:      userv1.InternalUserSystemNamespace,
		BrokerNamespace:    userv1.CredentialBrokerNamespace,
		ClusterName:        "cluster-a",
		Enabled:            true,
	}, &failingLeaseClient{Client: clientA, err: errors.New("cluster A API unavailable")}, issuerA)
	failing := p1MultiTargetRequest(server, "alice-token", "/v1/clusters/cluster-a/credentials/base", `{"requestedTTLSeconds":1800}`)
	if failing.Code != http.StatusServiceUnavailable {
		t.Fatalf("target A outage status = %d, body = %s", failing.Code, failing.Body.String())
	}
	working := p1MultiTargetRequest(server, "alice-token", "/v1/clusters/cluster-b/credentials/base", `{"requestedTTLSeconds":1800}`)
	if working.Code != http.StatusOK {
		t.Fatalf("target B status during target A outage = %d, body = %s", working.Code, working.Body.String())
	}
	if issuerA.calls != 0 || issuerB.calls != 1 {
		t.Fatalf("TokenRequest calls during outage = %d, %d", issuerA.calls, issuerB.calls)
	}

	server, clientA, clientB, issuerA, issuerB = newP1MultiTargetServer(t, now)
	issuedA := p1MultiTargetRequest(server, "alice-token", "/v1/clusters/cluster-a/credentials/base", `{"requestedTTLSeconds":1800}`)
	issuedB := p1MultiTargetRequest(server, "alice-token", "/v1/clusters/cluster-b/credentials/base", `{"requestedTTLSeconds":1800}`)
	if issuedA.Code != http.StatusOK || issuedB.Code != http.StatusOK {
		t.Fatalf("initial issue statuses = %d, %d", issuedA.Code, issuedB.Code)
	}
	var responseA issueResponse
	if err := json.Unmarshal(issuedA.Body.Bytes(), &responseA); err != nil {
		t.Fatal(err)
	}
	revoke := p1MultiTargetRequest(server, "alice-token", "/v1/clusters/cluster-a/credentials/"+responseA.LeaseID+"/revoke", "")
	if revoke.Code != http.StatusOK {
		t.Fatalf("target A revoke status = %d, body = %s", revoke.Code, revoke.Body.String())
	}
	leaseA := &userv1.CredentialLease{}
	if err := clientA.Get(context.Background(), client.ObjectKey{Namespace: userv1.CredentialBrokerNamespace, Name: responseA.LeaseID}, leaseA); err != nil {
		t.Fatal(err)
	}
	if leaseA.Status.Phase != userv1.CredentialLeaseRevoked {
		t.Fatalf("target A lease phase after revoke = %s", leaseA.Status.Phase)
	}
	leaseB := &userv1.CredentialLease{}
	var responseB issueResponse
	if err := json.Unmarshal(issuedB.Body.Bytes(), &responseB); err != nil {
		t.Fatal(err)
	}
	if err := clientB.Get(context.Background(), client.ObjectKey{Namespace: userv1.CredentialBrokerNamespace, Name: responseB.LeaseID}, leaseB); err != nil {
		t.Fatal(err)
	}
	if leaseB.Status.Phase != userv1.CredentialLeaseIssued || issuerA.calls != 1 || issuerB.calls != 1 {
		t.Fatalf("target B state/calls after target A revoke = phase %s, calls %d/%d", leaseB.Status.Phase, issuerA.calls, issuerB.calls)
	}
}

func TestMultiTargetClientCannotOverrideServerOwnedTargetSettings(t *testing.T) {
	now := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	server, _, _, issuerA, _ := newP1MultiTargetServer(t, now)
	request := p1MultiTargetRequest(server, "alice-token", "/v1/clusters/cluster-a/credentials/base", `{"requestedTTLSeconds":1800,"clusterServer":"https://attacker.example","kubernetesAudience":"attacker"}`)
	if request.Code != http.StatusBadRequest {
		t.Fatalf("server-owned override status = %d, body = %s", request.Code, request.Body.String())
	}
	if issuerA.calls != 0 {
		t.Fatalf("server-owned override reached TokenRequest: %d", issuerA.calls)
	}
	unknownTarget := p1MultiTargetRequest(server, "alice-token", "/v1/clusters/cluster-c/credentials/base", `{"requestedTTLSeconds":1800}`)
	if unknownTarget.Code != http.StatusNotFound {
		t.Fatalf("unknown target status = %d, body = %s", unknownTarget.Code, unknownTarget.Body.String())
	}
}

func newP1MultiTargetServer(t *testing.T, now time.Time) (*Server, client.Client, client.Client, *recordingTokenIssuer, *recordingTokenIssuer) {
	t.Helper()
	identity := userv1.Identity{Issuer: "https://issuer.example", Subject: "alice"}
	clientA, issuerA := multiTargetClient(t, identity, "cluster-a", now)
	clientB, issuerB := multiTargetClient(t, identity, "cluster-b", now)
	server, err := NewMultiTargetServer(map[string]TargetBackend{
		"cluster-a": mustStaticTargetBackend(t, TargetSettings{
			ID:                 "cluster-a",
			APIServer:          "https://api.cluster-a.example",
			ClusterServer:      "https://api.cluster-a.example",
			ClusterCAData:      []byte("ca-a"),
			KubernetesAudience: "audience-a",
			UserNamespace:      userv1.InternalUserSystemNamespace,
			BrokerNamespace:    userv1.CredentialBrokerNamespace,
			ClusterName:        "cluster-a",
			Enabled:            true,
		}, clientA, issuerA),
		"cluster-b": mustStaticTargetBackend(t, TargetSettings{
			ID:                 "cluster-b",
			APIServer:          "https://api.cluster-b.example",
			ClusterServer:      "https://api.cluster-b.example",
			ClusterCAData:      []byte("ca-b"),
			KubernetesAudience: "audience-b",
			UserNamespace:      userv1.InternalUserSystemNamespace,
			BrokerNamespace:    userv1.CredentialBrokerNamespace,
			ClusterName:        "cluster-b",
			Enabled:            true,
		}, clientB, issuerB),
	}, identityAuthenticator{principals: map[string]Principal{
		"admin-token": {Issuer: identity.Issuer, Subject: "admin", Groups: []string{"sealos.internal.admin"}},
		"alice-token": {Issuer: identity.Issuer, Subject: identity.Subject},
	}}, Config{
		AllowedIssuers: map[string]struct{}{identity.Issuer: {}},
		AdminGroup:     "sealos.internal.admin",
		SourceIP:       p1SourcePolicy(t),
		Clock:          func() time.Time { return now },
		PrepareTimeout: time.Second,
		PollInterval:   time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return server, clientA, clientB, issuerA, issuerB
}

func mustStaticTargetBackend(t *testing.T, settings TargetSettings, leaseClient client.Client, issuer TokenIssuer) TargetBackend {
	t.Helper()
	backend, err := NewStaticTargetBackend(settings, leaseClient, issuer)
	if err != nil {
		t.Fatal(err)
	}
	return backend
}

func p1SourcePolicy(t *testing.T) SourceIPPolicy {
	t.Helper()
	trusted, err := ParseCIDRs([]string{"127.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	allowed, err := ParseCIDRs([]string{"10.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	return SourceIPPolicy{TrustedProxyCIDRs: trusted, AllowedClientCIDRs: allowed, TrustedClientIPHeader: "X-Trusted-Client-IP"}
}

func p1MultiTargetRequest(server *Server, token, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	request.RemoteAddr = "127.0.0.1:443"
	request.Header.Set("X-Trusted-Client-IP", "10.1.2.3")
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response
}

func prepareMultiTargetElevatedLease(t *testing.T, leaseClient client.Client, lease *userv1.CredentialLease) {
	t.Helper()
	prepareMultiTargetElevatedLeaseObject(lease)
	if err := leaseClient.Status().Update(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
}

func prepareMultiTargetElevatedLeaseObject(lease *userv1.CredentialLease) {
	serviceAccountName := internalcredentials.ResourceName("internal-lease", lease.Namespace, lease.Name)
	secretName := internalcredentials.ResourceName("internal-lease-secret", lease.Namespace, lease.Name)
	bindingName := internalcredentials.ResourceName("internal-lease-binding", lease.Namespace, lease.Name)
	lease.Status.Phase = userv1.CredentialLeasePrepared
	lease.Status.ServiceAccountRef = &v1.ObjectReference{APIVersion: "v1", Kind: "ServiceAccount", Namespace: userv1.InternalUserSystemNamespace, Name: serviceAccountName, UID: types.UID("sa-" + lease.Name)}
	lease.Status.BoundSecretRef = &v1.ObjectReference{APIVersion: "v1", Kind: "Secret", Namespace: userv1.InternalUserSystemNamespace, Name: secretName, UID: types.UID("secret-" + lease.Name)}
	lease.Status.BindingRef = &v1.ObjectReference{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding", Name: bindingName, UID: types.UID("binding-" + lease.Name)}
}

type identityAuthenticator struct {
	principals map[string]Principal
}

func (a identityAuthenticator) Authenticate(_ context.Context, token string) (Principal, error) {
	principal, ok := a.principals[token]
	if !ok {
		return Principal{}, errors.New("unknown test token")
	}
	return principal, nil
}

type failingLeaseClient struct {
	client.Client
	err error
}

func (c *failingLeaseClient) Get(context.Context, types.NamespacedName, client.Object, ...client.GetOption) error {
	return c.err
}
