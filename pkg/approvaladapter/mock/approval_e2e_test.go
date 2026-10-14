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

package mock

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	userv1 "github.com/labring-sigs/sealos-access/api/v1"
	"github.com/labring-sigs/sealos-access/controllers"
	"github.com/labring-sigs/sealos-access/pkg/approvaladapter"
	"github.com/labring-sigs/sealos-access/pkg/broker"
	"github.com/labring-sigs/sealos-access/pkg/internalcredentials"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestApprovalAdapterToBrokerServerE2E(t *testing.T) {
	const clusterID = "cluster-a"
	now := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	target := userv1.Identity{Issuer: mockIssuer, Subject: "credential-target"}
	initiatorID := "approval-operator"

	mockExternal := NewMockServer()
	externalURL := mockExternal.Start()
	defer mockExternal.Close()
	approvalCode := mockExternal.CreateApproval(CreateApprovalRequest{
		ApprovalCode:   "e2e-approval",
		ClusterID:      clusterID,
		InitiatorID:    initiatorID,
		InitiatorEmail: initiatorID + "@example.internal",
		TargetSubject:  target.Subject,
		RequestedTTL:   1800,
		Reason:         "e2e elevated access",
		Form:           map[string]any{"targetSubject": target.Subject, "ttl": 1800},
	})
	if err := mockExternal.ApproveApproval(approvalCode); err != nil {
		t.Fatal(err)
	}

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := rbacv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := userv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	targetUser := &userv1.InternalUser{
		ObjectMeta: metav1.ObjectMeta{Name: internalcredentials.DeriveInternalUserName(target.Issuer, target.Subject), UID: "e2e-user-uid"},
		Spec:       userv1.InternalUserSpec{Identity: target, RoleProfile: userv1.BaseReadonlyProfile},
		Status:     userv1.InternalUserStatus{Phase: userv1.InternalUserActive},
	}
	leaseClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&userv1.CredentialLease{}, &userv1.InternalUser{}).
		WithObjects(targetUser, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: internalcredentials.ClusterOpsWriteRoleName}}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
				switch object := object.(type) {
				case *userv1.CredentialLease:
					if object.UID == "" {
						object.UID = types.UID("e2e-lease-uid")
					}
				case *corev1.Secret:
					if object.UID == "" {
						object.UID = types.UID("e2e-secret-uid")
					}
				}
				return c.Create(ctx, object, opts...)
			},
		}).Build()

	tokenIssuer := newBlockingTokenIssuer(broker.TokenResult{
		Token:      "e2e-token-value",
		Expiration: now.Add(30 * time.Minute),
		Audiences:  []string{"kubernetes.default.svc"},
	})
	defer tokenIssuer.Release()
	authenticator := e2eAuthenticator{
		target: broker.Principal{Issuer: target.Issuer, Subject: target.Subject, Groups: []string{"sealos.internal.admin"}},
		other:  broker.Principal{Issuer: target.Issuer, Subject: "different-user"},
	}
	trustedProxyCIDRs, err := broker.ParseCIDRs([]string{"127.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	allowedClientCIDRs, err := broker.ParseCIDRs([]string{"10.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	backend, err := broker.NewStaticTargetBackend(broker.TargetSettings{
		ID:                 clusterID,
		APIServer:          "https://kubernetes.example",
		ClusterServer:      "https://kubernetes.example",
		ClusterCAData:      []byte("test-ca"),
		KubernetesAudience: "kubernetes.default.svc",
		UserNamespace:      userv1.InternalUserSystemNamespace,
		BrokerNamespace:    userv1.CredentialBrokerNamespace,
		ClusterName:        clusterID,
		Enabled:            true,
	}, leaseClient, tokenIssuer)
	if err != nil {
		t.Fatal(err)
	}
	server, err := broker.NewMultiTargetServer(map[string]broker.TargetBackend{clusterID: backend}, authenticator, broker.Config{
		AllowedIssuers: map[string]struct{}{target.Issuer: {}},
		AdminGroup:     "sealos.internal.admin",
		SourceIP: broker.SourceIPPolicy{
			TrustedProxyCIDRs:     trustedProxyCIDRs,
			AllowedClientCIDRs:    allowedClientCIDRs,
			TrustedClientIPHeader: "X-Trusted-Client-IP",
		},
		PrepareTimeout:      2 * time.Second,
		ApprovalWaitTimeout: time.Hour,
		PollInterval:        5 * time.Millisecond,
		Clock:               func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	audit := &e2eAuditSink{}
	server.Audit = audit
	tlsServer := httptest.NewTLSServer(server.Handler())
	defer tlsServer.Close()
	tlsServer.TLS.MinVersion = tls.VersionTLS12
	httpClient := tlsServer.Client()
	httpClient.Timeout = 5 * time.Second
	approvalHTTPClient := *httpClient
	approvalHTTPClient.Transport = &e2eBearerTransport{base: httpClient.Transport, token: "target-token"}

	source := NewMockSource(externalURL)
	approver, err := approvaladapter.NewBrokerHTTPClient(tlsServer.URL, clusterID, &approvalHTTPClient)
	if err != nil {
		t.Fatal(err)
	}
	notifier := &e2eNotifierCapture{}
	if err := processApproval(context.Background(), source, approver, notifier, approvalCode); err != nil {
		t.Fatal(err)
	}

	leases := &userv1.CredentialLeaseList{}
	if err := leaseClient.List(context.Background(), leases); err != nil {
		t.Fatal(err)
	}
	if len(leases.Items) != 1 {
		t.Fatalf("lease count = %d, want 1", len(leases.Items))
	}
	leaseName := leases.Items[0].Name
	lease := &userv1.CredentialLease{}
	leaseKey := types.NamespacedName{Namespace: userv1.CredentialBrokerNamespace, Name: leaseName}
	if err := leaseClient.Get(context.Background(), leaseKey, lease); err != nil {
		t.Fatal(err)
	}
	if lease.Status.Phase != userv1.CredentialLeaseAwaitingRedemption || lease.Spec.Target != target || lease.Spec.Requester != target || lease.Spec.RequestedTTLSeconds != 1800 || lease.Spec.ApprovalReason != "e2e elevated access" {
		t.Fatalf("approval lease = %#v", lease)
	}
	reference := lease.Spec.ApprovalReference
	if reference == "" {
		t.Fatal("approval reference is empty")
	}
	retryResponse, err := approver.Approve(context.Background(), broker.ApprovalRecord{
		ClusterID:           clusterID,
		ApprovalID:          approvalCode,
		Target:              target,
		RequestedTTLSeconds: 1800,
		ApprovalReason:      "e2e elevated access",
	})
	if err != nil {
		t.Fatal(err)
	}
	if retryResponse.ApprovalReference != reference || retryResponse.ApprovalID != approvalCode {
		t.Fatalf("idempotent approval response = %#v, want reference %q", retryResponse, reference)
	}
	encodedResponse, err := json.Marshal(retryResponse)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"token", "kubeconfig", "secretData", "privateKey"} {
		if strings.Contains(strings.ToLower(string(encodedResponse)), strings.ToLower(forbidden)) {
			t.Fatalf("approval response contains forbidden field %q: %s", forbidden, encodedResponse)
		}
	}
	notifications := notifier.Notifications()
	if len(notifications) != 1 || notifications[0].ApprovalReference != reference || notifications[0].Recipient != target.Subject+"@example.internal" {
		t.Fatalf("notifications = %#v, want reference %q and target recipient", notifications, reference)
	}

	approvalAudit := findAuditEvent(audit.Events(), "approve_elevated")
	if approvalAudit == nil || approvalAudit.StatusCode != http.StatusCreated || approvalAudit.Result != "success" {
		t.Fatalf("approval audit = %#v, want HTTP 201 success", approvalAudit)
	}

	reconciler := &controllers.CredentialLeaseReconciler{
		Client:           leaseClient,
		UserNamespace:    userv1.InternalUserSystemNamespace,
		BrokerNamespace:  userv1.CredentialBrokerNamespace,
		ElevatedRoleName: internalcredentials.ClusterOpsWriteRoleName,
		Now:              func() time.Time { return now },
	}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: leaseKey}); err != nil {
		t.Fatal(err)
	}

	redeemResult := make(chan e2eHTTPResult, 1)
	go func() {
		request, err := http.NewRequest(http.MethodPost, tlsServer.URL+"/v1/clusters/"+clusterID+"/credentials/elevated/redeem", strings.NewReader(fmt.Sprintf(`{"approvalReference":%q}`, reference)))
		if err != nil {
			redeemResult <- e2eHTTPResult{err: err}
			return
		}
		request.Header.Set("Authorization", "Bearer target-token")
		request.Header.Set("X-Trusted-Client-IP", "10.1.2.3")
		response, err := httpClient.Do(request)
		redeemResult <- e2eHTTPResult{response: response, err: err}
	}()
	pendingDeadline := time.After(2 * time.Second)
	pendingTicker := time.NewTicker(5 * time.Millisecond)
	defer pendingTicker.Stop()
	for {
		current := &userv1.CredentialLease{}
		if err := leaseClient.Get(context.Background(), leaseKey, current); err != nil {
			t.Fatal(err)
		}
		if current.Status.Phase == userv1.CredentialLeasePending {
			if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: leaseKey}); err != nil {
				t.Fatal(err)
			}
			break
		}
		select {
		case <-pendingDeadline:
			t.Fatalf("redeem did not move lease to Pending; phase = %q", current.Status.Phase)
		case <-pendingTicker.C:
		}
	}
	select {
	case <-tokenIssuer.Started():
	case <-time.After(2 * time.Second):
		t.Fatal("redeem did not reach TokenIssuer")
	}
	prepared := &userv1.CredentialLease{}
	if err := leaseClient.Get(context.Background(), leaseKey, prepared); err != nil {
		t.Fatal(err)
	}
	if prepared.Status.Phase != userv1.CredentialLeasePrepared || prepared.Status.ConsumedAt == nil {
		t.Fatalf("lease while TokenIssuer is blocked = %#v, want Prepared with consumed marker", prepared.Status)
	}
	assertPreparedE2EResources(t, leaseClient, prepared, targetUser.Name)
	notificationJSON, err := json.Marshal(notifications[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"e2e-token-value", "kubeconfig", "e2e-secret-uid"} {
		if strings.Contains(string(notificationJSON), secret) {
			t.Fatalf("notification contains sensitive value %q: %s", secret, notificationJSON)
		}
	}
	tokenIssuer.Release()

	firstRedeem := <-redeemResult
	if firstRedeem.err != nil {
		t.Fatal(firstRedeem.err)
	}
	firstBody := readHTTPBody(t, firstRedeem.response)
	if firstRedeem.response.StatusCode != http.StatusOK || !strings.Contains(firstBody, "e2e-token-value") {
		t.Fatalf("first redeem = %d %s", firstRedeem.response.StatusCode, firstBody)
	}
	if tokenIssuer.Calls() != 1 {
		t.Fatalf("TokenIssuer calls after first redeem = %d, want 1", tokenIssuer.Calls())
	}
	if err := leaseClient.Get(context.Background(), leaseKey, lease); err != nil {
		t.Fatal(err)
	}
	if lease.Status.Phase != userv1.CredentialLeaseIssued || lease.Status.ExpirationTimestamp == nil {
		t.Fatalf("issued lease = %#v", lease.Status)
	}

	replay := redeem(t, httpClient, tlsServer.URL, clusterID, "target-token", reference)
	if replay.status != http.StatusConflict || tokenIssuer.Calls() != 1 {
		t.Fatalf("replay = %d %s, TokenIssuer calls = %d", replay.status, replay.body, tokenIssuer.Calls())
	}
	differentSubject := redeem(t, httpClient, tlsServer.URL, clusterID, "other-token", reference)
	if differentSubject.status != http.StatusForbidden || tokenIssuer.Calls() != 1 {
		t.Fatalf("different subject = %d %s, TokenIssuer calls = %d", differentSubject.status, differentSubject.body, tokenIssuer.Calls())
	}
	redeemAudit := findAuditEvent(audit.Events(), "redeem_elevated")
	if redeemAudit == nil || redeemAudit.StatusCode != http.StatusOK || redeemAudit.Result != "success" {
		t.Fatalf("redeem audit = %#v", redeemAudit)
	}
}

type e2eHTTPResult struct {
	response *http.Response
	err      error
}

type e2eRedeemResult struct {
	status int
	body   string
}

func redeem(t *testing.T, httpClient *http.Client, serverURL, clusterID, token, reference string) e2eRedeemResult {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, serverURL+"/v1/clusters/"+clusterID+"/credentials/elevated/redeem", strings.NewReader(fmt.Sprintf(`{"approvalReference":%q}`, reference)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-Trusted-Client-IP", "10.1.2.3")
	response, err := httpClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body := readHTTPBody(t, response)
	return e2eRedeemResult{status: response.StatusCode, body: body}
}

func readHTTPBody(t *testing.T, response *http.Response) string {
	t.Helper()
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func assertPreparedE2EResources(t *testing.T, leaseClient client.Client, lease *userv1.CredentialLease, targetName string) {
	t.Helper()
	wantLabels := map[string]string{
		userv1.ManagedByLabel:          "credential-lease-controller",
		userv1.CredentialLeaseLabel:    lease.Name,
		userv1.CredentialLeaseUIDLabel: string(lease.UID),
		userv1.InternalUserLabel:       targetName,
	}
	serviceAccount := &corev1.ServiceAccount{}
	serviceAccountKey := types.NamespacedName{Name: internalcredentials.ResourceName("internal-lease", lease.Namespace, lease.Name), Namespace: userv1.InternalUserSystemNamespace}
	if err := leaseClient.Get(context.Background(), serviceAccountKey, serviceAccount); err != nil {
		t.Fatal(err)
	}
	if serviceAccount.AutomountServiceAccountToken == nil || *serviceAccount.AutomountServiceAccountToken || !sameLabels(serviceAccount.Labels, wantLabels) {
		t.Fatalf("temporary ServiceAccount = %#v", serviceAccount)
	}

	binding := &rbacv1.ClusterRoleBinding{}
	if err := leaseClient.Get(context.Background(), types.NamespacedName{Name: internalcredentials.ResourceName("internal-lease-binding", lease.Namespace, lease.Name)}, binding); err != nil {
		t.Fatal(err)
	}
	if binding.RoleRef != (rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: internalcredentials.ClusterOpsWriteRoleName}) || len(binding.Subjects) != 1 || binding.Subjects[0].Name != serviceAccount.Name || binding.Subjects[0].Namespace != userv1.InternalUserSystemNamespace || !sameLabels(binding.Labels, wantLabels) {
		t.Fatalf("temporary ClusterRoleBinding = %#v", binding)
	}

	secret := &corev1.Secret{}
	if err := leaseClient.Get(context.Background(), types.NamespacedName{Name: internalcredentials.ResourceName("internal-lease-secret", lease.Namespace, lease.Name), Namespace: userv1.InternalUserSystemNamespace}, secret); err != nil {
		t.Fatal(err)
	}
	if secret.UID == "" || len(secret.Data) != 0 || len(secret.StringData) != 0 || secret.Type != corev1.SecretTypeOpaque || !sameLabels(secret.Labels, wantLabels) {
		t.Fatalf("bound Secret = %#v", secret)
	}
	if lease.Status.ServiceAccountRef == nil || lease.Status.ServiceAccountRef.Name != serviceAccount.Name || lease.Status.BoundSecretRef == nil || lease.Status.BoundSecretRef.Name != secret.Name || lease.Status.BoundSecretRef.UID != secret.UID || lease.Status.BindingRef == nil || lease.Status.BindingRef.Name != binding.Name {
		t.Fatalf("prepared references = %#v", lease.Status)
	}
}

func sameLabels(actual, expected map[string]string) bool {
	for key, value := range expected {
		if actual[key] != value {
			return false
		}
	}
	return true
}

type e2eAuthenticator struct {
	target broker.Principal
	other  broker.Principal
}

func (a e2eAuthenticator) Authenticate(_ context.Context, bearerToken string) (broker.Principal, error) {
	switch bearerToken {
	case "target-token":
		return a.target, nil
	case "other-token":
		return a.other, nil
	default:
		return broker.Principal{}, errors.New("unknown test bearer token")
	}
}

type blockingTokenIssuer struct {
	mu        sync.Mutex
	result    broker.TokenResult
	calls     int
	namespace string
	account   string
	spec      authenticationv1.TokenRequestSpec
	started   chan struct{}
	startOnce sync.Once
	release   chan struct{}
}

func newBlockingTokenIssuer(result broker.TokenResult) *blockingTokenIssuer {
	return &blockingTokenIssuer{result: result, started: make(chan struct{}), release: make(chan struct{})}
}

func (i *blockingTokenIssuer) Issue(ctx context.Context, namespace, serviceAccount string, spec authenticationv1.TokenRequestSpec) (broker.TokenResult, error) {
	i.mu.Lock()
	i.calls++
	i.namespace = namespace
	i.account = serviceAccount
	i.spec = spec
	i.startOnce.Do(func() { close(i.started) })
	i.mu.Unlock()
	select {
	case <-i.release:
		return i.result, nil
	case <-ctx.Done():
		return broker.TokenResult{}, ctx.Err()
	}
}

func (i *blockingTokenIssuer) Started() <-chan struct{} { return i.started }

func (i *blockingTokenIssuer) Release() {
	i.mu.Lock()
	select {
	case <-i.release:
	default:
		close(i.release)
	}
	i.mu.Unlock()
}

func (i *blockingTokenIssuer) Calls() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.calls
}

type e2eAuditSink struct {
	mu     sync.Mutex
	events []broker.AuditEvent
}

func (s *e2eAuditSink) Record(_ context.Context, event broker.AuditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
	return nil
}

func (s *e2eAuditSink) Events() []broker.AuditEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]broker.AuditEvent(nil), s.events...)
}

func findAuditEvent(events []broker.AuditEvent, operation string) *broker.AuditEvent {
	for i := range events {
		if events[i].Operation == operation {
			event := events[i]
			return &event
		}
	}
	return nil
}

type e2eNotifierCapture struct {
	mu            sync.Mutex
	notifications []approvaladapter.Notification
}

func (m *e2eNotifierCapture) Notify(_ context.Context, notification approvaladapter.Notification) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.notifications = append(m.notifications, notification)
	return nil
}

func (m *e2eNotifierCapture) Notifications() []approvaladapter.Notification {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]approvaladapter.Notification(nil), m.notifications...)
}

type e2eBearerTransport struct {
	base  http.RoundTripper
	token string
}

func (t *e2eBearerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	request = request.Clone(request.Context())
	request.Header.Set("Authorization", "Bearer "+t.token)
	request.Header.Set("X-Trusted-Client-IP", "10.1.2.3")
	return t.base.RoundTrip(request)
}
