# Sealos Access CLI

This guide describes `sealos-access login`, the person-facing command for obtaining
ordinary and approved temporary Kubernetes credentials through the Broker.

The CLI never selects a target identity or RBAC profile. For elevated access,
the external approval workflow sends the user a one-time reference, and the CLI
redeems that reference for a temporary elevated credential. `InternalUser`
lifecycle changes remain an administrator workflow.
The controlled administrator namespace for creating identities and submitting
approved records is documented in the Administrator commands section below.

## Command surface

The `sealos-access` binary is the single supported CLI entry point:

| Command | Role | Configuration and authorization |
| --- | --- | --- |
| `login` | Obtain a base credential | `user.yaml`; user OIDC login and Broker authorization |
| `elevated redeem` | Redeem an approved elevated credential | `user.yaml`; user OIDC login and one-time reference |
| `admin create` | Create or reconcile an `InternalUser` | `admin.yaml`; administrator kubeconfig and Kubernetes RBAC |
| `admin approve` | Submit an already-approved elevated record | `admin.yaml`; user OIDC login with the configured administrator group |

Command routing happens before configuration loading. User commands never read
`admin.yaml`, and administrator commands never fall back to `user.yaml`. The
CLI does not allow callers to choose arbitrary target URLs, audiences,
profiles, namespaces, credentials, or RBAC rules; those decisions remain
server-side. See the Administrator commands section below for the identity
lifecycle and administrator-specific flags.

## Prerequisites

- An active `InternalUser` has been provisioned for the OIDC issuer and subject
  by the administrator workflow.
- A public OIDC client is registered for the CLI. It must not require a client
  secret and must allow a loopback callback of the form
  `http://127.0.0.1:<random-port>/callback`.
- The CLI host can reach the configured OIDC issuer and Broker over HTTPS.
- The external approval adapter is deployed and has already submitted an approved
  record to the Broker. The adapter sends the resulting reference through the
  configured protected notification channel.
- The issuer's discovery document returns the same issuer URL supplied to the
  CLI, and exposes an authorization endpoint and a token endpoint.
- The selected output path, whether supplied by the config file or the command
  line, is a local regular-file path. The CLI refuses a symlink and writes the
  resulting kubeconfig with mode `0600`.

The Broker and OIDC CA files are optional only when their certificates are
already trusted by the host. Never disable certificate verification to work
around a trust error.

## Build

From the repository root:

```sh
make build-sealos-access TARGETARCH=amd64 GOOS=linux
./bin/sealos-access-amd64 --help
```

For local development:

```sh
go run ./cmd/sealos-access --help
```

## Configuration File

The CLI reads a YAML configuration file so the common Broker, OIDC, TLS, TTL,
and output settings do not need to be repeated on every invocation. Use
`--config path/to/config.yaml` to select a file explicitly. When `--config` is
omitted, the CLI uses this file when it exists:

```text
${XDG_CONFIG_HOME}/sealos/access/user.yaml
```

On systems without `XDG_CONFIG_HOME`, this is the equivalent platform user
configuration directory returned by the operating system, normally
`~/.config/sealos/access/user.yaml` on Linux. If no default file
exists, the required values must still be supplied by flags.

Example:

```yaml
broker-url: https://broker.example.internal
cluster-id: public-cluster-a
broker-ca-file: ./pki/broker-ca.pem
broker-server-name: internal-credential-broker.example.internal
oidc-issuer: https://login.example.internal
oidc-ca-file: ./pki/oidc-ca.pem
oidc-server-name: login.example.internal
client-id: internal-kc
output: ./internal-user-kubeconfig
requested-ttl-seconds: 3600
no-browser: false
login-hint: ""
```

Command-line flags override values from the file. The config file must contain
one YAML document and unknown fields are rejected. Paths are resolved relative
to the CLI's current working directory. Do not put tokens, kubeconfigs, client
secrets, or approval references in this file. The one-time
`--approval-reference` remains a redeem command-line argument. The configured
`output` is used by base and redeem commands.
The CLI does not create or modify the config file.

## Obtain a Base Credential

The normal command is:

```sh
./bin/sealos-access-amd64 login \
  --broker-url https://broker.example.internal \
  --cluster-id public-cluster-a \
  --oidc-issuer https://login.example.internal \
  --client-id internal-kc \
  --requested-ttl-seconds 3600 \
  --output ./internal-user-kubeconfig
```

The CLI then:

1. Fetches OIDC discovery metadata.
2. Starts a callback listener on `127.0.0.1` using an ephemeral port.
3. Generates `state`, `nonce`, and an S256 PKCE verifier.
4. Opens the authorization URL in the default browser.
5. Exchanges the returned authorization code and validates the ID-token nonce.
6. Sends the OIDC access token to the Broker's base-credential endpoint.
7. Writes the one-time kubeconfig to the explicitly selected output path.

The browser login must finish within five minutes. The local callback is only
bound to loopback and is not an externally reachable service.

The base profile accepts a requested TTL from 600 seconds through 86400
seconds. The Broker remains authoritative and may reject a value outside its
configured profile limits. The CLI does not print the access token or
kubeconfig; on success it prints only issuer, derived username, profile, Lease
ID, and expiration metadata.

Use the resulting file without printing it:

```sh
KUBECONFIG=./internal-user-kubeconfig kubectl get namespaces
stat -c '%a %n' ./internal-user-kubeconfig
```

The expected file mode is `600`. Treat the file as a bearer credential, do not
commit or upload it, and remove it after its credential expires or is revoked.

## Redeem an Elevated Credential

The user submits the configured approval form through the external
approval workflow. After the adapter reports the approval to the Broker, the
user receives the Broker-generated reference through the protected notification
channel. Redeem it as the same OIDC user:

```sh
./bin/sealos-access-amd64 elevated redeem \
  --cluster-id public-cluster-a \
  --approval-reference r1.approval-<lease-id>.<random-value> \
  --output ./internal-user-elevated-kubeconfig
```

The Broker derives both requester and target from the authenticated OIDC
identity and fixes the profile to `cluster-ops-write.v1`. It reads the persisted
TTL and target from the Broker's `AwaitingRedemption` Lease, consumes the
reference once, and only then creates the temporary elevated resources. The
CLI writes the returned kubeconfig with mode `0600` and prints only non-secret
metadata. If the reference is lost, start a new external approval instead of
trying to recover it from logs or Kubernetes.

An approval reference is not an authentication factor. Do not share it as a
substitute for OIDC login, and do not put it in public tickets or logs.

## Administrator commands

The controlled `sealos-access admin` namespace creates `InternalUser`
resources and submits already-approved elevated records. The `approve`
operation is the manual or break-glass equivalent of the external approval
adapter; it does not write `CredentialLease` directly.

### Authorization and prerequisites

- The CLI host can reach the Kubernetes API server.
- The selected kubeconfig authenticates an administrator with the narrowly
  scoped permission to create `internalusers.user.sealos.io`.
- The `InternalUser` CRD and validating webhook are installed. The webhook has
  a server-configured allowlist of HTTPS OIDC issuers.
- The issuer and subject identify one real internal person. The subject is an
  opaque, immutable OIDC subject, not a display name or email chosen by the
  caller.
- For `approve`, the operator has an independently approved record, a public
  OIDC client, and an OIDC identity carrying the Broker's exact administrator
  group.

The administrator kubeconfig must be protected according to the cluster's
administrator credential policy. The CLI does not elevate its own Kubernetes
permissions. The administrator identity should not receive Secret reads,
arbitrary RBAC writes, TokenRequest access, or
`credentialleases.user.sealos.io` mutation access merely to use these commands.

### Administrator configuration

The administrator commands use a separate non-secret configuration file. Pass
it explicitly with `--config ./admin.yaml`, or let the CLI use this file when
it exists:

```text
${XDG_CONFIG_HOME}/sealos/access/admin.yaml
```

On Linux without `XDG_CONFIG_HOME`, this is normally
`~/.config/sealos/access/admin.yaml`.

Example for `admin create`:

```yaml
kubeconfig: /secure/path/admin.kubeconfig
issuer: https://login.example.internal
subject: oidc-subject-1
```

Example for `admin approve`:

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

Command-line flags override file values. The file must contain one YAML
document and unknown fields are rejected. Paths are resolved relative to the
CLI's current working directory. Never put kubeconfig contents, client
certificates, private keys, bearer tokens, external approval credentials, or
Secret data in this file. The CLI does not create or modify it.

### Create an `InternalUser`

Use an administrator kubeconfig through the current client-go loading rules:

```sh
./bin/sealos-access-amd64 admin create \
  --issuer https://login.example.internal \
  --subject oidc-subject-1
```

Use `--kubeconfig /secure/path/admin.kubeconfig` when the administrator
context is not the current context.

The CLI validates the local identity shape, then creates one cluster-scoped
`InternalUser` with `base-readonly.v1`. Its name is `iu-` followed by the first
24 hexadecimal characters of `sha256(issuer + NUL + subject)`. The server-side
webhook remains authoritative for issuer allowlisting, identity immutability,
the fixed profile, and the derived name.

A successful command prints only the derived resource name and profile. It
never prints or persists a Secret, token, kubeconfig, or `CredentialLease`.
The Controller creates the stable ServiceAccount and base binding after
reconciliation; `sealos-access login` can request credentials only after the
user becomes active.

### Submit an approved elevated record

Use `admin approve` only for an independently approved manual or break-glass
record when the external approval adapter is unavailable:

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

The command performs a fresh OIDC Authorization Code + PKCE login. The Broker
validates the OIDC token and exact administrator group, then validates the
active `InternalUser`, elevated profile, and TTL before persisting an
`AwaitingRedemption` lease and returning a reference. It does not create a
temporary ServiceAccount, Secret, binding, or kubeconfig. Repeating the same
`approval-id` is idempotent; changing its target, TTL, or reason is rejected.

### Administrator flags

| Flag | Required | Description |
| --- | --- | --- |
| `--config` | No | YAML config file; otherwise the standard admin config path is used when present. |
| `--kubeconfig` | No | Administrator kubeconfig path. Empty uses standard client-go loading rules. |
| `--issuer` | Create/approve | HTTPS OIDC issuer; it must be in the server-side allowlist. |
| `--subject` | Create/approve | Opaque immutable OIDC subject; surrounding whitespace is rejected. |
| `--broker-url` | Approve | HTTPS Broker URL. |
| `--cluster-id` | Yes* | Server-allowlisted target cluster ID; it may come from `admin.yaml`. |
| `--broker-ca-file` | Approve | CA file for Broker TLS verification. |
| `--broker-server-name` | No | Broker TLS server name. |
| `--oidc-issuer` | Approve | HTTPS OIDC issuer used for administrator authentication. |
| `--oidc-ca-file` | No | CA file for OIDC TLS verification. |
| `--client-id` | Approve | Public OIDC client ID. |
| `--approval-id` | Approve | Immutable external approval instance ID. |
| `--approval-reason` | Approve | Human-readable approval reason. |
| `--requested-ttl-seconds` | Approve | Elevated TTL, limited to 600-3600 seconds. |
| `--no-browser` | No | Print the OIDC authorization URL instead of opening a browser. |

There are intentionally no `--name`, `--role-profile`, RBAC, Secret, token,
or `CredentialLease` flags. Names and profiles are platform-owned values.

### Administrator failure handling

- An HTTP or malformed issuer is rejected locally.
- An empty, surrounding-whitespace, or overlong subject is rejected locally.
- An issuer outside the server allowlist is rejected by the webhook.
- Recreating the same issuer/subject resolves to the same name and returns an
  already-exists error; it does not create a second identity.
- Kubernetes authorization, connectivity, CRD, and webhook errors are returned
  without exposing credential data.

Do not put the administrator kubeconfig, client key, or returned reference in
tickets, shell history, or source control. Do not use `approve` without an
independently recorded approval. Users redeem references through the user
commands; administrator commands do not issue or redeem a Kubernetes
kubeconfig.

## TLS Options

Use the CA and server-name flags when the OIDC issuer or Broker uses an
internal CA or a certificate name different from the URL host:

```sh
./bin/sealos-access-amd64 login \
  --broker-url https://broker.example.internal \
  --cluster-id public-cluster-a \
  --broker-ca-file ./pki/broker-ca.pem \
  --broker-server-name internal-credential-broker.example.internal \
  --oidc-issuer https://login.example.internal \
  --oidc-ca-file ./pki/oidc-ca.pem \
  --oidc-server-name login.example.internal \
  --client-id internal-kc \
  --requested-ttl-seconds 3600 \
  --output ./internal-user-kubeconfig
```

`--broker-ca-file` and `--oidc-ca-file` must contain PEM-encoded CA
certificates. `--broker-server-name` and `--oidc-server-name` control TLS
server-name verification; they do not bypass it.

## Non-Browser Login

For a host without a graphical browser, add `--no-browser`:

```sh
./bin/sealos-access-amd64 login \
  --broker-url https://broker.example.internal \
  --cluster-id public-cluster-a \
  --oidc-issuer https://login.example.internal \
  --client-id internal-kc \
  --requested-ttl-seconds 3600 \
  --no-browser \
  --output ./internal-user-kubeconfig
```

The CLI prints the one-time authorization URL. Open it in a browser that can
reach the OIDC provider, while keeping the CLI process running so the local
callback remains available. `--login-hint` may be supplied when the identity
provider supports it:

```sh
--login-hint alice@example.internal
```

Do not paste authorization URLs into tickets or logs. They contain transient
protocol values even though they do not contain the resulting Kubernetes
credential.

## Supported Flags

| Flag | Required | Description |
| --- | --- | --- |
| `--config` | No | YAML config file; otherwise the standard user config path is used when present. |
| `--broker-url` | Yes* | HTTPS Broker base URL. |
| `--cluster-id` | Yes* | Server-allowlisted target cluster ID; selects the target-aware Broker route. It may come from the user config file. |
| `--broker-ca-file` | No | PEM CA file for Broker TLS verification. |
| `--broker-server-name` | No | TLS server name for Broker verification. |
| `--oidc-issuer` | Yes* | HTTPS OIDC issuer URL. Discovery must match it exactly. |
| `--oidc-ca-file` | No | PEM CA file for OIDC TLS verification. |
| `--oidc-server-name` | No | TLS server name for OIDC verification. |
| `--client-id` | Yes* | Public OIDC client ID. |
| `--output` | Base/redeem only* | Local regular-file path for the kubeconfig; `-` is rejected. |
| `--requested-ttl-seconds` | Yes* | Requested credential lifetime; base allows 600-86400 seconds and elevated allows 600-3600 seconds. |
| `--approval-reference` | Elevated redeem only | One-time reference delivered by the external approval adapter. |
| `--no-browser` | No | Print the authorization URL instead of opening a browser. |
| `--login-hint` | No | Optional OIDC login hint. |

There is intentionally no CLI flag for profile, admin group, requested elevated
TTL, arbitrary URL, namespace, audience, or RBAC. `--cluster-id` is only an
allowlisted identifier; the Broker owns its URL, namespace, audience, and
credential mapping. The external form, adapter, Broker, and redeem target are
server-side policy. An asterisk means the value can instead come from the
configuration file.

## Troubleshooting

- `OIDC discovery issuer does not match configuration`: use the issuer URL
  returned by the provider, including its exact scheme, host, and path.
- `fetch OIDC discovery` or `exchange OIDC authorization code`: verify DNS,
  network policy, the CA file, and the TLS server-name flag.
- `OIDC authorization timed out`: complete the browser flow within five
  minutes and keep the original CLI process running.
- `Broker returned HTTP 403`: check that the authenticated OIDC identity is
  provisioned, active, and permitted by the Broker source-IP and identity
  policy.
- `requested TTL must be at least 10m` or an upper-limit error: choose a base
  profile TTL between 600 and 86400 seconds, or an elevated TTL between 600
  and 3600 seconds.
- `refusing symlink output path`: select a regular file path directly; do not
  replace this protection with a symlink.

Never retry a request by reading or copying the returned kubeconfig from logs.
If a one-time response is lost, start a new CLI request; the old Lease may
already be consumed.
