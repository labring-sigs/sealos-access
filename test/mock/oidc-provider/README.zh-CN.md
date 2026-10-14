# 测试用 OIDC Provider

此目录提供一个仅用于集成测试的轻量 OIDC Provider，覆盖内部凭证客户端
所需的最小流程：

- OpenID Connect Discovery 和 RSA JWKS
- Authorization Code + S256 PKCE
- 只保存在内存中的测试用户和一次性授权码
- 带可配置测试 claims 的 access token 和 ID token
- 通过 Testcontainers 在真实映射端口中运行 Provider

该 Provider 仅限测试使用。每次进程启动都会生成新的 RSA 密钥，不持久化
token 或 refresh token，不能部署到测试环境之外。

不依赖 Docker 时运行内存 HTTP 单测：

```sh
go test ./test/mock/oidc-provider/... -run 'Test(DiscoveryAndJWKS|AuthorizationCodePKCE|AuthorizationCodeConsumesCodeOnPKCEFailure|MintToken)$'
```

运行包含容器回路测试的全部测试：

```sh
go test ./test/mock/oidc-provider/...
```

`TestOIDCContainerRoundTrip` 还要求 Docker daemon 可访问，并且能够拉取
`test/mock/oidc-provider/Dockerfile` 使用的构建镜像。

使用 `oidc.New` 配合 `httptest.NewServer` 可运行快速测试；当被测程序需要
验证真实容器网络边界时，使用 `oidccontainer.Start`。
