# Internal User Credentials 端到端测试 TODO

状态：对现有验收测试计划的补充提案。这些条目扩展了并发、故障恢复、审批适配器全链路、
时钟偏差边界、审计完整性和信息泄露防护场景的覆盖范围，这些尚未被现有默认验收矩阵覆盖。

创建日期：2026-09-11

---

## P0 — 发布阻塞

- [ ] **E2E-ADP-001** — 审批适配器全链路
  > 外部审批系统发出 `APPROVED` 事件 → adapter 校验 → 携带 OIDC 管理员 group token 提交至 Broker →
  > Broker 创建 `AwaitingRedemption` CredentialLease → adapter 通知目标用户 →
  > 用户通过 CLI 登录并 redeem → 获取 elevated kubeconfig。验证五个组件间一致，
  > 审批引用仅存在于内存/通知渠道中，不入 Kubernetes 资源。

- [ ] **E2E-CONC-002** — 同一用户并发 elevated redeem（重放竞争）
  > 两个 CLI 进程同时 redeem 同一个 approval reference。第一个成功并消耗 lease；
  > 第二个被 Broker 重放屏障拒绝。恰好发生一次 TokenRequest，签发一个 kubeconfig。

- [ ] **E2E-FAIL-001** — TokenRequest 响应丢失
  > Broker 在已发出 TokenRequest（API Server 返回 `serviceaccounts/token` 响应）
  > 但尚未写出 HTTP 响应给 CLI 时崩溃。重启后已消耗的 lease 阻塞重放。
  > kubeconfig 永久丢失（一次性签发不变式）。

- [ ] **E2E-AUD-001** — 审计日志完整性（base 和 elevated 流程）
  > 执行完整的 base 签发和 elevated redeem 流程。每条关键操作（Broker API 调用、
  > Lease 状态变更、TokenRequest）均有对应 `AuditEvent`。审计日志中不包含
  > token、kubeconfig 或 Secret 数据。

---

## P1 — 生产发布前必经门禁

- [ ] **E2E-CONC-001** — 同一用户并发 base 请求
  > 同一用户的两个 CLI 进程并行发起 base 请求。各自获得独立的 CredentialLease
  > 和有效 kubeconfig。Controller 管理一个稳定 ServiceAccount；两个 TokenRequest 均成功。

- [ ] **E2E-CONC-003** — 管理员批量 suspend
  > 管理员同时 suspend 10 个以上活跃用户。所有新发行被阻断，稳定 binding 被移除，
  > 活跃 lease 转为 Revoked，Controller 最终清理所有临时资源。

- [ ] **E2E-CTRL-001** — Controller 中断期间用户操作
  > Controller 停止 5 分钟，期间创建用户并发行凭证。Controller 恢复后补建所有
  > 缺失的 ServiceAccount、ClusterRoleBinding 和空 bound Secret。最终状态与未中断时一致。

- [ ] **E2E-CTRL-003** — Controller 资源防误纳管
  > 外部操作删除活跃 CredentialLease 所属的 ServiceAccount 或 ClusterRoleBinding。
  > 若 lease 仍需该资源则 Controller 重建；若 lease 已 terminal 则保持删除。
  > 不遗留孤儿或所有权错误的资源。

- [ ] **E2E-AUTH-001** — Base 凭证 RBAC 权限校验
  > 使用 base kubeconfig 尝试 `kubectl create InternalUser` 和 `CredentialLease`。
  > `base-readonly.v1` profile 拒绝写入操作。

- [ ] **E2E-AUTH-002** — Elevated 凭证 RBAC 权限校验
  > 使用 elevated kubeconfig 尝试创建或删除 ServiceAccount 或 ClusterRoleBinding。
  > Elevated profile 拒绝其授权范围之外的操作。

- [ ] **E2E-AUTH-004** — 已撤销的 admin kubeconfig
  > 尝试使用已被撤销的 admin kubeconfig 执行管理操作。Kubernetes API 拒绝请求；
  > admin CLI 退出并报告明确错误。

- [ ] **E2E-TIME-002** — Token 过期（会话中）
  > 获取 TTL=10 分钟的 elevated kubeconfig。在第 9 分钟成功执行 kubectl 操作，
  > 在第 11 分钟操作失败。Kubernetes API 拒绝过期 token。

- [ ] **E2E-TIME-003** — 过期前手动 revoke
  > 管理员在 elevated lease 自然过期前手动 revoke。Controller 清理临时
  > ServiceAccount 和 binding。后续使用已签发 kubeconfig 的 kubectl 请求被拒绝。

- [ ] **E2E-TIME-004** — Broker 与 OIDC issuer 时钟偏差
  > 在 Broker 和 OIDC issuer 之间引入 +/- 60 秒时钟偏差。Token 的 `nbf`/`exp`
  > 验证在配置允许偏差范围内通过，超出范围拒绝。

- [ ] **E2E-ADP-003** — Redeem 前外部审批被撤销
  > 外部审批系统先后发送 `APPROVED` 和 `REVOKED`（间隔 10 秒以内）。adapter
  > 提交 approval 到 Broker；Broker 将 lease 标记为 Revoked。用户在 redeem 时
  > 发现 reference 已失效。

- [ ] **E2E-INF-001** — 错误信息泄露防护
  > 向 Broker API 发送格式错误的请求体和超大体量的请求。响应仅包含通用错误信息，
  > 不泄露内部配置、文件路径、堆栈或版本字符串。

- [ ] **E2E-AUD-002** — 失败登录审计
  > 同一客户端多次失败的 OIDC 登录。每次尝试生成一条审计记录，包含 IP、issuer、
  > subject；不记录密码、refresh token 或 Secret。

---

## P2 — 韧性与加固

- [ ] **E2E-CONC-004** — Broker 与 Controller 同时重启
  > Broker 在处理 base 请求时 Controller 正在滚动重启。TokenRequest 和 Lease
  > 持久化成功。Controller 恢复后 reconcil 到正确的最终状态。

- [ ] **E2E-FAIL-002** — Broker 在 Lease 持久化后但 HTTP 响应前崩溃
  > Broker 持久化 `CredentialLease`（状态=Consumed）后在写出 HTTP 200 响应前
  > 崩溃。CLI 收到连接错误。重试被 replay barrier 拒绝。不签发重复凭证。

- [ ] **E2E-FAIL-003** — Broker 滚动更新过程中有实时流量
  > 两个 Broker 副本依次滚动更新，期间有 base 请求到达。至少一个副本保持 Ready。
  > 短暂 503 可接受；不会错误或不一致地签发凭证。

- [ ] **E2E-FAIL-004** — Broker 与 Kubernetes API 网络分区
  > Broker 与 Kubernetes API 之间的网络断开 30 秒。TokenRequest 失败；Lease 不
  > 标记为 Prepared/Consumed。网络恢复后后续请求正常。

- [ ] **E2E-CTRL-002** — Controller 长时间中断后的 watch 积压
  > Controller 停止 10 分钟，期间创建大量 InternalUser 和 CredentialLease。
  > Controller 重启后正确处理积压事件，不产生重复资源或 reconcil 错误。

- [ ] **E2E-CTRL-004** — Admission webhook 不可用
  > Controller 的 binding webhook 不可用（fail-closed）。Controller 创建/更新
  > ClusterRoleBinding 的请求被拒绝并重试。Webhook 恢复后操作成功。

- [ ] **E2E-AUTH-003** — 同时使用 base 和 elevated 凭证
  > 用户同时在两个终端持有 base kubeconfig 和 elevated kubeconfig。各自按对应
  > profile 正常工作。Elevated token 过期后 `kubectl auth can-i` 对 elevated
  > 范围返回 false。

- [ ] **E2E-TIME-001** — Approval 过期边界（窗口边缘）
  > 用户在 elevated approval 即将过期前 1 秒 redeem。Broker 拒绝请求（approval
  > 已过期）。用户需重新申请。

- [ ] **E2E-ADP-002** — 审批适配器幂等重试
  > 外部审批系统重试同一个 `APPROVED` 事件（相同 approval ID）。adapter 重新
  > 提交到 Broker；Broker 返回已有 `AwaitingRedemption` lease。不产生重复 lease、
  > 重复通知或重复凭证。

- [ ] **E2E-ADP-004** — OIDC bearer token 轮换
  > Approval adapter 的 OIDC bearer token 过期或撤销。使用旧 token 的请求被 Broker 拒绝。
  > 轮换 token 后 adapter 恢复正常工作，无需手动干预 Broker。

- [ ] **E2E-INF-002** — 过期/撤销 reference 信息泄露
  > 尝试 redeem 已过期或已撤销的 approval reference。Broker 返回通用
  > "not found" 或 "inappropriate state" 错误，不泄露 reference 的原始
  > 目标用户、profile 或 issuer。

- [ ] **E2E-INF-003** — 不使用 CLI 直接访问 Broker
  > 使用 `curl` 访问 Broker API（无 OIDC bearer token）。返回 401 Unauthorized。
  > 不提供任何 kubeconfig 或凭证数据。

- [ ] **E2E-AUD-003** — 管理操作审计
  > 管理员执行创建、suspend、resume 和删除 InternalUser。每条管理操作产生
  > 包含身份、操作和时间戳的审计记录。

- [ ] **E2E-SCALE-001** — Broker 负载下扩缩
  > 在有 base 请求进行中时将 Broker 从 2 副本扩展到 5 副本。新副本加入 Service
  > 并开始服务。所有请求成功，无 5xx 错误。

- [ ] **E2E-SCALE-002** — Broker 优雅缩容
  > 在有请求处理过程中将 Broker 从 5 副本缩容到 2 副本。SIGTERM 允许处理中请求
  > 完成。剩余 2 副本处理所有流量。

---

## 优先级说明

| 优先级 | 含义 |
| --- | --- |
| **P0** | 必须通过后方可部署或开放外部路由。 |
| **P1** | 生产发布前必须通过。P1 例外需经书面安全审查和风险接受。 |
| **P2** | 韧性与加固；不设为发布门禁，但推荐用于生产信心。 |
