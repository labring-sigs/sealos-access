# Sealos Access 管理员命令使用说明

本文说明受控的 `sealos-access admin` 命名空间，用于创建 `InternalUser` 资源和提交已经批准的
高权限记录。`approve` 操作是 外部审批适配器的手动/破窗等价能力，不直接写入 `CredentialLease`。

## 授权和前提

- CLI 所在主机可以访问 Kubernetes API Server。
- 使用的 kubeconfig 认证管理员身份，并且只授予创建
  `internalusers.user.sealos.io` 所需的窄权限。
- 已安装 `InternalUser` CRD 及其校验 webhook。webhook 配置了服务端维护的 HTTPS OIDC
  issuer 白名单。
- issuer 和 subject 必须对应一个真实的内部人员。subject 是不透明、不可变的 OIDC subject，
  不是由 CLI 调用方选择的显示名或邮箱地址。
- 执行 `approve` 时，操作员必须持有批准流程产生的审批记录、public OIDC client，
  并使用携带 Broker 精确配置的管理员 group 的 OIDC 身份。

kubeconfig 本身是管理员凭据，必须按照集群管理员凭据策略保护。CLI 不会提升自己的 Kubernetes
权限。管理员身份不应仅因为使用此命令就获得 Secret 读取、任意 RBAC 写入、TokenRequest 或
`credentialleases.user.sealos.io` 修改权限。

## 构建

在仓库根目录执行：

```sh
make build-sealos-access TARGETARCH=amd64 GOOS=linux
./bin/sealos-access-amd64 admin --help
```

本地开发可以直接运行：

```sh
go run ./cmd/sealos-access --help
```

## 配置文件

`create` 命令可以从 YAML 文件加载非敏感默认值。显式指定方式如下：

```sh
./bin/sealos-access-amd64 admin create \
  --config ./admin.yaml
```

未指定 `--config` 时，如果以下文件存在，CLI 会自动使用：

```text
${XDG_CONFIG_HOME}/sealos/access/admin.yaml
```

没有设置 `XDG_CONFIG_HOME` 时，Linux 通常对应
`~/.config/sealos/access/admin.yaml`。示例：

```yaml
kubeconfig: /secure/path/admin.kubeconfig
issuer: https://login.example.internal
subject: oidc-subject-1
```

命令行参数会覆盖配置文件中的值。配置文件必须只包含一个 YAML 文档，未知字段会被拒绝。
`kubeconfig` 只允许填写路径，不能把 kubeconfig 内容、客户端证书、私钥、bearer token 或
其他凭据放入配置文件。CLI 不会创建或修改配置文件。相对路径按照 CLI 当前工作目录解析。

`approve` 配置也只能包含非敏感路径和审批 metadata：

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

不要在此文件中放入 OIDC token、外部审批系统凭据、kubeconfig 或 Secret 数据。

## 创建 InternalUser

使用当前 client-go 加载规则中的管理员 kubeconfig：

```sh
./bin/sealos-access-amd64 admin create \
  --issuer https://login.example.internal \
  --subject oidc-subject-1
```

如果管理员 context 不是当前 context，可以显式指定 kubeconfig：

```sh
./bin/sealos-access-amd64 admin create \
  --kubeconfig /secure/path/admin.kubeconfig \
  --issuer https://login.example.internal \
  --subject oidc-subject-1
```

CLI 会先校验身份格式，然后创建一个 cluster-scoped `InternalUser`，字段固定为：

- `spec.identity.issuer` 为 `--issuer`；
- `spec.identity.subject` 为 `--subject`；
- `spec.roleProfile` 固定为 `base-readonly.v1`；
- `metadata.name` 为 `iu-` 加上
  `sha256(issuer + NUL + subject)` 的前 24 位十六进制字符；
- `spec.suspend` 保持为 false。

服务端 webhook 仍然是最终校验边界，会检查 issuer 白名单、身份不可变规则、固定 profile 和
派生名称。创建成功时 CLI 只输出派生资源名称和 profile，不输出或落盘 Secret、token、kubeconfig
或 `CredentialLease`。

创建资源不会立即返回 Kubernetes credential。InternalUser/CredentialLease Controller 会在
reconcile 后创建稳定 ServiceAccount 和基础 binding。用户变为 Active 后，面向内部人员的
`sealos-access login` 才能申请凭证。

## 提交已批准的高权限记录

当外部审批适配器不可用时，只有在已有独立批准记录的情况下，才使用 `approve` 作为手动/破窗
入口。它先通过 Authorization Code + PKCE 重新进行 OIDC 登录，再调用 Broker 专用的 HTTPS endpoint：

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

Broker 会校验 OIDC token 及其精确的管理员 group，再校验 Active `InternalUser`、固定的高权限
profile 和 TTL，然后持久化 `AwaitingRedemption` Lease 并返回生成的 reference。CLI 显示该 reference，供批准的受保护渠道
转交给用户；不会创建临时 ServiceAccount、Secret、binding 或 kubeconfig。重复相同 `approval-id`
是幂等的，修改 target、TTL 或 reason 会被拒绝。

## 支持的参数

| 参数 | 是否必需 | 说明 |
| --- | --- | --- |
| `--config` | 否 | YAML 配置文件；未指定时使用存在的标准管理员配置路径。 |
| `--kubeconfig` | 否 | 管理员 kubeconfig 路径。为空时使用 client-go 标准加载规则。 |
| `--issuer` | create/approve | HTTPS OIDC issuer，同时必须在服务端白名单中。 |
| `--subject` | create/approve | 不透明、不可变的 OIDC subject；首尾空白会被拒绝。 |
| `--broker-url` | approve | HTTPS Broker URL。 |
| `--cluster-id` | 是* | 服务端 allowlist 中的审批目标集群 ID。也可以来自管理员配置文件。 |
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

## 失败处理

- HTTP 或格式错误的 issuer 会在本地被拒绝。
- 空 subject、首尾带空白的 subject 或过长 subject 会在本地被拒绝。
- 不在服务端白名单中的 issuer 会被 webhook 拒绝。
- 相同 issuer/subject 再次创建时会得到相同名称并返回已存在错误，不会产生第二个身份。
- Kubernetes 授权、连接、CRD 或 webhook 错误会直接返回，但不会泄露 credential 数据。

不要把管理员 kubeconfig、客户端私钥或返回的 reference 放入工单、shell history 或源码仓库。
没有独立批准记录时不要使用 `approve`。用户应使用 user CLI 兑换 reference；此命令不会签发或
兑换 Kubernetes kubeconfig。
