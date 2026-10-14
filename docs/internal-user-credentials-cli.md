# Sealos Access CLI

This guide describes `sealos-access login`, the person-facing command for obtaining
ordinary and approved temporary Kubernetes credentials through the Broker.

The CLI never selects a target identity or RBAC profile. For elevated access,
the external approval workflow sends the user a one-time reference, and the CLI
redeems that reference for a temporary elevated credential. `InternalUser`
lifecycle changes remain an administrator workflow.
The controlled administrator namespace for creating identities is documented in
[`internal-user-admin-cli.md`](internal-user-admin-cli.md).

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
