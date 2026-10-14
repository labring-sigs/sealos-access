# Sealos Access CLI Merge Plan

Status: Proposed

Last reviewed: October 12, 2026

This plan merges the person-facing `internal-user-cli` and the controlled
`internal-user-admin` entry points into one executable named `sealos-access`.
It changes the local command and distribution experience. It does not merge
the server-side authorization models or grant ordinary users administrator
authority. Because these commands have not been formally enabled, the change
uses a clean cutover and does not include a compatibility layer.

The Chinese companion is
[`sealos-access-cli-merge-plan.zh-CN.md`](sealos-access-cli-merge-plan.zh-CN.md).

## 1. Decision

Build and distribute one binary with an explicit administrator namespace:

```text
sealos-access login
sealos-access elevated redeem

sealos-access admin create
sealos-access admin approve
```

The `admin` namespace must be explicit; administrator mode must never be
selected by an arbitrary flag or by whichever configuration file happens to be
present.

The merge is a command-surface merge, not a trust-boundary merge:

- `login` and `elevated redeem` remain person-facing OIDC workflows;
- `admin create` remains authorized by the Kubernetes credentials supplied to
  the Kubernetes API client;
- `admin approve` remains authorized by the Broker's exact configured OIDC
  admin-group check;
- the Broker and Kubernetes API remain authoritative even when the same binary
  exposes all four commands.

## 2. Scope

### In scope

- Add a single `sealos-access` command dispatcher and subcommand parser.
- Preserve the existing base credential, elevated redemption, identity
  creation, and manual approval behavior.
- Isolate user and administrator configuration loading within the merged CLI.
- Share safe OIDC, HTTP/TLS, validation, and output helpers where appropriate.
- Update Make targets, Dockerfiles, test fixtures, command documentation, and
  architecture references.
- Remove the pre-release split entry points and build targets as part of the
  same change.

### Out of scope

- Changing Broker endpoints, lease semantics, role profiles, or TTL policy.
- Introducing mTLS authentication for the approval endpoint.
- Allowing ordinary users to create `InternalUser` objects or submit approvals.
- Adding a user-facing elevated request endpoint.
- Storing OIDC tokens, kubeconfigs, private keys, or approval references in the
  shared default configuration.

## 3. Command and authorization matrix

| Command | Current behavior | Credential source | Authoritative server-side check |
| --- | --- | --- | --- |
| `sealos-access login` | Issue the caller's base credential | OIDC Authorization Code + PKCE | Broker validates issuer, audience, claims, active target, and fixed profile |
| `sealos-access elevated redeem` | Redeem one approved elevated reference | Fresh OIDC login plus one-time reference | Broker derives the requester from OIDC and validates the persisted lease |
| `sealos-access admin create` | Create or reconcile one `InternalUser` | Administrator kubeconfig | Kubernetes authentication/RBAC and `InternalUser` admission validation |
| `sealos-access admin approve` | Submit a manual or break-glass elevated approval | OIDC Authorization Code + PKCE | Broker validates the OIDC token and exact configured admin group, then validates target/profile/TTL/state |

The CLI may reject obviously invalid combinations early, but a successful
client-side parse is never evidence of authorization. In particular, the
`admin` subcommands must not rely on a local `--admin` switch, a source IP, an
approval reference, or a user-provided group value.

## 4. Security boundary requirements

### 4.1 Explicit routing

Parse the first command segment before loading defaults or constructing clients:

- user commands load only user defaults and construct public OIDC/Broker
  clients;
- `admin create` loads only administrator defaults and constructs the
  Kubernetes API client;
- `admin approve` loads only administrator approval defaults and constructs the
  OIDC/Broker clients needed for the admin-group flow.

Unknown commands, misplaced administrator flags, and trailing positional
arguments must fail closed.

### 4.2 Configuration isolation

The new default paths should be distinct even though they share the `sealos`
directory:

```text
${XDG_CONFIG_HOME}/sealos/access/user.yaml
${XDG_CONFIG_HOME}/sealos/access/admin.yaml
```

The implementation must not merge the two schemas. A user command must never
fall back to `admin.yaml`, and an administrator command must never silently
consume `user.yaml`. No old-path fallback is required. Configuration files
continue to contain paths and non-secret defaults only.

### 4.3 Server-side authority

The merged binary must not weaken any existing Broker or Kubernetes checks:

- the Broker continues to derive normal and elevated requesters from verified
  OIDC claims;
- the Broker continues to fix the base and elevated profiles and target data;
- the `/v1/clusters/{clusterID}/internal/credentials/approve` endpoint continues to require the
  exact configured OIDC `groups` value after issuer, audience, signature, and
  time validation;
- `admin create` continues to rely on Kubernetes RBAC rather than the OIDC
  admin group;
- the `InternalUser` webhook remains authoritative for identity shape, profile,
  and deterministic naming.

### 4.4 Local secret handling

The merged binary must retain the current output protections:

- never print access tokens or kubeconfig contents;
- write kubeconfigs only to an explicitly selected local regular file with mode
  `0600` and reject symlink targets;
- do not put tokens, kubeconfigs, private keys, or approval references in
  either default YAML file;
- avoid logging command-line values that contain approval references or paths
  to administrator credentials.

## 5. TODO: Implementation

### TODO 1: Extract command implementations

- [x] Move the current user and administrator logic out of separate `main`
  packages behind independently testable command handlers.
- [x] Keep the user and admin configuration types separate.
- [x] Extract only genuinely shared helpers, such as OIDC PKCE state handling,
  HTTPS client construction, URL validation, and safe response decoding.
- [x] Keep Broker request payloads and authorization headers unchanged.

### TODO 2: Add the `sealos-access` dispatcher

- [x] Implement command-first routing for `login`, `elevated redeem`, `admin
  create`, and `admin approve`.
- [x] Make help output show the role split.
- [x] Make each subcommand expose only its own flags.

### TODO 3: Isolate configuration

- [x] Implement the new user/admin default paths.
- [x] Implement explicit `--config` handling.
- [x] Enforce strict schema separation without retaining old executable names or
  configuration paths.

### TODO 4: Update build and distribution surfaces

- [x] Update the user Makefile, Dockerfiles, mock CLI image, deployment
  metadata, acceptance commands, and generated help text.
- [ ] Sign and distribute the combined artifact through the same release
  controls as the current admin artifact because it contains administrator code
  paths. The repository currently has no standalone CLI artifact signing
  workflow, so this remains a release gate before publication.

## 6. TODO: Verification and acceptance

- [x] Parser tests cover every command, help path, unknown command, misplaced
  admin flag, and trailing argument case.
- [x] User commands cannot load administrator defaults or construct a Kubernetes
  client.
- [x] `admin create` cannot run without valid Kubernetes authorization.
- [x] `admin approve` is rejected by the Broker without the configured OIDC
  admin group and succeeds only with a verified token carrying that exact group.
- [x] Base issuance and elevated redemption retain their existing OIDC,
  reference, profile, TTL, kubeconfig, and file-mode behavior.
- [x] Configuration tests cover the new paths, strict user/admin schema
  separation, and unknown YAML fields.
- [x] The existing user, admin, Broker, approval, and acceptance tests invoke
  the merged command surface.

## 7. TODO: Rollout

- [ ] Land the command dispatcher and unit tests, removing the pre-release
  split entry points and targets in the same change.
- [ ] Update local docs, Make targets, Dockerfiles, and mock acceptance
  commands.
- [ ] Publish `sealos-access` as the only supported command.
- [ ] Run normal and elevated user acceptance, then administrator create and
  approve acceptance with explicit authorization failures.
