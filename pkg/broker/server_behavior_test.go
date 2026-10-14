package broker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	userv1 "github.com/labring-sigs/sealos-access/api/v1"
	"github.com/labring-sigs/sealos-access/pkg/internalcredentials"
	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func p1Config(t *testing.T) Config {
	t.Helper()
	trusted, err := ParseCIDRs([]string{"127.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	allowed, err := ParseCIDRs([]string{"10.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	return Config{
		AllowedIssuers: map[string]struct{}{"https://issuer.example": {}},
		AdminGroup:     "sealos.internal.admin",
		SourceIP:       SourceIPPolicy{TrustedProxyCIDRs: trusted, AllowedClientCIDRs: allowed, TrustedClientIPHeader: "X-Trusted-Client-IP"},
	}
}

func p1Context(ctx context.Context) context.Context {
	return withTargetID(ctx, testTargetID)
}

func p1Backend(t *testing.T, leaseClient client.Client, tokenIssuer TokenIssuer) TargetBackend {
	t.Helper()
	backend, err := NewStaticTargetBackend(TargetSettings{
		ID:                 testTargetID,
		APIServer:          "https://kubernetes.example",
		ClusterServer:      "https://kubernetes.example",
		ClusterCAData:      []byte("test-ca"),
		KubernetesAudience: "kubernetes.default.svc",
		UserNamespace:      userv1.InternalUserSystemNamespace,
		BrokerNamespace:    userv1.CredentialBrokerNamespace,
		ClusterName:        testTargetID,
		Enabled:            true,
	}, leaseClient, tokenIssuer)
	if err != nil {
		t.Fatal(err)
	}
	return backend
}

func TestNewMultiTargetServerNormalizesDefaultsAndRejectsInvalidConfiguration(t *testing.T) {
	client := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	authenticator := staticAuthenticator{principal: Principal{Issuer: "https://issuer.example", Subject: "alice"}}
	issuer := &recordingTokenIssuer{}
	validTargets := func() map[string]TargetBackend {
		return map[string]TargetBackend{testTargetID: p1Backend(t, client, issuer)}
	}

	for _, test := range []struct {
		name string
		make func() (*Server, error)
	}{
		{name: "missing target backend", make: func() (*Server, error) { return NewMultiTargetServer(nil, authenticator, p1Config(t)) }},
		{name: "missing authenticator", make: func() (*Server, error) { return NewMultiTargetServer(validTargets(), nil, p1Config(t)) }},
		{name: "nil target backend", make: func() (*Server, error) {
			return NewMultiTargetServer(map[string]TargetBackend{testTargetID: nil}, authenticator, p1Config(t))
		}},
		{name: "target key mismatch", make: func() (*Server, error) {
			return NewMultiTargetServer(map[string]TargetBackend{"other": p1Backend(t, client, issuer)}, authenticator, p1Config(t))
		}},
		{name: "disabled target", make: func() (*Server, error) {
			backend := p1Backend(t, client, issuer).(*StaticTargetBackend)
			backend.TargetSettings.Enabled = false
			return NewMultiTargetServer(validTargetMap(backend), authenticator, p1Config(t))
		}},
		{name: "incomplete target backend", make: func() (*Server, error) {
			settings := p1Backend(t, client, issuer).Settings()
			return NewMultiTargetServer(map[string]TargetBackend{testTargetID: &StaticTargetBackend{TargetSettings: settings, Issuer: issuer}}, authenticator, p1Config(t))
		}},
		{name: "missing admin group", make: func() (*Server, error) {
			cfg := p1Config(t)
			cfg.AdminGroup = ""
			return NewMultiTargetServer(validTargets(), authenticator, cfg)
		}},
		{name: "missing issuer allowlist", make: func() (*Server, error) {
			cfg := p1Config(t)
			cfg.AllowedIssuers = nil
			return NewMultiTargetServer(validTargets(), authenticator, cfg)
		}},
		{name: "invalid source policy", make: func() (*Server, error) {
			cfg := p1Config(t)
			cfg.SourceIP.TrustedClientIPHeader = " X-Client-IP"
			return NewMultiTargetServer(validTargets(), authenticator, cfg)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if server, err := test.make(); err == nil || server != nil {
				t.Fatalf("NewMultiTargetServer() = server %v, error %v; want configuration error", server, err)
			}
		})
	}

	server, err := NewMultiTargetServer(validTargets(), authenticator, p1Config(t))
	if err != nil {
		t.Fatal(err)
	}
	if server.Config.PrepareTimeout != 30*time.Second ||
		server.Config.ApprovalWaitTimeout != 24*time.Hour || server.Config.PollInterval != 250*time.Millisecond ||
		server.Config.MaxBodyBytes != 16*1024 || server.Config.RequestRateLimit != 10 || server.Config.RequestRateBurst != 20 || server.Config.ClockSkew != 30*time.Second ||
		server.Config.Clock == nil || server.requestLimiter == nil {
		t.Fatalf("normalized config = %#v", server.Config)
	}
}

func TestServerRejectsUnscopedCredentialRoutes(t *testing.T) {
	client := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	server, err := NewMultiTargetServer(
		map[string]TargetBackend{testTargetID: p1Backend(t, client, &recordingTokenIssuer{})},
		staticAuthenticator{principal: Principal{Issuer: "https://issuer.example", Subject: "alice"}},
		p1Config(t),
	)
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/credentials/base", bytes.NewBufferString(`{"requestedTTLSeconds":1800}`))
	request.RemoteAddr = "127.0.0.1:443"
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("unscoped route status = %d, want %d", response.Code, http.StatusNotFound)
	}
}

func validTargetMap(backend *StaticTargetBackend) map[string]TargetBackend {
	return map[string]TargetBackend{testTargetID: backend}
}

func TestNewMultiTargetServerRejectsNonFiniteAndNegativeRateLimits(t *testing.T) {
	client := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	authenticator := staticAuthenticator{principal: Principal{Issuer: "https://issuer.example", Subject: "alice"}}
	issuer := &recordingTokenIssuer{}
	for _, limit := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -0.1} {
		t.Run(strings.ReplaceAll(strings.TrimSpace(strconvFloat(limit)), ".", "_"), func(t *testing.T) {
			cfg := p1Config(t)
			cfg.RequestRateLimit = rateLimitForTest(limit)
			if server, err := NewMultiTargetServer(map[string]TargetBackend{testTargetID: p1Backend(t, client, issuer)}, authenticator, cfg); err == nil || server != nil {
				t.Fatalf("NewMultiTargetServer() = server %v, error %v; want rate-limit error", server, err)
			}
		})
	}
	for _, burst := range []int{-1} {
		t.Run("negative burst", func(t *testing.T) {
			cfg := p1Config(t)
			cfg.RequestRateBurst = burst
			if server, err := NewMultiTargetServer(map[string]TargetBackend{testTargetID: p1Backend(t, client, issuer)}, authenticator, cfg); err == nil || server != nil {
				t.Fatalf("NewMultiTargetServer() = server %v, error %v; want burst error", server, err)
			}
		})
	}
}

// These small helpers keep the table above readable without pulling float
// formatting or rate.Limit details into the assertions.
func rateLimitForTest(value float64) rate.Limit { return rate.Limit(value) }
func strconvFloat(value float64) string {
	switch {
	case math.IsNaN(value):
		return "NaN"
	case math.IsInf(value, 1):
		return "+Inf"
	case math.IsInf(value, -1):
		return "-Inf"
	default:
		return fmt.Sprintf("%g", value)
	}
}

func TestHandlerBypassesHealthzAndAuditsRejectedRequests(t *testing.T) {
	now := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	authenticator := &p1Authenticator{principal: Principal{Issuer: "https://issuer.example", Subject: "alice"}}
	server := newTestServer(t, fake.NewClientBuilder().WithScheme(testScheme()).Build(), authenticator, &recordingTokenIssuer{}, now)
	audit := &recordingAuditSink{}
	server.Audit = audit

	health := httptest.NewRecorder()
	server.Handler().ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK || len(audit.events) != 0 || atomic.LoadInt32(&authenticator.calls) != 0 {
		t.Fatalf("healthz = %d, audit events = %d, auth calls = %d", health.Code, len(audit.events), authenticator.calls)
	}

	rejected := p1HTTP(server, http.MethodGet, "/v1/credentials/missing", "")
	if rejected.Code != http.StatusNotFound || len(audit.events) != 1 {
		t.Fatalf("rejected request = %d, audit events = %d", rejected.Code, len(audit.events))
	}
	event := audit.events[0]
	if event.Operation != "get_lease" || event.Result != "rejected" || event.StatusCode != http.StatusNotFound || event.SourceIP != "10.1.2.3" || event.Requester == nil || event.Requester.Subject != "alice" || event.LeaseID != "missing" {
		t.Fatalf("audit event = %#v", event)
	}
}

func TestAuthenticateHTTPValidatesBearerPrincipalIssuerAndSourceIP(t *testing.T) {
	tests := []struct {
		name       string
		authorize  func(*http.Request)
		principal  Principal
		authErr    error
		wantOK     bool
		wantStatus int
	}{
		{name: "case insensitive bearer", authorize: func(r *http.Request) { r.Header.Set("Authorization", "bEaReR token") }, principal: Principal{Issuer: "https://issuer.example", Subject: "alice"}, wantOK: true},
		{name: "duplicate authorization", authorize: func(r *http.Request) { r.Header.Add("Authorization", "Bearer second") }, principal: Principal{Issuer: "https://issuer.example", Subject: "alice"}, wantStatus: http.StatusUnauthorized},
		{name: "missing bearer", authorize: func(r *http.Request) { r.Header.Del("Authorization") }, principal: Principal{Issuer: "https://issuer.example", Subject: "alice"}, wantStatus: http.StatusUnauthorized},
		{name: "malformed bearer", authorize: func(r *http.Request) { r.Header.Set("Authorization", "Basic token") }, principal: Principal{Issuer: "https://issuer.example", Subject: "alice"}, wantStatus: http.StatusUnauthorized},
		{name: "invalid principal", authorize: func(r *http.Request) { r.Header.Set("Authorization", "Bearer token") }, principal: Principal{Issuer: "http://issuer.example", Subject: "alice"}, wantStatus: http.StatusUnauthorized},
		{name: "disallowed issuer", authorize: func(r *http.Request) { r.Header.Set("Authorization", "Bearer token") }, principal: Principal{Issuer: "https://other.example", Subject: "alice"}, wantStatus: http.StatusUnauthorized},
		{name: "authenticator error", authorize: func(r *http.Request) { r.Header.Set("Authorization", "Bearer token") }, principal: Principal{Issuer: "https://issuer.example", Subject: "alice"}, authErr: errors.New("authentication failed"), wantStatus: http.StatusUnauthorized},
		{name: "untrusted source", authorize: func(r *http.Request) { r.Header.Set("Authorization", "Bearer token"); r.RemoteAddr = "192.0.2.1:443" }, principal: Principal{Issuer: "https://issuer.example", Subject: "alice"}, wantStatus: http.StatusForbidden},
	} // table entries are closed below
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			authenticator := &p1Authenticator{principal: test.principal, err: test.authErr}
			server := newTestServer(t, fake.NewClientBuilder().WithScheme(testScheme()).Build(), authenticator, &recordingTokenIssuer{}, time.Now())
			request := httptest.NewRequest(http.MethodGet, "/v1/credentials/missing", nil)
			request.RemoteAddr = "127.0.0.1:443"
			request.Header.Set("X-Trusted-Client-IP", "10.1.2.3")
			request.Header.Set("Authorization", "Bearer token")
			if test.name == "duplicate authorization" {
				test.authorize = func(r *http.Request) { r.Header.Add("Authorization", "Bearer second") }
			}
			if test.authorize != nil {
				test.authorize(request)
			}
			response := httptest.NewRecorder()
			principal, ok := server.authenticateHTTP(response, request)
			if ok != test.wantOK {
				t.Fatalf("authenticateHTTP ok = %v, want %v; principal = %#v", ok, test.wantOK, principal)
			}
			if test.wantOK {
				if response.Code != http.StatusOK || principal.Issuer != test.principal.Issuer || principal.Subject != test.principal.Subject {
					t.Fatalf("success response = %d principal = %#v", response.Code, principal)
				}
			} else if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
		})
	}
}

func TestHandleLeaseMethodsAuthorizationErrorsAndIdempotentRevoke(t *testing.T) {
	now := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	identity := userv1.Identity{Issuer: "https://issuer.example", Subject: "alice"}
	lease := &userv1.CredentialLease{ObjectMeta: metav1.ObjectMeta{Name: "lease-p1", Namespace: userv1.CredentialBrokerNamespace}, Spec: userv1.CredentialLeaseSpec{Target: identity, Requester: identity, Profile: userv1.BaseReadonlyProfile}, Status: userv1.CredentialLeaseStatus{Phase: userv1.CredentialLeaseIssued}}
	leaseClient := fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(&userv1.CredentialLease{}).WithObjects(lease).Build()
	server := newTestServer(t, leaseClient, staticAuthenticator{principal: Principal{Issuer: identity.Issuer, Subject: identity.Subject}}, &recordingTokenIssuer{}, now)

	for _, path := range []string{"/v1/credentials/", "/v1/credentials/lease-p1/extra", "/v1/credentials/lease-p1/revoke/extra"} {
		if response := p1HTTP(server, http.MethodGet, path, ""); response.Code != http.StatusNotFound {
			t.Fatalf("malformed path %q status = %d, want %d", path, response.Code, http.StatusNotFound)
		}
	}
	audit := &recordingAuditSink{}
	server.Audit = audit
	if response := p1HTTP(server, http.MethodPost, "/v1/credentials/lease-p1", ""); response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET lease with POST status = %d", response.Code)
	}
	if response := p1HTTP(server, http.MethodGet, "/v1/credentials/lease-p1", ""); response.Code != http.StatusOK {
		t.Fatalf("GET lease status = %d", response.Code)
	}
	if len(audit.events) != 2 || audit.events[1].Operation != "get_lease" || audit.events[1].Result != "success" ||
		audit.events[1].LeaseID != lease.Name || audit.events[1].Target == nil || audit.events[1].Target.Subject != identity.Subject ||
		audit.events[1].Profile != lease.Spec.Profile || audit.events[1].SourceIP != "10.1.2.3" {
		t.Fatalf("GET lease audit events = %#v", audit.events)
	}
	if response := p1HTTP(server, http.MethodGet, "/v1/credentials/lease-p1/revoke", ""); response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("revoke with GET status = %d", response.Code)
	}
	if response := p1HTTP(server, http.MethodPost, "/v1/credentials/lease-p1/revoke", ""); response.Code != http.StatusOK {
		t.Fatalf("first revoke status = %d, body = %s", response.Code, response.Body.String())
	}
	lastAudit := audit.events[len(audit.events)-1]
	if lastAudit.Operation != "revoke_lease" || lastAudit.Result != "success" || lastAudit.LeaseID != lease.Name ||
		lastAudit.Target == nil || lastAudit.Target.Subject != identity.Subject || lastAudit.Profile != lease.Spec.Profile {
		t.Fatalf("revoke audit event = %#v", lastAudit)
	}
	if response := p1HTTP(server, http.MethodPost, "/v1/credentials/lease-p1/revoke", ""); response.Code != http.StatusOK {
		t.Fatalf("idempotent revoke status = %d, body = %s", response.Code, response.Body.String())
	}

	server.Authenticator = staticAuthenticator{principal: Principal{Issuer: identity.Issuer, Subject: "bob"}}
	if response := p1HTTP(server, http.MethodGet, "/v1/credentials/lease-p1", ""); response.Code != http.StatusForbidden {
		t.Fatalf("other requester status = %d", response.Code)
	}
	server.Authenticator = staticAuthenticator{principal: Principal{Issuer: identity.Issuer, Subject: "admin", Groups: []string{server.Config.AdminGroup}}}
	if response := p1HTTP(server, http.MethodGet, "/v1/credentials/lease-p1", ""); response.Code != http.StatusOK {
		t.Fatalf("admin requester status = %d", response.Code)
	}

	missing := newTestServer(t, fake.NewClientBuilder().WithScheme(testScheme()).Build(), staticAuthenticator{principal: Principal{Issuer: identity.Issuer, Subject: identity.Subject}}, &recordingTokenIssuer{}, now)
	if response := p1HTTP(missing, http.MethodGet, "/v1/credentials/no-such-lease", ""); response.Code != http.StatusNotFound {
		t.Fatalf("missing lease status = %d", response.Code)
	}
	getError := errors.New("lease API unavailable")
	getFailing := fake.NewClientBuilder().WithScheme(testScheme()).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, types.NamespacedName, client.Object, ...client.GetOption) error {
			return getError
		},
	}).Build()
	failed := newTestServer(t, getFailing, staticAuthenticator{principal: Principal{Issuer: identity.Issuer, Subject: identity.Subject}}, &recordingTokenIssuer{}, now)
	response := p1HTTP(failed, http.MethodGet, "/v1/credentials/lease-p1", "")
	if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), getError.Error()) {
		t.Fatalf("API error response = %d %s", response.Code, response.Body.String())
	}
}

func TestBrokerLeaseStateHelpersCoverTimeoutCancellationReferencesAndCAS(t *testing.T) {
	now := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	identity := userv1.Identity{Issuer: "https://issuer.example", Subject: "alice"}
	baseClient := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	server := newTestServer(t, baseClient, staticAuthenticator{principal: Principal{Issuer: identity.Issuer, Subject: identity.Subject}}, &recordingTokenIssuer{}, now)

	server.Config.PrepareTimeout = 5 * time.Millisecond
	server.Config.PollInterval = time.Millisecond
	if _, err := server.waitPrepared(p1Context(context.Background()), "missing"); httpErrorCode(err) != http.StatusGatewayTimeout {
		t.Fatalf("waitPrepared timeout error = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := server.waitPrepared(p1Context(canceled), "missing"); httpErrorCode(err) != http.StatusServiceUnavailable {
		t.Fatalf("waitPrepared cancellation error = %v", err)
	}
	getError := errors.New("get failed")
	getFailing := fake.NewClientBuilder().WithScheme(testScheme()).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, types.NamespacedName, client.Object, ...client.GetOption) error {
			return getError
		},
	}).Build()
	setTestLeaseClient(server, getFailing)
	if _, err := server.waitPrepared(p1Context(context.Background()), "lease"); httpErrorCode(err) != http.StatusServiceUnavailable || strings.Contains(err.Error(), getError.Error()) == false {
		t.Fatalf("waitPrepared API error = %v", err)
	}
	prepared := p1PreparedLease("prepared", false, userv1.CredentialBrokerNamespace, identity, now)
	preparedClient := fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(&userv1.CredentialLease{}).WithObjects(prepared).Build()
	setTestLeaseClient(server, preparedClient)
	got, err := server.waitPrepared(p1Context(context.Background()), prepared.Name)
	if err != nil || got.Name != prepared.Name {
		t.Fatalf("waitPrepared prepared = %#v, %v", got, err)
	}
	terminal := prepared.DeepCopy()
	terminal.Name = "failed"
	terminal.Status.Phase = userv1.CredentialLeaseFailed
	terminalClient := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(terminal).Build()
	setTestLeaseClient(server, terminalClient)
	if _, err := server.waitPrepared(p1Context(context.Background()), terminal.Name); httpErrorCode(err) != http.StatusConflict {
		t.Fatalf("waitPrepared terminal error = %v", err)
	}

	setTestLeaseClient(server, baseClient)
	stableProfile, _ := internalcredentials.ProfileFor(userv1.BaseReadonlyProfile)
	temporaryProfile, _ := internalcredentials.ProfileFor(userv1.ClusterOpsWriteProfile)
	for _, test := range []struct {
		name    string
		lease   *userv1.CredentialLease
		profile internalcredentials.Profile
		valid   bool
	}{
		{name: "valid stable", lease: p1PreparedLease("stable", true, userv1.CredentialBrokerNamespace, identity, now), profile: stableProfile, valid: true},
		{name: "valid temporary", lease: p1PreparedLease("temporary", false, userv1.CredentialBrokerNamespace, identity, now), profile: temporaryProfile, valid: true},
		{name: "wrong namespace", lease: p1PreparedLease("wrong-ns", false, "other", identity, now), profile: temporaryProfile},
		{name: "invalid service account ref", lease: p1PreparedLease("bad-sa", false, userv1.CredentialBrokerNamespace, identity, now), profile: temporaryProfile},
		{name: "invalid secret uid", lease: p1PreparedLease("bad-secret", false, userv1.CredentialBrokerNamespace, identity, now), profile: temporaryProfile},
		{name: "invalid temporary binding", lease: p1PreparedLease("bad-binding", false, userv1.CredentialBrokerNamespace, identity, now), profile: temporaryProfile},
		{name: "stable binding present", lease: p1PreparedLease("stable-binding", true, userv1.CredentialBrokerNamespace, identity, now), profile: stableProfile},
	} {
		t.Run(test.name, func(t *testing.T) {
			switch test.name {
			case "invalid service account ref":
				test.lease.Status.ServiceAccountRef.Name = "wrong"
			case "invalid secret uid":
				test.lease.Status.BoundSecretRef.UID = ""
			case "invalid temporary binding":
				test.lease.Status.BindingRef.Name = "wrong"
			case "stable binding present":
				test.lease.Status.BindingRef = &corev1.ObjectReference{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding", Name: "unexpected"}
			}
			err := server.validatePreparedFor(p1Context(context.Background()), test.lease, test.profile)
			if test.valid && err != nil {
				t.Fatalf("validatePrepared() = %v", err)
			}
			if !test.valid && httpErrorCode(err) != http.StatusConflict {
				t.Fatalf("validatePrepared() = %v, want conflict", err)
			}
		})
	}

	prepared = p1PreparedLease("consume", false, userv1.CredentialBrokerNamespace, identity, now)
	setTestLeaseClient(server, fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(&userv1.CredentialLease{}).WithObjects(prepared).Build())
	consumedAt, err := server.markConsumed(p1Context(context.Background()), prepared)
	if err != nil || consumedAt.IsZero() {
		t.Fatalf("markConsumed() = %v, %v", consumedAt, err)
	}
	if prepared.Status.ConsumedAt == nil || prepared.Status.Phase != userv1.CredentialLeasePrepared {
		t.Fatalf("consumed lease = %#v", prepared.Status)
	}
	if _, err := server.markConsumed(p1Context(context.Background()), prepared); httpErrorCode(err) != http.StatusConflict {
		t.Fatalf("second markConsumed() = %v", err)
	}
	setTestLeaseClient(server, statusFailureClient(t, prepared, apierrors.NewNotFound(userv1.GroupVersion.WithResource("credentialleases").GroupResource(), prepared.Name)))
	notFoundCandidate := prepared.DeepCopy()
	notFoundCandidate.Status.ConsumedAt = nil
	if _, err := server.markConsumed(p1Context(context.Background()), notFoundCandidate); httpErrorCode(err) != http.StatusServiceUnavailable {
		t.Fatalf("markConsumed NotFound = %v", err)
	}
	setTestLeaseClient(server, statusFailureClient(t, prepared, context.Canceled))
	canceledCandidate := prepared.DeepCopy()
	canceledCandidate.Status.ConsumedAt = nil
	if _, err := server.markConsumed(p1Context(context.Background()), canceledCandidate); httpErrorCode(err) != http.StatusServiceUnavailable {
		t.Fatalf("markConsumed cancellation = %v", err)
	}
	prepared.Status.ConsumedAt = nil
	nonPrepared := prepared.DeepCopy()
	nonPrepared.Status.Phase = userv1.CredentialLeasePending
	if _, err := server.markConsumed(p1Context(context.Background()), nonPrepared); httpErrorCode(err) != http.StatusConflict {
		t.Fatalf("non-prepared markConsumed() = %v", err)
	}

	issued := p1PreparedLease("issued", false, userv1.CredentialBrokerNamespace, identity, now)
	issued.Status.ConsumedAt = &metav1.Time{Time: now}
	setTestLeaseClient(server, fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(&userv1.CredentialLease{}).WithObjects(issued).Build())
	if err := server.markIssued(p1Context(context.Background()), issued.Name, now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	wrongConsumed := now.Add(time.Second)
	if err := server.markIssued(p1Context(context.Background()), issued.Name, wrongConsumed, now.Add(time.Hour)); httpErrorCode(err) != http.StatusConflict {
		t.Fatalf("markIssued mismatch = %v", err)
	}
	revoked := issued.DeepCopy()
	revoked.Name = "revoked"
	revoked.ResourceVersion = ""
	revoked.UID = ""
	revoked.Status.Phase = userv1.CredentialLeaseRevoked
	revoked.Status.ConsumedAt = &metav1.Time{Time: now}
	if err := testLeaseClient(server).Create(context.Background(), revoked); err != nil {
		t.Fatal(err)
	}
	if err := server.markIssued(p1Context(context.Background()), revoked.Name, now, now.Add(time.Hour)); httpErrorCode(err) != http.StatusConflict {
		t.Fatalf("markIssued revoked = %v", err)
	}

	setTestLeaseClient(server, statusFailureClient(t, issued, errors.New("status failed")))
	if err := server.markIssued(p1Context(context.Background()), issued.Name, now, now.Add(time.Hour)); httpErrorCode(err) != http.StatusServiceUnavailable {
		t.Fatalf("markIssued status error = %v", err)
	}
	setTestLeaseClient(server, fake.NewClientBuilder().WithScheme(testScheme()).Build())
	if err := server.markIssued(p1Context(context.Background()), "missing", now, now.Add(time.Hour)); httpErrorCode(err) != http.StatusServiceUnavailable {
		t.Fatalf("markIssued NotFound = %v", err)
	}
}

func TestBrokerLeaseStateHelpersHandleAPIErrorsAndConcurrentCAS(t *testing.T) {
	now := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	identity := userv1.Identity{Issuer: "https://issuer.example", Subject: "alice"}
	lease := p1PreparedLease("cas", false, userv1.CredentialBrokerNamespace, identity, now)
	var updates int32
	client := fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(&userv1.CredentialLease{}).WithObjects(lease).WithInterceptorFuncs(interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, resource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if resource == "status" && !atomic.CompareAndSwapInt32(&updates, 0, 1) {
				return apierrors.NewConflict(userv1.GroupVersion.WithResource("credentialleases").GroupResource(), obj.GetName(), errors.New("CAS conflict"))
			}
			return c.Status().Update(ctx, obj, opts...)
		},
	}).Build()
	server := newTestServer(t, client, staticAuthenticator{principal: Principal{Issuer: identity.Issuer, Subject: identity.Subject}}, &recordingTokenIssuer{}, now)
	var group sync.WaitGroup
	errorsCh := make(chan error, 2)
	for i := 0; i < 2; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			errorsCh <- func() error {
				candidate := lease.DeepCopy()
				_, err := server.markConsumed(p1Context(context.Background()), candidate)
				return err
			}()
		}()
	}
	group.Wait()
	close(errorsCh)
	var successes, conflicts int
	for err := range errorsCh {
		if err == nil {
			successes++
		} else if httpErrorCode(err) == http.StatusConflict {
			conflicts++
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent markConsumed results = successes %d conflicts %d", successes, conflicts)
	}

	terminal := lease.DeepCopy()
	terminal.Status.Phase = userv1.CredentialLeaseRevoked
	if err := server.revoke(p1Context(context.Background()), terminal); err != nil {
		t.Fatalf("terminal revoke = %v", err)
	}
	statusError := errors.New("revoke status unavailable")
	setTestLeaseClient(server, statusFailureClient(t, lease, statusError))
	if err := server.revoke(p1Context(context.Background()), lease.DeepCopy()); httpErrorCode(err) != http.StatusServiceUnavailable || strings.Contains(err.Error(), statusError.Error()) == false {
		t.Fatalf("revoke status error = %v", err)
	}
	conflictClient := statusFailureClient(t, lease, apierrors.NewConflict(userv1.GroupVersion.WithResource("credentialleases").GroupResource(), lease.Name, errors.New("conflict")))
	setTestLeaseClient(server, conflictClient)
	if err := server.revoke(p1Context(context.Background()), lease.DeepCopy()); httpErrorCode(err) != http.StatusConflict {
		t.Fatalf("revoke conflict = %v", err)
	}
	notFoundClient := statusFailureClient(t, lease, apierrors.NewNotFound(userv1.GroupVersion.WithResource("credentialleases").GroupResource(), lease.Name))
	setTestLeaseClient(server, notFoundClient)
	if err := server.revoke(p1Context(context.Background()), lease.DeepCopy()); httpErrorCode(err) != http.StatusServiceUnavailable {
		t.Fatalf("revoke NotFound = %v", err)
	}
}

func TestSourceIPPolicySupportsIPv4IPv6AddressFormsNilCIDRsAndBoundaries(t *testing.T) {
	trusted, err := ParseCIDRs([]string{"127.0.0.1/32", "2001:db8::/32"})
	if err != nil {
		t.Fatal(err)
	}
	allowed, err := ParseCIDRs([]string{"10.0.0.0/24", "2001:db8:1::/48"})
	if err != nil {
		t.Fatal(err)
	}
	policy := SourceIPPolicy{TrustedProxyCIDRs: append([]*net.IPNet{nil}, trusted...), AllowedClientCIDRs: append([]*net.IPNet{nil}, allowed...), TrustedClientIPHeader: "X-Trusted-Client-IP"}
	for _, test := range []struct {
		name       string
		remoteAddr string
		clientIP   string
		wantIP     string
		wantErr    bool
	}{
		{name: "ipv4 host port lower boundary", remoteAddr: "127.0.0.1:443", clientIP: "10.0.0.0", wantIP: "10.0.0.0"},
		{name: "ipv4 bare upper boundary", remoteAddr: "127.0.0.1", clientIP: "10.0.0.255", wantIP: "10.0.0.255"},
		{name: "ipv6 host port", remoteAddr: "[2001:db8::1]:443", clientIP: "2001:db8:1::1", wantIP: "2001:db8:1::1"},
		{name: "ipv6 bare", remoteAddr: "2001:db8::1", clientIP: "2001:db8:1::ffff", wantIP: "2001:db8:1::ffff"},
		{name: "outside client boundary", remoteAddr: "127.0.0.1:443", clientIP: "10.0.1.0", wantErr: true},
		{name: "outside proxy boundary", remoteAddr: "127.0.0.2:443", clientIP: "10.0.0.1", wantErr: true},
		{name: "malformed remote", remoteAddr: "proxy.example:443", clientIP: "10.0.0.1", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.RemoteAddr = test.remoteAddr
			request.Header.Set("X-Trusted-Client-IP", test.clientIP)
			got, err := policy.Authorize(request)
			if test.wantErr {
				if err == nil {
					t.Fatalf("Authorize() = %v, nil error; want rejection", got)
				}
				return
			}
			if err != nil || got.String() != test.wantIP {
				t.Fatalf("Authorize() = %v, %v; want %s", got, err, test.wantIP)
			}
		})
	}
	if got, err := (SourceIPPolicy{TrustedProxyCIDRs: []*net.IPNet{nil}, AllowedClientCIDRs: allowed, TrustedClientIPHeader: "X-Trusted-Client-IP"}).Authorize(func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "127.0.0.1:443"
		r.Header.Set("X-Trusted-Client-IP", "10.0.0.1")
		return r
	}()); err == nil || got != nil {
		t.Fatalf("nil trusted CIDR = %v, %v; want rejection", got, err)
	}

	for _, values := range [][]string{{"10.0.0.1", "10.0.0.2"}, {" 10.0.0.1"}, {"10.0.0.1 "}} {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.RemoteAddr = "127.0.0.1:443"
		for _, value := range values {
			request.Header.Add("X-Trusted-Client-IP", value)
		}
		if got, err := policy.Authorize(request); err == nil || got != nil {
			t.Fatalf("ambiguous/whitespace header = %v, %v; want rejection", got, err)
		}
	}
}

type p1Authenticator struct {
	principal Principal
	err       error
	calls     int32
}

func (a *p1Authenticator) Authenticate(context.Context, string) (Principal, error) {
	atomic.AddInt32(&a.calls, 1)
	return a.principal, a.err
}

func p1HTTP(server *Server, method, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, testTargetPath(path), bytes.NewBufferString(body))
	request.RemoteAddr = "127.0.0.1:443"
	request.Header.Set("X-Trusted-Client-IP", "10.1.2.3")
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response
}

func p1PreparedLease(name string, stable bool, namespace string, identity userv1.Identity, now time.Time) *userv1.CredentialLease {
	lease := &userv1.CredentialLease{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}, Spec: userv1.CredentialLeaseSpec{Target: identity, Profile: userv1.ClusterOpsWriteProfile}, Status: userv1.CredentialLeaseStatus{Phase: userv1.CredentialLeasePrepared}}
	if stable {
		lease.Spec.Profile = userv1.BaseReadonlyProfile
		lease.Status.ServiceAccountRef = &corev1.ObjectReference{APIVersion: "v1", Kind: "ServiceAccount", Namespace: userv1.InternalUserSystemNamespace, Name: internalcredentials.DeriveInternalUserName(identity.Issuer, identity.Subject)}
		lease.Status.BoundSecretRef = &corev1.ObjectReference{APIVersion: "v1", Kind: "Secret", Namespace: userv1.InternalUserSystemNamespace, Name: "stable-secret", UID: "stable-secret-uid"}
		return lease
	}
	lease.Status.ServiceAccountRef = &corev1.ObjectReference{APIVersion: "v1", Kind: "ServiceAccount", Namespace: userv1.InternalUserSystemNamespace, Name: internalcredentials.ResourceName("internal-lease", namespace, name)}
	lease.Status.BoundSecretRef = &corev1.ObjectReference{APIVersion: "v1", Kind: "Secret", Namespace: userv1.InternalUserSystemNamespace, Name: "secret-" + name, UID: types.UID("uid-" + name)}
	lease.Status.BindingRef = &corev1.ObjectReference{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding", Name: internalcredentials.ResourceName("internal-lease-binding", namespace, name)}
	lease.Status.ExpirationTimestamp = &metav1.Time{Time: now.Add(time.Hour)}
	return lease
}

func statusFailureClient(t *testing.T, object client.Object, statusErr error) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(&userv1.CredentialLease{}).WithObjects(object).WithInterceptorFuncs(interceptor.Funcs{
		SubResourceUpdate: func(_ context.Context, _ client.Client, resource string, _ client.Object, _ ...client.SubResourceUpdateOption) error {
			if resource == "status" {
				return statusErr
			}
			return nil
		},
	}).Build()
}

func httpErrorCode(err error) int {
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.Code
	}
	return 0
}
