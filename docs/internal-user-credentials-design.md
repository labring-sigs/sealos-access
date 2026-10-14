# Internal User Credentials Design Archive

Status: design archive only. This document records the decisions made in the Q1-Q82
discussion. It is not an implementation or a deployment approval. No
InternalUser or CredentialLease API from this design is considered deployed.

## 1. Scope and principles

- One InternalUser represents exactly one real internal person. Shared accounts
  are forbidden.
- An internal user does not create a Sealos namespace and does not create the
  existing standard User resource.
- Daily credentials use Kubernetes ServiceAccount TokenRequest. Client
  certificates are reserved for an independent break-glass administrator path.
- Least privilege, fail-closed behavior, short-lived credentials, one-time
  delivery, and centralized audit are mandatory.
- The kubeconfig is a bearer credential. A copied kubeconfig can be used by
  somebody else; separate accounts and audit records do not provide
  cryptographic proof of the human holding the file.

## 2. Identity and lifecycle

InternalUser.spec contains only:

    identity:
      issuer: https://<allowlisted-oidc-issuer>
      subject: <opaque-immutable-oidc-subject>
    roleProfile: base-readonly.v1
    suspend: false

The issuer must be an HTTPS issuer from a server-side allowlist. The subject is
opaque and immutable. The issuer and subject cannot be changed after creation.
The caller does not provide a human-readable username. The controller derives a
stable Kubernetes-safe identifier:

    iu-<first-24-hex-digits-of-sha256(issuer + NUL + subject)>

The derived identifier is used for the stable ServiceAccount name and is exposed
in status, not accepted as an identity input. The object name must match the
derived identity. This prevents a rename from taking over another person's
credentials and makes duplicate identity creation deterministic.

Each person has one long-lived InternalUser and one stable ServiceAccount in
access-system. The identity remains stable while credentials are
issued per request.

spec.suspend: true immediately:

1. rejects new issuance;
2. removes the stable base permission binding;
3. revokes and cleans up all active leases and their bound objects.

The ServiceAccount and identity object remain so that a controlled resume can
reconcile the base binding. Permanent revocation is deletion. Finalizers must
remove all owned bindings, bound objects, temporary ServiceAccounts, and the
stable ServiceAccount before the identity object disappears.

## 3. API and resource placement

The design introduces two resources in user.sealos.io/v1:

- Cluster-scoped InternalUser.
- Namespaced CredentialLease, always in access-broker-system.

CredentialLease.spec records the target, requester identity, fixed profile,
requested TTL, and optional external approval reference. Its status records
only lifecycle and resource references. It must never contain a token,
kubeconfig, private key, or Secret data.

CredentialLease is an internal broker resource, not a user-facing API. Ordinary
internal users and administrator clients must not create, update, patch, or
delete CredentialLease objects through the Kubernetes API. They use the Broker
API instead; the Broker derives identity from verified OIDC claims and uses its
dedicated ServiceAccount to create or update the Lease. The Broker API is also
the only user-facing path for requesting, approving, or redeeming credentials.
The Controller observes and reconciles the Lease but is not exposed to users.

Kubernetes RBAC must deny ordinary user identities access to
credentialleases.user.sealos.io, including get, list, watch, create, update,
patch, and delete. Administrator access to Lease mutation is also denied so
that the admin endpoint remains the authorization and audit boundary. This
RBAC rule is independent of NetworkPolicy; network isolation does not grant or
remove Kubernetes API permissions. Users may query Lease status through the
Broker API, subject to identity and ownership checks.

The dedicated namespaces are:

- access-system: stable person ServiceAccounts, temporary
  ServiceAccounts, and empty TokenRequest bound Secrets.
- access-broker-system: CredentialLeases and the Broker deployment.
- access-controller-system: the independently deployed Controller and its
  webhook.

The existing User controller remains separate and unchanged.

## 4. Permission profiles

Profiles are platform-maintained, fixed, and versioned. Callers may select a
profile name but may not submit arbitrary RBAC rules or an arbitrary
ClusterRole.

### base-readonly.v1

- Stable binding to the person's ServiceAccount.
- Maximum credential TTL: 24 hours.
- Only explicitly listed get, list, and watch operations required by the
  approved operational command set.
- No generic Kubernetes view role.
- No Secrets, RBAC, identity resources, node credentials, webhook
  configuration, or other resources not in the reviewed allowlist.

### cluster-ops-write.v1

- Temporary binding only; never add elevated permission to the stable
  ServiceAccount.
- One temporary ServiceAccount and binding per lease.
- Maximum credential TTL: 1 hour.
- Requires an external approval record created or approved by an authorized
  administrator, plus an immutable external approval reference at issuance.
- The concrete resource list must be reviewed against admission controls before
  production use. In particular, workload write access can become a privilege
  escalation path if a user can create a Pod or workload that uses another
  ServiceAccount.

Profile names and their backing ClusterRoles are immutable by version. New
permissions require a new profile version and review; an existing version is
not silently broadened.

## 5. CredentialLease state machine

The allowed one-way phases are:

    Pending -> Prepared -> Issued
    Pending -> Failed
    Prepared -> Failed
    Prepared -> Revoked
    Issued -> Expired
    Issued -> Revoked

Failed, Revoked, and Expired are terminal. Broker and Controller must
never move a terminal lease back to a signable state.

For every lease, the Controller creates one unique empty Opaque bound Secret.
The Secret contains only references/metadata and never contains a token. The
Secret is bound to the TokenRequest so deleting it invalidates the associated
bound token according to Kubernetes token semantics.

For an elevated profile, the Controller additionally creates a temporary
ServiceAccount and ClusterRoleBinding. Cleanup order is:

1. remove the RoleBinding or ClusterRoleBinding;
2. delete the bound Secret;
3. delete the temporary ServiceAccount;
4. remove the Lease finalizer after all owned objects are gone.

Owner and lease labels must be checked before every adoption or deletion. An
owner mismatch is fail-closed: do not adopt, modify, or delete the object; keep
the finalizer and alert for manual handling.

The API Server's actual TokenRequest.status.expirationTimestamp is the source
of truth. The requested TTL is only an upper-bounded request. The actual
expiration must be stored in Lease status and used for cleanup. A periodic scan
must compensate for missed events, restarts, and failed cleanup, with a target
cleanup delay below one minute.

If TokenRequest succeeds but the response is lost, the lease is considered
consumed. The client must create a new lease; the original token is never
readable or re-deliverable from Kubernetes.

## 6. Credential Broker

The Broker is a separate deployment in access-broker-system.

The ordinary endpoint:

- authenticates the caller with OIDC;
- derives requester and target from the verified OIDC identity;
- ignores any caller-supplied target;
- automatically issues only base-readonly.v1;
- allows self-revocation only.

The ordinary client never writes a CredentialLease directly. If a workflow
needs a pending request or a redemption step, the client calls the
corresponding Broker API and the Broker performs the Lease operation after
rechecking the caller identity and allowed action.

The elevated workflow is initiated in the external approval system, not by an
ordinary user calling the Broker. The approval form must contain the
user's immutable OIDC subject, requested TTL, and human-readable reason. The
adapter fixes the OIDC issuer and accepted approval definition in its
deployment configuration; those values are never taken from the callback
payload.

The default flow is:

    External approval -> stateless adapter callback -> Broker internal approve
    -> Broker persists CredentialLease and generates reference
    -> adapter notifies the target -> user CLI redeems with OIDC + reference

The adapter verifies the callback token and optional signature, then
fetches the complete approval instance from the external system's API. It accepts only an
approved instance of the configured approval definition, validates the form
fields against the server-owned profile policy, looks up the initiator's email,
and calls `POST /v1/clusters/{clusterID}/internal/credentials/approve` with an OIDC token carrying
the exact configured administrator group. The adapter has no Kubernetes
credentials and never writes CredentialLease directly.
It does not persist event-consumption state. If the external approval system retries an event, the
same approval instance code is submitted again and the Broker's idempotency
key returns the original reference. A failed notification is therefore
retryable without creating another approval record.

The internal approval endpoint accepts only `cluster-ops-write.v1`, verifies
that the target InternalUser is active, creates an `AwaitingRedemption`
CredentialLease, and generates the one-time reference. It creates no temporary
ServiceAccount, Secret, or ClusterRoleBinding until the target user redeems.
The Broker persists the approval authorization and requested TTL; the adapter
does not own approval state. The external `approvalID` is the idempotency key.

The user calls `POST /v1/clusters/{clusterID}/credentials/elevated/redeem` with
only the reference.
The Broker authenticates the user, reads the persisted Lease, binds requester
and target to the verified OIDC identity, and consumes the Lease with a status
compare-and-swap before proceeding. The reference is not an authentication
factor and is valid only together with a fresh OIDC login by the target. The
former `POST /v1/credentials/elevated/request` endpoint has been removed.

The administrator endpoint is separate from the ordinary credential endpoint
and is used for the controlled InternalUser lifecycle. The `sealos-access admin`
`approve` operation is the break-glass/manual adapter capability: it submits an
already-approved record with a fresh OIDC token carrying the exact configured
administrator group. Neither the administrator namespace nor the adapter writes
CredentialLease directly.

The approval reference is an audit field, not an authentication factor. In the
the first version, approval is recorded by External approval system and submitted by the adapter,
or manually through the approved break-glass CLI. A text note or message URL
alone cannot authorize the Broker. The reference is delivered to the target
through the configured protected notification channel; it is not returned by
the approval callback endpoint.

The Broker creates the Lease spec and waits for Prepared. It then:

1. rechecks the target, requester, profile, status, ownership references, and
   suspension state;
2. marks the Lease consumed before the irreversible TokenRequest;
3. calls serviceaccounts/token with a server-fixed Kubernetes API audience;
4. verifies the returned token audience and actual expiration;
5. records Issued;
6. returns the kubeconfig exactly once.

The Broker must not create identities, read Secrets, create or modify arbitrary
RBAC, or issue a token for an inactive identity. Its Kubernetes identity uses a
projected ServiceAccount token with a short TTL and explicit audience. Default
automount is disabled and no long-lived Broker Secret is created.

The Broker's object permissions are limited to reading the required target
identity, creating/updating its own leases, and invoking TokenRequest in
access-system. It must not list all identities merely to resolve an
ordinary caller; the target should be derived deterministically and fetched by
name.

## 7. Authentication and CLI

The person CLI uses OIDC Authorization Code + PKCE:

- public OIDC client, with no client secret;
- local loopback callback;
- state and S256 PKCE verification;
- no refresh-token persistence.

The Broker verifies issuer, audience, signature, subject, expiration, and
not-before. The issuer and audience are server configuration; the caller
cannot choose them. The daily Kubernetes token audience is configured
separately from the OIDC Broker audience.

The first version provides a Broker API and CLI, not a full portal. The CLI:

Operational build and usage instructions are in
[`internal-user-credentials-cli.md`](internal-user-credentials-cli.md), with a
Chinese translation in
[`internal-user-credentials-cli.zh-CN.md`](internal-user-credentials-cli.zh-CN.md).

- does not print the token or kubeconfig;
- writes only to an explicitly selected file;
- refuses a symlink output path;
- writes with mode 0600;
- displays only issuer, derived username, profile, lease ID, and expiration.

The CLI may load these non-secret defaults from its YAML configuration file;
command-line flags override file values. The file may contain Broker/OIDC URLs,
TLS verification paths, the public client ID, a requested TTL, browser/login
preferences, and an output path, but never tokens, kubeconfigs, client secrets,
or one-time approval references. The standard path and `--config` behavior are
defined in the person-facing CLI usage guide. The administrator CLI has a
separate configuration file containing only a kubeconfig path, issuer, and
subject; its path and precedence rules are defined in its usage guide.

For elevated access, the CLI provides only `elevated redeem`. The user receives
the Broker-generated reference from the external approval workflow, then the
CLI performs a fresh OIDC login and sends only that reference. It writes the
one-time kubeconfig using the same 0600 rules as the base command. There is no
user-facing elevated request command.

The controlled `sealos-access admin` namespace talks directly to Kubernetes to create,
suspend, resume, or delete InternalUser. It also has an `approve` operation for
the manual/break-glass adapter path, which performs a fresh OIDC login and calls
the Broker's dedicated HTTPS endpoint. It is authorized by the exact configured
administrator OIDC group. It cannot read Secrets, create RBAC, or call
TokenRequest. The Broker cannot create a new identity. The unified
`sealos-access` binary implements the user and administrator operations; its
build and usage contract is documented in
[`internal-user-admin-cli.md`](internal-user-admin-cli.md).

Version one does not define an automatic personnel-directory synchronization
component. The controlled administrator CLI and administrator workflow are the
source of truth for the InternalUser lifecycle. Personnel-directory
availability is therefore not a Broker dependency or a BROKER-008 acceptance
condition.

## 8. Controller responsibilities

The InternalUser/CredentialLease Controller is independently deployed with a
narrow ServiceAccount:

- reconcile stable ServiceAccounts and base bindings;
- prepare and clean temporary lease resources;
- update API status and finalizers;
- never call TokenRequest;
- never read token data;
- never manage the existing standard User lifecycle.

The Controller has one deliberate privilege exception: it must create and
delete the platform-generated ClusterRoleBindings used by stable and
lease-specific profiles. Kubernetes RBAC cannot restrict a dynamic
ClusterRoleBinding `create` request by object name or label. Therefore this
permission must be treated as a trusted-controller boundary and protected in
production by an admission policy or equivalent isolated RBAC signer that
restricts role references, subjects, names, and ownership labels to the
platform-generated binding shapes. The Broker itself has no RBAC write
permission. Without that additional admission or signer control, the
Controller compromise threat model is not closed and production rollout is
not approved.

The base binding is removed when an identity is suspended. A user deletion
waits for lease cleanup before removing its finalizer. Both Controller and
Broker enforce profile and TTL limits so a single component failure does not
create a larger credential than intended.

## 9. Network, audit, and failure behavior

The Broker Service is not directly exposed. External traffic goes through
Higress/WAF, which must enforce:

- office or explicitly authorized source CIDRs;
- rate limits;
- request-size limits;
- source/header validation.

Source IP enforcement is performed at both Higress/WAF and the Broker. The
trusted request path is:

    client -> Higress/WAF -> Broker

Higress/WAF must determine the client IP from the explicitly configured trusted
proxy chain, remove any client-supplied Forwarded, X-Forwarded-For, and
X-Real-IP headers, and write the normalized address to an internal header such
as X-Trusted-Client-IP. It must then apply the source CIDR allowlist before
forwarding the request. A proxy or load balancer is part of the trusted chain
only when it is explicitly configured; a client-provided forwarding header is
never evidence of the source IP.

The Broker must accept connections only from the Higress egress identities,
using network policy. After verifying that the
peer is Higress, it may read X-Trusted-Client-IP and must apply the same source
CIDR allowlist again. Missing, malformed, or untrusted source headers fail
closed for the admin route. Direct access to the Broker Service must be
blocked so that the Header check cannot be bypassed.

The Broker also applies a server-configured in-process request rate limit and
request burst limit as a defense-in-depth control. The default is 10 requests
per second with a burst of 20, and health probes are excluded. The limit is
per Broker replica, so it does not replace the cluster-wide Higress/WAF rate
limit. Requests that exceed it receive a safe `429` response and never reach
credential issuance.

Broker OIDC authentication remains mandatory; an allowed IP is not an
identity credential. The admin route additionally requires the admin group.

The Broker, Controller, and approval adapter namespaces use default-deny
NetworkPolicies. Broker ingress is allowed from Higress and from the adapter's
dedicated namespace only for the OIDC admin-group approval endpoint. Broker egress is
limited to the Kubernetes API, OIDC/JWKS, and the protected audit/logging
sink. The adapter has no Kubernetes API permissions; its egress is limited to
the external approval API, the SMTP relay, and the Broker Service. `access-system`
is not reachable by ordinary application Pods. The adapter's public callback,
if used, must still be exposed through the configured Higress/WAF path.

Kubernetes Audit Policy must record serviceaccounts/token at Metadata level
only, with no Request or Response bodies. Broker logs and Kubernetes audit
records go to a protected centralized system. Records include subject,
requester, profile, Lease ID, approval reference, source IP, user-agent,
result, and timestamp, but never token, kubeconfig, refresh token, or Secret
data.

If OIDC or an approval system is unavailable:

- stop new issuance and elevation;
- let already issued tokens live only until their original expiration;
- keep revocation and cleanup available;
- never use cached approval or fail-open behavior.

## 10. Test and rollout gates

Before enabling any external route, a test cluster must cover:

- duplicate identity and rename/identity-takeover attempts;
- issuer allowlist, immutable identity, and immutable profile;
- ordinary self-only issuance and admin-only delegation;
- profile allowlist, fixed audience, TTL caps, and actual expiration;
- lost response after TokenRequest;
- suspension, self-revocation, administrator revocation, and deletion;
- temporary elevation never changing the stable binding;
- owner mismatch and fail-closed cleanup;
- finalizer cleanup after restarts and missed events;
- audit redaction and absence of token/kubeconfig data;
- source CIDR enforcement at Higress/WAF and Broker;
- forged forwarding headers, malformed source headers, and direct Broker
  bypass attempts fail closed.

A small in-memory OIDC issuer may be used only as test infrastructure. A
Testcontainers wrapper can exercise Discovery, signed JWT/JWKS validation, and
Authorization Code + PKCE without introducing a production identity
dependency. The provider must not persist production credentials or tokens.

The design is not ready for production until the profile resource list,
admission policy, issuer configuration, WAF CIDRs, NetworkPolicies, audit
retention, and administrator bindings have each received an explicit review.
