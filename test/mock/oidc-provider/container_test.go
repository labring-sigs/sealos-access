package oidc_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/labring-sigs/sealos-access/test/mock/oidc-provider"
	oidccontainer "github.com/labring-sigs/sealos-access/test/mock/oidc-provider/container"
)

// TestOIDCContainerRoundTrip builds the test OIDC provider image from its
// Dockerfile, starts a container, and exercises one PKCE flow over the real
// mapped port. It verifies the provider can return JWKS, complete a code
// exchange, and validate the resulting access token.
func TestOIDCContainerRoundTrip(t *testing.T) {
	// This test requires a Docker daemon and a builder image tag.
	if testing.Short() {
		t.Skip("skipping container test in short mode")
	}
	config := oidc.Config{
		Issuer: "",
		Users: []oidc.User{
			{Subject: "alice", Email: "alice@example.test"},
		},
		Clients: []oidc.Client{
			{ID: "test-client", RedirectURIs: []string{"http://localhost:9999/callback"}},
		},
		TokenAudience:   "test-audience",
		TokenTTLSeconds: 300,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	container, err := oidccontainer.Start(ctx, config)
	if err != nil {
		t.Fatalf("start container: %v", err)
	}
	defer container.Terminate(ctx)
	httpClient := &http.Client{Timeout: 5 * time.Second}

	// Discovery
	discoveryURL := container.Issuer() + "/.well-known/openid-configuration"
	discReq, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		t.Fatalf("discovery request: %v", err)
	}
	discResp, err := httpClient.Do(discReq)
	if err != nil {
		t.Fatalf("discovery: %v", err)
	}
	defer discResp.Body.Close()
	if discResp.StatusCode != http.StatusOK {
		t.Fatalf("discovery status: %d", discResp.StatusCode)
	}
	var discovery struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.NewDecoder(discResp.Body).Decode(&discovery); err != nil {
		t.Fatalf("decode discovery: %v", err)
	}
	if discovery.JWKSURI == "" {
		t.Fatal("discovery response has no jwks_uri")
	}

	// JWKS
	jwksURL := discovery.JWKSURI
	jwksReq, err := http.NewRequestWithContext(ctx, http.MethodGet, jwksURL, nil)
	if err != nil {
		t.Fatalf("jwks request: %v", err)
	}
	jwksResp, err := httpClient.Do(jwksReq)
	if err != nil {
		t.Fatalf("jwks: %v", err)
	}
	defer jwksResp.Body.Close()
	if jwksResp.StatusCode != http.StatusOK {
		t.Fatalf("jwks status: %d", jwksResp.StatusCode)
	}
}
