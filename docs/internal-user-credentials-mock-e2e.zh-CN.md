# Internal User 凭据 Mock 端到端测试流程

状态：测试环境和验收环境运行手册。

归档日期：2026-09-15。

本文使用 `./test/mock` 下的 mock 组件，对 `InternalUser` 和
`CredentialLease` 执行端到端测试。流程分为快速本地 acceptance 测试和手工
Kubernetes 集群测试两部分。

英文主文档见
[internal-user-credentials-mock-e2e.md](internal-user-credentials-mock-e2e.md)。

## 1. 范围与当前限制

Mock 组件包括：

| 组件 | 替代对象 | 用途 |
| --- | --- | --- |
| `test-oidc-provider` | 外部 OIDC issuer | Authorization Code + PKCE、JWKS、可配置用户和 claims |
| `mock-approval-server` | 外部审批 API | 创建、查询、通过和拒绝审批记录 |
| `mailhog` | SMTP relay | 通过 Web UI 捕获通知邮件 |
| `sealos-access` 调试 Pod | 本地 CLI 工作机 | 在集群内部执行统一 CLI 和 `curl` |

当前实现有两个细节会影响测试过程：

1. 仓库中的 `test-oidc-provider` Deployment 默认只监听 HTTP，而 Broker 和
   `sealos-access` 强制要求 HTTPS。下文的集群流程会为 provider 增加 TLS，
   并显式配置 Broker 和 CLI 信任该证书。
2. `cmd/internal-user-approval-adapter` 仍是集成骨架，默认不会选择
   `mock.NewMockSource` 和 `approvaladapter.EmailNotifier`。因此集群流程使用
   受支持的管理员手工审批路径：先由 `mock-approval-server` 创建权威审批记录，
   再通过 `sealos-access admin approve` 提交给 Broker。下文的本地 acceptance
   测试会覆盖完整的 source -> adapter -> Broker -> Controller -> redeem 链路。

Mock OIDC client 必须使用不带固定端口的 redirect URI，例如
`http://127.0.0.1/callback`，因为 CLI 会监听一个随机 loopback 端口。

## 2. 本地 acceptance 流程

这是在不需要 Kubernetes 集群的情况下，最快验证完整进程内行为的方法。

```sh
export REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

# OIDC discovery、PKCE、authorization code 消耗和 token mint。
go test ./test/mock/oidc-provider/... \
  -run 'Test(DiscoveryAndJWKS|AuthorizationCodePKCE|AuthorizationCodeConsumesCodeOnPKCEFailure|MintToken)$' \
  -count=1 -v

# mock 外部审批 -> adapter -> Broker -> CredentialLease -> redeem。
go test ./pkg/approvaladapter/mock \
  -run '^TestApprovalAdapterToBrokerServerE2E$' \
  -count=1 -v

# CLI reference redeem -> Broker -> Controller -> TokenIssuer。
go test -tags acceptance ./internal/access/usercmd \
  -run '^TestElevatedRedeemPreOIDC$' \
  -count=1 -v

# acceptance notification sink 和高权限 CLI package。
go test -tags acceptance \
  ./internal/access/usercmd \
  ./pkg/approvaladapter/acceptance \
  -count=1 -v

# InternalUser 和 CredentialLease 生命周期测试。
go test ./controllers/internalcredentialstest -count=1 -v
```

Acceptance OIDC issuer、audience 和管理员 group 必须与 Broker 配置一致。管理员审批
流程使用新鲜 OIDC token，不使用客户端证书。

完整的 `go test ./controllers/...` 还需要 envtest assets：

```sh
KUBEBUILDER_ASSETS="$(setup-envtest use 1.32.0 -p path)" go test ./controllers/...
```

没有这些 assets 时，envtest 会因为缺少 `etcd` 或 `kube-apiserver` 二进制而失败。

## 3. 集群拓扑与前置条件

```text
sealos-access
    |
    | OIDC Authorization Code + PKCE
    v
test-oidc-provider
    |
    | OIDC access token
    v
internal-credential-broker
    |
    | create/update
    v
CredentialLease
    |
    | watch / reconcile
    v
internal-user-controller
    |
    +--> 稳定 ServiceAccount
    +--> base ClusterRoleBinding
    +--> 临时 ServiceAccount / binding / 空 bound Secret
```

前置条件：

- 一个具有 cluster-admin 权限的测试 Kubernetes 集群。
- `kubectl`、Docker、OpenSSL、Python 3 和兼容的 Go toolchain。
- cert-manager 或等价证书签发机制。
- 所有节点都能访问的镜像仓库，或其他镜像加载机制。
- Broker 前面需要一个可信代理。每个需要认证的 Broker 请求都必须来自
  `trusted-proxy-cidrs` 内的 peer，并携带且只携带一个 `X-Trusted-Client-IP` 头，
  其值落在 `allowed-client-cidrs` 内。生产环境使用 Higress；本文的手工集群流程
  会启动一个仅用于测试的本地代理来注入该头。
- 允许修改运行 CLI 的工作机上的 `/etc/hosts`。
- 允许修改 Broker NetworkPolicy，使其能访问 mock OIDC provider（6.3 节）。
- 运行下列命令前执行 `export KUBECONFIG="${KUBECONFIG:-$HOME/.kube/config}"`；
  管理员 CLI 需要显式 kubeconfig。
- 不得使用生产凭据或生产审批记录。

预期断言：

| 阶段 | 必须结果 |
| --- | --- |
| 创建 `InternalUser` | Phase 为 `Active`；稳定 ServiceAccount 和 base binding 存在 |
| Base lease | `Pending -> Prepared -> Issued`；kubeconfig 可读取 Pods |
| Base 授权 | 不能创建 `InternalUser` 或 `CredentialLease` |
| Elevated approval | Lease 为 `AwaitingRedemption`，并包含 approval reference |
| Elevated redemption | Lease 为 `Issued`；返回 elevated kubeconfig |
| Elevated 资源 | 临时 ServiceAccount、binding 和空 Secret 属于该 lease |
| Elevated 授权 | 可创建 Events；不能创建 Deployment 或修改 RBAC |
| 重放 | 首次消费后，同一个 reference 被拒绝 |
| 清理 | 终态 lease 资源被删除；稳定资源保留到用户删除 |

## 4. 构建和部署

设置镜像仓库前缀：

```sh
export REGISTRY=registry.example.internal/test
```

构建控制面二进制：

```sh
export REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

make \
  build-internal-user-controller \
  build-broker \
  build-internal-user-admission \
  build-sealos-access \
  TARGETARCH=amd64
```

构建控制面镜像：

```sh
docker build --build-arg TARGETARCH=amd64 \
  -f Dockerfile.internal-user-controller \
  -t "$REGISTRY/sealos-internal-user-controller:latest" .

docker build --build-arg TARGETARCH=amd64 \
  -f Dockerfile.broker \
  -t "$REGISTRY/sealos-internal-credential-broker:latest" .

docker build --build-arg TARGETARCH=amd64 \
  -f Dockerfile.internal-user-admission \
  -t "$REGISTRY/sealos-internal-user-admission:latest" .
```

构建 mock 镜像：

```sh
# mock 构建上下文是仓库根目录。
docker build \
  -f test/mock/oidc-provider/Dockerfile \
  -t "$REGISTRY/sealos-test-oidc-provider:latest" .

docker build \
  -f test/mock/approval-server/Dockerfile \
  -t "$REGISTRY/sealos-mock-approval-server:latest" .
```

CLI Dockerfile 要求以 `controllers` 目录作为构建上下文：

```sh
cd "$REPO_ROOT"
docker build \
  -f test/mock/cli/Dockerfile \
  -t "$REGISTRY/sealos-access:latest" .
```

推送所有镜像：

```sh
docker push "$REGISTRY/sealos-internal-user-controller:latest"
docker push "$REGISTRY/sealos-internal-credential-broker:latest"
docker push "$REGISTRY/sealos-internal-user-admission:latest"
docker push "$REGISTRY/sealos-test-oidc-provider:latest"
docker push "$REGISTRY/sealos-mock-approval-server:latest"
docker push "$REGISTRY/sealos-access:latest"
```

部署 CRD 和组件：

```sh
cd "$REPO_ROOT"

kubectl apply -f config/crd/bases/user.sealos.io_internalusers.yaml
kubectl apply -f config/crd/bases/user.sealos.io_credentialleases.yaml

kubectl apply -k config/internal-user-controller
kubectl apply -k config/broker
kubectl apply -k config/internal-user-admission
kubectl apply -k test/mock
```

将默认镜像替换为上文构建的镜像：

```sh
kubectl -n access-controller-system set image deploy/internal-user-controller \
  controller="$REGISTRY/sealos-internal-user-controller:latest"

kubectl -n access-controller-system set image deploy/internal-user-admission \
  admission="$REGISTRY/sealos-internal-user-admission:latest"

kubectl -n access-broker-system set image deploy/internal-credential-broker \
  broker="$REGISTRY/sealos-internal-credential-broker:latest"

kubectl -n access-mock set image deploy/test-oidc-provider \
  test-oidc-provider="$REGISTRY/sealos-test-oidc-provider:latest"

kubectl -n access-mock set image deploy/mock-approval-server \
  mock-approval-server="$REGISTRY/sealos-mock-approval-server:latest"

kubectl -n access-mock set image deploy/sealos-access \
  cli="$REGISTRY/sealos-access:latest"
```

注意：Broker 镜像必须从本仓库构建。9.3 节使用的内部审批 endpoint
（`/v1/clusters/{clusterID}/internal/credentials/approve`）位于仓库的 `cmd/broker`；更早或只用于
acceptance 的 Broker 镜像可能对该路径返回 HTTP 404。如果镜像仓库需要认证，
请先确认 Deployment 的 `imagePullSecrets` 可用；失效的 pull secret 会导致
`ImagePullBackOff`，即使镜像本身可以被匿名拉取。

等待所有 Deployment：

```sh
kubectl -n access-controller-system rollout status deploy/internal-user-controller
kubectl -n access-controller-system rollout status deploy/internal-user-admission
kubectl -n access-broker-system rollout status deploy/internal-credential-broker
kubectl -n access-mock rollout status deploy/test-oidc-provider
kubectl -n access-mock rollout status deploy/mock-approval-server
kubectl -n access-mock rollout status deploy/mailhog
kubectl -n access-mock rollout status deploy/sealos-access
```

## 5. 为 OIDC mock 增加 TLS

设置测试变量：

```sh
export MOCK_NS=access-mock
export BROKER_NS=access-broker-system
export CTRL_NS=access-controller-system
export USER_NS=access-system

export OIDC_HOST=test-oidc-provider.access-mock.svc
export OIDC_ISSUER="https://${OIDC_HOST}:8443"
export OIDC_AUDIENCE=test-oidc-audience
export OIDC_CLIENT_ID=test-oidc-client

export BROKER_HOST=internal-credential-broker.access-broker-system.svc

export TEST_SUBJECT=test-user-001
export API_SERVER="$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')"
export CLUSTER_ID=mock-cluster

# CLI 访问仅用于测试的可信代理；管理员 CLI 通过 port-forward 直连 Broker。
export BROKER_PROXY_URL="https://127.0.0.1:19443"
export BROKER_DIRECT_URL="https://127.0.0.1:9443"
```

创建服务端证书：

```sh
kubectl -n "$MOCK_NS" apply -f - <<'YAML'
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: test-oidc-selfsigned
  namespace: access-mock
spec:
  selfSigned: {}
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: test-oidc-provider-tls
  namespace: access-mock
spec:
  secretName: test-oidc-provider-tls
  dnsNames:
  - test-oidc-provider.access-mock.svc
  - test-oidc-provider.access-mock.svc.cluster.local
  issuerRef:
    name: test-oidc-selfsigned
    kind: Issuer
YAML
```

等待证书签发：

```sh
kubectl -n "$MOCK_NS" wait --for=condition=Ready certificate/test-oidc-provider-tls --timeout=60s
```

注意：cert-manager `selfSigned` 签发的证书 subject 为空。Broker 和 CLI 使用的 Go
客户端把该 PEM 当作信任锚时可以正常校验，但 `curl --cacert` 可能报
`couldn't get X509-issuer name`；授权跳转请使用 `curl -k`。

挂载证书并启用 TLS：

```sh
kubectl -n "$MOCK_NS" patch deploy test-oidc-provider \
  --type=strategic \
  -p '{
    "spec": {
      "template": {
        "spec": {
          "containers": [
            {
              "name": "test-oidc-provider",
              "args": [
                "-listen-address", ":8080",
                "-tls-cert-file=/tls/tls.crt",
                "-tls-key-file=/tls/tls.key"
              ],
              "livenessProbe": {
                "httpGet": {"path": "/healthz", "port": "http", "scheme": "HTTPS"}
              },
              "readinessProbe": {
                "httpGet": {"path": "/healthz", "port": "http", "scheme": "HTTPS"}
              },
              "volumeMounts": [
                {"name": "tls", "mountPath": "/tls", "readOnly": true}
              ]
            }
          ],
          "volumes": [
            {"name": "tls", "secret": {"secretName": "test-oidc-provider-tls"}}
          ]
        }
      }
    }
  }'
```

通过 Service 暴露 HTTPS 端口：

```sh
kubectl -n "$MOCK_NS" patch svc test-oidc-provider \
  --type=merge \
  -p '{"spec":{"ports":[{"name":"https","port":8443,"targetPort":8080}]}}'
```

替换 OIDC client redirect URI。URI 不能包含固定端口：

```sh
kubectl -n "$MOCK_NS" set env deploy/test-oidc-provider \
  SEALOS_TEST_OIDC_CONFIG='{
    "issuer":"https://test-oidc-provider.access-mock.svc:8443",
    "users":[
      {
        "sub":"test-user-001",
        "email":"test-user-001@example.internal",
        "name":"Test User One"
      }
    ],
    "clients":[
      {
        "client_id":"test-oidc-client",
        "redirect_uris":["http://127.0.0.1/callback"]
      }
    ],
    "token_audience":"test-oidc-audience",
    "token_ttl_seconds":1800
  }'

kubectl -n "$MOCK_NS" rollout restart deploy/test-oidc-provider
kubectl -n "$MOCK_NS" rollout status deploy/test-oidc-provider
```

## 6. 为 mock issuer 配置 Controller、Broker 和 NetworkPolicy

本节修改 Broker 之前，先备份将要改动的对象，供第 10 节恢复：

```sh
mkdir -p /tmp/internal-user-e2e-backup
kubectl -n "$BROKER_NS" get cm internal-credential-broker-config -o yaml \
  > /tmp/internal-user-e2e-backup/broker-cm.yaml
kubectl -n "$BROKER_NS" get deploy internal-credential-broker -o yaml \
  > /tmp/internal-user-e2e-backup/broker-deploy.yaml
kubectl -n "$BROKER_NS" get netpol internal-credential-broker-controlled-egress -o yaml \
  > /tmp/internal-user-e2e-backup/broker-netpol.yaml
```

Broker 使用严格的多集群 YAML 配置。本流程将 `CLUSTER_ID` 作为其中一个 target
条目；不使用单集群环境变量模式。

### 6.1 Controller issuer allowlist

```sh
kubectl -n "$CTRL_NS" patch cm internal-user-controller-config \
  --type=merge \
  -p "{\"data\":{\"oidc-issuers\":\"${OIDC_ISSUER}\"}}"

kubectl -n "$CTRL_NS" rollout restart deploy/internal-user-controller
kubectl -n "$CTRL_NS" rollout status deploy/internal-user-controller
```

### 6.2 Broker 多集群 YAML 与源地址配置

Broker 要求直连 peer 落在 `trusted-proxy-cidrs` 内，并且请求必须携带且只携带一个
`X-Trusted-Client-IP` 头，其值落在 `allowed-client-cidrs` 内。生产环境由 Higress
注入该头；本文的 6.5 节用一个仅用于测试的本地代理扮演该角色，它通过 loopback
port-forward 连接 Broker，因此 peer 是 loopback，注入的客户端 IP 取默认
`10.0.0.0/8` 范围内的测试值：

```sh
kubectl config view --raw --minify \
  -o 'jsonpath={.clusters[0].cluster.certificate-authority-data}' \
  | base64 --decode > /tmp/mock-cluster.ca.crt
kubectl create token internal-credential-broker \
  --namespace "$BROKER_NS" \
  --audience https://kubernetes.default.svc.cluster.local \
  --duration 24h > /tmp/mock-cluster.token

kubectl -n "$BROKER_NS" create secret generic internal-credential-broker-target-credentials \
  --from-file="${CLUSTER_ID}.ca.crt=/tmp/mock-cluster.ca.crt" \
  --from-file="${CLUSTER_ID}.token=/tmp/mock-cluster.token" \
  --dry-run=client -o yaml | kubectl apply -f -

cat > /tmp/broker.yaml <<EOF
version: 1
oidc:
  issuer: ${OIDC_ISSUER}
  audience: ${OIDC_AUDIENCE}
  jwksURL: ""
  caFile: /etc/broker/oidc-ca/ca.crt
  serverName: ${OIDC_HOST}
targets:
- id: ${CLUSTER_ID}
  apiServer: ${API_SERVER}
  clusterServer: ${API_SERVER}
  caFile: /etc/broker/targets/${CLUSTER_ID}.ca.crt
  credentialFile: /etc/broker/targets/${CLUSTER_ID}.token
  kubernetesAudience: https://kubernetes.default.svc.cluster.local
  userNamespace: access-system
  brokerNamespace: access-broker-system
  clusterName: ${CLUSTER_ID}
  enabled: true
EOF

kubectl -n "$BROKER_NS" create configmap internal-credential-broker-config \
  --from-file=broker.yaml=/tmp/broker.yaml \
  --from-literal=config-file=/etc/broker/config/broker.yaml \
  --from-literal=request-rate-limit=10 \
  --from-literal=request-rate-burst=20 \
  --from-literal=trusted-proxy-cidrs=127.0.0.0/8 \
  --from-literal=allowed-client-cidrs=10.0.0.0/8 \
  --from-literal=trusted-client-ip-header=X-Trusted-Client-IP \
  --from-literal=tls-cert-file=/etc/broker/tls/tls.crt \
  --from-literal=tls-key-file=/etc/broker/tls/tls.key \
  --dry-run=client -o yaml | kubectl apply -f -
```

将 OIDC 服务端证书复制进 Broker 信任的 ConfigMap：

```sh
TMP_CA="$(mktemp)"
kubectl -n "$MOCK_NS" get secret test-oidc-provider-tls \
  -o jsonpath='{.data.tls\.crt}' | base64 -d > "$TMP_CA"

kubectl -n "$BROKER_NS" create secret generic internal-credential-broker-oidc-ca \
  --from-file=ca.crt="$TMP_CA" \
  --dry-run=client -o yaml | kubectl apply -f -

rm -f "$TMP_CA"
```

Deployment 已将 `internal-credential-broker-oidc-ca` 挂载到
`/etc/broker/oidc-ca`；不需要旧的 OIDC 环境变量或 Deployment patch。

### 6.3 允许 Broker 访问 mock OIDC provider

`config/broker/networkpolicy.yaml` 默认的 egress 规则只允许 acceptance OIDC。
开始流程前需要追加 mock provider 的 Pod 和 Service IP：

```sh
export MOCK_OIDC_IP="$(kubectl -n "$MOCK_NS" get svc test-oidc-provider -o jsonpath='{.spec.clusterIP}')"

cat > /tmp/broker-mock-egress.json <<EOF
{"spec":{"egress":[
  {"ports":[{"port":53,"protocol":"UDP"},{"port":53,"protocol":"TCP"}],
   "to":[{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"kube-system"}},"podSelector":{"matchLabels":{"k8s-app":"kube-dns"}}}]},
  {"ports":[{"port":443,"protocol":"TCP"}],"to":[{"ipBlock":{"cidr":"10.192.0.1/32"}}]},
  {"ports":[{"port":30443,"protocol":"TCP"}],"to":[{"ipBlock":{"cidr":"192.168.128.123/32"}}]},
  {"ports":[{"port":8443,"protocol":"TCP"}],
   "to":[{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"access-broker-system"}},"podSelector":{"matchLabels":{"app.kubernetes.io/name":"acceptance-oidc"}}}]},
  {"ports":[{"port":8080,"protocol":"TCP"},{"port":8443,"protocol":"TCP"}],
   "to":[{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"access-mock"}},"podSelector":{"matchLabels":{"app.kubernetes.io/name":"test-oidc-provider"}}}]},
  {"ports":[{"port":8443,"protocol":"TCP"}],"to":[{"ipBlock":{"cidr":"${MOCK_OIDC_IP}/32"}}]}
]}}
EOF

kubectl -n "$BROKER_NS" patch netpol internal-credential-broker-controlled-egress \
  --type=merge --patch-file /tmp/broker-mock-egress.json
```

其中 `10.192.0.1` 和 `192.168.128.123` 是本 acceptance 集群的 Kubernetes
Service IP 和控制面地址；在其它集群复用时请替换成本集群的值（或删除这两条规则）。

### 6.4 配置 Broker 管理员 group

elevated 流程通过新鲜 OIDC token 提交已审批记录。请将测试 issuer、audience 和精确的
管理员 group 配置到 Broker，并确保执行 `admin approve` 的 mock OIDC 用户在 `groups`
claim 中包含该 group。

Broker 审批 endpoint 不应配置 client CA 或客户端证书。当前配置明确不使用客户端证书
设置。

### 6.5 重启 Broker 并启动仅用于测试的可信代理

```sh
kubectl -n "$BROKER_NS" rollout restart deploy/internal-credential-broker
kubectl -n "$BROKER_NS" rollout status deploy/internal-credential-broker
```

把 Broker 暴露到本地（保持运行）：

```sh
kubectl -n "$BROKER_NS" port-forward svc/internal-credential-broker 9443:443
```

生成本地代理证书并写入代理脚本：

```sh
openssl req -x509 -newkey rsa:2048 -nodes -days 30 \
  -keyout /tmp/trusted-proxy.key -out /tmp/trusted-proxy.crt \
  -subj "/CN=127.0.0.1" -addext "subjectAltName=IP:127.0.0.1"

cat > /tmp/trusted_proxy.py <<'PY'
import http.client, ssl, sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

UPSTREAM_HOST = "127.0.0.1"
UPSTREAM_PORT = 9443
CLIENT_IP = "10.0.0.5"

class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "test-trusted-proxy"

    def _proxy(self):
        length = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(length) if length else None
        ctx = ssl._create_unverified_context()
        conn = http.client.HTTPSConnection(UPSTREAM_HOST, UPSTREAM_PORT, context=ctx, timeout=30)
        headers = {k: v for k, v in self.headers.items()
                   if k.lower() not in ("host", "content-length", "connection")}
        headers["X-Trusted-Client-IP"] = CLIENT_IP
        headers["Host"] = "internal-credential-broker.access-broker-system.svc"
        if body is not None:
            headers["Content-Length"] = str(len(body))
        try:
            conn.request(self.command, self.path, body=body, headers=headers)
            resp = conn.getresponse()
            data = resp.read()
            self.send_response(resp.status)
            for k, v in resp.getheaders():
                if k.lower() in ("transfer-encoding", "connection", "content-length"):
                    continue
                self.send_header(k, v)
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
        except Exception as exc:  # noqa: BLE001
            sys.stderr.write("proxy error: %s\n" % exc)
            sys.stderr.flush()
            self.send_response(502)
            self.send_header("Content-Length", "0")
            self.end_headers()
        finally:
            conn.close()

    do_GET = _proxy
    do_POST = _proxy
    do_DELETE = _proxy

    def log_message(self, *args):
        pass

ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
ctx.load_cert_chain("/tmp/trusted-proxy.crt", "/tmp/trusted-proxy.key")
ctx.set_alpn_protocols(["http/1.1"])
srv = ThreadingHTTPServer(("127.0.0.1", 19443), Handler)
srv.socket = ctx.wrap_socket(srv.socket, server_side=True)
print("trusted proxy listening on 127.0.0.1:19443", flush=True)
srv.serve_forever()

PY
```

运行代理（保持运行）：

```sh
python3 /tmp/trusted_proxy.py
```

## 7. 准备本地 CLI 访问并创建 InternalUser

将 mock OIDC 的 hostname 映射到工作机 loopback。Broker 通过本地可信代理
`127.0.0.1:19443` 访问，因此不需要为 Broker 配置 hosts：

```sh
sudo tee -a /etc/hosts >/dev/null <<EOF
127.0.0.1 ${OIDC_HOST}
EOF
```

在独立终端中保持以下进程运行：

```sh
# Terminal A: mock OIDC provider
kubectl -n "$MOCK_NS" port-forward svc/test-oidc-provider 8443:8443

# Terminal B: Broker（可信代理转发到该 port-forward）
kubectl -n "$BROKER_NS" port-forward svc/internal-credential-broker 9443:443

# Terminal C: 6.5 节的仅测试用可信代理
python3 /tmp/trusted_proxy.py
```

导出服务端证书：

```sh
kubectl -n "$MOCK_NS" get secret test-oidc-provider-tls \
  -o jsonpath='{.data.tls\.crt}' | base64 -d > /tmp/oidc-ca.crt

kubectl -n "$BROKER_NS" get secret internal-credential-broker-tls \
  -o jsonpath='{.data.tls\.crt}' | base64 -d > /tmp/broker-ca.crt
```

构建管理员 CLI 并计算确定性资源名：

```sh
cd "$REPO_ROOT"
make build-sealos-access TARGETARCH=amd64 GOOS="$(go env GOOS)"

export IU_NAME="$(python3 - <<PY
import hashlib
issuer = "${OIDC_ISSUER}"
subject = "${TEST_SUBJECT}"
print("iu-" + hashlib.sha256((issuer + "\0" + subject).encode()).hexdigest()[:24])
PY
)"
```

创建身份：

```sh
./bin/sealos-access-amd64 admin create \
  --kubeconfig "$KUBECONFIG" \
  --issuer "$OIDC_ISSUER" \
  --subject "$TEST_SUBJECT"

kubectl wait \
  --for=jsonpath='{.status.phase}'=Active \
  "internaluser/${IU_NAME}" \
  --timeout=120s
```

验证稳定资源：

```sh
kubectl get internaluser "$IU_NAME" -o yaml
kubectl -n "$USER_NS" get sa "$IU_NAME" -o yaml
kubectl get clusterrolebinding \
  -l "user.sealos.io/internal-user=${IU_NAME}" \
  -o yaml
```

预期：

```text
InternalUser.status.phase = Active
ServiceAccount.automountServiceAccountToken = false
ClusterRoleBinding.roleRef.name = internal-user-base-readonly-v1
```

## 8. 执行 base 凭据流程

构建并运行用户 CLI：

```sh
# 需要 Terminal B（Broker port-forward）和 Terminal C（可信代理）在运行。
# CLI 访问代理，由代理注入 X-Trusted-Client-IP。
./bin/sealos-access-amd64 login \
  --broker-url "$BROKER_PROXY_URL" \
  --cluster-id "$CLUSTER_ID" \
  --broker-ca-file /tmp/trusted-proxy.crt \
  --broker-server-name 127.0.0.1 \
  --oidc-issuer "$OIDC_ISSUER" \
  --oidc-ca-file /tmp/oidc-ca.crt \
  --oidc-server-name "$OIDC_HOST" \
  --client-id "$OIDC_CLIENT_ID" \
  --login-hint "$TEST_SUBJECT" \
  --requested-ttl-seconds 600 \
  --output /tmp/base.kubeconfig \
  --no-browser > /tmp/base-cli.out 2>&1 &

until AUTH_URL="$(grep -o 'https://[^ ]*' /tmp/base-cli.out | head -1)" && [ -n "$AUTH_URL" ]; do sleep 1; done
curl -k -sL "$AUTH_URL" >/dev/null
wait
cat /tmp/base-cli.out
```

交互使用时，去掉命令行末尾的 `> /tmp/base-cli.out 2>&1 &`，在浏览器中打开
打印出的 authorization URL 即可。

预期输出包含 lease ID、profile、username 和 expiration。记录 lease ID：

```sh
export BASE_LEASE=<lease-id-from-output>
```

检查 lease：

```sh
kubectl -n "$BROKER_NS" get credentiallease "$BASE_LEASE" -o yaml
```

预期：

```text
spec.profile = base-readonly.v1
status.phase = Issued
status.serviceAccountRef.name = iu-...
status.boundSecretRef.name = internal-lease-secret-...
status.bindingRef = null
```

验证 RBAC：

```sh
kubectl --kubeconfig /tmp/base.kubeconfig auth can-i get pods -n default
# yes

kubectl --kubeconfig /tmp/base.kubeconfig auth can-i create internalusers.user.sealos.io
# no

kubectl --kubeconfig /tmp/base.kubeconfig auth can-i \
  create credentialleases.user.sealos.io \
  -n access-broker-system
# no
```

## 9. 执行 elevated 凭据流程

### 9.1 创建并通过外部审批

使用 CLI 调试 Pod 中的 `curl`：

```sh
INSTANCE_CODE="$(kubectl -n "$MOCK_NS" exec deploy/sealos-access -- sh -c '
curl -sS -X POST http://mock-approval-server:8080/v1/approvals \
  -H "Content-Type: application/json" \
  -d "{
    \"approvalCode\":\"cluster-ops-write.v1\",
    \"clusterID\":\"mock-cluster\",
    \"initiatorID\":\"operator\",
    \"initiatorEmail\":\"operator@example.internal\",
    \"targetSubject\":\"test-user-001\",
    \"reason\":\"E2E elevated access\",
    \"requestedTTLSeconds\":600,
    \"form\":{
      \"subject\":\"test-user-001\",
      \"ttl\":\"600\",
      \"reason\":\"E2E elevated access\"
    }
  }"' | sed -E 's/.*"instanceCode":"([^"]+)".*/\1/')"

echo "$INSTANCE_CODE"

kubectl -n "$MOCK_NS" exec deploy/sealos-access -- \
  curl -sS -X POST \
  "http://mock-approval-server:8080/v1/approvals/${INSTANCE_CODE}/approve"

kubectl -n "$MOCK_NS" exec deploy/sealos-access -- \
  curl -sS \
  "http://mock-approval-server:8080/v1/approvals/${INSTANCE_CODE}"
```

### 9.2 验证 Broker OIDC 管理员 group endpoint

该步骤已在 6.4 节完成。如果之后恢复过 Broker，请先重新执行 6.4 节。

### 9.3 将审批记录提交给 Broker

```sh
cd "$REPO_ROOT"

./bin/sealos-access-amd64 admin approve \
  --issuer "$OIDC_ISSUER" \
  --subject "$TEST_SUBJECT" \
  --broker-url "$BROKER_DIRECT_URL" \
  --cluster-id "$CLUSTER_ID" \
  --broker-ca-file /tmp/broker-ca.crt \
  --broker-server-name "$BROKER_HOST" \
  --oidc-issuer "$OIDC_ISSUER" \
  --oidc-ca-file /tmp/oidc-ca.crt \
  --client-id "$OIDC_CLIENT_ID" \
  --approval-id "$INSTANCE_CODE" \
  --approval-reason "E2E elevated access" \
  --requested-ttl-seconds 600 \
  --no-browser
```

记录输出：

```sh
export APPROVAL_REFERENCE=<approval-reference-from-output>
export ELEVATED_LEASE="$(kubectl -n "$BROKER_NS" get credentialleases \
  -o jsonpath='{range .items[*]}{.spec.approvalID}{" "}{.metadata.name}{"\n"}{end}' |
  awk -v id="$INSTANCE_CODE" '$1 == id {print $2}')"
```

验证等待状态：

```sh
kubectl -n "$BROKER_NS" get credentiallease "$ELEVATED_LEASE" -o yaml
```

兑换前预期：

```text
spec.profile = cluster-ops-write.v1
status.phase = AwaitingRedemption
status.approvalExpirationTimestamp != nil
status.serviceAccountRef = null
status.boundSecretRef = null
status.bindingRef = null
```

### 9.4 兑换 elevated reference

```sh
# 需要 Terminal B（Broker port-forward）和 Terminal C（可信代理）在运行。
./bin/sealos-access-amd64 elevated redeem \
  --broker-url "$BROKER_PROXY_URL" \
  --cluster-id "$CLUSTER_ID" \
  --broker-ca-file /tmp/trusted-proxy.crt \
  --broker-server-name 127.0.0.1 \
  --oidc-issuer "$OIDC_ISSUER" \
  --oidc-ca-file /tmp/oidc-ca.crt \
  --oidc-server-name "$OIDC_HOST" \
  --client-id "$OIDC_CLIENT_ID" \
  --login-hint "$TEST_SUBJECT" \
  --approval-reference "$APPROVAL_REFERENCE" \
  --output /tmp/elevated.kubeconfig \
  --no-browser > /tmp/elevated-cli.out 2>&1 &

until AUTH_URL="$(grep -o 'https://[^ ]*' /tmp/elevated-cli.out | head -1)" && [ -n "$AUTH_URL" ]; do sleep 1; done
curl -k -sL "$AUTH_URL" >/dev/null
wait
cat /tmp/elevated-cli.out
```

预期 lease 状态：

```text
spec.profile = cluster-ops-write.v1
spec.target = spec.requester = test identity
status.phase = Issued
status.serviceAccountRef.name = internal-lease-...
status.bindingRef.name = internal-lease-binding-...
status.boundSecretRef.name = internal-lease-secret-...
status.consumedAt != nil
status.expirationTimestamp != nil
```

验证临时资源：

```sh
SA="$(kubectl -n "$BROKER_NS" get credentiallease "$ELEVATED_LEASE" -o jsonpath='{.status.serviceAccountRef.name}')"
SECRET="$(kubectl -n "$BROKER_NS" get credentiallease "$ELEVATED_LEASE" -o jsonpath='{.status.boundSecretRef.name}')"
BINDING="$(kubectl -n "$BROKER_NS" get credentiallease "$ELEVATED_LEASE" -o jsonpath='{.status.bindingRef.name}')"

kubectl -n "$USER_NS" get sa "$SA" -o yaml
kubectl -n "$USER_NS" get secret "$SECRET" -o yaml
kubectl get clusterrolebinding "$BINDING" -o yaml
```

必须满足：

```text
ServiceAccount.automountServiceAccountToken = false
Secret.data 为空
Secret.stringData 为空
ClusterRoleBinding.roleRef.name = internal-user-cluster-ops-write-v1
```

验证 elevated RBAC：

```sh
kubectl --kubeconfig /tmp/elevated.kubeconfig auth can-i get pods -n default
# yes

kubectl --kubeconfig /tmp/elevated.kubeconfig auth can-i create events -n default
# yes

kubectl --kubeconfig /tmp/elevated.kubeconfig auth can-i create deployments.apps -n default
# no

kubectl --kubeconfig /tmp/elevated.kubeconfig auth can-i \
  create serviceaccounts \
  -n access-system
# no
```

### 9.5 验证重放保护

使用不同的输出路径再次执行相同兑换：

```sh
# 需要 Terminal B（Broker port-forward）和 Terminal C（可信代理）在运行。
./bin/sealos-access-amd64 elevated redeem \
  --broker-url "$BROKER_PROXY_URL" \
  --cluster-id "$CLUSTER_ID" \
  --broker-ca-file /tmp/trusted-proxy.crt \
  --broker-server-name 127.0.0.1 \
  --oidc-issuer "$OIDC_ISSUER" \
  --oidc-ca-file /tmp/oidc-ca.crt \
  --oidc-server-name "$OIDC_HOST" \
  --client-id "$OIDC_CLIENT_ID" \
  --login-hint "$TEST_SUBJECT" \
  --approval-reference "$APPROVAL_REFERENCE" \
  --output /tmp/elevated-replay.kubeconfig \
  --no-browser > /tmp/elevated-replay.out 2>&1 &

until AUTH_URL="$(grep -o 'https://[^ ]*' /tmp/elevated-replay.out | head -1)" && [ -n "$AUTH_URL" ]; do sleep 1; done
curl -k -sL "$AUTH_URL" >/dev/null
wait || true
grep 'Broker returned HTTP 409' /tmp/elevated-replay.out
```

预期：`Broker returned HTTP 409`。不能出现第二次 TokenRequest，也不能产生
第二份 kubeconfig。

## 10. 收集证据并清理

删除资源前收集证据：

```sh
mkdir -p /tmp/internal-user-e2e-evidence

kubectl get internaluser "$IU_NAME" -o yaml \
  > /tmp/internal-user-e2e-evidence/internaluser.yaml

kubectl -n "$BROKER_NS" get credentiallease "$BASE_LEASE" -o yaml \
  > /tmp/internal-user-e2e-evidence/base-lease.yaml

kubectl -n "$BROKER_NS" get credentiallease "$ELEVATED_LEASE" -o yaml \
  > /tmp/internal-user-e2e-evidence/elevated-lease.yaml

kubectl -n "$CTRL_NS" logs deploy/internal-user-controller \
  > /tmp/internal-user-e2e-evidence/controller.log

kubectl -n "$BROKER_NS" logs deploy/internal-credential-broker \
  > /tmp/internal-user-e2e-evidence/broker.log
```

删除 lease 和身份：

```sh
kubectl -n "$BROKER_NS" delete credentiallease \
  "$BASE_LEASE" "$ELEVATED_LEASE" \
  --ignore-not-found

kubectl delete internaluser "$IU_NAME" --ignore-not-found
```

不要绕过 finalizer。必须允许 Controller 在 `InternalUser` 删除完成前清理临时资源。

删除 mock Deployment：

```sh
kubectl -n "$MOCK_NS" delete -k "$REPO_ROOT/test/mock"
```

恢复 Broker。先停止代理和 port-forward（在 Terminal B/C 中 Ctrl-C），移除临时
hosts 条目、删除临时 CA 资源，并恢复第 6 节开始时备份的对象：

```sh
sudo sed -i '/test-oidc-provider.access-mock.svc/d' /etc/hosts

kubectl -n "$BROKER_NS" delete configmap test-oidc-ca --ignore-not-found
for f in broker-cm broker-deploy broker-netpol; do
  python3 - "$f" <<'PY'
import json, sys, yaml
name = sys.argv[1]
path = f"/tmp/internal-user-e2e-backup/{name}.yaml"
doc = yaml.safe_load(open(path))
meta = doc.get("metadata", {})
for key in ("resourceVersion", "uid", "creationTimestamp", "generation", "managedFields"):
    meta.pop(key, None)
doc.pop("status", None)
json.dump(doc, open(f"/tmp/internal-user-e2e-backup/{name}.json", "w"))
PY
done

kubectl apply -f /tmp/internal-user-e2e-backup/broker-cm.json
kubectl apply -f /tmp/internal-user-e2e-backup/broker-netpol.json
kubectl apply -f /tmp/internal-user-e2e-backup/broker-deploy.json
```

`kubectl apply` 不会删除通过 6.2/6.4 节 strategic patch 加到 live 对象上的字段。
请检查恢复后的 Deployment，若仍残留 mock 专用项则显式删除：

```sh
kubectl -n "$BROKER_NS" get deploy internal-credential-broker -o json \
  | jq -r '.spec.template.spec.containers[0].env[].name,
           (.spec.template.spec.volumes[] | .name)'
```

恢复后的 Broker 不应引用 `test-oidc-ca` 或其它 mock OIDC 配置。必要时直接删除并按备份
重建 Deployment。

## 11. 完整 adapter 和 MailHog 路径

`mock-approval-server` 只保存外部审批状态，它本身不会向 Broker 提交审批。
`mailhog` 只有在部署的 adapter 显式使用 `approvaladapter.EmailNotifier` 时才会
收到邮件。

当前集成骨架默认不会接入 `mock.NewMockSource` 或 `EmailNotifier`。要在集群中测试
自动外部审批路径，需要增加一个只用于测试的 adapter entry point，接入：

```go
source := mock.NewMockSource(
    "http://mock-approval-server.access-mock.svc:8080",
)

notifier := &approvaladapter.EmailNotifier{
    SMTPHost: "mailhog.access-mock.svc",
    SMTPPort: 1025,
    From:     "internal-credential-broker@example.internal",
}

pipeline, err := approvaladapter.NewPipeline(source, brokerHTTPClient, notifier)
```

该 entry point 还需要一个事件入口或轮询机制，将外部 `instanceCode` 传入 pipeline。
完成接入后，通过以下命令检查 MailHog：

```sh
kubectl -n access-mock port-forward svc/mailhog 8025:8025
```

打开 `http://localhost:8025`，验证邮件包含 approval reference，但不包含 token、
kubeconfig、private key 或 Secret data。

在完成上述接线前，集群验收使用第 9 节的管理员手工审批路径，完整 adapter pipeline
使用第 2 节的本地 acceptance 测试。

## 12. 相关文档

- [系统概览](internal-user-credentials-overview.zh-CN.md)
- [实现架构](internal-user-credentials-architecture.zh-CN.md)
- [用户 CLI](internal-user-credentials-cli.zh-CN.md)
- [管理员 CLI](internal-user-admin-cli.zh-CN.md)
- [审批适配器](internal-user-approval-adapter.zh-CN.md)
- [E2E TODO 矩阵](internal-user-credentials-e2e-todo.zh-CN.md)
- [Mock 组件说明](../test/mock/README.md)
