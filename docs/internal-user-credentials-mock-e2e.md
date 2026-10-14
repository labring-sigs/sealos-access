# Internal User Credentials Mock E2E Test Process

Status: runbook for test and acceptance environments.

Tracked: 2026-09-15.

This document describes an end-to-end test process for `InternalUser` and
`CredentialLease` using the mock components under
`./test/mock`. It covers a fast local acceptance run and a
manual Kubernetes cluster run.

The Chinese companion is
[internal-user-credentials-mock-e2e.zh-CN.md](internal-user-credentials-mock-e2e.zh-CN.md).

## 1. Scope and current limitations

The mock components are:

| Component | Replaces | Purpose |
| --- | --- | --- |
| `test-oidc-provider` | External OIDC issuer | Authorization Code + PKCE, JWKS, configurable users and claims |
| `mock-approval-server` | External approval API | Create, inspect, approve, and reject approval records |
| `mailhog` | SMTP relay | Capture notification email through its Web UI |
| `sealos-access` debug pod | Local CLI workstation | Run the unified CLI and `curl` from inside the cluster |

Two current implementation details affect the procedure:

1. The checked-in `test-oidc-provider` Deployment listens for HTTP. The Broker
   and `sealos-access` require HTTPS. The cluster run below adds TLS to the
   provider and explicitly configures the Broker and CLI to trust it.
2. `cmd/internal-user-approval-adapter` is still an integration skeleton. It
   does not select `mock.NewMockSource` and `approvaladapter.EmailNotifier` by
   default. Therefore the cluster procedure uses the supported administrator
   manual approval path after `mock-approval-server` creates the authoritative
   approval record. The local acceptance tests below exercise the complete
   source -> adapter -> Broker -> Controller -> redemption path.

The mock OIDC client must use a redirect URI without a fixed port, such as
`http://127.0.0.1/callback`, because the CLI listens on a random loopback port.

## 2. Local acceptance path

This is the fastest way to validate the complete in-process behavior without a
Kubernetes cluster.

```sh
export REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

# OIDC discovery, PKCE, code consumption, and token minting.
go test ./test/mock/oidc-provider/... \
  -run 'Test(DiscoveryAndJWKS|AuthorizationCodePKCE|AuthorizationCodeConsumesCodeOnPKCEFailure|MintToken)$' \
  -count=1 -v

# Mock external approval -> adapter -> Broker -> CredentialLease -> redeem.
go test ./pkg/approvaladapter/mock \
  -run '^TestApprovalAdapterToBrokerServerE2E$' \
  -count=1 -v

# CLI reference redemption -> Broker -> Controller -> TokenIssuer.
go test -tags acceptance ./internal/access/usercmd \
  -run '^TestElevatedRedeemPreOIDC$' \
  -count=1 -v

# Acceptance notification sink and elevated CLI package.
go test -tags acceptance \
  ./internal/access/usercmd \
  ./pkg/approvaladapter/acceptance \
  -count=1 -v

# InternalUser and CredentialLease lifecycle tests.
go test ./controllers/internalcredentialstest -count=1 -v
```

The acceptance OIDC issuer, audience, and administrator group must match the
Broker configuration. The administrator approval flow uses a fresh OIDC token;
it does not use a client certificate.

The complete `go test ./controllers/...` suite additionally requires envtest
assets:

```sh
KUBEBUILDER_ASSETS="$(setup-envtest use 1.32.0 -p path)" go test ./controllers/...
```

Without those assets, envtest fails with a missing `etcd` or
`kube-apiserver` binary.

## 3. Cluster topology and prerequisites

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
    +--> stable ServiceAccount
    +--> base ClusterRoleBinding
    +--> temporary ServiceAccount / binding / empty bound Secret
```

Prerequisites:

- A test Kubernetes cluster with cluster-admin access.
- `kubectl`, Docker, OpenSSL, Python 3, and a compatible Go toolchain.
- cert-manager or an equivalent certificate provisioning mechanism.
- A container registry reachable by every node, or another image loading
  mechanism.
- A trusted proxy in front of the Broker. Every authenticated Broker request
  must arrive from a peer inside `trusted-proxy-cidrs` and carry exactly one
  `X-Trusted-Client-IP` header whose value is inside `allowed-client-cidrs`.
  Production uses Higress; the manual cluster run below starts a test-only
  local proxy that injects the header.
- Permission to modify `/etc/hosts` on the workstation running the CLI.
- Permission to edit the Broker NetworkPolicy so it can reach the mock OIDC
  provider (section 6.3).
- `export KUBECONFIG="${KUBECONFIG:-$HOME/.kube/config}"` before running the
  commands below; the administrator CLI needs an explicit kubeconfig.
- No production credentials or production approval records.

Expected assertions:

| Stage | Required result |
| --- | --- |
| `InternalUser` creation | Phase is `Active`; stable ServiceAccount and base binding exist |
| Base lease | `Pending -> Prepared -> Issued`; kubeconfig can read Pods |
| Base authorization | Cannot create `InternalUser` or `CredentialLease` |
| Elevated approval | Lease is `AwaitingRedemption` and has an approval reference |
| Elevated redemption | Lease is `Issued`; elevated kubeconfig is returned |
| Elevated resources | Temporary ServiceAccount, binding, and empty Secret belong to the lease |
| Elevated authorization | Can create Events; cannot create Deployment or modify RBAC |
| Replay | The same reference is rejected after first consumption |
| Cleanup | Terminal lease resources are removed; stable resources remain until user deletion |

## 4. Build and deploy

Set the image registry prefix:

```sh
export REGISTRY=registry.example.internal/test
```

Build the control-plane binaries:

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

Build the control-plane images:

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

Build the mock images:

```sh
# The mock build context is the repository root.
docker build \
  -f test/mock/oidc-provider/Dockerfile \
  -t "$REGISTRY/sealos-test-oidc-provider:latest" .

docker build \
  -f test/mock/approval-server/Dockerfile \
  -t "$REGISTRY/sealos-mock-approval-server:latest" .
```

The CLI Dockerfile expects the `controllers` directory as its build context:

```sh
cd "$REPO_ROOT"
docker build \
  -f test/mock/cli/Dockerfile \
  -t "$REGISTRY/sealos-access:latest" .
```

Push all images to the registry:

```sh
docker push "$REGISTRY/sealos-internal-user-controller:latest"
docker push "$REGISTRY/sealos-internal-credential-broker:latest"
docker push "$REGISTRY/sealos-internal-user-admission:latest"
docker push "$REGISTRY/sealos-test-oidc-provider:latest"
docker push "$REGISTRY/sealos-mock-approval-server:latest"
docker push "$REGISTRY/sealos-access:latest"
```

Deploy the CRDs and components:

```sh
cd "$REPO_ROOT"

kubectl apply -f config/crd/bases/user.sealos.io_internalusers.yaml
kubectl apply -f config/crd/bases/user.sealos.io_credentialleases.yaml

kubectl apply -k config/internal-user-controller
kubectl apply -k config/broker
kubectl apply -k config/internal-user-admission
kubectl apply -k test/mock
```

Replace the default images with the images built above:

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

Note: build the Broker image from this repository. The internal approval
endpoint used in section 9.3 (`/v1/clusters/{clusterID}/internal/credentials/approve`) lives in the
repository's `cmd/broker`; older or acceptance-only Broker images may return
HTTP 404 for it. If your registry requires credentials, verify the
Deployments' `imagePullSecrets` resolve before continuing: a broken pull
secret produces `ImagePullBackOff` even when the image itself is public.

Wait for all Deployments:

```sh
kubectl -n access-controller-system rollout status deploy/internal-user-controller
kubectl -n access-controller-system rollout status deploy/internal-user-admission
kubectl -n access-broker-system rollout status deploy/internal-credential-broker
kubectl -n access-mock rollout status deploy/test-oidc-provider
kubectl -n access-mock rollout status deploy/mock-approval-server
kubectl -n access-mock rollout status deploy/mailhog
kubectl -n access-mock rollout status deploy/sealos-access
```

## 5. Add TLS to the OIDC mock

Set the test variables:

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

# The CLI talks to the test-only trusted proxy; the administrator CLI talks to
# the Broker directly through a port-forward.
export BROKER_PROXY_URL="https://127.0.0.1:19443"
export BROKER_DIRECT_URL="https://127.0.0.1:9443"
```

Create a serving certificate:

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

Wait for the certificate to be issued:

```sh
kubectl -n "$MOCK_NS" wait --for=condition=Ready certificate/test-oidc-provider-tls --timeout=60s
```

Note: cert-manager `selfSigned` issues a certificate with an empty subject.
The Go clients used by the Broker and the CLI verify it correctly when the PEM
is used as the trust anchor, but `curl --cacert` may fail with
`couldn't get X509-issuer name`; use `curl -k` for the authorization
redirect.

Mount the certificate and enable TLS in the provider:

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

Expose the HTTPS port through the Service:

```sh
kubectl -n "$MOCK_NS" patch svc test-oidc-provider \
  --type=merge \
  -p '{"spec":{"ports":[{"name":"https","port":8443,"targetPort":8080}]}}'
```

Replace the OIDC client redirect URI. The URI must not contain a fixed port:

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

## 6. Configure the Controller, Broker, and NetworkPolicy for the mock issuer

Snapshot the Broker objects this section modifies so section 10 can restore
them:

```sh
mkdir -p /tmp/internal-user-e2e-backup
kubectl -n "$BROKER_NS" get cm internal-credential-broker-config -o yaml \
  > /tmp/internal-user-e2e-backup/broker-cm.yaml
kubectl -n "$BROKER_NS" get deploy internal-credential-broker -o yaml \
  > /tmp/internal-user-e2e-backup/broker-deploy.yaml
kubectl -n "$BROKER_NS" get netpol internal-credential-broker-controlled-egress -o yaml \
  > /tmp/internal-user-e2e-backup/broker-netpol.yaml
```

The Broker uses the strict multi-cluster YAML configuration. This mock flow
declares `CLUSTER_ID` as one target entry in that configuration; it does not
use a single-cluster environment-variable mode.

### 6.1 Controller issuer allowlist

```sh
kubectl -n "$CTRL_NS" patch cm internal-user-controller-config \
  --type=merge \
  -p "{\"data\":{\"oidc-issuers\":\"${OIDC_ISSUER}\"}}"

kubectl -n "$CTRL_NS" rollout restart deploy/internal-user-controller
kubectl -n "$CTRL_NS" rollout status deploy/internal-user-controller
```

### 6.2 Broker multi-cluster YAML and source-address configuration

The Broker requires the direct peer to be inside `trusted-proxy-cidrs` and a
single `X-Trusted-Client-IP` header whose value is inside
`allowed-client-cidrs`. Production uses Higress for this; the test-only proxy
in section 6.5 connects to the Broker through a loopback port-forward, so
`trusted-proxy-cidrs` is loopback and the injected client IP is a test value
inside the default `10.0.0.0/8` client range:

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

Copy the OIDC serving certificate into a ConfigMap trusted by the Broker:

```sh
TMP_CA="$(mktemp)"
kubectl -n "$MOCK_NS" get secret test-oidc-provider-tls \
  -o jsonpath='{.data.tls\.crt}' | base64 -d > "$TMP_CA"

kubectl -n "$BROKER_NS" create secret generic internal-credential-broker-oidc-ca \
  --from-file=ca.crt="$TMP_CA" \
  --dry-run=client -o yaml | kubectl apply -f -

rm -f "$TMP_CA"
```

The Deployment already mounts `internal-credential-broker-oidc-ca` at
`/etc/broker/oidc-ca`; no legacy OIDC environment variables or deployment
patch are required.

### 6.3 Allow the Broker to reach the mock OIDC provider

The default `config/broker/networkpolicy.yaml` egress rules only allow the
acceptance OIDC service. Add the mock provider Pod and Service IP before
starting the flow:

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

`10.192.0.1` and `192.168.128.123` are this acceptance cluster's Kubernetes
Service IP and control-plane address. Replace them with your cluster's values
(or drop those rules) before reusing the runbook on another cluster.

### 6.4 Configure the Broker administrator group

The elevated workflow submits the already-approved record with a fresh OIDC
token. Configure the Broker with the test issuer, audience, and exact admin
group used by the mock OIDC provider. The provider user used for `admin approve`
must include that group in its `groups` claim.

The Broker approval endpoint must not be configured with a client CA or client
certificate. Those client-certificate settings are intentionally absent from
the current configuration.

### 6.5 Restart the Broker and start the test-only trusted proxy

```sh
kubectl -n "$BROKER_NS" rollout restart deploy/internal-credential-broker
kubectl -n "$BROKER_NS" rollout status deploy/internal-credential-broker
```

Expose the Broker locally (leave this running):

```sh
kubectl -n "$BROKER_NS" port-forward svc/internal-credential-broker 9443:443
```

Generate a local certificate for the proxy and save the proxy script:

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

Run the proxy (leave this running):

```sh
python3 /tmp/trusted_proxy.py
```

## 7. Prepare local CLI access and create an InternalUser

Map the mock OIDC hostname to the workstation loopback interface. The Broker
is reached through the local trusted proxy on `127.0.0.1:19443`, so it does
not need a hosts entry:

```sh
sudo tee -a /etc/hosts >/dev/null <<EOF
127.0.0.1 ${OIDC_HOST}
EOF
```

Keep these long-running processes in separate terminals:

```sh
# Terminal A: mock OIDC provider
kubectl -n "$MOCK_NS" port-forward svc/test-oidc-provider 8443:8443

# Terminal B: Broker (the trusted proxy forwards to this port-forward)
kubectl -n "$BROKER_NS" port-forward svc/internal-credential-broker 9443:443

# Terminal C: test-only trusted proxy from section 6.5
python3 /tmp/trusted_proxy.py
```

Export the serving certificates:

```sh
kubectl -n "$MOCK_NS" get secret test-oidc-provider-tls \
  -o jsonpath='{.data.tls\.crt}' | base64 -d > /tmp/oidc-ca.crt

kubectl -n "$BROKER_NS" get secret internal-credential-broker-tls \
  -o jsonpath='{.data.tls\.crt}' | base64 -d > /tmp/broker-ca.crt
```

Build the administrator CLI and derive the deterministic resource name:

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

Create the identity:

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

Verify the stable resources:

```sh
kubectl get internaluser "$IU_NAME" -o yaml
kubectl -n "$USER_NS" get sa "$IU_NAME" -o yaml
kubectl get clusterrolebinding \
  -l "user.sealos.io/internal-user=${IU_NAME}" \
  -o yaml
```

Expected:

```text
InternalUser.status.phase = Active
ServiceAccount.automountServiceAccountToken = false
ClusterRoleBinding.roleRef.name = internal-user-base-readonly-v1
```

## 8. Run the base credential workflow

Build and run the user CLI:

```sh
# Terminals B (Broker port-forward) and C (trusted proxy) must be running.
# The CLI talks to the proxy, which injects X-Trusted-Client-IP.
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

For interactive use, run the same command without the `> /tmp/base-cli.out
2>&1 &` suffix and open the printed authorization URL in a browser.

Expected output includes the lease ID, profile, username, and expiration.
Record the lease ID:

```sh
export BASE_LEASE=<lease-id-from-output>
```

Inspect the lease:

```sh
kubectl -n "$BROKER_NS" get credentiallease "$BASE_LEASE" -o yaml
```

Expected:

```text
spec.profile = base-readonly.v1
status.phase = Issued
status.serviceAccountRef.name = iu-...
status.boundSecretRef.name = internal-lease-secret-...
status.bindingRef = null
```

Verify RBAC:

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

## 9. Run the elevated credential workflow

### 9.1 Create and approve an external approval

Use the CLI debug pod's `curl`:

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

### 9.2 Verify the Broker OIDC administrator-group endpoint

This is already done in section 6.4. If the Broker was restored since then,
repeat section 6.4 before continuing.

### 9.3 Submit the approved record to the Broker

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

Record the output:

```sh
export APPROVAL_REFERENCE=<approval-reference-from-output>
export ELEVATED_LEASE="$(kubectl -n "$BROKER_NS" get credentialleases \
  -o jsonpath='{range .items[*]}{.spec.approvalID}{" "}{.metadata.name}{"\n"}{end}' |
  awk -v id="$INSTANCE_CODE" '$1 == id {print $2}')"
```

Verify the waiting state:

```sh
kubectl -n "$BROKER_NS" get credentiallease "$ELEVATED_LEASE" -o yaml
```

Expected before redemption:

```text
spec.profile = cluster-ops-write.v1
status.phase = AwaitingRedemption
status.approvalExpirationTimestamp != nil
status.serviceAccountRef = null
status.boundSecretRef = null
status.bindingRef = null
```

### 9.4 Redeem the elevated reference

```sh
# Terminals B (Broker port-forward) and C (trusted proxy) must be running.
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

Expected lease state:

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

Verify the temporary resources:

```sh
SA="$(kubectl -n "$BROKER_NS" get credentiallease "$ELEVATED_LEASE" -o jsonpath='{.status.serviceAccountRef.name}')"
SECRET="$(kubectl -n "$BROKER_NS" get credentiallease "$ELEVATED_LEASE" -o jsonpath='{.status.boundSecretRef.name}')"
BINDING="$(kubectl -n "$BROKER_NS" get credentiallease "$ELEVATED_LEASE" -o jsonpath='{.status.bindingRef.name}')"

kubectl -n "$USER_NS" get sa "$SA" -o yaml
kubectl -n "$USER_NS" get secret "$SECRET" -o yaml
kubectl get clusterrolebinding "$BINDING" -o yaml
```

Required properties:

```text
ServiceAccount.automountServiceAccountToken = false
Secret.data is empty
Secret.stringData is empty
ClusterRoleBinding.roleRef.name = internal-user-cluster-ops-write-v1
```

Verify elevated RBAC:

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

### 9.5 Verify replay protection

Run the same redemption again with a different output path:

```sh
# Terminals B (Broker port-forward) and C (trusted proxy) must be running.
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

Expected: `Broker returned HTTP 409`. There must be no second TokenRequest and
no second kubeconfig.

## 10. Collect evidence and clean up

Collect evidence before deleting resources:

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

Delete the leases and identity:

```sh
kubectl -n "$BROKER_NS" delete credentiallease \
  "$BASE_LEASE" "$ELEVATED_LEASE" \
  --ignore-not-found

kubectl delete internaluser "$IU_NAME" --ignore-not-found
```

Do not bypass finalizers. The Controller must be allowed to clean temporary
resources before `InternalUser` deletion completes.

Remove the mock Deployment:

```sh
kubectl -n "$MOCK_NS" delete -k "$REPO_ROOT/test/mock"
```

Restore the Broker. Stop the proxy and port-forwards (Ctrl-C in terminals B
and C), remove the temporary hosts entry, delete the temporary CA resources,
and restore the objects snapshotted at the start of section 6:

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

`kubectl apply` does not remove fields added to the live object by the
strategic patches in sections 6.2 and 6.4. Verify the restored Deployment and
remove the mock-only items explicitly if they are still present:

```sh
kubectl -n "$BROKER_NS" get deploy internal-credential-broker -o json \
  | jq -r '.spec.template.spec.containers[0].env[].name,
           (.spec.template.spec.volumes[] | .name)'
```

The restored Broker must not reference `test-oidc-ca` or stale OIDC mock
settings. When in doubt, delete and recreate the Deployment from the restored
manifest.

## 11. Full adapter and MailHog path

`mock-approval-server` only stores external approval state. It does not submit
an approval to the Broker by itself. `mailhog` only receives mail when the
deployed adapter explicitly uses `approvaladapter.EmailNotifier`.

The current integration skeleton does not wire `mock.NewMockSource` or
`EmailNotifier` by default. To test the automatic external-approval path in a
cluster, add a test-only adapter entry point that wires:

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

The entry point also needs an event trigger or polling mechanism that passes
the external `instanceCode` into the pipeline. After that is available, inspect
MailHog with:

```sh
kubectl -n access-mock port-forward svc/mailhog 8025:8025
```

Open `http://localhost:8025` and verify that the message contains the approval
reference but no token, kubeconfig, private key, or Secret data.

Until that wiring exists, use the administrator manual approval path in
section 9 for cluster acceptance, and use the local acceptance tests in
section 2 for the complete adapter pipeline.

## 12. Related documents

- [Overview](internal-user-credentials-overview.md)
- [Architecture](internal-user-credentials-architecture.md)
- [User CLI](internal-user-credentials-cli.md)
- [Administrator CLI](internal-user-admin-cli.md)
- [Approval adapter](internal-user-approval-adapter.md)
- [E2E TODO matrix](internal-user-credentials-e2e-todo.md)
- [Mock components README](../test/mock/README.md)
