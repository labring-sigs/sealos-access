# Sealos Access

Sealos 的独立内部用户凭据访问组件。

本仓库包含 `InternalUser` 和 `CredentialLease` API、Controller 与 Admission
组件、多集群 Broker、审批适配器接口以及 `sealos-access` CLI。

## Helm Charts

部署有意拆成两张 chart：控制 chart 只安装到控制集群，目标 chart 分别安装到每个生产/目标集群。
除非某个集群明确同时承担两种角色，否则不要把两张 chart 安装到同一个集群。

### 控制集群

`charts/sealos-access-control-plane` 安装中央 Broker、审批适配器、Service、cert-manager
证书和限制性 NetworkPolicy。它不会安装 CRD、InternalUser Controller 或生产集群的目标 RBAC。

```sh
helm upgrade --install sealos-access-control-plane \
  ./charts/sealos-access-control-plane \
  --namespace access-broker-system \
  --create-namespace \
  --values examples/control-plane-values.yaml
```

为每个生产集群配置 `broker.targets`。Broker 会从由
`broker.targetCredentialsSecretName` 指定的已有 Secret 中读取目标集群 CA 和 bearer token，
key 必须是 `<target-id>.ca.crt` 和 `<target-id>.token`。不要把这些内容放入 Helm values 或 Git。

审批适配器同样依赖已有的 `adapter.broker.caSecretName` 和
`adapter.broker.bearerTokenSecretName` Secret。后者必须包含配置的 bearer-token key，也可以包含
`smtp-username` 和 `smtp-password`。

### 生产/目标集群

`charts/sealos-access-target-cluster` 安装两个 CRD、InternalUser Controller、Controller webhook、
RBAC admission 服务、审核过的权限 profile，以及最小权限的 Broker 目标侧 ServiceAccount/RBAC。

```sh
helm upgrade --install sealos-access-target-cluster \
  ./charts/sealos-access-target-cluster \
  --namespace access-controller-system \
  --create-namespace \
  --values examples/target-cluster-values.yaml
```

对 `broker.targets` 中的每个生产集群分别执行一次安装。目标 chart 会创建目标侧资源所需的
`access-system` 和 `access-broker-system` namespace。

## 前置条件与安全注意事项

- 默认通过 cert-manager 签发证书并注入 webhook CA；即使关闭
  `*.tls.createCertificate`，Admission webhook 仍需要等价的 CA 注入方案。
- 两张 chart 默认创建固定 namespace；当 `--create-namespace` 预先创建 Helm release namespace
  时会自动跳过它。如果 namespace 由外部系统管理，可设置 `namespaces.create: false`。
- 需要支持 NetworkPolicy 的 CNI。默认开启 default-deny；生产上线前通过
  `broker.networkPolicy.additionalEgress` 和 `adapter.networkPolicy.additionalEgress` 增加精确的
  API、OIDC、SMTP 和审批系统 CIDR 或 selector。
- 使用外部密钥管理系统或 `kubectl create secret` 创建目标凭据、OIDC CA、Broker CA 和适配器凭据。
- 启用目标前，替换所有示例 OIDC URL、目标 endpoint、cluster ID、SMTP 配置和源 IP CIDR。
- 每个目标集群上线前检查生成的 RBAC，并执行 `kubectl auth can-i` 验证。

## 构建与测试

```sh
make build
make test
```
