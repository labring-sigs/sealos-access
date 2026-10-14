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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v4"
	"github.com/labring-sigs/sealos-access/pkg/internalcredentials"
)

type OIDCConfig struct {
	Issuer            string
	Audience          string
	JWKSURL           string
	HTTPClient        *http.Client
	CacheTTL          time.Duration
	Clock             func() time.Time
	ClockSkew         time.Duration
	AllowedAlgorithms []string
}

type OIDCVerifier struct {
	issuer            string
	audience          string
	jwksURL           string
	httpClient        *http.Client
	cacheTTL          time.Duration
	clock             func() time.Time
	clockSkew         time.Duration
	allowedAlgorithms map[string]struct{}
	cache             oidcKeyCache
}

type oidcKeyCache struct {
	mu        sync.RWMutex
	keys      []oidcJWK
	expiresAt time.Time
}

type oidcJWK struct {
	KeyType string `json:"kty"`
	Use     string `json:"use"`
	Kid     string `json:"kid"`
	Alg     string `json:"alg"`
	N       string `json:"n"`
	E       string `json:"e"`
	Crv     string `json:"crv"`
	X       string `json:"x"`
	Y       string `json:"y"`
}

type oidcJWKS struct {
	Keys []oidcJWK `json:"keys"`
}

type oidcDiscovery struct {
	Issuer  string `json:"issuer"`
	JWKSURL string `json:"jwks_uri"`
}

func NewOIDCVerifier(config OIDCConfig) (*OIDCVerifier, error) {
	if err := internalcredentials.ValidateIssuer(config.Issuer); err != nil {
		return nil, fmt.Errorf("invalid OIDC issuer: %w", err)
	}
	if config.Audience == "" {
		return nil, errors.New("OIDC audience is required")
	}
	if config.JWKSURL != "" {
		if err := validateHTTPSURL(config.JWKSURL); err != nil {
			return nil, fmt.Errorf("invalid JWKS URL: %w", err)
		}
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{Timeout: 5 * time.Second}
	}
	if config.CacheTTL <= 0 {
		config.CacheTTL = 10 * time.Minute
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	if config.ClockSkew <= 0 {
		config.ClockSkew = 30 * time.Second
	}
	if len(config.AllowedAlgorithms) == 0 {
		config.AllowedAlgorithms = []string{"RS256"}
	}
	algorithms := make(map[string]struct{}, len(config.AllowedAlgorithms))
	for _, algorithm := range config.AllowedAlgorithms {
		switch algorithm {
		case "RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512":
			algorithms[algorithm] = struct{}{}
		default:
			return nil, fmt.Errorf("unsupported OIDC signing algorithm %q", algorithm)
		}
	}
	return &OIDCVerifier{
		issuer:            config.Issuer,
		audience:          config.Audience,
		jwksURL:           config.JWKSURL,
		httpClient:        config.HTTPClient,
		cacheTTL:          config.CacheTTL,
		clock:             config.Clock,
		clockSkew:         config.ClockSkew,
		allowedAlgorithms: algorithms,
	}, nil
}

func (v *OIDCVerifier) Authenticate(ctx context.Context, bearerToken string) (Principal, error) {
	if v == nil || bearerToken == "" {
		return Principal{}, errInvalidToken
	}
	claims := jwt.MapClaims{}
	parser := &jwt.Parser{
		ValidMethods:         sortedAlgorithms(v.allowedAlgorithms),
		UseJSONNumber:        true,
		SkipClaimsValidation: true,
	}
	token, err := parser.ParseWithClaims(bearerToken, claims, func(token *jwt.Token) (interface{}, error) {
		algorithm := token.Method.Alg()
		kid, ok := token.Header["kid"].(string)
		if !ok || kid == "" {
			return nil, errors.New("OIDC token kid is required")
		}
		return v.key(ctx, kid, algorithm)
	})
	if err != nil || token == nil || !token.Valid {
		return Principal{}, errInvalidToken
	}
	principal, err := v.principal(claims)
	if err != nil {
		return Principal{}, errInvalidToken
	}
	return principal, nil
}

func (v *OIDCVerifier) principal(claims jwt.MapClaims) (Principal, error) {
	issuer, ok := claims["iss"].(string)
	if !ok || issuer != v.issuer {
		return Principal{}, errors.New("OIDC issuer does not match configuration")
	}
	subject, ok := claims["sub"].(string)
	if !ok || internalcredentials.ValidateIdentity(issuer, subject) != nil {
		return Principal{}, errors.New("OIDC subject is invalid")
	}
	if !claimAudienceContains(claims["aud"], v.audience) {
		return Principal{}, errors.New("OIDC audience is invalid")
	}
	expiresAt, ok := claimTime(claims["exp"])
	if !ok || !expiresAt.After(v.clock().Add(-v.clockSkew)) {
		return Principal{}, errors.New("OIDC expiration is invalid")
	}
	if notBefore, exists := claims["nbf"]; exists {
		value, valid := claimTime(notBefore)
		if !valid || value.After(v.clock().Add(v.clockSkew)) {
			return Principal{}, errors.New("OIDC not-before is invalid")
		}
	}
	groups, err := claimGroups(claims["groups"])
	if err != nil {
		return Principal{}, err
	}
	return Principal{Issuer: issuer, Subject: subject, Groups: groups}, nil
}

func (v *OIDCVerifier) key(ctx context.Context, kid, algorithm string) (interface{}, error) {
	if _, ok := v.allowedAlgorithms[algorithm]; !ok {
		return nil, errors.New("OIDC algorithm is not allowed")
	}
	v.cache.mu.RLock()
	valid := v.clock().Before(v.cache.expiresAt)
	keys := append([]oidcJWK(nil), v.cache.keys...)
	v.cache.mu.RUnlock()
	if !valid {
		var err error
		keys, err = v.fetchKeys(ctx)
		if err != nil {
			return nil, err
		}
	} else if !hasKey(keys, kid, algorithm) {
		// A kid miss may be a normal signing-key rotation. Refresh once.
		var err error
		keys, err = v.fetchKeys(ctx)
		if err != nil {
			return nil, err
		}
	}
	for _, key := range keys {
		if key.Kid != kid || (key.Alg != "" && key.Alg != algorithm) || (key.Use != "" && key.Use != "sig") {
			continue
		}
		return parseJWK(key, algorithm)
	}
	return nil, errors.New("OIDC signing key not found")
}

func (v *OIDCVerifier) fetchKeys(ctx context.Context) ([]oidcJWK, error) {
	url := v.jwksURL
	if url == "" {
		discoveryURL := strings.TrimSuffix(v.issuer, "/") + "/.well-known/openid-configuration"
		var discovery oidcDiscovery
		if err := v.getJSON(ctx, discoveryURL, &discovery); err != nil {
			return nil, fmt.Errorf("fetch OIDC discovery: %w", err)
		}
		if discovery.Issuer != v.issuer {
			return nil, errors.New("OIDC discovery issuer does not match configuration")
		}
		url = discovery.JWKSURL
	}
	if err := validateHTTPSURL(url); err != nil {
		return nil, err
	}
	var document oidcJWKS
	if err := v.getJSON(ctx, url, &document); err != nil {
		return nil, fmt.Errorf("fetch OIDC JWKS: %w", err)
	}
	if len(document.Keys) == 0 {
		return nil, errors.New("OIDC JWKS contains no keys")
	}
	v.cache.mu.Lock()
	v.cache.keys = append([]oidcJWK(nil), document.Keys...)
	v.cache.expiresAt = v.clock().Add(v.cacheTTL)
	v.cache.mu.Unlock()
	return document.Keys, nil
}

func (v *OIDCVerifier) getJSON(ctx context.Context, url string, target interface{}) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	response, err := v.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("OIDC endpoint returned HTTP %d", response.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return nil
}

func parseJWK(key oidcJWK, algorithm string) (interface{}, error) {
	switch key.KeyType {
	case "RSA":
		n, err := decodeBase64URL(key.N)
		if err != nil {
			return nil, err
		}
		e, err := decodeBase64URL(key.E)
		if err != nil || len(e) == 0 || len(e) > 4 {
			return nil, errors.New("invalid RSA exponent")
		}
		exponent := 0
		for _, value := range e {
			exponent = exponent<<8 | int(value)
		}
		if exponent < 2 {
			return nil, errors.New("invalid RSA exponent")
		}
		if !strings.HasPrefix(algorithm, "RS") && !strings.HasPrefix(algorithm, "PS") {
			return nil, errors.New("JWK type does not match OIDC algorithm")
		}
		return &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: exponent}, nil
	case "EC":
		if !strings.HasPrefix(algorithm, "ES") {
			return nil, errors.New("JWK type does not match OIDC algorithm")
		}
		curve, ok := namedCurve(key.Crv)
		if !ok {
			return nil, errors.New("unsupported EC curve")
		}
		x, err := decodeBase64URL(key.X)
		if err != nil {
			return nil, err
		}
		y, err := decodeBase64URL(key.Y)
		if err != nil {
			return nil, err
		}
		publicKey := &ecdsa.PublicKey{Curve: curve, X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
		if !curve.IsOnCurve(publicKey.X, publicKey.Y) {
			return nil, errors.New("invalid EC public key")
		}
		return publicKey, nil
	default:
		return nil, errors.New("unsupported OIDC JWK type")
	}
}

func validateHTTPSURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || strings.ContainsAny(value, "\r\n") {
		return errors.New("URL must use HTTPS")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("URL must not contain userinfo, query, or fragment")
	}
	return nil
}

func decodeBase64URL(value string) ([]byte, error) {
	if value == "" {
		return nil, errors.New("empty base64url value")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		decoded, err = base64.URLEncoding.DecodeString(value)
	}
	return decoded, err
}

func hasKey(keys []oidcJWK, kid, algorithm string) bool {
	for _, key := range keys {
		if key.Kid == kid && (key.Alg == "" || key.Alg == algorithm) && (key.Use == "" || key.Use == "sig") {
			return true
		}
	}
	return false
}

func sortedAlgorithms(algorithms map[string]struct{}) []string {
	result := make([]string, 0, len(algorithms))
	for algorithm := range algorithms {
		result = append(result, algorithm)
	}
	return result
}

func namedCurve(name string) (elliptic.Curve, bool) {
	switch name {
	case "P-256":
		return elliptic.P256(), true
	case "P-384":
		return elliptic.P384(), true
	case "P-521":
		return elliptic.P521(), true
	default:
		return nil, false
	}
}

func claimAudienceContains(value interface{}, expected string) bool {
	switch audience := value.(type) {
	case string:
		return audience == expected
	case []interface{}:
		for _, candidate := range audience {
			if value, ok := candidate.(string); ok && value == expected {
				return true
			}
		}
	}
	return false
}

func claimTime(value interface{}) (time.Time, bool) {
	switch number := value.(type) {
	case json.Number:
		seconds, err := number.Int64()
		return time.Unix(seconds, 0), err == nil
	case float64:
		if number != float64(int64(number)) {
			return time.Time{}, false
		}
		return time.Unix(int64(number), 0), true
	default:
		return time.Time{}, false
	}
}

func claimGroups(value interface{}) ([]string, error) {
	if value == nil {
		return nil, nil
	}
	switch groups := value.(type) {
	case string:
		if groups == "" {
			return nil, errors.New("OIDC groups claim contains an empty group")
		}
		return []string{groups}, nil
	case []interface{}:
		result := make([]string, 0, len(groups))
		for _, group := range groups {
			value, ok := group.(string)
			if !ok || value == "" {
				return nil, errors.New("OIDC groups claim is invalid")
			}
			result = append(result, value)
		}
		return result, nil
	default:
		return nil, errors.New("OIDC groups claim is invalid")
	}
}
