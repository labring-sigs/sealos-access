# Internal User Credentials E2E Test TODO

Status: proposed supplement to the existing acceptance test plan. These items
extend coverage to concurrency, failure recovery, approval-adapter full chain,
clock-skew boundaries, audit completeness, and information-disclosure scenarios
that are not yet covered by the existing acceptance matrix.

Tracked: 2026-09-11.

---

## P0 — Release-blocking

- [ ] **E2E-ADP-001** — Full approval-adapter chain
  > Send an `APPROVED` event from the external approval system → adapter
  > validates → submits with an OIDC admin-group token to Broker → Broker creates
  > `AwaitingRedemption` CredentialLease → adapter notifies the target user →
  > user logs in via CLI and redeems → elevated kubeconfig is returned. Verify
  > consistency across all five components and that the approval reference is
  > delivered only in memory/notification, never in Kubernetes resources.

- [ ] **E2E-CONC-002** — Same-user concurrent elevated redeem (replay race)
  > Two CLI processes redeem the identical approval reference simultaneously.
  > The first succeeds and consumes the lease; the second is rejected by the
  > Broker's replay barrier. Exactly one TokenRequest occurs and one kubeconfig
  > is issued.

- [ ] **E2E-FAIL-001** — TokenRequest response loss
  > Broker crashes between sending the TokenRequest (API Server returns
  > `serviceaccounts/token` response) and writing the HTTP response to the CLI.
  > After restart, the consumed lease blocks replay. The kubeconfig is lost
  > permanently (one-time issuance invariant).

- [ ] **E2E-AUD-001** — Audit completeness for base and elevated flows
  > Run a complete base issuance and an elevated redeem. Every critical
  > operation (Broker API call, Lease state transition, TokenRequest) has a
  > corresponding `AuditEvent`. No token, kubeconfig, or Secret data appears
  > in the audit log.

---

## P1 — Required before production rollout

- [ ] **E2E-CONC-001** — Same-user concurrent base requests
  > Two CLI processes for the same user initiate base requests in parallel.
  > Each obtains a distinct CredentialLease and a valid kubeconfig. The
  > Controller manages exactly one stable ServiceAccount; both TokenRequests
  > succeed.

- [ ] **E2E-CONC-003** — Admin bulk suspend
  > Administrator suspends 10+ active users simultaneously. All new issuances
  > are blocked, stable bindings are removed, active leases become Revoked, and
  > the Controller cleans up all temporary resources.

- [ ] **E2E-CTRL-001** — Controller outage during user operations
  > Controller is stopped for 5 minutes while users are created and credentials
  > are issued. After recovery, the Controller creates all missing
  > ServiceAccounts, ClusterRoleBindings, and empty bound Secrets. End state
  > matches the uninterrupted baseline.

- [ ] **E2E-CTRL-003** — Controller resource adoption prevention
  > An external actor deletes the owned ServiceAccount or ClusterRoleBinding
  > for an active CredentialLease. The Controller recreates the resource if the
  > lease still requires it, or leaves it absent if the lease is terminal. No
  > orphaned or wrongly-owned resources remain.

- [ ] **E2E-AUTH-001** — Base credential RBAC enforcement
  > Using the base kubeconfig, attempt `kubectl create` on `InternalUser` and
  > `CredentialLease`. The `base-readonly.v1` profile denies write operations.

- [ ] **E2E-AUTH-002** — Elevated credential RBAC enforcement
  > Using the elevated kubeconfig, attempt to create or delete a
  > ServiceAccount or ClusterRoleBinding. The elevated profile denies
  > operations outside its granted scope.

- [ ] **E2E-AUTH-004** — Revoked admin kubeconfig
  > Attempt to use an administrator kubeconfig whose credentials have been
  > revoked. The Kubernetes API rejects the request; the admin CLI exits
  > with a clear error.

- [ ] **E2E-TIME-002** — Token expiry mid-session
  > Obtain an elevated kubeconfig with TTL=10 min. Succeed at t+9 min, then
  > fail at t+11 min. The Kubernetes API rejects the expired token.

- [ ] **E2E-TIME-003** — Manual revoke before token expiry
  > Administrator revokes an elevated lease before its natural expiry. The
  > Controller removes the temporary ServiceAccount and binding. Subsequent
  > kubectl requests using the issued kubeconfig are rejected.

- [ ] **E2E-TIME-004** — Clock skew between Broker and OIDC issuer
  > Introduce a +/- 60 s clock skew between the Broker and the OIDC issuer.
  > Token `nbf`/`exp` validation succeeds within the configured skew window
  > and fails outside it.

- [ ] **E2E-ADP-003** — External approval revoked before redemption
  > External approval system sends `APPROVED` then `REVOKED` within 10 s. The
  > adapter submits the approval to Broker; Broker marks the lease as Revoked
  > before the user can redeem. The reference is dead on arrival.

- [ ] **E2E-INF-001** — Error information disclosure
  > Send a malformed request body and an oversized request to the Broker API.
  > The response contains only a generic error; no internal configuration,
  > file paths, stack traces, or version strings are leaked.

- [ ] **E2E-AUD-002** — Failed login audit
  > Repeated failed OIDC logins from the same client. Each attempt produces an
  > audit record containing IP, issuer, and subject; no passwords, refresh
  > tokens, or secrets are logged.

---

## P2 — Resilience & hardening

- [ ] **E2E-CONC-004** — Broker + Controller simultaneous restart
  > Broker is processing a base request while the Controller is being rolled.
  > The TokenRequest and Lease persistence succeed. The Controller recovers
  > and reconciles to the correct end state.

- [ ] **E2E-FAIL-002** — Broker crash after Lease persistence but before HTTP response
  > Broker persists the `CredentialLease` (state=Consumed) then crashes before
  > writing the HTTP 200 response. The CLI sees a connection error. A retry is
  > rejected by the replay barrier. No duplicate credential is issued.

- [ ] **E2E-FAIL-003** — Broker rolling update with live traffic
  > Both Broker replicas are rolled in sequence while base requests arrive.
  > At least one replica stays Ready throughout. Brief 503 errors are tolerated;
  > no credential is issued incorrectly or inconsistently.

- [ ] **E2E-FAIL-004** — Broker-Kubernetes API network partition
  > Network between Broker and Kubernetes API is severed for 30 seconds.
  > TokenRequest fails; the Lease is not marked Prepared/Consumed. After
  > recovery, subsequent requests succeed.

- [ ] **E2E-CTRL-002** — Controller watch backlog after extended outage
  > Controller is stopped for 10 minutes while many InternalUsers and
  > CredentialLeases are created. On restart, the Controller processes the
  > backlog without producing duplicate resources or reconciliation errors.

- [ ] **E2E-CTRL-004** — Admission webhook outage
  > The Controller's binding webhook is unavailable (fail-closed). Controller
  > attempts to create/update a ClusterRoleBinding are rejected; the Controller
  > retries. After the webhook recovers, the operation succeeds.

- [ ] **E2E-AUTH-003** — Simultaneous base and elevated usage
  > A user holds both a base kubeconfig and an elevated kubeconfig in two
  > terminals. Each works according to its profile. After the elevated token
  > expires, `kubectl auth can-i` for the elevated scope returns false.

- [ ] **E2E-TIME-001** — Approval expiry race (edge of window)
  > User redeems an elevated reference exactly 1 second before the approval
  > expiry. The Broker rejects the request because the approval is already
  > expired. The user must request a fresh approval.

- [ ] **E2E-ADP-002** — Approval adapter idempotent retry
  > External approval system retries the same `APPROVED` event (same approval
  > ID). The adapter re-submits to Broker; Broker returns the existing
  > `AwaitingRedemption` lease. No duplicate lease, no duplicate notification,
  > and no duplicate credential.

- [ ] **E2E-ADP-004** — OIDC bearer-token rotation
  > The approval adapter's OIDC bearer token expires or is revoked. Requests
  > with the old token are rejected. After rotating the token, the adapter
  > resumes normal operation without manual Broker intervention.

- [ ] **E2E-INF-002** — Expired/revoked reference information leak
  > Attempt to redeem an expired or revoked approval reference. The Broker
  > returns a generic "not found" or "inappropriate state" error without
  > revealing the reference's original target user, profile, or issuer.

- [ ] **E2E-INF-003** — Direct Broker access without CLI
  > Access the Broker API with `curl` (no OIDC bearer token). Returns 401
  > Unauthorized. No kubeconfig or credential data is served.

- [ ] **E2E-AUD-003** — Administrator action audit
  > Administrator creates, suspends, resumes, and deletes an InternalUser.
  > Each management action produces an audit record with identity, action,
  > and timestamp.

- [ ] **E2E-SCALE-001** — Broker scale up under load
  > Scale Broker from 2 to 5 replicas while base requests are in flight. New
  > replicas join the Service and start serving. All requests succeed; no 5xx
  > errors.

- [ ] **E2E-SCALE-002** — Broker scale down with graceful shutdown
  > Scale Broker from 5 to 2 replicas while requests are being processed.
  > SIGTERM allows in-flight requests to complete. The remaining 2 replicas
  > handle all traffic.

---

## Key

| Priority | Meaning |
| --- | --- |
| **P0** | Must pass before deployment or external-route enablement. |
| **P1** | Must pass before production rollout. A P1 exception requires a written security review and risk acceptance. |
| **P2** | Hardening and resilience; no release gate, but recommended for production confidence. |
