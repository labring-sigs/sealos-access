# Internal User 凭据 Broker 配置说明

本文说明如何配置和部署 Internal User 凭据 Broker。当前文档对应直接凭据架构：
一个中央 Broker 通过 OIDC 认证用户，并可以为一个或多个目标 Kubernetes 集群签发
凭据。

Broker 强制使用严格、带版本的多集群 YAML 配置，不提供单目标回退或兼容模式。

## 部署模型

中央 Broker 运行在自己的 `access-broker-system` 集群/命名空间中。每个目标
集群都使用独立的 Kubernetes API 地址、CA 和最小权限 ServiceAccount bearer token。

```text
CLI / 审批适配器
          |
          | HTTPS + OIDC bearer token
          v
  中央 Broker 集群
          |
          +--> 目标集群 A API Server
          |
          +--> 目标集群 B API Server
          |
          `--> OIDC issuer Discovery/JWKS
```

Broker 不创建 `InternalUser`。身份创建仍由管理员操作完成。Broker 只读取
`InternalUser`，管理目标 `CredentialLease` 状态，执行 `TokenRequest` 和
`TokenReview`，并返回包含短期 token 的 kubeconfig。

## 必需配置

将 `BROKER_CONFIG_FILE` 设置为挂载的 `broker.yaml` 路径，通常是
`/etc/broker/config/broker.yaml`。文件包含一份 OIDC 配置和每个目标集群的明确配置。
每个启用的目标都会创建独立的 REST client 和 bearer-token 文件。

配置文件缺失、无效或没有启用目标时，Broker 会在启动阶段退出。目标 API、audience、
namespace、CA 文件和凭据始终从该 YAML 读取。

## 多集群 YAML

YAML 必须只包含一个文档，启用严格未知字段校验，目前要求 `version: 1`。配置错误会
让 Broker 启动失败，不会带着不完整的目标列表启动。

下面是两个目标的示例：

```yaml
version: 1

oidc:
  issuer: https://login.example.internal
  audience: sealos-internal-credential-broker
  # 为空时使用 issuer/.well-known/openid-configuration。
  jwksURL: ""
  # issuer 使用镜像默认信任库之外的私有 CA 时必须提供。
  caFile: /etc/broker/oidc-ca/ca.crt
  # OIDC TLS 的可选 SNI/server-name 覆盖值。
  serverName: ""

targets:
- id: public-cluster-a
  # Broker 可以访问的地址，可以是 Broker 网络内的私网地址。
  apiServer: https://api.cluster-a.example.internal
  # 写入返回给用户的 kubeconfig 的地址。
  clusterServer: https://api.cluster-a.example
  caFile: /etc/broker/targets/public-cluster-a.ca.crt
  credentialFile: /etc/broker/targets/public-cluster-a.token
  kubernetesAudience: https://kubernetes.default.svc.cluster.local
  userNamespace: access-system
  brokerNamespace: access-broker-system
  clusterName: public-cluster-a
  enabled: true

- id: public-cluster-b
  apiServer: https://api.cluster-b.example.internal
  clusterServer: https://api.cluster-b.example
  caFile: /etc/broker/targets/public-cluster-b.ca.crt
  credentialFile: /etc/broker/targets/public-cluster-b.token
  kubernetesAudience: https://kubernetes.default.svc.cluster.local
  userNamespace: access-system
  brokerNamespace: access-broker-system
  clusterName: public-cluster-b
  enabled: true
```

### OIDC 字段

| 字段 | 必填 | 含义 |
| --- | --- | --- |
| `issuer` | 是 | 精确的 HTTPS issuer URL。token 的 `iss` claim 以及使用 Discovery 时 Discovery 返回的 `issuer` 都必须匹配。 |
| `audience` | 是 | Broker 接受的 OIDC audience，必须与 CLI/审批流程请求的 audience 匹配。 |
| `jwksURL` | 否 | HTTPS JWKS 地址。为空时，Broker 请求 `/.well-known/openid-configuration` 并使用其中的 `jwks_uri`。 |
| `caFile` | 否 | OIDC TLS 校验使用的挂载 PEM CA 文件；设置后路径必须存在。 |
| `serverName` | 否 | OIDC TLS 请求使用的 server name/SNI 覆盖值。 |

Broker 使用有界超时的 HTTP client；Discovery 或 JWKS 无法访问时会 fail closed。签名
密钥缓存十分钟；遇到未知 `kid` 时会刷新一次，可支持正常的签名密钥轮换。

### Target 字段

| 字段 | 必填 | 含义 |
| --- | --- | --- |
| `id` | 是 | 不可变的路由 ID，必须是小写 DNS-like 文本，并在文件内唯一。客户端只能提交该 ID，不能提交目标 URL 或凭据。 |
| `apiServer` | 是 | Broker 使用的 HTTPS Kubernetes API 地址，不一定是返回给用户的地址。 |
| `clusterServer` | 是 | 写入返回 kubeconfig 的 HTTPS Kubernetes API 地址，必须从用户网络可达。 |
| `caFile` | 是 | `apiServer` 使用的挂载 PEM CA 文件。 |
| `credentialFile` | 是 | 只包含目标 ServiceAccount bearer token 的挂载文件。 |
| `kubernetesAudience` | 是 | 目标 `TokenRequest` 使用并由 Broker 校验的 audience。 |
| `userNamespace` | 是 | 目标 `InternalUser` Controller 管理的 ServiceAccount 和 bound Secret 所在 namespace。 |
| `brokerNamespace` | 是 | 目标 `CredentialLease` 所在 namespace。 |
| `clusterName` | 否 | 人类可读的集群名称，默认值为 `id`；它不是客户端选择 endpoint 的依据。 |
| `enabled` | 否（默认 `false`） | 是否启用目标，至少一个目标必须启用；活动目标必须显式设置为 `true`。 |

所有 URL 必须是没有 query 或 fragment 的绝对 HTTPS URL。重复 ID、缺少文件、未知字段、
不支持的版本以及没有启用目标的配置都会在启动时被拒绝。即使 `enabled: false`，该条目
仍会被校验；删除过期条目，或在部署前提供它引用的文件。

## Secret 与文件布局

YAML 可以放在 ConfigMap 中，但 CA 和 bearer token 必须放在 Secret 或外部 Secret
Manager 中。不要把 token、kubeconfig、私钥或 Secret 数据放进代码仓库或 ConfigMap。

随附 Deployment 期望以下文件路径：

| 挂载路径 | 来源 | 内容 |
| --- | --- | --- |
| `/etc/broker/config/broker.yaml` | ConfigMap `internal-credential-broker-config` 的 `broker.yaml` key | 非 Secret 多集群 YAML |
| `/etc/broker/oidc-ca/ca.crt` | Secret `internal-credential-broker-oidc-ca` 的 `ca.crt` key | OIDC issuer CA（需要时） |
| `/etc/broker/targets/<id>.ca.crt` | Secret `internal-credential-broker-target-credentials` 的 `<id>.ca.crt` key | 目标 API CA |
| `/etc/broker/targets/<id>.token` | Secret `internal-credential-broker-target-credentials` 的 `<id>.token` key | 目标 ServiceAccount bearer token |
| `/etc/broker/tls/tls.crt` | Secret `internal-credential-broker-tls` 的 `tls.crt` key | Broker 服务端证书 |
| `/etc/broker/tls/tls.key` | Secret `internal-credential-broker-tls` 的 `tls.key` key | Broker 服务端私钥 |

目标 Secret volume 会把 Secret key 直接映射为文件名。因此 key 必须与 `broker.yaml` 中
的路径一致，例如 `public-cluster-a.ca.crt` 和 `public-cluster-a.token`。

## 准备目标集群

每个目标集群执行一次以下步骤。

1. 确认目标集群已经有 `access-system` namespace、`InternalUser` 和
   `CredentialLease` CRD，以及 InternalUser Controller。
2. 如果目标集群还没有 Broker ServiceAccount 使用的 namespace，创建它：

   ```sh
   kubectl --context "$TARGET_CONTEXT" create namespace access-broker-system \
     --dry-run=client -o yaml | kubectl --context "$TARGET_CONTEXT" apply -f -
   ```

3. 将 `target-rbac.yaml` 应用到目标集群：

   ```sh
   kubectl --context "$TARGET_CONTEXT" apply \
     -f ./config/broker/target-rbac.yaml
   ```

   它只授予 `InternalUser` 的 `get`、`TokenReview` 的 `create`、目标 namespace 内
   `CredentialLease` 的有限权限，以及 `access-system` 中
   `serviceaccounts/token` 的 `create`。不授予 `cluster-admin`、Secret 数据读取或任意
   RBAC 修改权限。

4. 使用 YAML 中配置的 audience 签发目标 ServiceAccount token。初始化时可以使用：

   ```sh
   kubectl --context "$TARGET_CONTEXT" create token \
     internal-credential-broker \
     --namespace access-broker-system \
     --audience https://kubernetes.default.svc.cluster.local \
     --duration 24h > public-cluster-a.token
   ```

   轮换周期应短于 token 生命周期。如果目标集群限制了最大生命周期，应提高轮换频率，
   或使用已批准的外部 Secret 管理流程。

5. 从集群管理员信任包获取目标 API CA，保存为 `public-cluster-a.ca.crt`。当当前
   kubeconfig 使用 `certificate-authority-data` 时，可以使用：

   ```sh
   kubectl --context "$TARGET_CONTEXT" config view --raw --minify \
     -o 'jsonpath={.clusters[0].cluster.certificate-authority-data}' \
     | base64 --decode > public-cluster-a.ca.crt
   ```

6. 对每个目标重复上述过程，然后创建中央 Broker 使用的 target Secret。处理 token
   文件时保持文件权限和临时目录安全：

   ```sh
   kubectl -n access-broker-system create secret generic \
     internal-credential-broker-target-credentials \
     --from-file=public-cluster-a.ca.crt=public-cluster-a.ca.crt \
     --from-file=public-cluster-a.token=public-cluster-a.token \
     --from-file=public-cluster-b.ca.crt=public-cluster-b.ca.crt \
     --from-file=public-cluster-b.token=public-cluster-b.token \
     --dry-run=client -o yaml | kubectl apply -f -
   ```

7. 如果 OIDC issuer 使用私有 CA，创建 OIDC CA Secret：

   ```sh
   kubectl -n access-broker-system create secret generic \
     internal-credential-broker-oidc-ca \
     --from-file=ca.crt=oidc-ca.crt \
     --dry-run=client -o yaml | kubectl apply -f -
   ```

## 部署中央 Broker

1. 替换 ConfigMap `broker.yaml` 中的所有占位符，并根据实际环境修改
   NetworkPolicy 中的 egress CIDR 和端口。生产环境不能使用基础清单中的示例地址。
2. 在仓库根目录渲染并应用中央资源：

   ```sh
   kubectl kustomize ./config/broker >/tmp/internal-user-broker.yaml
   kubectl apply -f /tmp/internal-user-broker.yaml
   ```

   `target-rbac.yaml` 不在该 Kustomization 中，这是有意设计；它必须分别应用到每个
   目标集群。

3. 将 ConfigMap 的 `config-file` 设置为 `/etc/broker/config/broker.yaml`，让
   Deployment 选择多集群模式。应在 Kustomization apply 之后执行 patch，避免后续
   基础清单 apply 把它覆盖：

   ```sh
   kubectl -n access-broker-system patch configmap \
     internal-credential-broker-config --type merge \
     -p '{"data":{"config-file":"/etc/broker/config/broker.yaml"}}'
   ```

4. 等待 rollout 并检查启动校验：

   ```sh
   kubectl -n access-broker-system rollout restart \
     deploy/internal-credential-broker
   kubectl -n access-broker-system rollout status \
     deploy/internal-credential-broker
   kubectl -n access-broker-system logs \
     deploy/internal-credential-broker --tail=200
   ```

YAML 只在进程启动时加载。YAML、CA 路径、NetworkPolicy 或非 Secret 环境变量变化都需要
rollout。仅替换某个目标的 token 文件则不同：client-go 会重新读取 `BearerTokenFile`，
所以只轮换目标 A 的 token 不需要重启 Broker，也不会影响目标 B。

## 环境变量和 flags

以下配置适用于强制使用的多集群 Broker 配置：

| 名称或 flag | 必填 | 默认值 / 含义 |
| --- | --- | --- |
| `BROKER_CONFIG_FILE` / `--config` | 是 | 挂载的带版本多集群 YAML 文件路径。 |
| `BROKER_TLS_CERT_FILE` / `--tls-cert-file` | 是 | Broker HTTPS 证书。 |
| `BROKER_TLS_KEY_FILE` / `--tls-key-file` | 是 | Broker HTTPS 私钥。 |
| `BROKER_ADMIN_GROUP` / `--admin-group` | 否 | 默认 `sealos.internal.admin`，内部审批要求的 OIDC group。 |
| `BROKER_TRUSTED_PROXY_CIDRS` | 是 | 逗号分隔的 Higress/WAF egress CIDR。 |
| `BROKER_ALLOWED_CLIENT_CIDRS` | 是 | 规范化客户端来源 CIDR 列表。 |
| `BROKER_TRUSTED_CLIENT_IP_HEADER` | 是 | 默认 `X-Trusted-Client-IP`，只有可信代理可以设置该 header。 |
| `BROKER_REQUEST_RATE_LIMIT` / `--request-rate-limit` | 否 | 默认每秒 10 个请求。 |
| `BROKER_REQUEST_RATE_BURST` / `--request-rate-burst` | 否 | 默认 20 个请求。 |
| `--listen-address` | 否 | 默认 `:8443`，没有对应的环境变量。 |

## 网络要求

Broker Pod 必须能够：

- 通过 HTTPS 解析并访问配置的 OIDC issuer，以及需要时的 Discovery 和 JWKS；
- 通过 HTTPS 访问每个目标的 `apiServer`；
- 只接受配置的 Higress/WAF egress CIDR 和审批适配器 namespace/pod selector 的 HTTPS
  请求；
- 访问 NetworkPolicy 允许的 DNS 和 Kubernetes API。

`clusterServer` 是用户侧值，不必从 Broker Pod 可达，但必须从用户网络可达。不要用
“允许所有公网 egress”来解决访问问题；应在 NetworkPolicy 中逐个声明目标 API 和 OIDC
目的地。

## API 路由

多集群客户端只能选择 allowlist 中的 `clusterID`：

| 操作 | 路由 |
| --- | --- |
| Base 凭据 | `POST /v1/clusters/{clusterID}/credentials/base` |
| Elevated 兑换 | `POST /v1/clusters/{clusterID}/credentials/elevated/redeem` |
| 提交审批 | `POST /v1/clusters/{clusterID}/internal/credentials/approve` |
| Lease 状态 | `GET /v1/clusters/{clusterID}/credentials/{leaseID}` |
| Lease 撤销 | `POST /v1/clusters/{clusterID}/credentials/{leaseID}/revoke` |

`clusterID` 不是 URL 或凭据选择器。Broker 会将它解析为服务端配置的目标条目。客户端
不能通过请求 body 覆盖 `apiServer`、`clusterServer`、namespace、audience、CA 或 bearer
token。

## RBAC 校验

应用清单后，在中央集群和每个目标集群执行以下命令。若部署使用了不同的 ServiceAccount
名称，请替换 subject：

```sh
SUBJECT='system:serviceaccount:access-broker-system:internal-credential-broker'

kubectl auth can-i get internalusers.user.sealos.io --as "$SUBJECT"
kubectl auth can-i create tokenreviews.authentication.k8s.io --as "$SUBJECT"
kubectl auth can-i get credentialleases.user.sealos.io \
  -n access-broker-system --as "$SUBJECT"
kubectl auth can-i create credentialleases.user.sealos.io \
  -n access-broker-system --as "$SUBJECT"
kubectl auth can-i create serviceaccounts/token \
  -n access-system --as "$SUBJECT"
```

以下权限必须被拒绝：

```sh
kubectl auth can-i get secrets --all-namespaces --as "$SUBJECT"
kubectl auth can-i create clusterrolebindings.rbac.authorization.k8s.io \
  --as "$SUBJECT"
kubectl auth can-i '*' '*' --all-namespaces --as "$SUBJECT"
```

具体结果是环境专项发布门禁；仓库中的 RBAC 测试只验证清单边界。

## 轮换和停用

- **目标 token：** 使用相同 audience 签发新 token，更新对应的 `<id>.token` Secret key，
  然后验证目标 API。仅 token 替换不需要重启 Broker。
- **目标 CA：** 更新 `<id>.ca.crt`，等待 Secret 挂载传播后重启 Broker。
- **OIDC CA 或 YAML：** 更新 Secret/ConfigMap 后重启 Broker。
- **停用目标：** 设置 `enabled: false`，从客户端移除该目标路由，并重启 Broker。处理完
  未完成 lease 后，再按事故/runbook 流程删除目标凭据 Secret key。
- **删除目标：** 撤销或过期其未完成 lease，删除目标 RBAC 和凭据，移除 YAML 条目，并
  确认旧 `clusterID` 请求返回 `404`。

不要记录 token、kubeconfig、Secret 数据或私钥。每个目标 token 和返回的 kubeconfig 都
必须按 bearer credential 保护。

## 故障排查

| 现象 | 检查项 |
| --- | --- |
| `unsupported Broker config version` 或 unknown field | 确认 `version: 1`、字段拼写和只有一个 YAML 文档。 |
| `file is unavailable` | 检查 Secret key、挂载路径以及 `caFile`/`credentialFile` 拼写。 |
| `no enabled target backend is available` | 确认至少一个 target 为 `enabled: true`，且引用的两个文件都已挂载。 |
| OIDC Discovery/JWKS timeout | 检查 DNS、NetworkPolicy egress、issuer CA、`serverName` 和精确 issuer URL。 |
| 目标 API `401` | 检查目标 token、过期时间、API CA、endpoint 和 audience。 |
| 目标 API `403` | 在目标集群执行 `kubectl auth can-i`，确认已应用目标 RBAC 模板。 |
| Broker 启动阶段拒绝配置 | 检查 `BROKER_CONFIG_FILE`、YAML 字段、挂载的 CA/token 文件以及启用的 target。 |
| `clusterID` 返回 `404` | 确认 ID 与启用的 YAML target 完全一致，并且 YAML 变更后已重启 Broker。 |

## 源码和清单

- 中央部署：`./config/broker/`
- 多集群配置类型和校验：`./pkg/broker/config.go`
- 目标 client 创建：`./cmd/broker/main.go`
- 每目标 RBAC 模板：`./config/broker/target-rbac.yaml`
