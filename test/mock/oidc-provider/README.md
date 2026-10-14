# Test OIDC Provider

This directory contains a small OIDC provider for integration tests. It
implements only the flows needed by the internal credential clients:

- OpenID Connect discovery and RSA JWKS
- Authorization Code with S256 PKCE
- In-memory users and one-time authorization codes
- Signed access and ID tokens with configurable test claims
- A Testcontainers wrapper for running the provider over a real mapped port

The provider is test-only. It generates a new RSA key on every process start,
does not persist tokens or refresh tokens, and must not be deployed outside a
test environment.

Run the in-memory HTTP tests without Docker with:

```sh
go test ./test/mock/oidc-provider/... -run 'Test(DiscoveryAndJWKS|AuthorizationCodePKCE|AuthorizationCodeConsumesCodeOnPKCEFailure|MintToken)$'
```

Run all tests, including the container round trip, with:

```sh
go test ./test/mock/oidc-provider/...
```

`TestOIDCContainerRoundTrip` additionally requires a reachable Docker daemon
and permission to pull the builder image used by `test/mock/oidc-provider/Dockerfile`.

Use `oidc.New` with `httptest.NewServer` for fast tests. Use
`oidccontainer.Start` when the consumer must exercise a real container network
boundary.
