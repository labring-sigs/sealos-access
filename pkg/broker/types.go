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
	"errors"
	"fmt"
	"math"
	"net"
	"time"

	userv1 "github.com/labring-sigs/sealos-access/api/v1"
	"github.com/labring-sigs/sealos-access/pkg/internalcredentials"
	"golang.org/x/time/rate"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Principal is the result of verifying a Broker OIDC bearer token.
type Principal struct {
	Issuer  string
	Subject string
	Groups  []string
}

func (p Principal) Identity() userv1.Identity {
	return userv1.Identity{Issuer: p.Issuer, Subject: p.Subject}
}

type Authenticator interface {
	Authenticate(ctx context.Context, bearerToken string) (Principal, error)
}

type TokenResult struct {
	Token      string
	Expiration time.Time
	Audiences  []string
}

type TokenIssuer interface {
	Issue(ctx context.Context, namespace, serviceAccount string, spec authenticationv1.TokenRequestSpec) (TokenResult, error)
}

// TargetSettings contains server-owned settings for one Kubernetes target.
// Secret material is intentionally absent; the backend owns the clients that
// were constructed from the target credential.
type TargetSettings struct {
	ID                 string
	APIServer          string
	ClusterServer      string
	ClusterCAData      []byte
	KubernetesAudience string
	UserNamespace      string
	BrokerNamespace    string
	ClusterName        string
	Enabled            bool
}

// TargetBackend is the narrow operation boundary used by HTTP handlers. A
// direct Kubernetes backend and a future per-cluster Agent backend implement
// the same contract.
type TargetBackend interface {
	Settings() TargetSettings
	LeaseClient() client.Client
	TokenIssuer() TokenIssuer
}

// StaticTargetBackend is the direct-credential implementation for one target.
// It is also useful for tests and for wiring a future remote Agent backend.
type StaticTargetBackend struct {
	TargetSettings TargetSettings
	Lease          client.Client
	Issuer         TokenIssuer
}

func NewStaticTargetBackend(settings TargetSettings, leaseClient client.Client, tokenIssuer TokenIssuer) (*StaticTargetBackend, error) {
	if settings.ClusterName == "" {
		settings.ClusterName = settings.ID
	}
	if err := validateTargetSettings(settings); err != nil {
		return nil, err
	}
	if leaseClient == nil || tokenIssuer == nil {
		return nil, errors.New("target backend requires a Lease client and TokenIssuer")
	}
	settings.ClusterCAData = append([]byte(nil), settings.ClusterCAData...)
	return &StaticTargetBackend{TargetSettings: settings, Lease: leaseClient, Issuer: tokenIssuer}, nil
}

func (b *StaticTargetBackend) Settings() TargetSettings {
	if b == nil {
		return TargetSettings{}
	}
	settings := b.TargetSettings
	settings.ClusterCAData = append([]byte(nil), settings.ClusterCAData...)
	return settings
}

func (b *StaticTargetBackend) LeaseClient() client.Client {
	if b == nil {
		return nil
	}
	return b.Lease
}

func (b *StaticTargetBackend) TokenIssuer() TokenIssuer {
	if b == nil {
		return nil
	}
	return b.Issuer
}

// AuditEvent contains only non-secret request metadata. In particular, it has
// no token, kubeconfig, Secret data, or refresh token field by design.
type AuditEvent struct {
	Timestamp         time.Time        `json:"timestamp"`
	Operation         string           `json:"operation"`
	ClusterID         string           `json:"clusterID,omitempty"`
	Requester         *userv1.Identity `json:"requester,omitempty"`
	Target            *userv1.Identity `json:"target,omitempty"`
	Profile           string           `json:"profile,omitempty"`
	LeaseID           string           `json:"leaseID,omitempty"`
	ApprovalReference string           `json:"approvalReference,omitempty"`
	SourceIP          string           `json:"sourceIP,omitempty"`
	UserAgent         string           `json:"userAgent,omitempty"`
	Result            string           `json:"result"`
	StatusCode        int              `json:"statusCode"`
}

// AuditSink receives one event after a Broker request has been processed.
// Implementations must not persist request or response bodies.
type AuditSink interface {
	Record(ctx context.Context, event AuditEvent) error
}

// SourceIPPolicy requires the Broker request to come from a configured
// Higress egress address and validates the normalized client IP header.
type SourceIPPolicy struct {
	TrustedProxyCIDRs     []*net.IPNet
	AllowedClientCIDRs    []*net.IPNet
	TrustedClientIPHeader string
}

// Config contains only server-owned policy. Callers cannot override any field
// through an HTTP request.
type Config struct {
	AllowedIssuers      map[string]struct{}
	AdminGroup          string
	SourceIP            SourceIPPolicy
	PrepareTimeout      time.Duration
	ApprovalWaitTimeout time.Duration
	PollInterval        time.Duration
	MaxBodyBytes        int64
	RequestRateLimit    rate.Limit
	RequestRateBurst    int
	Clock               func() time.Time
	ClockSkew           time.Duration
}

type Server struct {
	Authenticator  Authenticator
	Audit          AuditSink
	Config         Config
	Targets        map[string]TargetBackend
	requestLimiter *rate.Limiter
}

type targetContextKey struct{}

func withTargetID(ctx context.Context, targetID string) context.Context {
	return context.WithValue(ctx, targetContextKey{}, targetID)
}

func targetIDFromContext(ctx context.Context) string {
	targetID, _ := ctx.Value(targetContextKey{}).(string)
	return targetID
}

// HTTPError is deliberately safe to return to the caller. Internal Kubernetes
// errors are retained only as wrapped causes and are never serialized.
type HTTPError struct {
	Code    int
	Message string
	Cause   error
}

func (e *HTTPError) Error() string {
	if e.Cause == nil {
		return e.Message
	}
	return fmt.Sprintf("%s: %v", e.Message, e.Cause)
}

var (
	errInvalidToken = errors.New("invalid bearer token")
	errLeaseBusy    = errors.New("credential lease is not ready")
)

type issueRequest struct {
	RequestedTTLSeconds int64 `json:"requestedTTLSeconds"`
}

type adminIssueRequest struct {
	ClusterID           string          `json:"clusterID,omitempty"`
	Target              userv1.Identity `json:"target"`
	RequestedTTLSeconds int64           `json:"requestedTTLSeconds"`
	ApprovalReference   string          `json:"approvalReference"`
}

type elevatedRedeemRequest struct {
	ApprovalReference string `json:"approvalReference"`
}

// ApprovalRecord is the non-secret record submitted by an authorized
// approval adapter after an external approval has completed.
type ApprovalRecord struct {
	ClusterID           string          `json:"clusterID,omitempty"`
	ApprovalID          string          `json:"approvalID"`
	Target              userv1.Identity `json:"target"`
	RequestedTTLSeconds int64           `json:"requestedTTLSeconds"`
	ApprovalReason      string          `json:"approvalReason"`
}

// ApprovalResponse contains the one-time reference generated by the Broker.
// The reference must be delivered only to the target user through a protected
// channel and must never be treated as an authentication factor.
type ApprovalResponse struct {
	ClusterID           string    `json:"clusterID"`
	ApprovalID          string    `json:"approvalID"`
	ApprovalReference   string    `json:"approvalReference"`
	Profile             string    `json:"profile"`
	RequestedTTLSeconds int64     `json:"requestedTTLSeconds"`
	ApprovalExpiresAt   time.Time `json:"approvalExpiresAt"`
}

type internalApprovalRequest = ApprovalRecord
type approvalResponse = ApprovalResponse

type issueResponse struct {
	ClusterID           string    `json:"clusterID"`
	LeaseID             string    `json:"leaseID"`
	Profile             string    `json:"profile"`
	Username            string    `json:"username"`
	ExpirationTimestamp time.Time `json:"expirationTimestamp"`
	Kubeconfig          string    `json:"kubeconfig"`
}

type leaseResponse struct {
	ClusterID           string                      `json:"clusterID"`
	LeaseID             string                      `json:"leaseID"`
	Profile             string                      `json:"profile"`
	Phase               userv1.CredentialLeasePhase `json:"phase"`
	ExpirationTimestamp *metav1.Time                `json:"expirationTimestamp,omitempty"`
	ConsumedAt          *metav1.Time                `json:"consumedAt,omitempty"`
}

func (s *Server) normalizeConfig() error {
	if s.Authenticator == nil {
		return errors.New("broker requires an authenticator")
	}
	if len(s.Targets) == 0 {
		return errors.New("broker requires at least one target backend")
	}
	if s.Config.AdminGroup == "" {
		return errors.New("broker admin group is required")
	}
	for targetID, backend := range s.Targets {
		if backend == nil {
			return fmt.Errorf("target %q backend is nil", targetID)
		}
		settings := backend.Settings()
		if settings.ID != targetID {
			return fmt.Errorf("target backend key %q does not match target ID %q", targetID, settings.ID)
		}
		if !settings.Enabled {
			return fmt.Errorf("target %q is disabled", targetID)
		}
		if backend.LeaseClient() == nil || backend.TokenIssuer() == nil {
			return fmt.Errorf("target %q backend is incomplete", targetID)
		}
		if err := validateTargetSettings(settings); err != nil {
			return err
		}
	}
	if s.Config.PrepareTimeout <= 0 {
		s.Config.PrepareTimeout = 30 * time.Second
	}
	if s.Config.ApprovalWaitTimeout <= 0 {
		s.Config.ApprovalWaitTimeout = 24 * time.Hour
	}
	if s.Config.PollInterval <= 0 {
		s.Config.PollInterval = 250 * time.Millisecond
	}
	if s.Config.MaxBodyBytes <= 0 {
		s.Config.MaxBodyBytes = 16 * 1024
	}
	if math.IsNaN(float64(s.Config.RequestRateLimit)) || math.IsInf(float64(s.Config.RequestRateLimit), 0) || s.Config.RequestRateLimit < 0 {
		return errors.New("broker request rate limit must be a finite non-negative number")
	}
	if s.Config.RequestRateLimit == 0 {
		s.Config.RequestRateLimit = 10
	}
	if s.Config.RequestRateBurst < 0 {
		return errors.New("broker request rate burst must not be negative")
	}
	if s.Config.RequestRateBurst == 0 {
		s.Config.RequestRateBurst = 20
	}
	s.requestLimiter = rate.NewLimiter(s.Config.RequestRateLimit, s.Config.RequestRateBurst)
	if s.Config.Clock == nil {
		s.Config.Clock = time.Now
	}
	if s.Config.ClockSkew <= 0 {
		s.Config.ClockSkew = 30 * time.Second
	}
	if len(s.Config.AllowedIssuers) == 0 {
		return errors.New("broker issuer allowlist is required")
	}
	if err := s.Config.SourceIP.Validate(); err != nil {
		return err
	}
	return nil
}

func newHTTPError(code int, message string, cause error) *HTTPError {
	return &HTTPError{Code: code, Message: message, Cause: cause}
}

func (s *Server) now() time.Time {
	if s.Config.Clock != nil {
		return s.Config.Clock()
	}
	return time.Now()
}

func (s *Server) issuerAllowed(issuer string) bool {
	_, ok := s.Config.AllowedIssuers[issuer]
	return ok
}

func validatePrincipal(p Principal) error {
	return internalcredentials.ValidateIdentity(p.Issuer, p.Subject)
}

func isAdmin(p Principal, group string) bool {
	for _, candidate := range p.Groups {
		if candidate == group {
			return true
		}
	}
	return false
}

func namespaced(name, namespace string) types.NamespacedName {
	return types.NamespacedName{Name: name, Namespace: namespace}
}

func tokenRequestSpec(ttl time.Duration, audience string, secret *corev1.ObjectReference) authenticationv1.TokenRequestSpec {
	return authenticationv1.TokenRequestSpec{
		Audiences:         []string{audience},
		ExpirationSeconds: ptr.To(int64(ttl / time.Second)),
		BoundObjectRef: &authenticationv1.BoundObjectReference{
			Kind:       secret.Kind,
			APIVersion: secret.APIVersion,
			Name:       secret.Name,
			UID:        secret.UID,
		},
	}
}
