package broker

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"
)

func TestOIDCVerifierValidatesSignatureAndClaims(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	verifier, err := NewOIDCVerifier(OIDCConfig{Issuer: "https://issuer.example", Audience: "broker", JWKSURL: "https://issuer.example/jwks", Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	verifier.cache.mu.Lock()
	verifier.cache.keys = []oidcJWK{{
		KeyType: "RSA", Kid: "key-1", Use: "sig", Alg: "RS256",
		N: base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()), E: "AQAB",
	}}
	verifier.cache.expiresAt = now.Add(time.Hour)
	verifier.cache.mu.Unlock()
	signedToken := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": "https://issuer.example", "sub": "alice", "aud": "broker", "exp": now.Add(time.Minute).Unix(), "nbf": now.Add(-time.Minute).Unix(), "groups": []string{"sealos.internal.user"},
	})
	signedToken.Header["kid"] = "key-1"
	token, err := signedToken.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := verifier.Authenticate(t.Context(), token)
	if err != nil {
		t.Fatal(err)
	}
	if principal.Subject != "alice" || len(principal.Groups) != 1 || principal.Groups[0] != "sealos.internal.user" {
		t.Fatalf("unexpected principal: %#v", principal)
	}

	wrongAudienceToken := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": "https://issuer.example", "sub": "alice", "aud": "other", "exp": now.Add(time.Minute).Unix(),
	})
	wrongAudienceToken.Header["kid"] = "key-1"
	wrongAudience, err := wrongAudienceToken.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.Authenticate(t.Context(), wrongAudience); err == nil {
		t.Fatal("wrong OIDC audience was accepted")
	}
}

func TestOIDCVerifierRejectsUnexpectedAlgorithm(t *testing.T) {
	if _, err := NewOIDCVerifier(OIDCConfig{Issuer: "https://issuer.example", Audience: "broker", JWKSURL: "https://issuer.example/jwks", AllowedAlgorithms: []string{"none"}}); err == nil {
		t.Fatal("unexpected OIDC algorithm was accepted in configuration")
	}
}

func TestOIDCVerifierFailsClosedWhenJWKSIsUnavailable(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	verifier, err := NewOIDCVerifier(OIDCConfig{
		Issuer:     "https://issuer.example",
		Audience:   "broker",
		JWKSURL:    "https://issuer.example/jwks",
		HTTPClient: &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("OIDC provider unavailable") })},
		Clock:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": "https://issuer.example", "sub": "alice", "aud": "broker", "exp": now.Add(time.Minute).Unix(),
	})
	token.Header["kid"] = "key-unavailable"
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.Authenticate(t.Context(), signed); err == nil {
		t.Fatal("OIDC authentication succeeded while JWKS was unavailable")
	}
}

func TestOIDCVerifierSupportsPrivateTLSDiscoveryAndJWKSRotation(t *testing.T) {
	keyOne, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyTwo, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	var mu sync.RWMutex
	activeKey := keyOne
	activeKid := "key-one"
	issuer := "https://issuer.internal.example"
	httpClient := &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		mu.RLock()
		key, kid := activeKey, activeKid
		mu.RUnlock()
		var value interface{}
		switch request.URL.Path {
		case "/.well-known/openid-configuration":
			value = map[string]string{"issuer": issuer, "jwks_uri": issuer + "/jwks"}
		case "/jwks":
			value = oidcJWKS{Keys: []oidcJWK{testRSAJWK(key, kid)}}
		default:
			return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(bytes.NewReader(nil)), Request: request}, nil
		}
		body, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(body)), Request: request}, nil
	})}

	verifier, err := NewOIDCVerifier(OIDCConfig{
		Issuer:     issuer,
		Audience:   "broker",
		HTTPClient: httpClient,
		CacheTTL:   time.Hour,
		Clock:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	first := signOIDCTestToken(t, keyOne, issuer, "key-one", now)
	if _, err := verifier.Authenticate(t.Context(), first); err != nil {
		t.Fatalf("authenticate with private-TLS discovery key = %v", err)
	}

	mu.Lock()
	activeKey = keyTwo
	activeKid = "key-two"
	mu.Unlock()
	rotated := signOIDCTestToken(t, keyTwo, issuer, "key-two", now)
	if _, err := verifier.Authenticate(t.Context(), rotated); err != nil {
		t.Fatalf("authenticate after JWKS rotation = %v", err)
	}
}

func TestOIDCVerifierFailsClosedOnDiscoveryTimeout(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	verifier, err := NewOIDCVerifier(OIDCConfig{
		Issuer:   "https://issuer.example",
		Audience: "broker",
		HTTPClient: &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("discovery timeout")
		})},
		Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.Authenticate(t.Context(), signOIDCTestToken(t, key, "https://issuer.example", "timeout-key", now)); err == nil {
		t.Fatal("OIDC authentication succeeded when discovery timed out")
	}
}

func testRSAJWK(key *rsa.PrivateKey, kid string) oidcJWK {
	return oidcJWK{
		KeyType: "RSA",
		Use:     "sig",
		Kid:     kid,
		Alg:     "RS256",
		N:       base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
		E:       "AQAB",
	}
}

func signOIDCTestToken(t *testing.T, key *rsa.PrivateKey, issuer, kid string, now time.Time) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": issuer,
		"sub": "alice",
		"aud": "broker",
		"exp": now.Add(time.Minute).Unix(),
		"nbf": now.Add(-time.Minute).Unix(),
	})
	token.Header["kid"] = kid
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}
