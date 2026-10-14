# Sealos Access CLI 使用说明

本文说明面向内部人员的 `sealos-access login` 命令。它通过 Broker 获取普通凭证和经过审批的
临时高权限 Kubernetes 凭证。

该 CLI 不选择 target identity 或 RBAC profile。高权限流程由外部审批系统发起，并通过受保护
渠道把一次性 reference 发送给用户，CLI 只负责兑换临时高权限凭证。`InternalUser` 生命周期仍由
管理员流程维护，具体见下方“管理员命令”章节。

## 命令面

`sealos-access` 是唯一支持的 CLI 入口：

| 命令 | 角色 | 配置与授权 |
| --- | --- | --- |
| `login` | 获取 base 凭证 | `user.yaml`；用户 OIDC 登录和 Broker 授权 |
| `elevated redeem` | 兑换已批准的高权限凭证 | `user.yaml`；用户 OIDC 登录和一次性 reference |
| `admin create` | 创建或协调 `InternalUser` | `admin.yaml`；管理员 kubeconfig 和 Kubernetes RBAC |
| `admin approve` | 提交已经批准的高权限记录 | `admin.yaml`；携带配置的管理员 group 的用户 OIDC 登录 |

CLI 会先根据命令路由，再加载配置。用户命令不会读取 `admin.yaml`，管理员命令也不会
回退读取 `user.yaml`。调用方不能选择任意 target URL、audience、profile、namespace、
凭据或 RBAC 规则；这些决策仍由服务端控制。身份生命周期和管理员专属参数见下方
“管理员命令”章节。

## 使用前提

- 管理员流程已经为该 OIDC issuer 和 subject 创建并激活 `InternalUser`。
- 已注册供 CLI 使用的 public OIDC client。不需要 client secret，并且必须允许
  `http://127.0.0.1:<random-port>/callback` 形式的 loopback 回调。
- CLI 所在主机可以通过 HTTPS 访问配置的 OIDC issuer 和 Broker。
- 外部审批适配器已经部署，并已将批准记录提交给 Broker；适配器会通过受保护通知渠道发送
  Broker 返回的 reference。
- issuer 的 Discovery 文档返回与 CLI 参数完全一致的 issuer URL，并提供授权端点
  和 token 端点。
- 必须通过配置文件或命令行显式指定本地普通文件输出路径。CLI 会拒绝符号链接，并将
  kubeconfig 写为 `0600` 权限。

当主机已经信任对应证书时，Broker 和 OIDC CA 文件可以不提供；否则使用对应的
CA 参数。不要为了绕过证书错误而关闭证书校验。

## 构建

在仓库根目录执行：

```sh
make build-sealos-access TARGETARCH=amd64 GOOS=linux
./bin/sealos-access-amd64 --help
```

本地开发可以直接运行：

```sh
go run ./cmd/sealos-access --help
```

## 配置文件

CLI 支持 YAML 配置文件，因此每次运行不必重复填写 Broker、OIDC、TLS、TTL 和输出路径等
常用参数。可以通过 `--config path/to/config.yaml` 显式指定文件。未指定 `--config` 时，若
标准用户配置路径存在，CLI 会自动使用：

```text
${XDG_CONFIG_HOME}/sealos/access/user.yaml
```

没有设置 `XDG_CONFIG_HOME` 的系统使用操作系统返回的用户配置目录；Linux 通常为
`~/.config/sealos/access/user.yaml`。如果默认文件不存在，必需值仍需通过命令行
参数提供。

示例：

```yaml
broker-url: https://broker.example.internal
cluster-id: public-cluster-a
broker-ca-file: ./pki/broker-ca.pem
broker-server-name: internal-credential-broker.example.internal
oidc-issuer: https://login.example.internal
oidc-ca-file: ./pki/oidc-ca.pem
oidc-server-name: login.example.internal
client-id: internal-kc
output: ./internal-user-kubeconfig
requested-ttl-seconds: 3600
no-browser: false
login-hint: ""
```

命令行参数会覆盖配置文件中的值。配置文件必须只包含一个 YAML 文档，未知字段会被拒绝。
路径按照 CLI 当前工作目录解析。不要在配置文件中放入 token、kubeconfig、client secret 或
审批 reference。一次性的 `--approval-reference` 仍必须在 redeem 命令中显式提供。配置的
`output` 用于 base 和 redeem 命令。CLI 不会创建或修改
配置文件。

## 获取普通凭证

常用命令如下：

```sh
./bin/sealos-access-amd64 login \
  --broker-url https://broker.example.internal \
  --cluster-id public-cluster-a \
  --oidc-issuer https://login.example.internal \
  --client-id internal-kc \
  --requested-ttl-seconds 3600 \
  --output ./internal-user-kubeconfig
```

CLI 会依次执行：

1. 获取 OIDC Discovery 元数据。
2. 在 `127.0.0.1` 的随机端口启动回调监听器。
3. 生成 `state`、`nonce` 和 S256 PKCE verifier。
4. 在默认浏览器中打开授权 URL。
5. 交换授权码并校验 ID token 的 nonce。
6. 将 OIDC access token 发送到 Broker 的普通凭证 endpoint。
7. 将一次性 kubeconfig 写入明确指定的输出路径。

浏览器登录必须在五分钟内完成。回调只监听 loopback，不是外部可访问的服务。

普通 profile 的请求 TTL 为 600 至 86400 秒。最终限制由 Broker 服务端执行，超出
profile 限制的值会被拒绝。CLI 不打印 access token 或 kubeconfig；成功时只显示
issuer、推导出的 username、profile、Lease ID 和过期时间等元数据。

不要打印 kubeconfig 内容，直接使用输出文件：

```sh
KUBECONFIG=./internal-user-kubeconfig kubectl get namespaces
stat -c '%a %n' ./internal-user-kubeconfig
```

预期文件权限为 `600`。将该文件视为 bearer credential，不要提交到 Git、上传或
复制到不受控位置；凭证过期或撤销后应删除。

## 兑换高权限凭证

用户先在外部审批流程中提交配置的审批表单。适配器向 Broker 报告批准后，会通过受保护通知
渠道发送 Broker 生成的 reference。用户再以同一个 OIDC 身份兑换：

```sh
./bin/sealos-access-amd64 elevated redeem \
  --cluster-id public-cluster-a \
  --approval-reference r1.approval-<lease-id>.<random-value> \
  --output ./internal-user-elevated-kubeconfig
```

Broker 从已认证的 OIDC identity 推导 requester 和 target，并将 profile 固定为
`cluster-ops-write.v1`。它从 `AwaitingRedemption` Lease 读取已持久化的 TTL 和 target，单次
消费 reference 后才创建临时高权限资源。CLI 将返回的 kubeconfig 以 `0600` 写入文件，只显示
非敏感元数据。如果 reference 丢失，应重新发起外部审批，不要从日志或 Kubernetes 中恢复它。

审批 reference 不是认证因子，不能替代 OIDC 登录。不要把它分享给他人或写入公开工单和日志。

## 管理员命令

受控的 `sealos-access admin` 命名空间用于创建 `InternalUser` 资源和提交已经批准的高权限记录。
`approve` 是外部审批适配器不可用时的手动或破窗等价能力，不直接写入 `CredentialLease`。

### 授权和前提

- CLI 所在主机可以访问 Kubernetes API Server；
- 使用的 kubeconfig 认证管理员身份，并且只授予创建
  `internalusers.access.sealos.io` 所需的窄权限；
- 已安装 `InternalUser` CRD 及校验 webhook，webhook 配置了服务端维护的 HTTPS OIDC issuer 白名单；
- issuer 和 subject 必须对应真实内部人员。subject 是不透明、不可变的 OIDC subject，不能由调用方
  用显示名或邮箱代替；
- 执行 `approve` 时，操作员必须持有独立审批记录、public OIDC client，并使用携带 Broker 精确
  配置管理员 group 的 OIDC 身份。

kubeconfig 本身是管理员凭据，必须按照集群管理员凭据策略保护。CLI 不会提升自己的 Kubernetes
权限。管理员身份不应仅因为使用这些命令就获得 Secret 读取、任意 RBAC 写入、TokenRequest 或
`credentialleases.access.sealos.io` 修改权限。

### 管理员配置

管理员命令使用独立的非敏感配置文件。可以显式指定 `--config ./admin.yaml`；未指定时，如果
以下文件存在，CLI 会自动使用：

```text
${XDG_CONFIG_HOME}/sealos/access/admin.yaml
```

在未设置 `XDG_CONFIG_HOME` 的 Linux 上，通常是 `~/.config/sealos/access/admin.yaml`。

`admin create` 示例：

```yaml
kubeconfig: /secure/path/admin.kubeconfig
issuer: https://login.example.internal
subject: oidc-subject-1
```

`admin approve` 示例：

```yaml
issuer: https://login.example.internal
subject: oidc-subject-1
broker-url: https://internal-credential-broker.example.internal
cluster-id: public-cluster-a
broker-ca-file: ./pki/broker-ca.pem
broker-server-name: internal-credential-broker.example.internal
oidc-issuer: https://login.example.internal
oidc-ca-file: ./pki/oidc-ca.pem
client-id: internal-admin
approval-id: approval-instance-123
approval-reason: incident remediation
requested-ttl-seconds: 1800
```

命令行参数会覆盖文件中的值。配置文件必须只包含一个 YAML 文档，未知字段会被拒绝；路径按
CLI 当前工作目录解析。不能把 kubeconfig 内容、客户端证书、私钥、bearer token、外部审批凭据
或 Secret 数据放入文件。CLI 不会创建或修改该文件。

### 创建 `InternalUser`

使用当前 client-go 加载规则中的管理员 kubeconfig：

```sh
./bin/sealos-access-amd64 admin create \
  --issuer https://login.example.internal \
  --subject oidc-subject-1
```

如果管理员 context 不是当前 context，增加 `--kubeconfig /secure/path/admin.kubeconfig`。

CLI 会先校验身份格式，然后创建一个 cluster-scoped `InternalUser`，固定使用
`base-readonly.v1`。资源名为 `iu-` 加上 `sha256(issuer + NUL + subject)` 的前 24 位十六进制字符。
服务端 webhook 仍然是 issuer 白名单、身份不可变、固定 profile 和派生名称的最终校验边界。

创建成功时 CLI 只输出派生资源名称和 profile，不输出或落盘 Secret、token、kubeconfig 或
`CredentialLease`。Controller 在 reconcile 后创建稳定 ServiceAccount 和基础 binding；用户变为
Active 后，`sealos-access login` 才能申请凭证。

### 提交已批准的高权限记录

当外部审批适配器不可用时，只有在已有独立审批记录的情况下，才使用 `admin approve` 作为手动或
破窗入口：

```sh
./bin/sealos-access-amd64 admin approve \
  --issuer https://login.example.internal \
  --subject oidc-subject-1 \
  --broker-url https://internal-credential-broker.example.internal \
  --cluster-id public-cluster-a \
  --broker-ca-file ./pki/broker-ca.pem \
  --oidc-issuer https://login.example.internal \
  --oidc-ca-file ./pki/oidc-ca.pem \
  --client-id internal-admin \
  --approval-id approval-instance-123 \
  --approval-reason "incident remediation" \
  --requested-ttl-seconds 1800
```

命令会重新执行 OIDC Authorization Code + PKCE 登录。Broker 校验 OIDC token 和精确的管理员 group，
再校验 Active `InternalUser`、高权限 profile 和 TTL，然后持久化 `AwaitingRedemption` lease 并返回
reference。它不会创建临时 ServiceAccount、Secret、binding 或 kubeconfig。重复相同 `approval-id`
是幂等的，修改 target、TTL 或 reason 会被拒绝。

### 管理员参数

| 参数 | 是否必需 | 说明 |
| --- | --- | --- |
| `--config` | 否 | YAML 配置文件；未指定时使用存在的标准管理员配置路径。 |
| `--kubeconfig` | 否 | 管理员 kubeconfig 路径；为空时使用 client-go 标准加载规则。 |
| `--issuer` | create/approve | HTTPS OIDC issuer，且必须在服务端白名单中。 |
| `--subject` | create/approve | 不透明、不可变的 OIDC subject；首尾空白会被拒绝。 |
| `--broker-url` | approve | HTTPS Broker URL。 |
| `--cluster-id` | 是* | 服务端 allowlist 中的目标集群 ID，也可以来自 `admin.yaml`。 |
| `--broker-ca-file` | approve | Broker TLS 校验 CA 文件。 |
| `--broker-server-name` | 否 | Broker TLS server name。 |
| `--oidc-issuer` | approve | 用于管理员认证的 HTTPS OIDC issuer。 |
| `--oidc-ca-file` | 否 | OIDC TLS 校验 CA 文件。 |
| `--client-id` | approve | Public OIDC client ID。 |
| `--approval-id` | approve | 外部审批实例不可变 ID。 |
| `--approval-reason` | approve | 人工可读的审批原因。 |
| `--requested-ttl-seconds` | approve | 高权限 TTL，限制为 600-3600 秒。 |
| `--no-browser` | 否 | 打印 OIDC 授权 URL，而不是自动打开浏览器。 |

CLI 特意没有 `--name`、`--role-profile`、RBAC、Secret、token 或 `CredentialLease` 参数。
名称和 profile 都是平台维护的值。

### 管理员失败处理

- HTTP 或格式错误的 issuer 会在本地被拒绝；
- 空 subject、首尾带空白的 subject 或过长 subject 会在本地被拒绝；
- 不在服务端白名单中的 issuer 会被 webhook 拒绝；
- 相同 issuer/subject 再次创建时会得到相同名称并返回已存在错误，不会创建第二个身份；
- Kubernetes 授权、连接、CRD 或 webhook 错误会直接返回，但不会泄露 credential 数据。

不要把管理员 kubeconfig、客户端私钥或返回的 reference 放入工单、shell history 或源码仓库。
没有独立审批记录时不要使用 `approve`。用户通过用户命令兑换 reference；管理员命令不会签发或
兑换 Kubernetes kubeconfig。

## TLS 参数

当 OIDC issuer 或 Broker 使用内部 CA，或者证书名称与 URL host 不同，可以使用：

```sh
./bin/sealos-access-amd64 login \
  --broker-url https://broker.example.internal \
  --cluster-id public-cluster-a \
  --broker-ca-file ./pki/broker-ca.pem \
  --broker-server-name internal-credential-broker.example.internal \
  --oidc-issuer https://login.example.internal \
  --oidc-ca-file ./pki/oidc-ca.pem \
  --oidc-server-name login.example.internal \
  --client-id internal-kc \
  --requested-ttl-seconds 3600 \
  --output ./internal-user-kubeconfig
```

`--broker-ca-file` 和 `--oidc-ca-file` 必须是 PEM 编码的 CA 证书文件。
`--broker-server-name` 和 `--oidc-server-name` 用于 TLS server-name 校验，不能
关闭校验。

## 无图形浏览器登录

在没有图形浏览器的主机上增加 `--no-browser`：

```sh
./bin/sealos-access-amd64 login \
  --broker-url https://broker.example.internal \
  --cluster-id public-cluster-a \
  --oidc-issuer https://login.example.internal \
  --client-id internal-kc \
  --requested-ttl-seconds 3600 \
  --no-browser \
  --output ./internal-user-kubeconfig
```

CLI 会打印一次性授权 URL。请在能够访问 OIDC Provider 的浏览器中打开它，并保持
原 CLI 进程运行，以便本地回调仍然可用。身份提供方支持时，可以增加：

```sh
--login-hint alice@example.internal
```

不要把授权 URL 粘贴到工单或日志中。它虽然不包含最终 Kubernetes credential，仍然
包含临时协议参数。

## 参数表

| 参数 | 是否必需 | 说明 |
| --- | --- | --- |
| `--config` | 否 | YAML 配置文件；未指定时使用存在的标准用户配置路径。 |
| `--broker-url` | 是* | HTTPS Broker 基础 URL。 |
| `--cluster-id` | 是* | 服务端 allowlist 中的目标集群 ID；用于选择 target-aware Broker 路由。也可以来自用户配置文件。 |
| `--broker-ca-file` | 否 | Broker TLS 校验使用的 PEM CA 文件。 |
| `--broker-server-name` | 否 | Broker TLS 校验使用的 server name。 |
| `--oidc-issuer` | 是* | HTTPS OIDC issuer URL，必须与 Discovery 完全一致。 |
| `--oidc-ca-file` | 否 | OIDC TLS 校验使用的 PEM CA 文件。 |
| `--oidc-server-name` | 否 | OIDC TLS 校验使用的 server name。 |
| `--client-id` | 是* | Public OIDC client ID。 |
| `--output` | base/redeem 必需* | kubeconfig 本地普通文件路径；拒绝 `-`。 |
| `--requested-ttl-seconds` | 是* | 请求普通凭证生命周期；普通 profile 使用 600 至 86400 秒。高权限 TTL 来自外部审批表单。 |
| `--approval-reference` | elevated redeem 必需 | 外部审批适配器发送的一次性兑换 reference。 |
| `--no-browser` | 否 | 打印授权 URL，不自动打开浏览器。 |
| `--login-hint` | 否 | 可选的 OIDC login hint。 |

CLI 特意没有 profile、admin group、高权限 TTL、任意 URL、namespace、audience 或 RBAC 参数。
`--cluster-id` 只能填写服务端 allowlist 中的标识符；目标 URL、namespace、audience 和凭据映射
仍由 Broker 管理。外部表单、适配器、Broker 和兑换 target 都是服务端策略。带 `*` 的参数也可以
来自配置文件。

## 常见问题

- `OIDC discovery issuer does not match configuration`：使用 Provider 实际返回的
  issuer URL，包括完全一致的 scheme、host 和 path。
- `fetch OIDC discovery` 或 `exchange OIDC authorization code`：检查 DNS、网络策略、
  CA 文件和 TLS server-name 参数。
- `OIDC authorization timed out`：在五分钟内完成浏览器登录，并保持原 CLI 进程运行。
- `Broker returned HTTP 403`：确认 OIDC identity 已创建、处于 Active 状态，并满足
  Broker 的源 IP 和身份策略。
- `requested TTL must be at least 10m` 或上限错误：普通 profile 设置为 600 至 86400 秒，
  高权限设置为 600 至 3600 秒。
- `refusing symlink output path`：直接选择普通文件路径，不要通过符号链接绕过保护。

一次性响应丢失时，不要从日志中读取或复制 kubeconfig；应重新运行 CLI 创建新请求，
因为旧 Lease 可能已经被消耗。
