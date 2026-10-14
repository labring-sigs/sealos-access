# Sealos Access CLI 合并计划

状态：提议中

最后审阅日期：2026 年 10 月 12 日

本计划将面向人员的 `internal-user-cli` 和受控的
`internal-user-admin` 入口合并为一个名为 `sealos-access` 的可执行文件。
它改变本地命令和分发体验，但不合并服务端授权模型，也不会因此赋予普通
用户管理员权限。由于这些命令尚未正式启用，本次采用一次性切换，不提供
兼容层。

英文主文档为
[`sealos-access-cli-merge-plan.md`](sealos-access-cli-merge-plan.md)。

## 1. 决策

构建并分发一个带有明确管理员命名空间的二进制：

```text
sealos-access login
sealos-access elevated redeem

sealos-access admin create
sealos-access admin approve
```

必须显式使用 `admin` 命名空间；不能通过任意 flag，或仅仅因为某个配置文件
存在，就自动进入管理员模式。

这次合并是命令入口的合并，不是信任边界的合并：

- `login` 和 `elevated redeem` 仍然是面向人员的 OIDC 流程；
- `admin create` 仍然由传给 Kubernetes API 客户端的 Kubernetes 凭证授权；
- `admin approve` 仍然由 Broker 配置的精确 OIDC admin group 检查授权；
- 即使一个二进制暴露全部命令，Broker 和 Kubernetes API 仍然是最终授权方。

## 2. 范围

### 包含

- 增加统一的 `sealos-access` 命令分发器和子命令解析器；
- 保留普通凭证签发、高权限兑换、身份创建和手动审批的现有行为；
- 在合并后的 CLI 内隔离普通用户和管理员配置加载；
- 在安全的前提下共享 OIDC、HTTP/TLS、校验和输出辅助逻辑；
- 更新 Make 目标、Dockerfile、测试 fixture、命令文档和架构引用；
- 在同一变更中移除尚未正式启用的拆分入口和构建目标。

### 不包含

- 修改 Broker endpoint、lease 语义、role profile 或 TTL 策略；
- 为审批 endpoint 引入 mTLS 认证；
- 允许普通用户创建 `InternalUser` 或提交审批；
- 增加面向用户的高权限申请 endpoint；
- 将 OIDC token、kubeconfig、私钥或 approval reference 存入共享默认配置。

## 3. 命令与授权矩阵

| 命令 | 当前行为 | 凭证来源 | 最终的服务端检查 |
| --- | --- | --- | --- |
| `sealos-access login` | 签发调用者的 base 凭证 | OIDC Authorization Code + PKCE | Broker 校验 issuer、audience、claims、active target 和固定 profile |
| `sealos-access elevated redeem` | 兑换一个已批准的高权限 reference | 新鲜 OIDC 登录 + 一次性 reference | Broker 从 OIDC 推导 requester 并校验持久化 lease |
| `sealos-access admin create` | 创建或协调一个 `InternalUser` | 管理员 kubeconfig | Kubernetes 认证/RBAC 和 `InternalUser` admission 校验 |
| `sealos-access admin approve` | 提交手动或破窗高权限审批 | OIDC Authorization Code + PKCE | Broker 校验 OIDC token 和精确的 admin group，并继续校验 target/profile/TTL/state |

CLI 可以提前拒绝明显无效的参数组合，但客户端解析成功不代表获得授权。
尤其是 `admin` 子命令不能依赖本地 `--admin` 开关、源 IP、approval reference
或调用者自己提供的 group 值。

## 4. 安全边界要求

### 4.1 显式路由

在加载默认值或创建客户端之前解析第一个命令段：

- 用户命令只能加载用户默认配置，并创建 public OIDC/Broker 客户端；
- `admin create` 只能加载管理员默认配置，并创建 Kubernetes API 客户端；
- `admin approve` 只能加载管理员审批默认配置，并创建该 admin-group 流程所需的 OIDC/Broker 客户端。

未知命令、放错位置的管理员 flag 和多余位置参数都必须 fail closed。

### 4.2 配置隔离

新的默认路径应当位于同一个 `sealos` 目录下，但彼此独立：

```text
${XDG_CONFIG_HOME}/sealos/access/user.yaml
${XDG_CONFIG_HOME}/sealos/access/admin.yaml
```

实现不能合并两个 schema。用户命令不能回退读取 `admin.yaml`，管理员命令
也不能静默读取 `user.yaml`。不需要保留旧路径回退。配置文件仍然只能包含
路径和非敏感默认值。

### 4.3 服务端最终授权

合并后的二进制不能削弱现有 Broker 或 Kubernetes 检查：

- Broker 继续从已验证的 OIDC claims 推导普通和高权限请求者；
- Broker 继续固定 base 和 elevated profile 以及 target 数据；
- `/v1/clusters/{clusterID}/internal/credentials/approve` 继续在完成 issuer、audience、签名和时间
  校验后要求精确配置的 OIDC `groups` 值；
- `admin create` 继续依赖 Kubernetes RBAC，而不是 OIDC admin group；
- `InternalUser` webhook 继续负责身份形状、profile 和确定性命名的最终校验。

### 4.4 本地敏感信息处理

合并后的二进制必须保留现有输出保护：

- 绝不打印 access token 或 kubeconfig 内容；
- 只向明确选择的本地普通文件写入 kubeconfig，权限为 `0600`，并拒绝符号链接目标；
- 不把 token、kubeconfig、私钥或 approval reference 放进任一默认 YAML 文件；
- 避免记录包含 approval reference 或管理员凭证路径的命令行值。

## 5. TODO：实现

### TODO 1：抽取命令实现

- [x] 将当前用户和管理员逻辑从两个独立的 `main` 包中抽取为可独立测试的
  命令处理器。
- [x] 继续保持用户和管理员配置类型分开。
- [x] 只共享真正通用的辅助逻辑，例如 OIDC PKCE state、HTTPS client、URL
  校验和安全响应解码。
- [x] 不修改 Broker 请求 body 或授权 header。

### TODO 2：增加 `sealos-access` 分发器

- [x] 实现 `login`、`elevated redeem`、`admin create` 和 `admin approve` 的
  命令优先路由。
- [x] 让帮助信息体现用户/管理员分工。
- [x] 让每个子命令只暴露自己的 flags。

### TODO 3：隔离配置

- [x] 实现新的用户/管理员默认路径。
- [x] 实现显式 `--config` 处理。
- [x] 严格隔离 schema，不保留旧可执行文件名称和配置路径。

### TODO 4：更新构建和分发面

- [x] 更新 user Makefile、Dockerfile、mock CLI 镜像、部署元数据、验收命令和
  生成的帮助文本。
- [ ] 合并后的 artifact 包含管理员代码路径，必须使用与当前管理员 artifact
  相同的签名和发布控制。仓库当前没有独立 CLI artifact 的签名 workflow，
  因此该项仍是发布前的 gate。

## 6. TODO：验证与验收

- [x] Parser 测试覆盖所有命令、help、未知命令、放错位置的管理员 flag 和多余
  参数。
- [x] 用户命令不能加载管理员默认配置，也不能创建 Kubernetes client。
- [x] `admin create` 没有有效 Kubernetes 授权时不能执行。
- [x] 没有配置的 OIDC admin group 时，`admin approve` 被 Broker 拒绝；只有携带
  精确 group 且 token 已验证时才能成功。
- [x] Base 签发和高权限兑换保留现有 OIDC、reference、profile、TTL、kubeconfig
  和文件权限行为。
- [x] 配置测试覆盖新路径、严格的用户/管理员 schema 隔离和未知 YAML 字段。
- [x] 现有 user、admin、Broker、approval 和 acceptance 测试都改用合并后的命令面。

## 7. TODO：上线

- [ ] 落地命令分发器和单元测试，并在同一变更中删除尚未正式启用的拆分入口和构建目标。
- [ ] 更新本地文档、Make 目标、Dockerfile 和 mock 验收命令。
- [ ] 将 `sealos-access` 作为唯一支持的命令发布。
- [ ] 先执行普通和高权限用户验收，再执行管理员 create/approve 以及明确的授权失败验收。
