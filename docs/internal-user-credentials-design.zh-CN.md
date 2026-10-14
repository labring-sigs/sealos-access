# 内部用户凭证方案讨论归档

状态：仅归档设计。本文记录 Q1-Q82 已确认的结论，不是实现说明，也不是上线批准。
本方案中的 InternalUser 和 CredentialLease 当前均不视为已部署。

## 1. 范围和原则

- 一个 InternalUser 只对应一个真实内部人员，禁止多人共享账号。
- 内部用户不创建 Sealos namespace，也不创建现有的标准 User 资源。
- 日常凭证使用 Kubernetes ServiceAccount TokenRequest。客户端证书不纳入日常流程，
  只保留给独立的 break-glass 管理员体系。
- 强制最小权限、失败关闭、短时凭证、一次性发放和集中审计。
- kubeconfig 仍是 bearer credential。文件被复制后可以被他人冒用；独立账号和审计
  不能密码学证明实际持有文件的人是谁。

## 2. 身份和生命周期

InternalUser.spec 只包含：

    identity:
      issuer: https://<白名单中的 OIDC issuer>
      subject: <不透明且不可变的 OIDC subject>
    roleProfile: base-readonly.v1
    suspend: false

issuer 必须是服务端配置白名单中的 HTTPS issuer。subject 是不透明值，issuer 和
subject 创建后都不能修改。调用方不能提交可变的人类 username，Controller 根据
issuer 和 subject 生成稳定的 Kubernetes 标识：

    iu-<sha256(issuer + NUL + subject) 的前 24 位十六进制字符>

这个标识用于稳定 ServiceAccount 名称，并只出现在 status，不作为身份输入。对象名
必须和身份推导结果一致。这样改名不能接管他人的凭证，同一身份的重复创建也会稳定
冲突。

每个人长期保留一个 InternalUser 和一个位于 access-system 的稳定
ServiceAccount，凭证按次签发。

spec.suspend: true 必须立即：

1. 拒绝新的签发请求；
2. 删除稳定账号的基础权限绑定；
3. 撤销并清理该人员所有活跃 Lease 及其绑定对象。

暂停时保留 ServiceAccount 和身份对象，恢复时重新绑定基础权限。永久撤销通过删除
InternalUser 完成；finalizer 必须先清理所有绑定、bound object、临时
ServiceAccount 和稳定 ServiceAccount，之后才能移除身份对象。

## 3. API 和资源位置

方案包含 user.sealos.io/v1 下的两个资源：

- 集群级 InternalUser；
- namespaced CredentialLease，固定放在 access-broker-system。

CredentialLease.spec 记录 target、requester 身份、固定 profile、请求 TTL 和可选
外部审批引用。status 只记录生命周期和资源引用，绝不能保存 token、kubeconfig、
私钥或 Secret 数据。

专用 namespace：

- access-system：稳定人员 ServiceAccount、临时 ServiceAccount 和空的
  TokenRequest bound Secret；
- access-broker-system：CredentialLease 和 Broker；
- access-controller-system：独立部署的 Controller 和 webhook。

现有 User controller 保持独立，不改变其行为。

## 4. 权限档位

权限档位由平台维护、固定并版本化。调用方只能选择 profile 名称，不能提交任意
RBAC 规则或任意 ClusterRole。

### base-readonly.v1

- 绑定到人员的稳定 ServiceAccount；
- 最大凭证 TTL 为 24 小时；
- 只允许实际运维命令所需的、明确列出的 get/list/watch；
- 不使用 Kubernetes 通用 view 角色；
- 明确排除 Secret、RBAC、身份资源、节点凭证、webhook 配置及未评审资源。

### cluster-ops-write.v1

- 只允许临时绑定，绝不修改稳定 ServiceAccount 的权限；
- 每个 Lease 创建独立的临时 ServiceAccount 和绑定；
- 最大凭证 TTL 为 1 小时；
- 必须有由授权管理员创建或批准的外部审批记录，并在签发时提供不可变的外部审批引用；
- 上线前必须结合 admission policy 评审具体资源清单。特别是，如果能创建带有其他
  ServiceAccount 的 Pod 或 workload，workload 写权限可能转化为提权路径。

profile 名称和对应 ClusterRole 按版本保持不变。新增权限必须创建新的 profile 版本
并重新评审，不能静默扩大旧版本权限。

## 5. CredentialLease 状态机

允许的单向迁移：

    Pending -> Prepared -> Issued
    Pending -> Failed
    Prepared -> Failed
    Prepared -> Revoked
    Issued -> Expired
    Issued -> Revoked

Failed、Revoked、Expired 是终态，Broker 和 Controller 不能把终态 Lease 改回可签发
状态。

每个 Lease 由 Controller 创建一个唯一的空 Opaque bound Secret。Secret 只保存引用
和元数据，不保存 token；它作为 TokenRequest 的 bound object，删除它即可按照
Kubernetes bound token 语义使对应 token 失效。

高权限 profile 额外创建临时 ServiceAccount 和 ClusterRoleBinding。清理顺序固定为：

1. 删除 RoleBinding 或 ClusterRoleBinding；
2. 删除 bound Secret；
3. 删除临时 ServiceAccount；
4. 确认所有子资源已经消失后移除 Lease finalizer。

每次接管或删除前都必须校验 owner 和 Lease labels。owner 不匹配时失败关闭：不接管、
不修改、不删除，保留 finalizer 并告警，等待人工处理。

API Server 返回的 TokenRequest.status.expirationTimestamp 是有效期事实来源。请求
TTL 只是上限，不能代替实际过期时间；实际过期时间必须写入 Lease status，并由
Controller 按此清理。周期性扫描需要补偿事件丢失、重启和异常，目标清理延迟小于
1 分钟。

如果 TokenRequest 成功但响应丢失，Lease 仍视为已消耗。CLI 必须创建新的 Lease，
原 token 不能从 Kubernetes 读取或再次获取。

## 6. Credential Broker

Broker 独立部署在 access-broker-system。

普通接口：

- 使用 OIDC 认证调用方；
- 从已验证 OIDC 身份推导 requester 和 target；
- 忽略请求体中的 target；
- 只自动签发 base-readonly.v1；
- 只允许本人撤销自己的 Lease。

管理员接口用于受控的 InternalUser 生命周期，与普通凭证接口分离。`sealos-access admin` 的
`approve` 操作是手动/破窗适配器能力：它使用携带服务端精确配置管理员 group 的新鲜 OIDC
token 提交已经批准的记录。管理员命名空间和适配器都不能直接写 CredentialLease。

审批 reference 只是审计字段，不是认证因子。第一版审批记录由 外部审批系统和适配器完成，也可以
通过批准的破窗 CLI 手动提交。备注或消息链接本身不能授权 Broker。reference 通过配置的受
保护通知渠道发送给目标用户，审批回调 endpoint 不返回 reference。

Broker 创建 Lease spec 并等待 Controller 将其准备为 Prepared，然后：

1. 再次检查 target、requester、profile、status、资源引用和暂停状态；
2. 在不可逆的 TokenRequest 之前先标记 Lease 已消耗；
3. 使用服务端固定的 Kubernetes API audience 调用 serviceaccounts/token；
4. 校验返回 token 的 audience 和实际过期时间；
5. 写入 Issued；
6. 只返回一次 kubeconfig。

Broker 不能创建身份、读取 Secret、创建或修改任意 RBAC，也不能为非 Active 身份
签发 token。Broker 自身使用短 TTL、固定 audience 的 projected ServiceAccount token；
关闭默认 automount，不创建长期 Broker Secret。

Broker 的 Kubernetes 权限只允许读取必要的目标身份、创建和更新自己 namespace
中的 Lease，以及在 access-system 调用 TokenRequest。普通身份解析应通过确定性
名称 Get 完成，不能为了查找调用者而列出全部身份。

## 7. 认证和 CLI

人员 CLI 使用 OIDC Authorization Code + PKCE：

- 使用不带 client secret 的 public client；
- 通过本机 loopback 接收回调；
- 校验 state 和 S256 PKCE；
- 不持久化 refresh token。

高权限流程从外部审批系统开始，普通用户不能调用 Broker 发起高权限申请。审批表单
必须包含用户不透明且不可变的 OIDC subject、申请 TTL 和人工可读的原因。适配器在部署配置中
固定 OIDC issuer 和允许的审批定义，这些值不能由回调 payload 提供。

默认流程如下：

    外部审批系统 -> 无状态适配器回调 -> Broker internal approve
    -> Broker 持久化 CredentialLease 并生成 reference
    -> 适配器通知目标用户 -> 用户 CLI 使用 OIDC + reference redeem

适配器校验回调 token 和可选的外部系统签名，然后从外部审批系统 API 重新读取完整审批实例。它只
接受已配置审批定义的 APPROVED 实例，按服务端 profile 策略校验表单字段，查询发起人的邮箱，
再通过携带服务端精确配置管理员 group 的 OIDC token 调用
`POST /v1/clusters/{clusterID}/internal/credentials/approve`。适配器没有 Kubernetes 凭据，也不直接写
CredentialLease。它不保存事件消费状态。外部审批系统重试时使用相同的审批实例 code
再次提交，由 Broker 的幂等键返回原 reference；通知失败也可以安全重试。

internal approve 接口只接受 `cluster-ops-write.v1`，验证目标 InternalUser 为 Active，创建
`AwaitingRedemption` CredentialLease 并生成一次性 reference。用户兑换前不会创建临时
ServiceAccount、Secret 或 ClusterRoleBinding。审批授权和申请 TTL 由 Broker 持久化，适配器
不拥有审批状态；外部 `approvalID` 是幂等键。

用户调用 `POST /v1/clusters/{clusterID}/credentials/elevated/redeem` 时只提交 reference。Broker 认证用户，从持久化
Lease 读取目标和 TTL，将 requester/target 绑定为已验证 OIDC identity，并在继续签发前通过
status CAS 消费 Lease。reference 不是认证因子，只有目标用户的新 OIDC 登录与之组合才有效。
旧的 `POST /v1/credentials/elevated/request` endpoint 已移除。

Broker 校验 issuer、audience、签名、subject、过期时间和 not-before。issuer 和
audience 都由服务端配置，调用方不能自定义。OIDC Broker audience 与 Kubernetes
TokenRequest audience 分开配置。

第一版只提供 Broker API 和 CLI，不立即开发完整门户。CLI：

CLI 的构建和使用说明见
[`internal-user-credentials-cli.md`](internal-user-credentials-cli.md)，中文版本见
[`internal-user-credentials-cli.zh-CN.md`](internal-user-credentials-cli.zh-CN.md)。

- 不打印 token 或 kubeconfig；
- 只写入用户明确指定的文件；
- 拒绝符号链接输出路径；
- 文件权限设置为 0600；
- 只显示 issuer、推导 username、profile、Lease ID 和过期时间。

CLI 可以从 YAML 配置文件加载这些非敏感默认值，命令行参数优先覆盖配置文件。配置文件可以
包含 Broker/OIDC URL、TLS 校验路径、public client ID、请求 TTL、浏览器/登录偏好和输出路径，
但不能包含 token、kubeconfig、client secret 或一次性审批 reference。标准路径和 `--config`
行为以面向内部人员的 CLI 使用说明为准。管理员 CLI 使用独立配置文件，只包含 kubeconfig
路径、issuer 和 subject；路径及优先级规则以管理员 CLI 使用说明为准。

高权限 CLI 只提供 `elevated redeem`。用户从外部审批流程收到 Broker 生成的 reference 后，
CLI 重新进行 OIDC 登录，并只发送 reference；兑换出的 kubeconfig 按普通凭证相同的 0600 规则
写入。CLI 没有用户侧的 elevated request 命令。

受控的 `sealos-access admin` 命名空间直接访问 Kubernetes，负责创建、暂停、恢复和删除
InternalUser；同时提供通过新鲜 OIDC 登录调用 Broker HTTPS endpoint 的 `approve` 手动/破窗
操作。它必须携带服务端精确配置的管理员 OIDC group；不能读取 Secret、创建 RBAC 或调用
TokenRequest。Broker 不能创建新身份。统一的 `sealos-access` 二进制实现用户和管理员操作，
构建和使用约定见 [`internal-user-admin-cli.zh-CN.md`](internal-user-admin-cli.zh-CN.md)。

第一版不定义自动的人员目录同步组件。受控管理员 CLI 和管理员流程是 InternalUser
生命周期的事实来源。因此人员目录的可用性不是 Broker 的依赖，也不属于 BROKER-008
的验收条件。

## 8. Controller 职责

InternalUser/CredentialLease Controller 独立部署，使用窄权限 ServiceAccount：

- 管理稳定 ServiceAccount 和基础权限绑定；
- 准备、撤销和清理临时 Lease 资源；
- 更新 API status 和 finalizer；
- 不调用 TokenRequest；
- 不读取 token 数据；
- 不管理现有标准 User 的生命周期。

Controller 有一个明确的权限例外：它必须创建和删除稳定 profile 以及 Lease 专属 profile
使用的平台 ClusterRoleBinding。Kubernetes RBAC 不能按对象名称或 label 限制动态
ClusterRoleBinding 的 `create` 请求。因此该权限必须视为可信 Controller 边界，并在生产
环境通过 admission policy 或等价的隔离 RBAC signer 保护，限制 role reference、subject、
名称和 owner label 只能匹配平台生成的 binding 形状。Broker 自身没有 RBAC 写权限。如果
没有额外的 admission 或 signer 控制，Controller 被攻破后的威胁模型未闭环，不能批准生产
上线。

暂停时删除基础绑定；删除身份时必须等待 Lease 清理完成后再移除 finalizer。
Controller 和 Broker 都执行 profile、TTL 等限制，避免单个组件失控后扩大凭证能力。

## 9. 网络、审计和故障行为

Broker Service 不直接暴露。外部流量只能经过 Higress/WAF，WAF 必须执行：

- 办公网或明确授权的 IP 段限制；
- 限流；
- 请求大小限制；
- 来源和必要请求头校验。

允许的 IP 不能替代 OIDC 身份认证；Broker 仍必须强制 OIDC，管理员接口还要强制
管理员 group。

Broker 还要应用服务端配置的进程内请求限流和 burst 限制，作为纵深防御。默认值为每秒
10 个请求、burst 20，健康检查不消耗限流额度。该限制按每个 Broker 副本分别计算，不能
替代集群级 Higress/WAF 限流。超限请求返回安全的 `429`，且不会进入凭证签发流程。

Broker、Controller 和审批适配器 namespace 默认拒绝网络流量。Broker 允许 Higress 入站，
并只为 OIDC admin-group approve endpoint 允许来自适配器 namespace 的入站；Broker egress 只允许访问
Kubernetes API、OIDC/JWKS 和受保护的审计/日志出口。适配器没有 Kubernetes API 权限，egress
只允许外部审批系统 API、SMTP relay 和 Broker Service。`access-system` 不允许普通业务 Pod
访问；适配器的回调入口如果对外使用，也必须经过配置的 Higress/WAF 路径。

Kubernetes Audit Policy 对 serviceaccounts/token 只记录 Metadata，不能记录 Request
或 Response。Broker 结构化日志和 Kubernetes audit 发送到受保护的集中系统，记录
subject、requester、profile、Lease ID、审批引用、来源 IP、user-agent、结果和时间，
绝不记录 token、kubeconfig、refresh token 或 Secret 数据。

OIDC 或审批系统不可用时：

- 停止新的签发和提权；
- 已签发 token 只按原过期时间继续有效；
- 撤销和清理链路仍必须可用；
- 不能使用缓存审批，也不能 fail-open。

## 10. 测试和上线门禁

开放任何外部入口前，测试集群必须覆盖：

- 身份重复、改名和接管尝试；
- issuer 白名单、身份不可变和 profile 不可变；
- 普通人员只能自助签发，管理员才能代签；
- profile 白名单、固定 audience、TTL 上限和实际过期时间；
- TokenRequest 成功但响应丢失；
- 暂停、自助撤销、管理员撤销和删除；
- 临时提权不改变稳定账号绑定；
- owner 冲突和失败关闭清理；
- 重启、漏事件后的 finalizer 清理；
- 审计脱敏以及 token/kubeconfig 不落日志。

可以使用仅用于测试的内存 OIDC issuer 和 Testcontainers，覆盖 Discovery、签名
JWT/JWKS 校验和 Authorization Code + PKCE，不把测试身份系统引入生产。测试 provider
不能持久化生产凭证或 token。

在 profile 资源清单、admission policy、issuer 配置、WAF CIDR、NetworkPolicy、审计
留存策略和管理员绑定分别完成明确评审前，本方案不具备生产上线条件。
