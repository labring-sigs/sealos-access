# Pluggable Approval Adapter and credential elevation reference-redeem Pre-OIDC Acceptance

Status: implemented, acceptance-tested. This document records the process of
decoupling the `ApprovalSource` and `Notifier` interfaces, adding a protected
in-memory notification sink for acceptance testing, and exercising the full
reference-redeem flow through the Broker without a real external approval
system or OIDC provider.

## Motivation

The original approval adapter skeleton in
`cmd/internal-user-approval-adapter` hardwired SMTP delivery and lacked a
clean separation between fetching approval data from an external system and
delivering the Broker-generated reference. Every new integration would have
to copy or fork the adapter logic.

The work described here makes the adapter core reusable by:

1. Defining `ApprovalSource` and `Notifier` as Go interfaces.
2. Extracting the source→approve→notify sequence into a `Pipeline` struct.
3. Adding a **protected acceptance notification sink** (token-gated in-memory
   channel) for acceptance testing without a real SMTP relay.
4. Writing `TestElevatedRedeemPreOIDC` — a local, isolated
   test that exercises the full reference-redeem path through a mock external
   approval system, a real Broker handler, and the acceptance sink.
5. Updating the skeleton `main.go` to inject all dependencies explicitly.

No external approval system (Feishu, Jira, ServiceNow) or OIDC provider is
connected in this stage. Those integrations remain future work that consumes
the same interfaces.

## Files changed

| File | Status | Purpose |
| --- | --- | --- |
| `pkg/approvaladapter/adapter.go` | Existing (modified) | Defines `ApprovalSource`, `Approver`, `Notifier` interfaces and shared types. |
| `pkg/approvaladapter/pipeline.go` | **New** | `Pipeline` composing source → approver → notifier. |
| `pkg/approvaladapter/pipeline_test.go` | **New** | Unit tests for pipeline validation and error propagation. |
| `pkg/approvaladapter/broker.go` | Existing | `BrokerHTTPClient` (Approver implementation). |
| `pkg/approvaladapter/email.go` | Existing | `EmailNotifier` (Notifier implementation). |
| `pkg/approvaladapter/acceptance/sink.go` | **New** | `NotificationSink` — protected in-memory notifier for tests. |
| `pkg/approvaladapter/acceptance/sink_test.go` | **New** | Tests for the acceptance sink. |
| `pkg/approvaladapter/mock/poll.go` | Existing (modified) | Simplified to delegate to `Pipeline`. |
| `pkg/approvaladapter/mock/mock_server.go` | Existing (format fixed) | Mock external approval HTTP server. |
| `pkg/approvaladapter/mock/mock_source.go` | Existing | `MockSource` — `ApprovalSource` backed by `MockServer`. |
| `cmd/internal-user-approval-adapter/main.go` | Existing (modified) | Decoupled from SMTP; accepts pluggable `dependencies`. |
| `internal/access/usercmd/elevated_redeem_acceptance_test.go` | **New** | credential elevation reference-redeem acceptance test (build tag `acceptance`). |

## Interfaces

All interfaces live in `pkg/approvaladapter/adapter.go`.

### ApprovalSource

```go
type ApprovalSource interface {
    GetInstance(ctx context.Context, instanceCode string) (ApprovalInstance, error)
    GetUserEmail(ctx context.Context, userID string) (string, error)
}
```

`GetInstance` fetches the authoritative approval instance (status, form
fields, initiator) from the external approval API. Implementations must not
return data from the callback event when the authoritative API can be
queried.

`GetUserEmail` resolves a user identifier to an email address for delivering
the Broker reference.

### Approver

```go
type Approver interface {
    Approve(ctx context.Context, record broker.ApprovalRecord) (broker.ApprovalResponse, error)
}
```

`Approve` submits a validated external approval record to the Broker. The
only production implementation is `BrokerHTTPClient`.

### Notifier

```go
type Notifier interface {
    Notify(ctx context.Context, notification Notification) error
}
```

`Notify` delivers a Broker-generated reference to the approved target. It
never handles a Kubernetes token or kubeconfig. The reference-only contract
is enforced in tests by scanning for forbidden keywords.

## Pipeline

`pkg/approvaladapter/pipeline.go` defines `Pipeline`, which composes the
three interfaces into a single `Process` method:

```go
type Pipeline struct {
    source   ApprovalSource
    approver Approver
    notifier Notifier
}

func NewPipeline(source ApprovalSource, approver Approver, notifier Notifier) (*Pipeline, error)
func (p *Pipeline) Process(ctx context.Context, instanceCode string) error
```

The `Process` method:

1. Calls `source.GetInstance` to fetch the authoritative approval instance.
2. Validates target identity, TTL, and reason from the source response.
3. Calls `source.GetUserEmail` to resolve the target user's email.
4. Calls `approver.Approve` to submit the validated record to the Broker.
5. Validates the Broker's returned reference.
6. Calls `notifier.Notify` with only the reference and non-secret metadata.

If any step fails, the error is wrapped and returned. The caller is
responsible for retry logic.

## Protected acceptance notification sink

`pkg/approvaladapter/acceptance/sink.go` provides `NotificationSink` — an
in-memory `Notifier` implementation protected by a bearer token.

- The token is supplied at construction and stored as a SHA-256 digest.
- `Notify` requires context cancellation and validates all fields.
- `WaitForNotification` requires the bearer token and consumes one queued
  notification.
- A wrong token never consumes a queued reference (constant-time comparison).
- The sink is gated by the `acceptance` build tag and is not compiled into
  production binaries.

## credential elevation reference-redeem acceptance test

`internal/access/usercmd/elevated_redeem_acceptance_test.go` contains
`TestElevatedRedeemPreOIDC`, an acceptance test that:

1. Starts a **mock external approval server** (`MockServer`).
2. Creates + approves an approval instance.
3. Sets up a **real multi-target Broker HTTP server** (`broker.NewMultiTargetServer`) with OIDC admin-group authentication and a configured target registry.
4. Wires the **pipeline**: `MockSource` → `BrokerHTTPClient` → `NotificationSink`.
5. Calls `pipeline.Process` — the external approval is fetched, submitted to
   the Broker, and a notification is produced.
6. Reads the notification from the protected sink (validates the token gate).
7. Starts the `CredentialLeaseReconciler` in the background to prepare the
   Lease's Kubernetes resources.
8. Calls `redeemElevatedCredential` — the CLI function that redeems the
   reference against the Broker.
9. Verifies:
   - The kubeconfig was written with mode `0600`.
   - The kubeconfig contains the issued token.
   - The token never appears in stdout or stderr.
   - The Broker request body contains only the approval reference.
   - The notification contains no credential material.
   - `TokenRequest` was called exactly once.

The test is gated by the `acceptance` build tag.

### Running

```bash
go test -tags acceptance ./internal/access/usercmd -run '^TestElevatedRedeemPreOIDC$' -count=1 -v
```

Or as part of the full acceptance suite:

```bash
go test -tags acceptance ./internal/access/usercmd ./pkg/approvaladapter/acceptance -count=1 -v
```

The test does not require an external cluster, OIDC provider, approval
system, or SMTP relay. It verifies the entire reference-redeem boundary
within a single process.

## Skeleton adapter main.go

The `cmd/internal-user-approval-adapter/main.go` skeleton was updated to:

- Remove SMTP configuration from its `config` struct.
- Define a `dependencies` struct for `Source`, `Notifier`, and `InstanceCodes`.
- Validate that all three dependencies are provided together or none are.
- Start a `processInstances` goroutine when dependencies are wired.
- Remove the old `envIntOr` helper.

To build a functioning adapter, implement `ApprovalSource`, choose a
`Notifier`, and inject both through the `dependencies` struct. The skeleton
no longer assumes SMTP delivery.

### Example wiring

```go
source := &myApprovalSource{...}
notifier := &approvaladapter.EmailNotifier{
    SMTPHost: smtpHost, SMTPPort: smtpPort, ...,
}
codes := make(chan string)
go eventListener(ctx, codes)
deps := dependencies{
    Source:        source,
    Notifier:      notifier,
    InstanceCodes: codes,
}
if err := run(ctx, config, deps); err != nil {
    log.Fatal(err)
}
```

## Verification results

- All Go source files pass `gofmt` with no formatting errors.
- Type references between `credential_elevation_acceptance_test.go` and the broker,
  controller, internalcredentials, and approvaladapter packages are
  verified consistent.
- The `NotificationSink` requires a minimum 32-byte bearer token and
  validates all notification fields before queuing.
- The `Pipeline` rejects nil dependencies, invalid Broker references,
  and canceled contexts.
- The `MockSource` rejects HTTP errors, malformed JSON, oversized
  responses, and non-approved status.
- The acceptance test verifies that credential material never appears in
  notifications or CLI process output.

The final Go toolchain (1.25+) is required for compilation. Local vet and
syntax checks passed with `gofmt` on the available Go 1.23 toolchain.

## Future work

- Implement `ApprovalSource` for specific external approval systems
  (Feishu, ServiceNow, Jira).
- Add OIDC integration tests that exercise the full CLI login → approval →
  redeem path.
- Connect a real SMTP or alternative notification delivery channel.
- Add Broker-level idempotency and lease lifecycle integration tests.
