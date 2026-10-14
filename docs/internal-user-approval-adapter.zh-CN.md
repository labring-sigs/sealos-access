# 审批适配器说明

`internal-user-approval-adapter` 是 `cluster-ops-write.v1` 的外部审批桥接程序。它接收
外部审批系统发送的已批准事件，从外部审批 API 重新查询权威审批实例，携带管理员 group 的
OIDC token 通过 Broker HTTPS endpoint 提交一条非敏感审批记录，再通过邮件把 Broker 生成的
reference 发送给目标用户。

适配器关于审批事件本身是无状态的。它不持久化 event ID、审批记录、reference、token 或
kubeconfig，只把外部系统重试时的相同实例 code 再次提交给 Broker。Broker 持久化
`CredentialLease`，并以实例 code 作为幂等键。适配器可以在内存中缓存外部审批 API 的短期
访问令牌。

## 架构

```text
外部审批系统发送已批准事件
        |
        v
校验事件真实性（签名/令牌）
        |
        v
[ApprovalSource] 从外部审批 API 获取审批实例和表单
        |
        v
校验审批定义 code、OIDC subject、TTL 和 reason
        |
        v
[Approver] 携带 OIDC 管理员 group token 调用 /v1/clusters/{clusterID}/internal/credentials/approve
        |
        v
[Notifier] 将返回的 reference 发送到发起人的邮箱
```

事件 payload 只是唤醒信号。目标 identity、TTL 和 reason 来自 `ApprovalSource` 重新读取的
完整审批实例；OIDC issuer 和审批定义 code 来自固定部署配置。事件 endpoint 只返回
`processed` 或 `ignored`，绝不在响应中返回 reference。

## 抽象接口

所有接口定义在
[`pkg/approvaladapter/adapter.go`](../pkg/approvaladapter/adapter.go)。

### ApprovalSource

`ApprovalSource` 是唯一需要您实现的接口。它从外部审批系统获取权威审批数据：

```go
type ApprovalSource interface {
    GetInstance(ctx context.Context, instanceCode string) (ApprovalInstance, error)
    GetUserEmail(ctx context.Context, userID string) (string, error)
}
```

- `GetInstance` 从外部审批 API 获取完整的审批实例（状态、表单字段、发起人）。
  `ApprovalInstance` 包含 `InstanceCode`、`ApprovalCode`、`ClusterID`、`Status`、
  `InitiatorID` 以及 JSON 编码的 `Form`。`ClusterID` 必填，并且必须对应 Broker
  已配置的 target。实现**不得**在可查询权威 API 时仅返回回调事件中的数据。
- `GetUserEmail` 将用户标识解析为邮箱地址，用于投递 Broker reference。

**基于轮询的实现** — 定时轮询外部系统的审批列表，检测新的 APPROVED 实例，获取每个实例
并送入适配器处理流水线。

**基于 Webhook 的实现** — 通过 HTTP endpoint 接收回调事件，提取实例 code，调用
`GetInstance` 获取权威数据，校验后继续处理。

### Approver

`Approver` 将已验证的外部审批记录提交给 Broker：

```go
type Approver interface {
    Approve(ctx context.Context, record broker.ApprovalRecord) (broker.ApprovalResponse, error)
}
```

### Notifier

`Notifier` 将 Broker 生成的一次性 reference 投递给已批准的申请人。它永远不会接触
Kubernetes token 或 kubeconfig：

```go
type Notification struct {
    ClusterID         string
    Recipient         string
    ApprovalID        string
    ApprovalReference string
    Profile           string
    ExpiresAt         time.Time
}

type Notifier interface {
    Notify(ctx context.Context, notification Notification) error
}
```

## 已有实现

### BrokerHTTPClient（Approver）

[`BrokerHTTPClient`](../pkg/approvaladapter/broker.go) 将已批准的记录提交到 Broker
专用 HTTPS endpoint。需要 Broker HTTPS URL、Broker CA，以及带有服务端精确配置管理员
group 的 OIDC Authorization header 的 `*http.Client`。该实现强制使用 HTTPS，将
`ApprovalRecord` 编码为 JSON，并解码 `ApprovalResponse`。

```go
client, _ := approvaladapter.NewBrokerHTTPClient(brokerURL, clusterID, httpClient)
response, err := client.Approve(ctx, record)
```

提交目标地址为 `{BrokerURL}/v1/clusters/{clusterID}/internal/credentials/approve`。

### EmailNotifier（Notifier）

[`EmailNotifier`](../pkg/approvaladapter/email.go) 仅通过 SMTP 发送一次性 Broker
reference 和非敏感审批元数据。邮件正文中绝不包含 token 或 kubeconfig。

```go
notifier := &approvaladapter.EmailNotifier{
    SMTPHost: smtpHost, SMTPPort: smtpPort, SMTPUsername: smtpUser,
    SMTPPassword: smtpPass, From: smtpFrom, Subject: subject,
}
err := notifier.Notify(ctx, notification)
```

## 骨架 main.go

入口点位于
[`cmd/internal-user-approval-adapter/main.go`](../cmd/internal-user-approval-adapter/main.go)，
有意设计为骨架。它装配了 `BrokerHTTPClient` 和 `EmailNotifier`，从环境变量和命令行参数
解析配置，校验必填项，然后启动带 `/healthz` endpoint 的 HTTPS 服务。

**要构建可运行的适配器，需实现 `ApprovalSource` 并将其接入 `run()`：**

```go
func run(ctx context.Context, config config) error {
    // 装配可复用组件。
    approver, _ := approvaladapter.NewBrokerHTTPClient(config.BrokerURL, httpClient)
    notifier := &approvaladapter.EmailNotifier{...}

    // 实现并接入你的 ApprovalSource。
    source := &myApprovalSource{...}

    // 启动处理（示例：webhook handler 或轮询循环）。
    mux := http.NewServeMux()
    mux.HandleFunc("/v1/internal/approval/events", func(w http.ResponseWriter, r *http.Request) {
        // 1. 校验事件真实性。
        // 2. 提取实例 code。
        // 3. 调用 source.GetInstance(ctx, instanceCode)。
        // 4. 校验实例（状态、审批 code、字段）。
        // 5. 构造 broker.ApprovalRecord。
        // 6. 调用 approver.Approve(ctx, record)。
        // 7. 通过 source.GetUserEmail(ctx, initiatorID) 解析邮箱。
        // 8. 调用 notifier.Notify(ctx, notification)。
    })

    // 启动 HTTPS 服务...
}
```

骨架读取的关键配置变量：

| 变量 | 用途 |
| --- | --- |
| `BROKER_URL` | Broker HTTPS endpoint |
| `BROKER_CA_FILE` | Broker 服务端 CA 证书 |
| `BROKER_BEARER_TOKEN` | 服务端精确配置管理员 group 的 OIDC bearer token |
| `BROKER_BEARER_TOKEN_FILE` | 包含该 OIDC bearer token 的文件 |
| `SMTP_HOST` / `SMTP_PORT` | SMTP 中继地址 |
| `SMTP_USERNAME` / `SMTP_PASSWORD` | SMTP 凭据（可选） |
| `SMTP_FROM` | 发件邮箱地址 |
| `ADAPTER_TLS_CERT_FILE` / `ADAPTER_TLS_KEY_FILE` | 适配器 HTTPS 监听的服务端 TLS |

请添加您自己的外部审批系统凭据变量（API key、应用凭据、Webhook 签名密钥等）。

## 表单约定

适配器依赖外部审批系统中定义的固定表单 schema，并在适配器侧进行配置。典型字段包括：

| 字段 | 示例 | 规则 |
| --- | --- | --- |
| Subject | `alice` | InternalUser 的不透明、不可变 OIDC subject。 |
| TTL | `1800` | 秒数，Broker 策略限制在 600 到 3600 秒。 |
| Reason | `incident remediation` | 必填的人工可读原因，最多 2048 字节。 |

适配器从配置（如 `INTERNAL_USER_TARGET_ISSUER`）固定 OIDC issuer，不接受事件或表单中
的 issuer。读取的实例状态必须是 `APPROVED`，审批定义必须等于配置的审批 code。

## 重试与幂等性

适配器不持久化 event ID 或审批记录。幂等性委托给 Broker：Broker 使用
`ApprovalRecord.ApprovalID`（外部系统的实例 code）作为幂等键。同一实例 code 再次提交时，
Broker 返回已有的 `ApprovalResponse`，不会创建重复的 lease。

- 外部 API 查询、Broker 提交或邮件发送失败时，适配器返回可重试的 HTTP 503（webhook 模式）
  或在下一轮轮询中重试（轮询模式）。
- 如果 Broker 已经成功而邮件发送失败，下一次尝试会通过 Broker 的幂等路径得到同一
  reference 并重试通知。
- 不需要适配器数据库或事件消费 checkpoint。

## 本地运行

在仓库根目录构建：

```sh
make build-internal-user-approval-adapter TARGETARCH=amd64
./bin/internal-user-approval-adapter-amd64 --help
```

进程要求 HTTPS 回调监听、外部审批系统凭据、配置的审批 code、固定 OIDC issuer、Broker
CA、服务端精确配置管理员 group 的 OIDC bearer token。还要求配置 SMTP relay 和发件地址，
避免审批成功后无法交付 reference。

不要把 API 密钥、签名令牌、SMTP 密码或私钥放入 ConfigMap。请使用 Kubernetes Secrets
或外部密钥存储。

## Kubernetes 部署

清单位于 `config/internal-user-approval-adapter`。应用前：

1. 替换所有占位 URL、审批 code、issuer、SMTP host 和准确的 NetworkPolicy egress CIDR；
2. 创建 `internal-credential-approval-adapter-secrets`，写入外部审批系统、SMTP 凭据和
   Broker bearer token；
3. 创建 `internal-credential-approval-adapter-broker-ca`，以 `ca.crt` 提供 Broker 服务端 CA；
4. 通过批准的 ingress/WAF 路由暴露适配器的事件 endpoint（如 `/v1/internal/approval/events`），
   只转发回调，不转发 Broker 用户接口。

适配器 ServiceAccount 设置 `automountServiceAccountToken: false`，没有 Kubernetes RBAC。
NetworkPolicy 只允许配置的 ingress gateway 回调入站、适配器到 Broker 的访问、外部审批
API 和批准的 SMTP relay 出站。部署前必须替换模板中的文档占位公网 CIDR。

## 验收覆盖

可复用的 `Pipeline`、`MockSource` 和受保护的 `NotificationSink` 由单元测试和进程内
reference 兑换验收测试覆盖。`internal/access/usercmd/elevated_redeem_acceptance_test.go`
中的 `TestElevatedRedeemPreOIDC` 验证：

- mock 审批记录能够被读取、校验并提交给 Broker；
- Broker 返回的 reference 只能通过受保护的通知 sink 交付；
- 高权限兑换会写入权限为 `0600` 的 kubeconfig；
- 通知和 Broker 请求中不包含 token 或 kubeconfig；
- Broker 恰好执行一次 TokenRequest。

运行专项验收测试：

```sh
go test -tags acceptance ./internal/access/usercmd -run '^TestElevatedRedeemPreOIDC$' -count=1 -v
```

受保护的 acceptance sink 只在 `acceptance` build tag 下编译，不会进入生产二进制。
运行完整的进程内 acceptance suite：

```sh
go test -tags acceptance ./internal/access/usercmd ./pkg/approvaladapter/acceptance -count=1 -v
```

该测试在单进程内运行，不能替代基于集群的 mock E2E runbook。适配器在将具体的
`ApprovalSource` 和外部事件监听器接入 `cmd/internal-user-approval-adapter/main.go`
之前，仍然是集成骨架。

## 故障行为汇总

| 故障场景 | 后果 |
| --- | --- |
| 外部 API 查询失败 | 重试（503 或下一轮轮询） |
| Broker 提交失败 | 重试；基于实例 code 幂等 |
| 邮件发送失败 | 下一次事件投递时重试；Broker 返回已有 reference |
| 校验失败（状态、code、字段） | 返回 `ignored`；不重试 |

用户收到 reference 后，使用 `sealos-access elevated redeem` 并重新进行 OIDC 登录。
reference 不是认证因子。适配器永远不会接触最终的 Kubernetes token 或 kubeconfig。

管理员 `sealos-access admin approve` 命令是适配器提交 Broker 能力的手动/破窗等价入口。
