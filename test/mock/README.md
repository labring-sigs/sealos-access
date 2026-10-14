# Mock Components for Internal User Credentials E2E Testing

This directory contains deployable mock/simulation components that replace
external dependencies for end-to-end testing of the internal user credentials
system on Kubernetes.

For the complete local and cluster test runbook, including the required OIDC
TLS configuration and current adapter limitations, see
[Internal User Credentials Mock E2E Test Process](../../docs/internal-user-credentials-mock-e2e.md).

## Components

| Component | Image | Replaces | Description |
|-----------|-------|----------|-------------|
| `test-oidc-provider` | `ghcr.io/labring/sealos-test-oidc-provider` | External OIDC issuer (Okta, Keycloak, etc.) | In-memory OIDC provider with configurable users, clients, and token claims |
| `mock-approval-server` | `ghcr.io/labring/sealos-mock-approval-server` | External approval system API | In-memory HTTP server for creating, querying, approving, and rejecting approvals |
| `mailhog` | `mailhog/mailhog` | SMTP relay | Captures all outgoing emails; inspect via Web UI at port 8025 |

## Quick Start

```sh
# Deploy all mock components into the access-mock namespace
kubectl apply -k ./test/mock

# Verify all pods are ready
kubectl -n access-mock get pods
```

## Service Endpoints (in-cluster)

| Service | Port | Endpoint |
|---------|------|----------|
| `test-oidc-provider` | 8080 | `http://test-oidc-provider.access-mock.svc:8080` |
| `mock-approval-server` | 8080 | `http://mock-approval-server.access-mock.svc:8080` |
| `mailhog` SMTP | 1025 | `mailhog.access-mock.svc:1025` |
| `mailhog` Web UI | 8025 | `http://mailhog.access-mock.svc:8025` |

## Mock Approval Server API

```text
POST /v1/approvals                    — create an approval (requires JSON body)
GET  /v1/approvals/{id}               — get approval status
POST /v1/approvals/{id}/approve       — approve
POST /v1/approvals/{id}/reject        — reject
GET  /healthz                         — health check
```

### Create an approval

```sh
curl -X POST http://mock-approval-server.access-mock.svc:8080/v1/approvals \
  -d '{
    "approvalCode": "cluster-ops-write.v1",
    "initiatorID": "test-user-001",
    "initiatorEmail": "test-user-001@example.internal",
    "targetSubject": "test-user-001",
    "reason": "Incident remediation - mock E2E test",
    "requestedTTLSeconds": 1800,
    "form": {"subject": "test-user-001", "ttl": "1800", "reason": "Incident remediation"}
  }'
```

### Approve an approval

```sh
curl -X POST http://mock-approval-server.access-mock.svc:8080/v1/approvals/{instanceCode}/approve
```

### Pre-seed approved approvals

Set the `SEALOS_MOCK_APPROVAL_SEED` environment variable on the deployment:

```json
[
  {
    "approvalCode": "cluster-ops-write.v1",
    "initiatorID": "test-user-001",
    "initiatorEmail": "test-user-001@example.internal",
    "targetSubject": "test-user-001",
    "reason": "Pre-seeded approval",
    "requestedTTLSeconds": 1800,
    "form": {"subject": "test-user-001", "ttl": "1800", "reason": "Pre-seeded"}
  }
]
```

## OIDC Test Provider Default Users

| Subject | Email | Name |
|---------|-------|------|
| `test-user-001` | `test-user-001@example.internal` | Test User One |
| `test-user-002` | `test-user-002@example.internal` | Test User Two |

Update the `SEALOS_TEST_OIDC_CONFIG` environment variable in the deployment
to add or modify users, clients, or issuer URL.

## Configuring Approval Adapter for Mock Mode

When deploying the `internal-user-approval-adapter`, point it at the mock
services:

```yaml
# ConfigMap overrides
data:
  broker-url: https://internal-credential-broker.access-broker-system.svc
  smtp-host: mailhog.access-mock.svc
  smtp-port: "1025"
  smtp-from: internal-credential-broker@example.internal
```

The `ApprovalSource` implementation for the adapter should use:
- Base URL: `http://mock-approval-server.access-mock.svc:8080`
- No TLS (HTTP-only mock)

---

## CLI Debug Pod

A convenience deployment (`sealos-access`) provides the unified CLI and
`curl` pre-installed in a running Alpine container. Exec into it to run commands
or inspect the mock HTTP endpoints interactively:

```sh
# Deploy (included in the top-level kustomization)
kubectl apply -k ./test/mock

# Exec into the pod
kubectl -n access-mock exec -it deploy/sealos-access -- /bin/sh

# Inside the pod, run CLI commands directly
sealos-access --help
sealos-access admin --help
```

### Sample config (place inside the pod)

The user and administrator subcommands read separate YAML configuration files
from `~/.config/sealos/access/user.yaml` and
`~/.config/sealos/access/admin.yaml`. For mock mode:

```sh
mkdir -p ~/.config/sealos/access
cat > ~/.config/sealos/access/user.yaml << 'CONF'
broker-url: https://internal-credential-broker.access-broker-system.svc:8443
broker-server-name: internal-credential-broker.access-broker-system.svc
oidc-issuer: https://test-oidc-provider.access-mock.svc
client-id: test-oidc-client
output: /tmp/internal-user-kubeconfig
requested-ttl-seconds: 3600
no-browser: true
CONF
```

### Obtain a base credential (inside the pod)

```sh
sealos-access login \
  --broker-url https://internal-credential-broker.access-broker-system.svc:8443 \
  --oidc-issuer https://test-oidc-provider.access-mock.svc \
  --client-id test-oidc-client \
  --no-browser \
  --output /tmp/internal-user-kubeconfig

# Follow the printed authorization URL to complete OIDC login,
# then the kubeconfig is written to /tmp/internal-user-kubeconfig.
```

### Administrator operations (inside the pod)

The admin CLI requires a kubeconfig for creating InternalUser objects.
Mount your admin kubeconfig or use the pod's ServiceAccount:

```sh
sealos-access admin create \
  --issuer https://test-oidc-provider.access-mock.svc \
  --subject test-user-001
```

### Build the CLI image

```sh
cd .
docker build -t sealos-access -f test/mock/cli/Dockerfile .
```
