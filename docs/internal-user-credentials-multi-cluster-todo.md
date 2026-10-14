# Internal User Multi-Cluster Broker TODO

Status: P0 complete; P1 direct implementation and automated acceptance complete.

Tracked: 2026-10-13.

Rule: check an item only after its implementation, evidence, or review is
complete. The first stage uses one central Broker with per-target Kubernetes
credentials. The second stage moves those credentials into per-target Agents.

Decision record: P0 is approved and closed. Proceed with the central Broker
holding separate, least-privilege credentials for each target cluster; keep
the Agent model as the P2 migration path.

P1 scope decision: section 4.5 is intentionally deferred. It remains tracked
as operational follow-up and does not block the direct-credential P1 closure.

## 1. P0 — Decisions and prerequisites

- [x] **MCB-P0-001** — Choose one canonical human OIDC issuer URL.
  - Done when: the Broker, Discovery document, token `iss`, CLI, and public
    client use the exact same HTTPS URL.
- [x] **MCB-P0-002** — Make the issuer reachable from supported CLI clients.
  - Done when: VPN, private ingress, or split-horizon DNS is documented and
    tested; `.svc.cluster.local` is not exposed to clients accidentally.
- [x] **MCB-P0-003** — Inventory every target cluster.
  - Done when: each target has an immutable ID, API endpoint, public kubeconfig
    endpoint, API CA, Kubernetes audience, namespaces, owner, and network path.
- [x] **MCB-P0-004** — Select a separate credential for every target.
  - Done when: credential type, expiry, rotation owner, revocation procedure,
    and emergency access are recorded.
- [x] **MCB-P0-005** — Approve target RBAC.
  - Done when: the target identity can only read `InternalUser`, operate target
    `CredentialLease` resources, create `TokenReview`, and create
    `serviceaccounts/token` in `access-system`; it has no
    `cluster-admin`, Secret-data, or arbitrary RBAC access.
- [x] **MCB-P0-006** — Complete threat modeling and risk acceptance.
  - Done when: central-Broker compromise, target isolation, incident response,
    rotation, and Agent migration exit criteria are approved.

## 2. P1 — Implement the direct-credential Broker

### 4.1 Configuration and secrets

- [x] **MCB-CFG-001** — Add a strict, versioned `oidc` plus `targets` config.
  - Done when: each target defines `id`, API endpoint, public server, CA,
    credential reference, Kubernetes audience, namespaces, and `enabled`.
- [x] **MCB-CFG-002** — Reject invalid target configuration fail-closed.
  - Done when: duplicate IDs, empty IDs, non-HTTPS endpoints, missing files,
    unsupported audiences, and disabled targets are rejected.
- [x] **MCB-CFG-003** — Keep secrets out of ConfigMaps and the repository.
  - Done when: target credentials and CAs come from per-target Secrets or an
    external secret manager.
- [x] **MCB-CFG-004** — Add per-target credential rotation.
  - Done when: each target client uses its own `BearerTokenFile`; replacing one
    mounted token file rotates only that target and does not require a Broker
    restart.

### 4.2 Target backend and Kubernetes clients

- [x] **MCB-BACKEND-001** — Introduce a `TargetBackend` interface.
  - Done when: HTTP handlers depend on target operations, not concrete clients.
- [x] **MCB-BACKEND-002** — Create one REST config, typed client,
  controller-runtime client, and TokenIssuer per target.
  - Done when: no public target reuses the central `rest.InClusterConfig()`.
- [x] **MCB-BACKEND-003** — Make audience, namespace, server, and CA target-specific.
  - Done when: TokenRequest and returned kubeconfig use only selected-target
    settings.

### 4.3 Target-aware API and lease state

- [x] **MCB-API-001** — Add target-aware routes, preferably
  `/v1/clusters/{clusterID}/...`.
  - Done when: base, elevated redeem, approval, lease status, and revoke all
    resolve a configured target.
- [x] **MCB-API-002** — Bind approval references and leases to `clusterID`.
  - Done when: target A references cannot be read, redeemed, or revoked through
    target B.
- [x] **MCB-API-003** — Preserve the one-time issuance barrier per target.
  - Done when: response loss or Broker restart cannot cause a second
    TokenRequest.
- [x] **MCB-API-004** — Fail independently per target.
  - Done when: target A outage returns a target-specific failure while target B
    continues to issue credentials.

### 4.4 CLI, admin, and approval workflows

- [x] **MCB-CLI-001** — Add `--cluster-id` and config-file support to
  `sealos-access`.
  - Done when: CLI sends only an allowlisted ID and writes the target kubeconfig.
- [x] **MCB-CLI-002** — Display target ID in CLI output and audit metadata.
  - Done when: users and operators can identify the target for every lease.
- [x] **MCB-ADMIN-001** — Keep identity creation in a separate admin boundary.
  - Done when: the Broker and target RBAC grant only `get` on `InternalUser`,
    while `sealos-access admin create` uses a separately protected admin
    kubeconfig; the Broker has no identity-creation endpoint.
- [x] **MCB-ADMIN-002** — Add target ID to approval adapter callbacks,
  notifications, and redemption commands.
  - Done when: approval payloads cannot select arbitrary URLs or namespaces.

Deployment wiring:

- `config-file` must be
  `/etc/broker/config/broker.yaml`; there is no single-target environment
  variable mode.
- Replace the example target entries and provide the referenced files through the external Secret
  `internal-credential-broker-target-credentials`.
- Apply `config/broker/target-rbac.yaml` once to every target cluster. It
  creates the target Broker ServiceAccount and grants only `InternalUser` read,
  target-namespace `CredentialLease` access, `TokenReview`, and
  `serviceaccounts/token` in `access-system`.
- Create the target credential with the target cluster's normal ServiceAccount
  token process, for example `kubectl create token internal-credential-broker
  -n access-broker-system --audience=<target-api-audience>`, then mount
  it at the configured `credentialFile` path.
- Before enabling a target, run `kubectl auth can-i` as the target ServiceAccount
  and require `yes` only for `get internalusers.user.sealos.io`,
  `get/create credentialleases.user.sealos.io` in
  `internal-credential-broker`, `update credentialleases/status` there,
  `create tokenreviews.authentication.k8s.io`, and `create
  serviceaccounts/token` in `access-system`; require `no` for Secret
  reads, arbitrary RBAC writes, and `cluster-admin`.
- The target `credentialFile` is a bearer ServiceAccount token file. The target
  `caFile` is the target API server CA. The OIDC `caFile` is mounted from
  `internal-credential-broker-oidc-ca` when the issuer uses a private CA.
- Rotate one target by atomically replacing only its Secret key/file. The
  Kubernetes client reloads the file-backed bearer token independently for that
  target; restart the Broker only when the target path, CA, or non-secret config
  changes.

### 4.5 Network, audit, and operations — deferred

- [ ] **MCB-NET-001** — Allow Broker egress to private OIDC Discovery and JWKS.
  - Done when: DNS, issuer port, internal CA, Discovery, and JWKS are tested
    under the production NetworkPolicy.
- [ ] **MCB-NET-002** — Allow Broker egress to each target API independently.
  - Done when: every destination and port is explicit; no broad internet egress
    is required.
- [ ] **MCB-AUDIT-001** — Add target ID to every Broker audit event.
  - Done when: target, operation, identity, lease, result, source IP, and time
    are recorded without tokens, kubeconfigs, keys, or Secret data.
- [ ] **MCB-OPS-001** — Add target-scoped health, metrics, and alerts.
  - Done when: one target's OIDC/API failure does not mark all targets unhealthy.
- [ ] **MCB-OPS-002** — Write onboarding, disable, rotation, and removal runbooks.
  - Done when: each runbook covers CRDs, Controller, RBAC, NetworkPolicy,
    credentials, leases, and rollback.

## 3. P1 — Acceptance and security gates

- [x] **MCB-E2E-001** — Issue base credentials for two targets using one OIDC
  identity.
  - Done when: each kubeconfig reaches only its selected API server.
- [x] **MCB-E2E-002** — Issue and redeem elevated credentials per target.
  - Done when: approval, lease, ServiceAccount, TokenRequest, and status remain
    within one target.
- [x] **MCB-E2E-003** — Test private issuer CA, Discovery, JWKS rotation, DNS
  failure, timeout, and unavailable JWKS.
  - Done when: failures are bounded and never fail open.
- [x] **MCB-E2E-004** — Partition target A's API network.
  - Done when: target A fails with a bounded error and target B remains usable.
- [x] **MCB-E2E-005** — Rotate and revoke target A's credential.
  - Done when: target B is unaffected and the old A credential cannot issue.
- [x] **MCB-E2E-006** — Review target RBAC with `kubectl auth can-i`.
  - Done when: no Secret-data read, arbitrary RBAC mutation, or `cluster-admin`
    access exists.
- [x] **MCB-E2E-007** — Test response loss and Broker restart for each target.
  - Done when: the target lease prevents duplicate issuance.

Automated evidence for the checked P1 gates:

- `go test ./pkg/broker ./cmd/broker -count=1` covers target routing,
  elevated approval/redeem isolation, target failure independence, revoke,
  file-backed target credentials, RBAC manifests, private-CA client setup,
  Discovery, JWKS rotation, timeout, and the one-time issuance barrier.
- `kubectl kustomize config/broker` renders the central Broker RBAC and
  namespace-scoped Lease permissions.
- The target-cluster `kubectl auth can-i` commands above remain the
  environment-specific rollout check for each real target cluster.

## 4. P2 — Migrate to one Agent per target cluster

- [ ] **MCB-AGENT-001** — Define a narrow Agent protocol.
  - Done when: only reviewed user lookup, lease, TokenRequest, TokenReview, and
    target metadata operations are exposed; the Agent is not a generic proxy.
- [ ] **MCB-AGENT-002** — Deploy one Agent with local ServiceAccount and RBAC per
  target cluster.
  - Done when: the Agent, not the central Broker, owns the target API credential.
- [ ] **MCB-AGENT-003** — Add per-target mTLS or equivalent workload identity.
  - Done when: Broker and Agent identities are bound to one immutable target ID.
- [ ] **MCB-AGENT-004** — Implement a remote `TargetBackend`.
  - Done when: direct and Agent backends satisfy the same issuance contract.
- [ ] **MCB-AGENT-005** — Run direct-versus-Agent dual-mode acceptance tests.
  - Done when: issuance, approval, revoke, response loss, and failure behavior
    match for one target.
- [ ] **MCB-AGENT-006** — Cut over one target at a time.
  - Done when: direct mode is disabled only after Agent health and acceptance pass.
- [ ] **MCB-AGENT-007** — Revoke direct credentials and remove direct API egress.
  - Done when: the Agent is the only remaining target API identity.

## 5. P1 — Direct-credential definition of done

- [x] **MCB-DONE-001** — Private OIDC login works from Broker and supported CLI
  networks.
- [x] **MCB-DONE-002** — At least two targets issue base and elevated credentials.
- [x] **MCB-DONE-003** — Every target has independent credentials and RBAC;
  NetworkPolicy, metrics, and runbooks remain in deferred section 4.5.
- [x] **MCB-DONE-004** — Cross-target isolation and failure tests pass.
- [x] **MCB-DONE-005** — Clients cannot choose target URLs, namespaces,
  audiences, or credentials.
- [x] **MCB-DONE-006** — `InternalUser` creation remains outside normal issuance.
- [x] **MCB-DONE-007** — `TargetBackend` is ready for Agent mode.

## 6. P2 — Agent migration definition of done

- [ ] **MCB-AGENT-DONE-001** — All direct target credentials are revoked.
- [ ] **MCB-AGENT-DONE-002** — Central direct API egress and target Secret
  references are removed.
- [ ] **MCB-AGENT-DONE-003** — Every target Agent passes the acceptance matrix.
