# Internal User Approval Adapter

`internal-user-approval-adapter` is the external approval bridge for
`cluster-ops-write.v1`. It receives an approved event from an external
approval system, fetches the authoritative approval instance, submits one
non-secret approval record to the Broker, and delivers the Broker-generated
reference to the target by email.

The adapter is intentionally stateless with respect to approval events. It
does not persist event IDs, approval records, references, tokens, or
kubeconfigs. External system retries are handled by submitting the same
instance code; the Broker persists the `CredentialLease` and uses that code
as its idempotency key. The adapter may cache a short-lived access token
for the external approval API in memory.

## Architecture

```text
External approval system sends approved event
        |
        v
Validate event authenticity (signature / token)
        |
        v
[ApprovalSource] Fetch instance + form from external approval API
        |
        v
Validate approval definition code, OIDC subject, TTL, and reason
        |
        v
[Approver] POST /v1/clusters/{clusterID}/internal/credentials/approve with an OIDC admin-group token
        |
        v
[Notifier] Send returned reference to the initiator's email
```

The event payload is only a wake-up signal. Target identity, TTL, and reason
come from the complete approval instance fetched by the `ApprovalSource`. The
OIDC issuer and approval definition code are fixed deployment values. The
event endpoint returns only `processed` or `ignored`; it never returns the
reference.

## Abstract interfaces

All interfaces are defined in
[`pkg/approvaladapter/adapter.go`](../pkg/approvaladapter/adapter.go).

### ApprovalSource

`ApprovalSource` is the only interface you must implement. It fetches
authoritative approval data from the external approval system:

```go
type ApprovalSource interface {
    GetInstance(ctx context.Context, instanceCode string) (ApprovalInstance, error)
    GetUserEmail(ctx context.Context, userID string) (string, error)
}
```

- `GetInstance` retrieves the full approval instance (status, form fields,
  initiator) from the external approval API. The `ApprovalInstance` contains
  `InstanceCode`, `ApprovalCode`, `ClusterID`, `Status`, `InitiatorID`, and a
  JSON-encoded `Form`. `ClusterID` is required and must identify a configured
  Broker target. Implementations **must not** return data from the callback event
  when the authoritative API can be queried.
- `GetUserEmail` resolves a user identifier to an email address for
  delivering the Broker reference.

**Polling-based implementation** — poll the external system's approval list
endpoint periodically, detect new APPROVED instances, fetch each one, and
feed it to the adapter pipeline.

**Webhook-based implementation** — receive callback events via an HTTP
endpoint, extract the instance code, call `GetInstance` to fetch the
authoritative data, validate it, then proceed.

### Approver

`Approver` submits a validated external approval record to the Broker:

```go
type Approver interface {
    Approve(ctx context.Context, record broker.ApprovalRecord) (broker.ApprovalResponse, error)
}
```

### Notifier

`Notifier` delivers the Broker-generated one-time reference to the approved
target. It never handles a Kubernetes token or kubeconfig:

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

## Available implementations

### BrokerHTTPClient (Approver)

[`BrokerHTTPClient`](../pkg/approvaladapter/broker.go) submits an approved
record to the Broker's dedicated HTTPS endpoint. It requires an HTTPS Broker
URL and an `*http.Client` configured with the Broker CA and an Authorization
header carrying an OIDC token from the configured administrator group. The
client enforces HTTPS, encodes the `ApprovalRecord` as JSON, and decodes the
`ApprovalResponse`.

```go
client, _ := approvaladapter.NewBrokerHTTPClient(brokerURL, clusterID, httpClient)
response, err := client.Approve(ctx, record)
```

The endpoint is set to `{BrokerURL}/v1/clusters/{clusterID}/internal/credentials/approve`.

### EmailNotifier (Notifier)

[`EmailNotifier`](../pkg/approvaladapter/email.go) sends only the one-time
Broker reference and non-secret approval metadata via SMTP. It never includes
a token or kubeconfig in the message body.

```go
notifier := &approvaladapter.EmailNotifier{
    SMTPHost: smtpHost, SMTPPort: smtpPort, SMTPUsername: smtpUser,
    SMTPPassword: smtpPass, From: smtpFrom, Subject: subject,
}
err := notifier.Notify(ctx, notification)
```

## Skeleton main.go

The entry point at
[`cmd/internal-user-approval-adapter/main.go`](../cmd/internal-user-approval-adapter/main.go)
is deliberately a skeleton. It wires the `BrokerHTTPClient` and
`EmailNotifier`, parses configuration from environment variables and flags,
validates required settings, and starts an HTTPS server with a `/healthz`
endpoint.

**To build a functioning adapter, implement `ApprovalSource` and wire it into
`run()`:**

```go
func run(ctx context.Context, config config) error {
    // Wire the reusable components.
    approver, _ := approvaladapter.NewBrokerHTTPClient(config.BrokerURL, httpClient)
    notifier := &approvaladapter.EmailNotifier{...}

    // Implement and wire your ApprovalSource.
    source := &myApprovalSource{...}

    // Start processing (example: webhook handler or polling loop).
    mux := http.NewServeMux()
    mux.HandleFunc("/v1/internal/approval/events", func(w http.ResponseWriter, r *http.Request) {
        // 1. Validate event authenticity.
        // 2. Extract instance code.
        // 3. Call source.GetInstance(ctx, instanceCode).
        // 4. Validate instance (status, approval code, fields).
        // 5. Build broker.ApprovalRecord.
        // 6. Call approver.Approve(ctx, record).
        // 7. Resolve email via source.GetUserEmail(ctx, initiatorID).
        // 8. Call notifier.Notify(ctx, notification).
    })

    // Start HTTPS server...
}
```

Key configuration variables read by the skeleton:

| Variable | Purpose |
| --- | --- |
| `BROKER_URL` | Broker HTTPS endpoint |
| `BROKER_CA_FILE` | Broker server CA certificate |
| `BROKER_BEARER_TOKEN` | OIDC bearer token for the configured administrator group |
| `BROKER_BEARER_TOKEN_FILE` | File containing that OIDC bearer token |
| `SMTP_HOST` / `SMTP_PORT` | SMTP relay address |
| `SMTP_USERNAME` / `SMTP_PASSWORD` | SMTP credentials (optional) |
| `SMTP_FROM` | Email sender address |
| `ADAPTER_TLS_CERT_FILE` / `ADAPTER_TLS_KEY_FILE` | Server TLS for the adapter's HTTPS listener |

Add your own variables for the external approval system credentials (API
key, app credentials, webhook signing secret, etc.).

## Form contract

The adapter relies on a fixed form schema defined in the external approval
system and configured on the adapter side. Typical fields include:

| Field | Example | Rule |
| --- | --- | --- |
| Subject | `alice` | Immutable OIDC subject of the InternalUser. |
| TTL | `1800` | Seconds; Broker policy limits this to 600 through 3600. |
| Reason | `incident remediation` | Required operator-readable reason, at most 2048 bytes. |

The adapter fixes the OIDC issuer from configuration (e.g.
`INTERNAL_USER_TARGET_ISSUER`) and does not accept an issuer from the event
or form. It also requires the fetched instance status to be `APPROVED` and
its approval definition to match the configured approval code.

## Retry and idempotency

The adapter does not persist event IDs or approval records. Idempotency is
delegated to the Broker: the Broker uses `ApprovalRecord.ApprovalID` (the
external system's instance code) as its idempotency key. If the same instance
code is submitted again, the Broker returns the existing
`ApprovalResponse` instead of creating a duplicate lease.

- If the external API lookup, Broker submission, or email delivery fails, the
  adapter returns a retryable HTTP 503 (webhook model) or logs and retries
  on the next poll cycle (polling model).
- If Broker submission succeeded but email delivery failed, the next attempt
  receives the same reference from the Broker's idempotency path and retries
  notification.
- No adapter database or event-consumption checkpoint is required.

## Run locally

Build from the repository root:

```sh
make build-internal-user-approval-adapter TARGETARCH=amd64
./bin/internal-user-approval-adapter-amd64 --help
```

The process requires HTTPS for its callback listener, the external approval
system credentials, a configured approval code, a fixed OIDC issuer, a Broker
CA, an OIDC bearer token for the configured administrator group, and an SMTP
relay with sender address.

Do not put API secrets, signing tokens, SMTP passwords, or private keys in a
ConfigMap. Use Kubernetes Secrets or an external secrets store.

## Kubernetes deployment

The manifests are in
`config/internal-user-approval-adapter`. Before applying them:

1. replace all placeholder URLs, approval code, issuer, SMTP host, and exact
   NetworkPolicy egress CIDRs;
2. create `internal-credential-approval-adapter-secrets` with the external
   approval system, SMTP credentials, and Broker bearer token;
3. create `internal-credential-approval-adapter-broker-ca` containing the
   Broker server CA as `ca.crt`;
4. expose the adapter's event endpoint (e.g. `/v1/internal/approval/events`)
   through the approved ingress/WAF route, forwarding only the callback and
   not the Broker's user routes.

The adapter ServiceAccount has `automountServiceAccountToken: false` and no
Kubernetes RBAC. NetworkPolicy allows callback ingress from the configured
ingress gateway, Broker egress to the adapter namespace, external approval
API egress, and the approved SMTP relay. Replace the documentation-only
public CIDRs in the template before deployment.

## Failure behavior summary

| Failure | Consequence |
| --- | --- |
| External API lookup fails | Retry (503 or next poll cycle) |
| Broker submission fails | Retry; idempotent on instance code |
| Email delivery fails | Retry on next event delivery; Broker returns existing reference |
| Validation fails (status, code, fields) | Return `ignored`; no retry |

The user must redeem the reference with a fresh OIDC login through
`sealos-access elevated redeem`. The reference is not an authentication
factor. The adapter never receives or handles the resulting Kubernetes token
or kubeconfig.

The administrator `sealos-access admin approve` command is the documented
manual/break-glass equivalent of the adapter's Broker submission capability.
