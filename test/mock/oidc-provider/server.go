// Package oidc provides a deliberately small OIDC provider for tests.
//
// It is not intended for development, staging, or production authentication.
// Keys and authorization state live only in memory and are generated per
// process.
package oidc

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	ConfigEnv            = "SEALOS_TEST_OIDC_CONFIG"
	defaultTokenTTL      = 5 * time.Minute
	authorizationCodeTTL = 2 * time.Minute
)

type User struct {
	Subject string         `json:"sub"`
	Email   string         `json:"email,omitempty"`
	Name    string         `json:"name,omitempty"`
	Groups  []string       `json:"groups,omitempty"`
	Claims  map[string]any `json:"claims,omitempty"`
}

type Client struct {
	ID           string   `json:"client_id"`
	RedirectURIs []string `json:"redirect_uris"`
}

type Config struct {
	Issuer          string   `json:"issuer,omitempty"`
	Users           []User   `json:"users,omitempty"`
	Clients         []Client `json:"clients,omitempty"`
	TokenAudience   string   `json:"token_audience,omitempty"`
	TokenTTLSeconds int64    `json:"token_ttl_seconds,omitempty"`
}

type Provider struct {
	mu      sync.Mutex
	config  Config
	users   map[string]User
	clients map[string]Client
	codes   map[string]authorizationCode
	key     *rsa.PrivateKey
	kid     string
}

type authorizationCode struct {
	clientID    string
	redirectURI string
	subject     string
	challenge   string
	nonce       string
	expiresAt   time.Time
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
	IDToken     string `json:"id_token"`
}

// New creates an in-memory test provider.
func New(config Config) (*Provider, error) {
	config, err := normalizeConfig(config)
	if err != nil {
		return nil, err
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate OIDC signing key: %w", err)
	}
	kid, err := randomString(12)
	if err != nil {
		return nil, fmt.Errorf("generate OIDC key ID: %w", err)
	}
	users := make(map[string]User, len(config.Users))
	for _, user := range config.Users {
		users[user.Subject] = user
	}
	clients := make(map[string]Client, len(config.Clients))
	for _, client := range config.Clients {
		clients[client.ID] = client
	}
	return &Provider{
		config:  config,
		users:   users,
		clients: clients,
		codes:   make(map[string]authorizationCode),
		key:     key,
		kid:     kid,
	}, nil
}

// NewFromJSON creates a provider from the JSON used by the test container.
func NewFromJSON(data []byte) (*Provider, error) {
	var config Config
	if len(data) != 0 {
		if err := json.Unmarshal(data, &config); err != nil {
			return nil, fmt.Errorf("decode OIDC config: %w", err)
		}
	}
	return New(config)
}

// Handler returns the provider HTTP handler.
func (p *Provider) Handler() http.Handler {
	return p
}

// KeyID returns the current signing key ID.
func (p *Provider) KeyID() string {
	return p.kid
}

// PublicKey returns the current signing public key for direct JWT assertions
// in tests.
func (p *Provider) PublicKey() *rsa.PublicKey {
	return &p.key.PublicKey
}

func (p *Provider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := p.requestPath(r)
	switch path {
	case "/.well-known/openid-configuration":
		p.handleDiscovery(w, r)
	case "/jwks.json":
		p.handleJWKS(w, r)
	case "/authorize":
		p.handleAuthorize(w, r)
	case "/token":
		p.handleToken(w, r)
	case "/healthz":
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	default:
		http.NotFound(w, r)
	}
}

func (p *Provider) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	issuer := p.issuerForRequest(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                issuer,
		"authorization_endpoint":                endpoint(issuer, "/authorize"),
		"token_endpoint":                        endpoint(issuer, "/token"),
		"jwks_uri":                              endpoint(issuer, "/jwks.json"),
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"scopes_supported":                      []string{"openid", "profile", "email", "groups"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"code_challenge_methods_supported":      []string{"S256"},
	})
}

func (p *Provider) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	modulus := base64.RawURLEncoding.EncodeToString(p.key.N.Bytes())
	exponent := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(p.key.E)).Bytes())
	writeJSON(w, http.StatusOK, map[string]any{
		"keys": []map[string]string{{
			"kty": "RSA",
			"use": "sig",
			"alg": "RS256",
			"kid": p.kid,
			"n":   modulus,
			"e":   exponent,
		}},
	})
}

func (p *Provider) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOAuthError(w, http.StatusMethodNotAllowed, "invalid_request", "authorization endpoint requires GET")
		return
	}
	query := r.URL.Query()
	client, ok := p.client(query.Get("client_id"))
	if !ok || !redirectAllowed(client.RedirectURIs, query.Get("redirect_uri")) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "unknown client or redirect URI")
		return
	}
	if query.Get("response_type") != "code" || !hasScope(query.Get("scope"), "openid") {
		p.redirectAuthorizeError(w, r, query.Get("redirect_uri"), query.Get("state"), "unsupported_response_type", "response_type=code and scope=openid are required")
		return
	}
	challenge := query.Get("code_challenge")
	if query.Get("code_challenge_method") != "S256" || len(challenge) != 43 {
		p.redirectAuthorizeError(w, r, query.Get("redirect_uri"), query.Get("state"), "invalid_request", "S256 code challenge is required")
		return
	}
	subject := query.Get("login_hint")
	if subject == "" {
		subject = query.Get("test_user")
	}
	if subject == "" {
		p.mu.Lock()
		if len(p.users) == 1 {
			for candidate := range p.users {
				subject = candidate
			}
		}
		p.mu.Unlock()
	}
	if !p.hasUser(subject) {
		p.redirectAuthorizeError(w, r, query.Get("redirect_uri"), query.Get("state"), "access_denied", "test user is not configured")
		return
	}
	code, err := randomString(32)
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "failed to create authorization code")
		return
	}
	p.mu.Lock()
	p.codes[code] = authorizationCode{
		clientID:    client.ID,
		redirectURI: query.Get("redirect_uri"),
		subject:     subject,
		challenge:   challenge,
		nonce:       query.Get("nonce"),
		expiresAt:   time.Now().Add(authorizationCodeTTL),
	}
	p.mu.Unlock()
	redirect, err := url.Parse(query.Get("redirect_uri"))
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "invalid redirect URI")
		return
	}
	redirectQuery := redirect.Query()
	redirectQuery.Set("code", code)
	if state := query.Get("state"); state != "" {
		redirectQuery.Set("state", state)
	}
	redirect.RawQuery = redirectQuery.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusFound)
}

func (p *Provider) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOAuthError(w, http.StatusMethodNotAllowed, "invalid_request", "token endpoint requires POST")
		return
	}
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "invalid form")
		return
	}
	if r.Form.Get("grant_type") != "authorization_code" {
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "authorization_code is required")
		return
	}
	codeValue := r.Form.Get("code")
	p.mu.Lock()
	authCode, ok := p.codes[codeValue]
	if ok {
		delete(p.codes, codeValue)
	}
	p.mu.Unlock()
	if !ok || time.Now().After(authCode.expiresAt) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "authorization code is invalid or expired")
		return
	}
	if r.Form.Get("client_id") != authCode.clientID || r.Form.Get("redirect_uri") != authCode.redirectURI {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "authorization code binding is invalid")
		return
	}
	verifier := r.Form.Get("code_verifier")
	digest := sha256.Sum256([]byte(verifier))
	if !validCodeVerifier(verifier) || base64.RawURLEncoding.EncodeToString(digest[:]) != authCode.challenge {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
		return
	}
	user, ok := p.user(authCode.subject)
	if !ok {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "test user no longer exists")
		return
	}
	issuer := p.issuerForRequest(r)
	ttl := p.tokenTTL()
	now := time.Now().UTC()
	expiresAt := now.Add(ttl)
	accessAudience := authCode.clientID
	if p.config.TokenAudience != "" {
		accessAudience = p.config.TokenAudience
	}
	accessToken, err := p.signJWT(issuer, user, accessAudience, now, expiresAt, nil)
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "failed to sign access token")
		return
	}
	idTokenClaims := map[string]any{"azp": authCode.clientID}
	if authCode.nonce != "" {
		idTokenClaims["nonce"] = authCode.nonce
	}
	idToken, err := p.signJWT(issuer, user, authCode.clientID, now, expiresAt, idTokenClaims)
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "failed to sign ID token")
		return
	}
	writeJSON(w, http.StatusOK, tokenResponse{
		AccessToken: accessToken,
		TokenType:   "Bearer",
		ExpiresIn:   int64(ttl / time.Second),
		IDToken:     idToken,
	})
}

// MintToken creates a signed JWT for tests that exercise a resource server
// directly without going through the authorization-code flow.
func (p *Provider) MintToken(issuer, subject, audience string, ttl time.Duration) (string, error) {
	if strings.TrimSpace(issuer) == "" || strings.TrimSpace(audience) == "" {
		return "", errors.New("issuer and audience are required")
	}
	user, ok := p.user(subject)
	if !ok {
		return "", fmt.Errorf("unknown test user %q", subject)
	}
	if ttl <= 0 {
		ttl = p.tokenTTL()
	}
	now := time.Now().UTC()
	return p.signJWT(issuer, user, audience, now, now.Add(ttl), nil)
}

func (p *Provider) signJWT(issuer string, user User, audience string, issuedAt, expiresAt time.Time, extra map[string]any) (string, error) {
	claims := map[string]any{
		"iss": userIssuer(issuer),
		"sub": user.Subject,
		"aud": audience,
		"iat": issuedAt.Unix(),
		"exp": expiresAt.Unix(),
	}
	if user.Email != "" {
		claims["email"] = user.Email
		claims["email_verified"] = true
	}
	if user.Name != "" {
		claims["name"] = user.Name
	}
	if user.Groups != nil {
		claims["groups"] = append([]string(nil), user.Groups...)
	}
	for key, value := range user.Claims {
		if !reservedClaim(key) {
			claims[key] = value
		}
	}
	for key, value := range extra {
		claims[key] = value
	}
	header := map[string]any{"alg": "RS256", "kid": p.kid, "typ": "JWT"}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	unsigned := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(claimsJSON)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, p.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func normalizeConfig(config Config) (Config, error) {
	config.Issuer = strings.TrimRight(strings.TrimSpace(config.Issuer), "/")
	if config.Issuer != "" {
		parsed, err := url.Parse(config.Issuer)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return Config{}, errors.New("OIDC issuer must be an absolute URL without query or fragment")
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return Config{}, errors.New("OIDC issuer must use http or https")
		}
	}
	if len(config.Users) == 0 {
		config.Users = []User{{Subject: "test-user", Email: "test-user@example.invalid", Name: "Test User"}}
	}
	if len(config.Clients) == 0 {
		config.Clients = []Client{{ID: "test-client", RedirectURIs: []string{"http://127.0.0.1/callback"}}}
	}
	seenUsers := make(map[string]struct{}, len(config.Users))
	for _, user := range config.Users {
		if user.Subject == "" {
			return Config{}, errors.New("OIDC test user subject is required")
		}
		if _, exists := seenUsers[user.Subject]; exists {
			return Config{}, fmt.Errorf("duplicate OIDC test user subject %q", user.Subject)
		}
		seenUsers[user.Subject] = struct{}{}
	}
	seenClients := make(map[string]struct{}, len(config.Clients))
	for _, client := range config.Clients {
		if client.ID == "" || len(client.RedirectURIs) == 0 {
			return Config{}, errors.New("OIDC client ID and redirect URI are required")
		}
		if _, exists := seenClients[client.ID]; exists {
			return Config{}, fmt.Errorf("duplicate OIDC client ID %q", client.ID)
		}
		seenClients[client.ID] = struct{}{}
		for _, redirectURI := range client.RedirectURIs {
			parsed, err := url.Parse(redirectURI)
			if err != nil || parsed.Host == "" || parsed.Fragment != "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
				return Config{}, fmt.Errorf("invalid OIDC redirect URI %q", redirectURI)
			}
		}
	}
	if config.TokenTTLSeconds <= 0 {
		config.TokenTTLSeconds = int64(defaultTokenTTL / time.Second)
	}
	if config.TokenTTLSeconds > int64(time.Hour/time.Second) {
		return Config{}, errors.New("OIDC test token TTL must not exceed one hour")
	}
	return config, nil
}

func (p *Provider) tokenTTL() time.Duration {
	return time.Duration(p.config.TokenTTLSeconds) * time.Second
}

func (p *Provider) client(id string) (Client, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	client, ok := p.clients[id]
	return client, ok
}

func (p *Provider) user(subject string) (User, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	user, ok := p.users[subject]
	return user, ok
}

func (p *Provider) hasUser(subject string) bool {
	_, ok := p.user(subject)
	return ok
}

func (p *Provider) issuerForRequest(r *http.Request) string {
	if p.config.Issuer != "" {
		return p.config.Issuer
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (p *Provider) requestPath(r *http.Request) string {
	if p.config.Issuer == "" {
		return r.URL.Path
	}
	parsed, err := url.Parse(p.config.Issuer)
	if err != nil || parsed.Path == "" || parsed.Path == "/" {
		return r.URL.Path
	}
	prefix := strings.TrimRight(parsed.Path, "/")
	if r.URL.Path == prefix {
		return "/"
	}
	if strings.HasPrefix(r.URL.Path, prefix+"/") {
		return strings.TrimPrefix(r.URL.Path, prefix)
	}
	return r.URL.Path
}

func (p *Provider) redirectAuthorizeError(w http.ResponseWriter, r *http.Request, redirectURI, state, code, description string) {
	redirect, err := url.Parse(redirectURI)
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, code, description)
		return
	}
	query := redirect.Query()
	query.Set("error", code)
	query.Set("error_description", description)
	if state != "" {
		query.Set("state", state)
	}
	redirect.RawQuery = query.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusFound)
}

func writeOAuthError(w http.ResponseWriter, status int, code, description string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": description})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func endpoint(issuer, suffix string) string {
	return strings.TrimRight(issuer, "/") + suffix
}

func userIssuer(issuer string) string {
	return strings.TrimRight(issuer, "/")
}

func hasScope(scope, wanted string) bool {
	for _, item := range strings.Fields(scope) {
		if item == wanted {
			return true
		}
	}
	return false
}

func contains(items []string, wanted string) bool {
	for _, item := range items {
		if item == wanted {
			return true
		}
	}
	return false
}

func redirectAllowed(registered []string, requested string) bool {
	if contains(registered, requested) {
		return true
	}
	requestedURL, err := url.Parse(requested)
	if err != nil || requestedURL.Fragment != "" || requestedURL.RawQuery != "" {
		return false
	}
	for _, candidate := range registered {
		registeredURL, err := url.Parse(candidate)
		if err != nil || registeredURL.Fragment != "" || registeredURL.RawQuery != "" {
			continue
		}
		if registeredURL.Scheme != "http" || requestedURL.Scheme != registeredURL.Scheme ||
			registeredURL.Hostname() != requestedURL.Hostname() || registeredURL.Path != requestedURL.Path ||
			registeredURL.Port() != "" || requestedURL.Port() == "" {
			continue
		}
		if registeredURL.Hostname() == "127.0.0.1" || registeredURL.Hostname() == "::1" || registeredURL.Hostname() == "localhost" {
			return true
		}
	}
	return false
}

func reservedClaim(key string) bool {
	switch key {
	case "iss", "sub", "aud", "iat", "exp", "nbf", "jti", "nonce", "azp":
		return true
	default:
		return false
	}
}

func validCodeVerifier(verifier string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	for _, char := range verifier {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || strings.ContainsRune("-._~", char) {
			continue
		}
		return false
	}
	return true
}

func randomString(size int) (string, error) {
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}
