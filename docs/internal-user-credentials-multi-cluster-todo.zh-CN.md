# Internal User 多集群 Broker TODO

状态：P0 已完成；P1 直接凭据实现和自动化验收已完成。

跟踪日期：2026-10-13。

规则：只有在实现、证据或评审完成后才能勾选。第一阶段是中央 Broker 持有每个目标
集群的独立 Kubernetes 凭据；第二阶段将这些凭据迁移到每集群一个 Agent。

决策记录：P0 已确认并关闭。继续采用中央 Broker 持有每个目标集群独立的最小权限凭据；
将 Agent 模式保留为 P2 迁移路径。

P1 范围决策：4.5 节按当前决定暂缓。它继续作为运维后续事项跟踪，不阻塞直接凭据 P1 关闭。

## 1. P0 — 决策与前置条件

- [x] **MCB-P0-001** — 选择一个人员 OIDC issuer 规范 URL。
  - 完成条件：Broker、Discovery、token `iss`、CLI 和 public client 使用完全相同的 HTTPS URL。
- [x] **MCB-P0-002** — 让支持的 CLI 客户端能够访问 issuer。
  - 完成条件：VPN、内网入口或 split-horizon DNS 已记录并验证；没有误把
    `.svc.cluster.local` 暴露给客户端。
- [x] **MCB-P0-003** — 盘点全部目标集群。
  - 完成条件：每个目标都有不可变 ID、API endpoint、public kubeconfig endpoint、API CA、
    Kubernetes audience、namespace、负责人和网络路径。
- [x] **MCB-P0-004** — 为每个目标选择独立 API 凭据。
  - 完成条件：凭据类型、过期时间、轮换负责人、吊销流程和紧急访问方式已记录。
- [x] **MCB-P0-005** — 审批目标 RBAC。
  - 完成条件：目标身份只能读取 `InternalUser`、操作目标 `CredentialLease`、创建
    `TokenReview` 和在 `access-system` 中创建 `serviceaccounts/token`；没有
    `cluster-admin`、Secret 数据或任意 RBAC 权限。
- [x] **MCB-P0-006** — 完成威胁模型和风险接受。
  - 完成条件：中央 Broker 被攻破、目标隔离、事件响应、轮换和 Agent 迁移退出条件已批准。

## 2. P1 — 实现直接凭据 Broker

### 4.1 配置与 Secret

- [x] **MCB-CFG-001** — 增加严格、带版本的 `oidc` 与 `targets` 配置。
  - 完成条件：每个目标定义 `id`、API endpoint、public server、CA、凭据引用、Kubernetes
    audience、namespace 和 `enabled`。
- [x] **MCB-CFG-002** — 对无效目标配置执行 fail closed。
  - 完成条件：重复 ID、空 ID、非 HTTPS endpoint、缺失文件、不支持 audience 和未启用目标
    都会被拒绝。
- [x] **MCB-CFG-003** — 禁止将 Secret 放入 ConfigMap 或代码仓库。
  - 完成条件：目标凭据和 CA 来自每目标 Secret 或外部 Secret 管理系统。
- [x] **MCB-CFG-004** — 增加每目标凭据轮换。
  - 完成条件：每个目标 client 使用独立的 `BearerTokenFile`；只替换一个挂载的 token 文件即可
    轮换该目标，不需要重启 Broker。

### 4.2 Target backend 与 Kubernetes client

- [x] **MCB-BACKEND-001** — 引入 `TargetBackend` 接口。
  - 完成条件：HTTP handler 依赖目标操作，不依赖具体 Kubernetes client。
- [x] **MCB-BACKEND-002** — 为每个目标创建独立 REST config、typed client、controller-runtime
  client 和 TokenIssuer。
  - 完成条件：公网目标不复用中央集群的 `rest.InClusterConfig()`。
- [x] **MCB-BACKEND-003** — 让 audience、namespace、server 和 CA 按目标配置。
  - 完成条件：TokenRequest 和返回的 kubeconfig 只使用选中目标的设置。

### 4.3 目标感知 API 与 lease 状态

- [x] **MCB-API-001** — 增加目标路由，优先采用 `/v1/clusters/{clusterID}/...`。
  - 完成条件：base、elevated redeem、approval、lease status 和 revoke 都解析配置中的目标。
- [x] **MCB-API-002** — 将 approval reference 和 lease 绑定到 `clusterID`。
  - 完成条件：目标 A 的对象不能通过目标 B 读取、兑换或撤销。
- [x] **MCB-API-003** — 每个目标保留一次性签发 barrier。
  - 完成条件：响应丢失或 Broker 重启不能产生第二次 TokenRequest。
- [x] **MCB-API-004** — 每个目标独立失败。
  - 完成条件：目标 A 中断时返回目标级错误，目标 B 仍能签发。

### 4.4 CLI、管理员和审批流程

- [x] **MCB-CLI-001** — `sealos-access` 增加 `--cluster-id` 和配置文件支持。
  - 完成条件：CLI 只发送 allowlist 中的 ID，并写入目标 kubeconfig。
- [x] **MCB-CLI-002** — CLI 输出和审计元数据增加 target ID。
  - 完成条件：用户和运维人员可以为每个 lease 识别目标。
- [x] **MCB-ADMIN-001** — 保持身份创建在独立管理边界内。
  - 完成条件：Broker 和目标 RBAC 对 `InternalUser` 只有 `get` 权限；`sealos-access admin create`
    使用独立且受保护的管理员 kubeconfig；Broker 没有身份创建 endpoint。
- [x] **MCB-ADMIN-002** — 审批适配器回调、通知和兑换命令增加 target ID。
  - 完成条件：审批 payload 不能选择任意 URL 或 namespace。

部署接线：

- `config-file` 必须设置为
  `/etc/broker/config/broker.yaml`；不提供单集群环境变量模式。
- 替换示例目标，并通过外部 Secret
  `internal-credential-broker-target-credentials` 提供引用的文件。
- 在每个目标集群应用一次 `config/broker/target-rbac.yaml`。它创建目标 Broker ServiceAccount，
  只授予读取 `InternalUser`、目标 namespace 的 `CredentialLease` 权限、`TokenReview` 以及在
  `access-system` 中创建 `serviceaccounts/token` 的权限。
- 按目标集群的标准 ServiceAccount token 流程生成目标凭据，例如：
  `kubectl create token internal-credential-broker -n access-broker-system --audience=<target-api-audience>`，
  再挂载到配置的 `credentialFile` 路径。
- 启用目标前，使用目标 ServiceAccount 执行 `kubectl auth can-i`：只有读取
  `internalusers.user.sealos.io`、在 `internal-credential-broker` 中读取/创建
  `credentialleases.user.sealos.io` 和更新 `credentialleases/status`、创建
  `tokenreviews.authentication.k8s.io`、在 `access-system` 中创建
  `serviceaccounts/token` 必须返回 `yes`；读取 Secret、任意 RBAC 写操作和
  `cluster-admin` 必须返回 `no`。
- 目标 `credentialFile` 是 ServiceAccount bearer token 文件；目标 `caFile` 是目标
  API Server CA。OIDC 使用私有 CA 时，从 `internal-credential-broker-oidc-ca` 挂载
  OIDC `caFile`。
- 轮换单个目标时，只原子替换对应 Secret key/file。Kubernetes client 会独立重新读取该目标的
  file-backed bearer token；只有目标路径、CA 或非 Secret 配置变化时才需要重启 Broker。

### 4.5 网络、审计与运维 — 暂缓

- [ ] **MCB-NET-001** — 放行 Broker 到内网 OIDC Discovery 和 JWKS 的 egress。
  - 完成条件：在生产 NetworkPolicy 和 CA 配置下验证 DNS、issuer 端口、Discovery 和 JWKS。
- [ ] **MCB-NET-002** — 为每个目标 API 独立放行 egress。
  - 完成条件：每个目的地和端口都是明确配置，不需要宽泛公网 egress。
- [ ] **MCB-AUDIT-001** — 所有 Broker 审计事件增加 target ID。
  - 完成条件：记录目标、操作、身份、lease、结果、源 IP 和时间，但不记录 token、kubeconfig、
    key 或 Secret 数据。
- [ ] **MCB-OPS-001** — 增加目标级健康检查、指标和告警。
  - 完成条件：一个目标的 OIDC/API 故障不会让所有目标都不健康。
- [ ] **MCB-OPS-002** — 编写目标接入、禁用、轮换和移除 runbook。
  - 完成条件：每份 runbook 都覆盖 CRD、Controller、RBAC、NetworkPolicy、凭据、lease 和回滚。

## 3. P1 — 验收与安全门禁

- [x] **MCB-E2E-001** — 使用一个 OIDC identity 为两个目标签发 base 凭据。
  - 完成条件：每份 kubeconfig 只能访问选中的 API Server。
- [x] **MCB-E2E-002** — 按目标签发和兑换 elevated 凭据。
  - 完成条件：approval、lease、ServiceAccount、TokenRequest 和 status 不跨目标。
- [x] **MCB-E2E-003** — 测试内部 issuer CA、Discovery、JWKS rotation、DNS 失败、timeout 和
  JWKS 不可用。
  - 完成条件：错误有界且不会 fail open。
- [x] **MCB-E2E-004** — 断开目标 A 的 API 网络。
  - 完成条件：A 返回受控错误，B 仍然可用。
- [x] **MCB-E2E-005** — 轮换并吊销目标 A 凭据。
  - 完成条件：B 不受影响，旧 A 凭据不能继续签发。
- [x] **MCB-E2E-006** — 使用 `kubectl auth can-i` 审查目标 RBAC。
  - 完成条件：没有 Secret 数据读取、任意 RBAC mutation 或 `cluster-admin`。
- [x] **MCB-E2E-007** — 为每个目标测试响应丢失和 Broker 重启。
  - 完成条件：目标 lease 阻止重复签发。

已勾选 P1 门禁的自动化证据：

- `go test ./pkg/broker ./cmd/broker -count=1` 覆盖目标路由、elevated 审批/兑换隔离、目标故障
  独立性、撤销、file-backed 目标凭据、RBAC 清单、私有 CA client 配置、Discovery、JWKS rotation、
  timeout 和一次性签发 barrier。
- `kubectl kustomize config/broker` 验证中央 Broker RBAC 和 namespace-scoped Lease 权限可以渲染。
- 上述目标集群 `kubectl auth can-i` 命令仍是每个真实目标集群上线前的环境专项检查。

## 4. P2 — 迁移到每集群一个 Agent

- [ ] **MCB-AGENT-001** — 定义窄范围 Agent 协议。
  - 完成条件：只暴露审核过的 user lookup、lease、TokenRequest、TokenReview 和目标元数据；
    Agent 不是通用 proxy。
- [ ] **MCB-AGENT-002** — 每个目标集群部署一个本地 ServiceAccount/RBAC 的 Agent。
  - 完成条件：目标 API 凭据由 Agent 持有，而不是中央 Broker 持有。
- [ ] **MCB-AGENT-003** — 增加每目标 mTLS 或等价 workload identity。
  - 完成条件：Broker 和 Agent 身份绑定到一个不可变 target ID。
- [ ] **MCB-AGENT-004** — 实现远程 `TargetBackend`。
  - 完成条件：direct 和 Agent backend 满足相同的签发契约。
- [ ] **MCB-AGENT-005** — 执行 direct/Agent 双模式验收。
  - 完成条件：一个目标的签发、审批、撤销、响应丢失和故障行为一致。
- [ ] **MCB-AGENT-006** — 按目标逐步切换。
  - 完成条件：Agent 健康和验收通过后才禁用对应 direct mode。
- [ ] **MCB-AGENT-007** — 吊销直接凭据并移除直接 API egress。
  - 完成条件：Agent 是目标 API 唯一剩余身份。

## 5. P1 — 直接凭据完成标准

- [x] **MCB-DONE-001** — 内网 OIDC 登录在 Broker 和支持的 CLI 网络中可用。
- [x] **MCB-DONE-002** — 至少两个目标可以签发 base 和 elevated 凭据。
- [x] **MCB-DONE-003** — 每个目标都有独立凭据和 RBAC；NetworkPolicy、指标和 runbook 保留在暂缓的 4.5 节。
- [x] **MCB-DONE-004** — 跨目标隔离和故障测试通过。
- [x] **MCB-DONE-005** — 客户端不能选择目标 URL、namespace、audience 或凭据。
- [x] **MCB-DONE-006** — `InternalUser` 创建仍在普通签发流程之外。
- [x] **MCB-DONE-007** — `TargetBackend` 已准备好 Agent 模式。

## 6. P2 — Agent 迁移完成标准

- [ ] **MCB-AGENT-DONE-001** — 吊销所有中央直接目标凭据。
- [ ] **MCB-AGENT-DONE-002** — 移除中央直接 API egress 和目标 Secret 引用。
- [ ] **MCB-AGENT-DONE-003** — 每个目标 Agent 通过验收矩阵。
