# Sealos Access Administrator Commands

This guide describes the controlled `sealos-access admin` namespace
for creating `InternalUser` resources and submitting an already-approved
elevated record. The `approve` operation is the manual/break-glass equivalent
of the external approval adapter; it does not write `CredentialLease` directly.

## Authorization and prerequisites

- The CLI host can reach the Kubernetes API server.
- The selected kubeconfig authenticates an administrator identity with the
  narrowly scoped permission to create `internalusers.user.sealos.io`.
- The `InternalUser` CRD and its validating webhook are installed. The webhook
  has a server-configured allowlist of HTTPS OIDC issuers.
- The issuer and subject identify one real internal person. The subject is an
  opaque, immutable OIDC subject, not a display name or email address chosen by
  the CLI caller.
- For `approve`, the operator has an approval record from the approved workflow,
  a public OIDC client, and an OIDC identity carrying the Broker's exact
  configured administrator group.

The kubeconfig is the administrator credential and must be protected according
to the cluster's administrator credential policy. This CLI does not elevate its
own Kubernetes permissions. The administrator identity should not receive
Secret reads, arbitrary RBAC writes, TokenRequest access, or
`credentialleases.user.sealos.io` mutation access merely to use this command.

## Build

From the repository root:

```sh
make build-sealos-access TARGETARCH=amd64 GOOS=linux
./bin/sealos-access-amd64 admin --help
```

For local development:

```sh
go run ./cmd/sealos-access --help
```

## Configuration file

The `create` command can load its non-secret defaults from a YAML file. The
explicit form is:

```sh
./bin/sealos-access-amd64 admin create \
  --config ./admin.yaml
```

When `--config` is omitted, the CLI uses this file when it exists:

```text
${XDG_CONFIG_HOME}/sealos/access/admin.yaml
```

On systems without `XDG_CONFIG_HOME`, this is normally
`~/.config/sealos/access/admin.yaml` on Linux. Example:

```yaml
kubeconfig: /secure/path/admin.kubeconfig
issuer: https://login.example.internal
subject: oidc-subject-1
```

Command-line flags override values from the file. The file must contain one
YAML document and unknown fields are rejected. `kubeconfig` is only a path;
never put kubeconfig contents, client certificates, private keys, bearer
tokens, or other credentials in this file. The CLI does not create or modify
the configuration file. Relative paths are resolved from the CLI's current
working directory.

An `approve` configuration may contain only non-secret paths and approval
metadata:

```yaml
issuer: https://login.example.internal
subject: oidc-subject-1
broker-url: https://internal-credential-broker.example.internal
cluster-id: public-cluster-a
broker-ca-file: ./pki/broker-ca.pem
broker-server-name: internal-credential-broker.example.internal
oidc-issuer: https://login.example.internal
oidc-ca-file: ./pki/oidc-ca.pem
client-id: internal-admin
approval-id: approval-instance-123
approval-reason: incident remediation
requested-ttl-seconds: 1800
```

Do not put OIDC tokens, External approval system credentials, kubeconfigs, or
Secret data in this file.

## Create an InternalUser

Use an administrator kubeconfig through the current client-go loading rules:

```sh
./bin/sealos-access-amd64 admin create \
  --issuer https://login.example.internal \
  --subject oidc-subject-1
```

Use an explicit kubeconfig when the administrator context is not the current
context:

```sh
./bin/sealos-access-amd64 admin create \
  --kubeconfig /secure/path/admin.kubeconfig \
  --issuer https://login.example.internal \
  --subject oidc-subject-1
```

The CLI validates the local shape of the identity, then creates exactly one
cluster-scoped `InternalUser` with:

- `spec.identity.issuer` set to `--issuer`;
- `spec.identity.subject` set to `--subject`;
- `spec.roleProfile` fixed to `base-readonly.v1`;
- `metadata.name` derived as
  `iu-` plus the first 24 hexadecimal characters of
  `sha256(issuer + NUL + subject)`;
- `spec.suspend` left false.

The server-side webhook remains authoritative. It checks the issuer allowlist,
identity immutability rules, fixed profile, and derived name. A successful
command prints only the derived resource name and profile. It never prints or
persists a Secret, token, kubeconfig, or `CredentialLease`.

Creating the resource does not immediately return a Kubernetes credential. The
InternalUser/CredentialLease Controller observes the resource and creates the
stable ServiceAccount and base binding after reconciliation. The person-facing
`sealos-access login` can request a credential only after the user becomes active.

## Submit an Approved Elevated Record

Use `approve` only for a manually approved or break-glass record when the
external approval adapter is unavailable. It performs a fresh OIDC
Authorization Code + PKCE login and calls the Broker's dedicated HTTPS
endpoint:

```sh
./bin/sealos-access-amd64 admin approve \
  --issuer https://login.example.internal \
  --subject oidc-subject-1 \
  --broker-url https://internal-credential-broker.example.internal \
  --cluster-id public-cluster-a \
  --broker-ca-file ./pki/broker-ca.pem \
  --oidc-issuer https://login.example.internal \
  --oidc-ca-file ./pki/oidc-ca.pem \
  --client-id internal-admin \
  --approval-id approval-instance-123 \
  --approval-reason "incident remediation" \
  --requested-ttl-seconds 1800
```

The Broker validates the OIDC token, including the exact configured
administrator group, then validates the active `InternalUser`, fixed elevated
profile, and TTL before persisting an `AwaitingRedemption` Lease and returning
the generated reference. The CLI displays the reference so it can be delivered
through the approved protected channel. It does not create temporary
ServiceAccounts, Secrets, bindings, or a kubeconfig. Repeating the same
`approval-id` is idempotent; changing its target, TTL, or reason is rejected.

## Supported flags

| Flag | Required | Description |
| --- | --- | --- |
| `--config` | No | YAML config file; otherwise the standard admin config path is used when present. |
| `--kubeconfig` | No | Administrator kubeconfig path. Empty uses the standard client-go loading rules. |
| `--issuer` | Create/approve | HTTPS OIDC issuer. It must also be present in the server-side allowlist. |
| `--subject` | Create/approve | Opaque immutable OIDC subject; leading/trailing whitespace is rejected. |
| `--broker-url` | Approve | HTTPS Broker URL. |
| `--cluster-id` | Yes* | Server-allowlisted target cluster ID for approval. It may come from the admin config file. |
| `--broker-ca-file` | Approve | CA file for Broker TLS verification. |
| `--broker-server-name` | No | Broker TLS server name. |
| `--oidc-issuer` | Approve | HTTPS OIDC issuer used for administrator authentication. |
| `--oidc-ca-file` | No | CA file for OIDC TLS verification. |
| `--client-id` | Approve | Public OIDC client ID. |
| `--approval-id` | Approve | Immutable external approval instance ID. |
| `--approval-reason` | Approve | Human-readable approval reason. |
| `--requested-ttl-seconds` | Approve | Elevated TTL, limited to 600-3600 seconds. |
| `--no-browser` | No | Print the OIDC authorization URL instead of opening a browser. |

There are intentionally no `--name`, `--role-profile`, RBAC, Secret, token, or
`CredentialLease` flags. Names and profiles are platform-owned values.

## Failure handling

- An HTTP or malformed issuer is rejected locally.
- An empty, surrounding-whitespace, or overlong subject is rejected locally.
- An issuer outside the server allowlist is rejected by the webhook.
- Creating the same issuer/subject again resolves to the same name and returns
  an already-exists error; it does not create a second identity.
- Kubernetes authorization, connectivity, CRD, and webhook errors are returned
  without exposing credential data.

Do not put the administrator kubeconfig, client key, or returned reference in
tickets, shell history, or source control. Do not use `approve` without an
independently recorded approval. Users redeem references through the user CLI;
this command does not issue or redeem a Kubernetes kubeconfig.
