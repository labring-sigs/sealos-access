//go:build acceptance

package usercmd

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	userv1 "github.com/labring-sigs/sealos-access/api/v1"
	"github.com/labring-sigs/sealos-access/controllers"
	"github.com/labring-sigs/sealos-access/pkg/approvaladapter"
	approvalacceptance "github.com/labring-sigs/sealos-access/pkg/approvaladapter/acceptance"
	"github.com/labring-sigs/sealos-access/pkg/approvaladapter/mock"
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

// TestElevatedRedeemPreOIDC validates the local credential elevation reference-redeem
// boundary. It uses a mock ApprovalSource, a real BrokerHTTPClient and
// Broker handler, and a protected in-memory notification sink. OIDC and a
// real external approval system are intentionally outside this stage.
func TestElevatedRedeemPreOIDC(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	now := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	target := userv1.Identity{Issuer: "https://mock-issuer.example", Subject: "alice"}

	external := mock.NewMockServer()
	externalURL := external.Start()
	defer external.Close()
	instanceCode := external.CreateApproval(mock.CreateApprovalRequest{
		ApprovalCode: "credential-elevation-approval", ClusterID: "cluster-a", InitiatorID: "operator", InitiatorEmail: "operator@example.internal",
		TargetSubject: target.Subject, RequestedTTL: 1800, Reason: "credential elevation reference-redeem acceptance",
	})
	if err := external.ApproveApproval(instanceCode); err != nil {
		t.Fatal(err)
	}

	leaseClient, err := acceptanceLeaseClient(t, target)
	if err != nil {
		t.Fatal(err)
	}
	tokenIssuer := &acceptanceTokenIssuer{result: broker.TokenResult{
		Token: "credential-elevation-token-value", Expiration: now.Add(30 * time.Minute), Audiences: []string{"kubernetes.default.svc"},
	}}
	authenticator := acceptanceAuthenticator{principal: broker.Principal{Issuer: target.Issuer, Subject: target.Subject}}
	trustedProxyCIDRs, err := broker.ParseCIDRs([]string{"127.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	allowedClientCIDRs, err := broker.ParseCIDRs([]string{"10.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	backend, err := broker.NewStaticTargetBackend(broker.TargetSettings{
		ID:                 "cluster-a",
		APIServer:          "https://kubernetes.example",
		ClusterServer:      "https://kubernetes.example",
		ClusterCAData:      []byte("test-ca"),
		KubernetesAudience: "kubernetes.default.svc",
		UserNamespace:      userv1.InternalUserSystemNamespace,
		BrokerNamespace:    userv1.CredentialBrokerNamespace,
		ClusterName:        "cluster-a",
		Enabled:            true,
	}, leaseClient, tokenIssuer)
	if err != nil {
		t.Fatal(err)
	}
	server, err := broker.NewMultiTargetServer(map[string]broker.TargetBackend{"cluster-a": backend}, authenticator, broker.Config{
		AllowedIssuers: map[string]struct{}{target.Issuer: {}}, AdminGroup: "sealos.internal.admin",
		SourceIP:       broker.SourceIPPolicy{TrustedProxyCIDRs: trustedProxyCIDRs, AllowedClientCIDRs: allowedClientCIDRs, TrustedClientIPHeader: "X-Trusted-Client-IP"},
		PrepareTimeout: 2 * time.Second, ApprovalWaitTimeout: time.Hour,
		PollInterval: 2 * time.Millisecond, Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	tlsServer := httptest.NewTLSServer(server.Handler())
	defer tlsServer.Close()
	tlsServer.TLS.MinVersion = tls.VersionTLS12
	brokerClient := tlsServer.Client()
	brokerClient.Timeout = 5 * time.Second
	approvalClient := *brokerClient
	approvalClient.Transport = &acceptanceBearerRoundTripper{base: brokerClient.Transport, token: "alice-access-token"}

	approver, err := approvaladapter.NewBrokerHTTPClient(tlsServer.URL, "cluster-a", &approvalClient)
	if err != nil {
		t.Fatal(err)
	}
	accessToken := strings.Repeat("acceptance-access-", 4)
	notifier, err := approvalacceptance.NewNotificationSink(accessToken)
	if err != nil {
		t.Fatal(err)
	}
	pipeline, err := approvaladapter.NewPipeline(mock.NewMockSource(externalURL), approver, notifier)
	if err != nil {
		t.Fatal(err)
	}
	if err := pipeline.Process(ctx, instanceCode); err != nil {
		t.Fatal(err)
	}
	notification, err := notifier.WaitForNotification(ctx, accessToken)
	if err != nil {
		t.Fatal(err)
	}
	if notification.ApprovalReference == "" || notification.Recipient != "alice@example.internal" {
		t.Fatalf("notification = %#v", notification)
	}
	reference := notification.ApprovalReference

	reconciler := &controllers.CredentialLeaseReconciler{
		Client: leaseClient, UserNamespace: userv1.InternalUserSystemNamespace,
		BrokerNamespace: userv1.CredentialBrokerNamespace, ElevatedRoleName: internalcredentials.ClusterOpsWriteRoleName,
		Now: func() time.Time { return now },
	}
	go reconcileAcceptanceLeases(ctx, leaseClient, reconciler)

	recordingClient := *brokerClient
	recordingClient.Transport = &recordingRoundTripper{base: brokerClient.Transport}
	outputPath := filepath.Join(t.TempDir(), "elevated.kubeconfig")
	var stdout, stderr bytes.Buffer
	response, err := redeemElevatedCredentialForCluster(ctx, &recordingClient, tlsServer.URL, "cluster-a", "alice-access-token", reference)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeCredentialResponse(cliConfig{OIDCIssuer: target.Issuer, ClusterID: "cluster-a", Output: outputPath, Stdout: &stdout, Stderr: &stderr}, response); err != nil {
		t.Fatal(err)
	}

	requestBody := recordingClient.Transport.(*recordingRoundTripper).BodyForPath("/v1/clusters/cluster-a/credentials/elevated/redeem")
	var requestFields map[string]any
	if err := json.Unmarshal(requestBody, &requestFields); err != nil {
		t.Fatal(err)
	}
	if len(requestFields) != 1 || requestFields["approvalReference"] != reference {
		t.Fatalf("CLI redeem request = %#v, want reference-only body", requestFields)
	}
	info, err := os.Stat(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("kubeconfig mode = %o, want 600", info.Mode().Perm())
	}
	kubeconfig, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(kubeconfig), "credential-elevation-token-value") {
		t.Fatal("restricted kubeconfig did not contain the issued credential")
	}
	if strings.Contains(stdout.String(), "credential-elevation-token-value") || strings.Contains(stderr.String(), "credential-elevation-token-value") ||
		strings.Contains(stdout.String(), "apiVersion:") || strings.Contains(stderr.String(), "apiVersion:") {
		t.Fatalf("credential material appeared in process output: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	notificationJSON, err := json.Marshal(notification)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"token", "kubeconfig", "secretData", "privateKey", "credential-elevation-token-value"} {
		if strings.Contains(strings.ToLower(string(notificationJSON)), strings.ToLower(forbidden)) {
			t.Fatalf("notification contains forbidden value %q", forbidden)
		}
	}
	if tokenIssuer.Calls() != 1 {
		t.Fatalf("TokenRequest calls = %d, want 1", tokenIssuer.Calls())
	}
}

func acceptanceLeaseClient(t *testing.T, target userv1.Identity) (client.Client, error) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := rbacv1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := userv1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	user := &userv1.InternalUser{
		ObjectMeta: metav1.ObjectMeta{Name: internalcredentials.DeriveInternalUserName(target.Issuer, target.Subject), UID: "credential-elevation-user-uid"},
		Spec:       userv1.InternalUserSpec{Identity: target, RoleProfile: userv1.BaseReadonlyProfile},
		Status:     userv1.InternalUserStatus{Phase: userv1.InternalUserActive},
	}
	return fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&userv1.CredentialLease{}, &userv1.InternalUser{}).
		WithObjects(user, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: internalcredentials.ClusterOpsWriteRoleName}}).
		WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
			switch object := object.(type) {
			case *userv1.CredentialLease:
				if object.UID == "" {
					object.UID = types.UID("credential-elevation-lease-uid")
				}
			case *corev1.Secret:
				if object.UID == "" {
					object.UID = types.UID("credential-elevation-secret-uid")
				}
			}
			return c.Create(ctx, object, opts...)
		}}).Build(), nil
}

func reconcileAcceptanceLeases(ctx context.Context, leaseClient client.Client, reconciler *controllers.CredentialLeaseReconciler) {
	ticker := time.NewTicker(2 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			leases := &userv1.CredentialLeaseList{}
			if err := leaseClient.List(ctx, leases, client.InNamespace(userv1.CredentialBrokerNamespace)); err != nil {
				continue
			}
			for i := range leases.Items {
				_, _ = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: leases.Items[i].Namespace, Name: leases.Items[i].Name}})
			}
		}
	}
}

type acceptanceAuthenticator struct{ principal broker.Principal }

func (a acceptanceAuthenticator) Authenticate(_ context.Context, token string) (broker.Principal, error) {
	if token != "alice-access-token" {
		return broker.Principal{}, errors.New("invalid acceptance token")
	}
	return a.principal, nil
}

type acceptanceTokenIssuer struct {
	mu     sync.Mutex
	result broker.TokenResult
	calls  int
}

func (i *acceptanceTokenIssuer) Issue(_ context.Context, _ string, _ string, _ authenticationv1.TokenRequestSpec) (broker.TokenResult, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.calls++
	return i.result, nil
}

func (i *acceptanceTokenIssuer) Calls() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.calls
}

type recordingRoundTripper struct {
	mu     sync.Mutex
	base   http.RoundTripper
	bodies map[string][]byte
}

func (r *recordingRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	request.Body = io.NopCloser(bytes.NewReader(body))
	request.Header.Set("X-Trusted-Client-IP", "10.1.2.3")
	r.mu.Lock()
	if r.bodies == nil {
		r.bodies = make(map[string][]byte)
	}
	r.bodies[request.URL.Path] = append([]byte(nil), body...)
	r.mu.Unlock()
	return r.base.RoundTrip(request)
}

func (r *recordingRoundTripper) BodyForPath(path string) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]byte(nil), r.bodies[path]...)
}

type acceptanceBearerRoundTripper struct {
	base  http.RoundTripper
	token string
}

func (r *acceptanceBearerRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	request = request.Clone(request.Context())
	request.Header.Set("Authorization", "Bearer "+r.token)
	return r.base.RoundTrip(request)
}
