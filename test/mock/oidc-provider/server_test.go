package oidc

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestDiscoveryAndJWKS(t *testing.T) {
	provider, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	server := newTestServer(t, provider)

	discovery := getJSON(t, server.URL+"/.well-known/openid-configuration")
	if discovery["issuer"] != server.URL {
		t.Fatalf("issuer = %v, want %s", discovery["issuer"], server.URL)
	}
	if discovery["authorization_endpoint"] != server.URL+"/authorize" {
		t.Fatalf("authorization endpoint = %v", discovery["authorization_endpoint"])
	}
	if discovery["token_endpoint"] != server.URL+"/token" {
		t.Fatalf("token endpoint = %v", discovery["token_endpoint"])
	}
	if discovery["jwks_uri"] != server.URL+"/jwks.json" {
		t.Fatalf("JWKS URI = %v", discovery["jwks_uri"])
	}

	jwks := getJSON(t, server.URL+"/jwks.json")
	keys, ok := jwks["keys"].([]any)
	if !ok || len(keys) != 1 {
		t.Fatalf("JWKS keys = %#v", jwks["keys"])
	}
	key, ok := keys[0].(map[string]any)
	if !ok {
		t.Fatalf("JWKS key type = %T", keys[0])
	}
	if key["kid"] != provider.KeyID() || key["alg"] != "RS256" || key["kty"] != "RSA" {
		t.Fatalf("unexpected JWKS key = %#v", key)
	}
}

func TestAuthorizationCodePKCE(t *testing.T) {
	provider, err := New(Config{
		Users:   []User{{Subject: "alice", Email: "alice@example.invalid", Groups: []string{"ops"}}},
		Clients: []Client{{ID: "internal-kc", RedirectURIs: []string{"http://127.0.0.1/callback"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	server := newTestServer(t, provider)

	verifier := strings.Repeat("v", 43)
	challenge := pkceChallenge(verifier)
	callback := "http://127.0.0.1/callback"
	authorizeURL := server.URL + "/authorize?" + url.Values{
		"client_id":             []string{"internal-kc"},
		"redirect_uri":          []string{callback},
		"response_type":         []string{"code"},
		"scope":                 []string{"openid profile email groups"},
		"state":                 []string{"state-123"},
		"nonce":                 []string{"nonce-123"},
		"login_hint":            []string{"alice"},
		"code_challenge":        []string{challenge},
		"code_challenge_method": []string{"S256"},
	}.Encode()
	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	response, err := client.Get(authorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusFound {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("authorize status = %d, body = %s", response.StatusCode, body)
	}
	location, err := url.Parse(response.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if location.Query().Get("state") != "state-123" || location.Query().Get("code") == "" {
		t.Fatalf("authorization response = %s", location)
	}

	token := postToken(t, server.URL+"/token", url.Values{
		"grant_type":    []string{"authorization_code"},
		"code":          []string{location.Query().Get("code")},
		"client_id":     []string{"internal-kc"},
		"redirect_uri":  []string{callback},
		"code_verifier": []string{verifier},
	})
	if token.TokenType != "Bearer" || token.AccessToken == "" || token.IDToken == "" {
		t.Fatalf("unexpected token response = %#v", token)
	}
	claims := jwtClaims(t, token.IDToken)
	if claims["iss"] != server.URL || claims["sub"] != "alice" || claims["aud"] != "internal-kc" || claims["nonce"] != "nonce-123" {
		t.Fatalf("ID token claims = %#v", claims)
	}
	if !verifyJWT(t, provider.PublicKey(), token.IDToken) {
		t.Fatal("ID token signature did not verify")
	}

	status, body := postTokenStatus(t, server.URL+"/token", url.Values{
		"grant_type":    []string{"authorization_code"},
		"code":          []string{location.Query().Get("code")},
		"client_id":     []string{"internal-kc"},
		"redirect_uri":  []string{callback},
		"code_verifier": []string{verifier},
	})
	if status != http.StatusBadRequest || !strings.Contains(string(body), "invalid_grant") {
		t.Fatalf("reusing authorization code status=%d body=%s", status, body)
	}
}

func TestAuthorizationCodeConsumesCodeOnPKCEFailure(t *testing.T) {
	provider, err := New(Config{Clients: []Client{{ID: "client", RedirectURIs: []string{"http://127.0.0.1/callback"}}}})
	if err != nil {
		t.Fatal(err)
	}
	server := newTestServer(t, provider)

	verifier := strings.Repeat("v", 43)
	query := url.Values{
		"client_id":             []string{"client"},
		"redirect_uri":          []string{"http://127.0.0.1/callback"},
		"response_type":         []string{"code"},
		"scope":                 []string{"openid"},
		"code_challenge":        []string{pkceChallenge(verifier)},
		"code_challenge_method": []string{"S256"},
	}
	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	response, err := client.Get(server.URL + "/authorize?" + query.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	location, err := url.Parse(response.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	code := location.Query().Get("code")
	if code == "" {
		t.Fatal("authorization response did not contain a code")
	}
	status, _ := postTokenStatus(t, server.URL+"/token", url.Values{
		"grant_type":    []string{"authorization_code"},
		"code":          []string{code},
		"client_id":     []string{"client"},
		"redirect_uri":  []string{"http://127.0.0.1/callback"},
		"code_verifier": []string{"wrong-verifier"},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("wrong verifier status = %d", status)
	}
	status, _ = postTokenStatus(t, server.URL+"/token", url.Values{
		"grant_type":    []string{"authorization_code"},
		"code":          []string{code},
		"client_id":     []string{"client"},
		"redirect_uri":  []string{"http://127.0.0.1/callback"},
		"code_verifier": []string{verifier},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("replayed code after PKCE failure status = %d", status)
	}
}

func TestMintToken(t *testing.T) {
	provider, err := New(Config{Users: []User{{Subject: "alice", Claims: map[string]any{"role": "operator", "iss": "attacker"}}}})
	if err != nil {
		t.Fatal(err)
	}
	token, err := provider.MintToken("http://issuer.test", "alice", "credential-broker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	claims := jwtClaims(t, token)
	if claims["iss"] != "http://issuer.test" || claims["sub"] != "alice" || claims["aud"] != "credential-broker" || claims["role"] != "operator" {
		t.Fatalf("minted token claims = %#v", claims)
	}
	if claims["iss"] == "attacker" {
		t.Fatal("user claims overwrote reserved issuer claim")
	}
	if !verifyJWT(t, provider.PublicKey(), token) {
		t.Fatal("minted token signature did not verify")
	}
}

func pkceChallenge(verifier string) string {
	digest := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func getJSON(t *testing.T, endpoint string) map[string]any {
	t.Helper()
	response, err := http.Get(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d", endpoint, response.StatusCode)
	}
	var value map[string]any
	if err := json.NewDecoder(response.Body).Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func postToken(t *testing.T, endpoint string, form url.Values) tokenResponse {
	t.Helper()
	response, err := http.PostForm(endpoint, form)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("POST %s status = %d body = %s", endpoint, response.StatusCode, body)
	}
	var value tokenResponse
	if err := json.NewDecoder(response.Body).Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func postTokenStatus(t *testing.T, endpoint string, form url.Values) (int, []byte) {
	t.Helper()
	response, err := http.PostForm(endpoint, form)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, body
}

func jwtClaims(t *testing.T, token string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT has %d parts", len(parts))
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(data, &claims); err != nil {
		t.Fatal(err)
	}
	return claims
}

func verifyJWT(t *testing.T, key *rsa.PublicKey, token string) bool {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	return rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature) == nil
}

func newTestServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	return server
}
