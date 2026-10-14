# Internal User 凭据：Keycloak OIDC 接入（飞书为上游）

状态：在 internal user 凭据体系与飞书之间引入 Keycloak 的规划清单。凭据体系
信任 Keycloak，Keycloak 负责把身份联合到飞书。当前仓库没有 Keycloak 或
飞书相关实现。

英文主文档：
[internal-user-credentials-keycloak-feishu-onboarding.md](internal-user-credentials-keycloak-feishu-onboarding.md)。

## 1. 目标架构

```text
用户 / CLI
   |  OIDC authorization code + PKCE
   v
Keycloak realm  ---- identity brokering ---->  Feishu（上游 IdP）
   ^
   |  JWKS + issuer（RS256 JWT access token）
   |
internal-credential-broker  <--- Bearer JWT --- CLI
```

凭据体系不再直接访问飞书，只信任一个稳定的 Keycloak realm issuer；上游身份源
变化时不需要改 Broker、Controller 或 `InternalUser` 数据。

## 2. Keycloak 解决了直连飞书的哪些阻塞点

| 直连飞书的阻塞点 | Keycloak 如何解决 |
| --- | --- |
| 飞书 access_token 通常不透明，Broker 要求可用 JWKS 验签的 JWT | Keycloak 的 access token 是带 `kid` 的 RS256 JWT，并暴露 realm JWKS |
| 飞书没有标准 OIDC discovery/JWKS | Keycloak 必有 discovery 与 `/protocol/openid-connect/certs` |
| issuer/subject 不可控或不稳定 | issuer 是你的 realm URL，`sub` 是稳定的 Keycloak 用户 ID |
| 无法自定义 `aud`/`groups` | 通过 client scope mapper 注入 audience 与 `groups` |
| 上游 IdP 变化会破坏身份 | 只改 Keycloak 的 IdP，`InternalUser` 身份不变 |

Keycloak **不解决**的部分：

- elevated 审批集成（Keycloak 不是审批系统）；
- Broker 源地址策略（仍需 `X-Trusted-Client-IP`）；
- Broker 的网络 egress / CA 信任（只是改成指向 Keycloak）；
- CLI 回调端口问题（Keycloak 的通配 redirect URI 有帮助，见 3.3）。

## 3. 体系要求（不变的部分）

### 3.1 Token

`./pkg/broker/oidc.go` 把 Bearer token 当作签名 JWT，用 JWKS
验签，并要求：

- 带 `kid` 头，算法受支持（默认 `RS256`）；
- `iss` 与配置的 `oidc-issuer` 完全一致；
- `aud` 包含配置的 `oidc-audience`（精确字符串匹配）；
- `sub`、`exp` 有效，可选 `nbf`；
- 可选 `groups`（字符串或字符串数组）。

CLI（`./cmd/sealos-access/main.go`）发送的是 OIDC
**access_token**。Keycloak 的 access token 是签名 JWT，因此天然满足。

### 3.2 Issuer 与端点

Keycloak realm 的标准形式：

- issuer：`https://<keycloak-host>/realms/<realm>`
- discovery：`<issuer>/.well-known/openid-configuration`
- JWKS：`<issuer>/protocol/openid-connect/certs`

discovery 文档里的 `issuer` 必须等于配置的 `oidc-issuer`。该值由 Keycloak 的
hostname / frontend URL 配置决定，接线 Broker 前先固定下来。

### 3.3 CLI 回调 URI

CLI 当前监听 `http://127.0.0.1:<随机端口>/callback`。Keycloak 对 public
client 支持通配 redirect URI（例如 `http://127.0.0.1/*`），可覆盖随机端口；
请在你的 Keycloak 版本上确认，否则给 CLI 加固定回调端口。

### 3.4 Subject 稳定性

Broker 用 token 的 `sub` 作为用户身份；`InternalUser.spec.identity.subject`
必须等于 Keycloak 的 `sub`，即 Keycloak 用户 UUID。请把 realm 用户与 ID 当作
持久数据：

- 备份 realm 与 Keycloak 数据库；
- 不要通过删除/重建用户来“修”属性（UUID 会变）；
- 迁移 realm 时保留用户 ID。

若需要由上游身份推导的确定性 subject，可配置 Keycloak mapper 写入固定
subject，或把上游 ID 存为用户属性并映射到自定义 claim。

## 4. Keycloak 侧配置

### 4.1 Realm、hostname、TLS

1. 创建独立 realm（例如 `sealos`）。
2. 设置稳定的 frontend URL / `KC_HOSTNAME`，使 issuer 精确为
   `https://<keycloak-host>/realms/sealos`，且 CLI 与 Broker 都能访问；
   在代理后面要开启对应的 proxy header 配置。
3. 用 Broker 能信任的 CA 终止 TLS（该 CA 要挂进 Broker；镜像是 `scratch`，
   见 5.2）。

### 4.2 给 CLI 的 client

- Public client，standard flow，**PKCE S256**，无 client secret。
- Redirect URI：`http://127.0.0.1/*`（确认通配支持）或 CLI 的固定端口。
- CLI 传 `--client-id <client>`；除非把它映射进 token audience，Broker 不需要
  client ID。

### 4.3 Audience mapper

Broker 对 `aud` 做精确字符串比较。添加一个带 **Audience** mapper 的 client
scope，把 Broker 的 audience（例如 `sealos-internal-credential-broker`）注入
access token，并把 Broker 的 `oidc-audience` 设为同一个值。

### 4.4 Groups mapper（可选，用于 OIDC 管理员路径）

Broker 的 `groups` 是精确匹配（`claimGroups` 不做路径归一化）。Keycloak 的
**Group Membership** mapper 输出的是带前导斜杠的完整组路径，例如
`/sealos.internal.admin`。

二选一：

- 把 Broker 的管理员组设为映射后的精确值（`/sealos.internal.admin`）；或
- 用 mapper 输出不带斜杠的 `sealos.internal.admin`。

Broker HTTP API 的 OIDC 管理员操作（包括 elevated 审批提交）都需要 groups；该流程不使用
mTLS。

### 4.5 签名与 token 生命周期

- 保持 RS256（默认），token 头带 `kid`，JWKS 有匹配公钥；密钥轮换会自动更新
  JWKS。
- access token 生命周期保持较短（5–15 分钟）即可：CLI 拿到后立即用于 Broker
  请求。不要用它当凭据有效期；凭据 TTL 由 Broker 的 profile 控制。

### 4.6 飞书 Identity Provider

在 realm 中创建 Identity Provider 指向飞书：

- 飞书支持 OIDC：使用通用 **OpenID Connect v1.0** provider（authorization /
  token / userinfo / JWKS 端点、client ID/secret、scopes、issuer）。
- 飞书支持 SAML：使用 **SAML v2.0** provider。
- 飞书只有 OAuth 2.0 且 token 不透明：Keycloak 没有内置飞书 provider，需要
  实现 Keycloak `IdentityProvider` SPI（或使用社区维护的飞书 provider 插件，
  或在 Keycloak 前再加一层 token 交换桥接）。

配置 attribute mapper，至少导入邮箱与稳定的用户标识；配置首次登录
first-broker-login 流程，让用户首次登录时创建并关联。

飞书侧要配置 Keycloak broker 的回调地址：

```text
https://<keycloak-host>/realms/<realm>/broker/<idp-alias>/endpoint
```

## 5. 凭据体系侧配置

先备份即将修改的对象：

```sh
mkdir -p /tmp/keycloak-onboarding-backup
kubectl -n access-broker-system get cm internal-credential-broker-config -o yaml \
  > /tmp/keycloak-onboarding-backup/broker-cm.yaml
kubectl -n access-broker-system get deploy internal-credential-broker -o yaml \
  > /tmp/keycloak-onboarding-backup/broker-deploy.yaml
kubectl -n access-broker-system get netpol internal-credential-broker-controlled-egress -o yaml \
  > /tmp/keycloak-onboarding-backup/broker-netpol.yaml
```

### 5.1 Broker

Broker 只接受严格、带版本的多集群 YAML 配置。编辑 `broker.yaml` 的 `oidc` 部分，
并在 `targets` 列表中保留所有 Kubernetes 集群；每个集群重复一个 target 条目：

```yaml
version: 1
oidc:
  issuer: https://<keycloak-host>/realms/<realm>
  audience: <broker-audience-added-by-the-mapper>
  jwksURL: ""
  caFile: /etc/broker/oidc-ca/ca.crt
  serverName: <keycloak-host>
targets:
- id: <target-id>
  apiServer: https://<target-api>
  clusterServer: https://<target-public-api>
  caFile: /etc/broker/targets/<target-id>.ca.crt
  credentialFile: /etc/broker/targets/<target-id>.token
  kubernetesAudience: https://kubernetes.default.svc.cluster.local
  userNamespace: access-system
  brokerNamespace: access-broker-system
  clusterName: <target-id>
  enabled: true
```

保持 `BROKER_CONFIG_FILE` 为 `/etc/broker/config/broker.yaml`。每个 target 的 CA 和
bearer token 文件应按
[`internal-user-credentials-broker-config.md`](internal-user-credentials-broker-config.md)
中的 per-target Secret 挂载。`jwksURL` 留空即使用 discovery；若 discovery 不可用或
经过代理，再显式设置 Keycloak JWKS endpoint。然后重启 Broker：

```sh
kubectl -n access-broker-system rollout restart deploy/internal-credential-broker
kubectl -n access-broker-system rollout status deploy/internal-credential-broker
```

### 5.2 CA bundle

`./Dockerfile.broker` 是 `FROM scratch`，没有系统 CA。把
Keycloak 的 CA（或 CA bundle）挂载到 `broker.yaml` 的 `caFile` 路径，不要依赖系统根证书。

### 5.3 NetworkPolicy 与源地址策略

- Broker egress 只需放行到 Keycloak：Keycloak 在集群内就用 pod selector，
  在外部就放行其 TCP 443。飞书的 egress 现在由 Keycloak 承担。
- Broker 认证请求仍要求 peer 在 `trusted-proxy-cidrs` 内，并携带且只携带一个
  `X-Trusted-Client-IP` 头，其值在 `allowed-client-cidrs` 内；生产由 Higress
  注入。不要把 Broker 直接暴露给客户端。

### 5.4 Controller issuer 白名单

```sh
kubectl -n access-controller-system patch cm internal-user-controller-config \
  --type=merge \
  -p '{"data":{"oidc-issuers":"https://<keycloak-host>/realms/<realm>,https://test-oidc-provider.access-mock.svc:8443"}}'

kubectl -n access-controller-system rollout restart deploy/internal-user-controller
kubectl -n access-controller-system rollout status deploy/internal-user-controller
```

只有迁移确实需要时才在 Controller 白名单中保留旧 issuer；Broker 校验的是严格 YAML
中配置的 issuer。

### 5.5 创建测试身份

```sh
./bin/sealos-access-amd64 admin create \
  --kubeconfig "$KUBECONFIG" \
  --issuer "https://<keycloak-host>/realms/<realm>" \
  --subject "<keycloak-sub-uuid>"

kubectl get internalusers -o wide
kubectl wait --for=jsonpath='{.status.phase}'=Active \
  internaluser/<derived-iu-name> --timeout=120s
```

名字派生规则：`iu-<sha256(issuer + NUL + subject) 前 24 位 hex>`。

## 6. Elevated 审批（Keycloak 不影响这部分）

Keycloak 只负责认证，不做审批。elevated 流程仍需要审批源携带服务端精确配置管理员 group
的 OIDC token，把权威记录提交到 `POST /v1/clusters/{clusterID}/internal/credentials/approve`：

1. 为审批系统实现 `approvaladapter.ApprovalSource`（飞书审批 API：实例查询 +
   邮箱解析）。
2. 增加事件入口（回调或轮询），并通过 Ingress/route 暴露 adapter 给该审批系统。
3. 接入带 OIDC bearer token 的 `BrokerHTTPClient` 与通知渠道（SMTP `EmailNotifier` 或需要
   自行实现的飞书消息通知）。
4. 审批记录不含敏感信息：只提交 approval ID、目标身份、TTL 和 reason；
   Broker 返回一次性 reference。

adapter 实现前，使用 `sealos-access admin approve` 路径做 acceptance。命令和授权
细节见[Sealos Access CLI](sealos-access-cli.zh-CN.md)说明。

## 7. 需要完成的工作

| 项目 | 负责方 | 说明 |
| --- | --- | --- |
| Keycloak 部署、DB、TLS、备份 | 平台 | 稳定 issuer URL 与 CA |
| 飞书 IdP 可行性（OIDC/SAML/自定义 SPI） | 平台 + 飞书管理员 | 接线 mapper 前先定方案 |
| Realm、CLI client、PKCE、redirect URI | 平台 | 通配或固定端口 |
| Audience mapper | 平台 | 必须与 Broker `oidc-audience` 精确一致 |
| Groups mapper（可选） | 平台 | 与 Broker 管理员组精确一致 |
| Broker issuer/audience/CA/egress 更新 | 集群运维 | 含 netpol 与重启 |
| Controller 白名单更新 | 集群运维 | 逗号分隔多个 issuer |
| `InternalUser` subject = Keycloak `sub` | 集群运维 | UUID 稳定性与备份 |
| 审批 adapter | 应用团队 | Keycloak 不解决 |

## 8. 切换顺序

1. 部署 Keycloak，固定稳定 issuer 与 TLS；验证 discovery 与 JWKS。
2. 配置飞书 IdP 并完成一次登录；解码 Keycloak access token，检查 `iss`、
   `aud`、`sub`、`groups`。
3. 添加 audience（及可选 groups）mapper；再次检查 token。
4. 备份 Broker 对象，更新 issuer/audience/CA/egress 并重启；确认 Broker 能拉
   到 JWKS。
5. 把 Keycloak issuer 加入 Controller 白名单并重启。
6. 为某个 Keycloak 用户创建 `InternalUser`，跑 base 流程。
7. 跑 elevated 流程（adapter，或首次 acceptance 用 `sealos-access admin approve`）。

## 9. 验证清单

- [ ] Discovery 的 `issuer` 与配置的 `oidc-issuer` 一致。
- [ ] `jwks_uri` 为 `<issuer>/protocol/openid-connect/certs`。
- [ ] access token 头带 `kid`；claims 含 `iss`、`aud`、`sub`、`exp`；`aud`
      包含配置的 audience。
- [ ] 若使用 groups，其值与 Broker 管理员组精确一致。
- [ ] Broker 审计/日志出现成功的 `issue_base`。
- [ ] `InternalUser` 为 `Active` 且名字符合派生规则。
- [ ] base kubeconfig：可读 pods/services/deployments/replicasets，禁止创建
      `InternalUser`/`CredentialLease`。
- [ ] elevated kubeconfig：允许 events create/patch，禁止写
      deployment/SA/RBAC。
- [ ] 同一 approval reference 重放返回 HTTP 409。

## 10. 回滚

恢复 Broker 的 ConfigMap、Deployment、NetworkPolicy 备份，删除临时 CA
ConfigMap/Secret，并重启 Broker。恢复后先检查对象内容和 Broker 健康状态，再恢复
流量。注意 `kubectl apply` 不会自动删除 strategic patch 添加的字段，回滚后必须检查
线上对象。

## 11. 相关文档

- [系统概览](internal-user-credentials-overview.zh-CN.md)
- [实现架构](internal-user-credentials-architecture.zh-CN.md)
- [E2E 测试 TODO](internal-user-credentials-e2e-todo.zh-CN.md)
- [审批适配器](internal-user-approval-adapter.zh-CN.md)
- [Sealos Access CLI](sealos-access-cli.zh-CN.md)
