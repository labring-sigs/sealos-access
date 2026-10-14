# Internal User Credential Broker Configuration

This document describes how to configure and deploy the Internal User
Credential Broker. It covers the current direct-credential architecture: one
central Broker authenticates users through OIDC and can issue credentials for
one or more target Kubernetes clusters.

The Broker requires the strict, versioned multi-cluster YAML configuration.
There is no single-target fallback or compatibility mode.

## Deployment model

The central Broker runs in the `access-broker-system` namespace of its
own cluster. For every target cluster, the Broker uses a separate Kubernetes
API endpoint, CA bundle, and least-privilege ServiceAccount bearer token.

```text
CLI / approval adapter
          |
          | HTTPS + OIDC bearer token
          v
  central Broker cluster
          |
          +--> target cluster A API server
          |
          +--> target cluster B API server
          |
          `--> OIDC issuer Discovery/JWKS
```

The Broker does not create `InternalUser` objects. Identity creation remains
an administrator operation. The Broker only reads `InternalUser`, manages the
target `CredentialLease` state, performs `TokenRequest` and `TokenReview`, and
returns a kubeconfig containing a short-lived token.

## Required configuration

Set `BROKER_CONFIG_FILE` to the mounted path of `broker.yaml`, normally
`/etc/broker/config/broker.yaml`. The file contains one OIDC configuration and
an explicit entry for every target cluster. Each enabled target gets its own
REST client and bearer-token file.

The Broker exits during startup when the file is missing, invalid, or contains
no enabled target. Target API servers, audiences, namespaces, CA files, and
credentials are always read from this YAML file.

## Multi-cluster YAML

The YAML must contain exactly one document, uses strict unknown-field
validation, and currently requires `version: 1`. A configuration error stops
the Broker instead of starting with a partial target set.

Example with two targets:

```yaml
version: 1

oidc:
  issuer: https://login.example.internal
  audience: sealos-internal-credential-broker
  # Leave empty to use issuer/.well-known/openid-configuration.
  jwksURL: ""
  # Required when the issuer uses a private CA not in the image trust store.
  caFile: /etc/broker/oidc-ca/ca.crt
  # Optional SNI/server-name override for the issuer TLS connection.
  serverName: ""

targets:
- id: public-cluster-a
  # Endpoint reachable by the Broker. It may be private to the Broker network.
  apiServer: https://api.cluster-a.example.internal
  # Endpoint embedded in the kubeconfig returned to users.
  clusterServer: https://api.cluster-a.example
  caFile: /etc/broker/targets/public-cluster-a.ca.crt
  credentialFile: /etc/broker/targets/public-cluster-a.token
  kubernetesAudience: https://kubernetes.default.svc.cluster.local
  userNamespace: access-system
  brokerNamespace: access-broker-system
  clusterName: public-cluster-a
  enabled: true

- id: public-cluster-b
  apiServer: https://api.cluster-b.example.internal
  clusterServer: https://api.cluster-b.example
  caFile: /etc/broker/targets/public-cluster-b.ca.crt
  credentialFile: /etc/broker/targets/public-cluster-b.token
  kubernetesAudience: https://kubernetes.default.svc.cluster.local
  userNamespace: access-system
  brokerNamespace: access-broker-system
  clusterName: public-cluster-b
  enabled: true
```

### OIDC fields

| Field | Required | Meaning |
| --- | --- | --- |
| `issuer` | Yes | Exact HTTPS issuer URL. The token `iss` claim and, when Discovery is used, the Discovery `issuer` must match it. |
| `audience` | Yes | OIDC audience accepted by the Broker. It must match the audience requested by the CLI/approval flow. |
| `jwksURL` | No | HTTPS JWKS endpoint. If empty, the Broker fetches `/.well-known/openid-configuration` and follows `jwks_uri`. |
| `caFile` | No | Mounted PEM CA bundle used for OIDC TLS verification. The path must exist when set. |
| `serverName` | No | TLS server name/SNI override for OIDC requests. |

The Broker uses a bounded HTTP client and fails closed when Discovery or JWKS
cannot be fetched. Signing keys are cached for ten minutes; an unknown `kid`
causes one refresh attempt, which supports normal signing-key rotation.

### Target fields

| Field | Required | Meaning |
| --- | --- | --- |
| `id` | Yes | Immutable routing ID. It must be lowercase DNS-like text and unique within the file. Clients may submit this ID, but may not submit any target URL or credential. |
| `apiServer` | Yes | HTTPS Kubernetes API endpoint used by the Broker. This is not necessarily the endpoint returned to users. |
| `clusterServer` | Yes | HTTPS Kubernetes API endpoint written into returned kubeconfigs. It must be reachable from the user network. |
| `caFile` | Yes | Mounted PEM CA bundle for `apiServer`. |
| `credentialFile` | Yes | Mounted file containing only the target ServiceAccount bearer token. |
| `kubernetesAudience` | Yes | Audience used for the target `TokenRequest` and verified by the Broker. |
| `userNamespace` | Yes | Namespace containing the target `InternalUser`-managed ServiceAccounts and bound Secrets. |
| `brokerNamespace` | Yes | Namespace containing target `CredentialLease` objects. |
| `clusterName` | No | Human-readable cluster name. Defaults to `id`; it is not used as a client-selected endpoint. |
| `enabled` | No (default `false`) | Enables the target. At least one target must be enabled. Set it explicitly to `true` for an active target. |

All URLs must be absolute HTTPS URLs without a query or fragment. Duplicate
IDs, missing files, unknown fields, unsupported versions, and a configuration
with no enabled target are rejected at startup. Disabled entries are still
validated, so remove stale entries or provide their referenced files before
deploying.

## Secret and file layout

Keep the YAML in a ConfigMap, but keep CA and bearer-token material in Secrets
or an external secret manager. Never put a token, kubeconfig, private key, or
Secret data in the repository or ConfigMap.

The supplied Deployment expects these file paths:

| Mounted path | Source | Contents |
| --- | --- | --- |
| `/etc/broker/config/broker.yaml` | ConfigMap `internal-credential-broker-config`, key `broker.yaml` | Non-secret multi-cluster YAML |
| `/etc/broker/oidc-ca/ca.crt` | Secret `internal-credential-broker-oidc-ca`, key `ca.crt` | OIDC issuer CA, when needed |
| `/etc/broker/targets/<id>.ca.crt` | Secret `internal-credential-broker-target-credentials`, key `<id>.ca.crt` | Target API CA |
| `/etc/broker/targets/<id>.token` | Secret `internal-credential-broker-target-credentials`, key `<id>.token` | Target ServiceAccount bearer token |
| `/etc/broker/tls/tls.crt` | Secret `internal-credential-broker-tls`, key `tls.crt` | Broker server certificate |
| `/etc/broker/tls/tls.key` | Secret `internal-credential-broker-tls`, key `tls.key` | Broker server private key |

The target Secret volume maps Secret keys directly to filenames. Therefore the
keys must match the paths used in `broker.yaml`, for example
`public-cluster-a.ca.crt` and `public-cluster-a.token`.

## Provisioning a target cluster

Run the following steps once for every target cluster.

1. Ensure the target has the `access-system` namespace, the
   `InternalUser` and `CredentialLease` CRDs, and the InternalUser Controller.
2. Create the namespace used by the target Broker ServiceAccount if it does
   not already exist:

   ```sh
   kubectl --context "$TARGET_CONTEXT" create namespace access-broker-system \
     --dry-run=client -o yaml | kubectl --context "$TARGET_CONTEXT" apply -f -
   ```

3. Apply `target-rbac.yaml` to the target cluster:

   ```sh
   kubectl --context "$TARGET_CONTEXT" apply \
     -f ./config/broker/target-rbac.yaml
   ```

   This grants only `get` on `InternalUser`, `create` on `TokenReview`,
   target-namespace `CredentialLease` access, and
   `create` on `serviceaccounts/token` in `access-system`. It does not
   grant `cluster-admin`, Secret-data reads, or arbitrary RBAC mutation.

4. Issue a target ServiceAccount token with the exact configured audience.
   For example, the following creates a token file for initial provisioning:

   ```sh
   kubectl --context "$TARGET_CONTEXT" create token \
     internal-credential-broker \
     --namespace access-broker-system \
     --audience https://kubernetes.default.svc.cluster.local \
     --duration 24h > public-cluster-a.token
   ```

   Use a rotation period shorter than the token lifetime. If the target
   cluster imposes a shorter maximum lifetime, rotate more frequently or use
   the approved external secret-management process.

5. Obtain the target API CA from the cluster-admin trust bundle and save it as
   `public-cluster-a.ca.crt`. The following command works when the current
   kubeconfig stores the CA as `certificate-authority-data`:

   ```sh
   kubectl --context "$TARGET_CONTEXT" config view --raw --minify \
     -o 'jsonpath={.clusters[0].cluster.certificate-authority-data}' \
     | base64 --decode > public-cluster-a.ca.crt
   ```

6. Repeat the process for every target and create the central target Secret.
   Keep the token files protected while constructing the Secret:

   ```sh
   kubectl -n access-broker-system create secret generic \
     internal-credential-broker-target-credentials \
     --from-file=public-cluster-a.ca.crt=public-cluster-a.ca.crt \
     --from-file=public-cluster-a.token=public-cluster-a.token \
     --from-file=public-cluster-b.ca.crt=public-cluster-b.ca.crt \
     --from-file=public-cluster-b.token=public-cluster-b.token \
     --dry-run=client -o yaml | kubectl apply -f -
   ```

7. If the OIDC issuer uses a private CA, create the OIDC CA Secret:

   ```sh
   kubectl -n access-broker-system create secret generic \
     internal-credential-broker-oidc-ca \
     --from-file=ca.crt=oidc-ca.crt \
     --dry-run=client -o yaml | kubectl apply -f -
   ```

## Deploying the central Broker

1. Replace every placeholder in the ConfigMap's `broker.yaml` and update the
   installation-specific egress CIDRs and ports in the NetworkPolicy. Do not
   use the example addresses from the base manifest in production.
2. Render and apply the central resources from the repository root:

   ```sh
   kubectl kustomize ./config/broker >/tmp/internal-user-broker.yaml
   kubectl apply -f /tmp/internal-user-broker.yaml
   ```

   `target-rbac.yaml` is intentionally not included in this Kustomization. It
   is applied separately to each target cluster.

3. Set the ConfigMap key `config-file` to
   `/etc/broker/config/broker.yaml` so the Deployment selects multi-cluster
   mode. Apply this patch after the Kustomization so a later base-manifest
   apply does not overwrite it:

   ```sh
   kubectl -n access-broker-system patch configmap \
     internal-credential-broker-config --type merge \
     -p '{"data":{"config-file":"/etc/broker/config/broker.yaml"}}'
   ```

4. Wait for the rollout and inspect startup validation:

   ```sh
   kubectl -n access-broker-system rollout restart \
     deploy/internal-credential-broker
   kubectl -n access-broker-system rollout status \
     deploy/internal-credential-broker
   kubectl -n access-broker-system logs \
     deploy/internal-credential-broker --tail=200
   ```

The YAML is loaded only during process startup. Changes to YAML, CA paths,
NetworkPolicy, or non-secret environment variables require a rollout. Replacing
one target's token file is different: client-go rereads `BearerTokenFile`, so a
token-only rotation for target A does not require a Broker restart and does not
change target B.

## Environment variables and flags

These values apply to the required multi-cluster Broker configuration:

| Name or flag | Required | Default / meaning |
| --- | --- | --- |
| `BROKER_CONFIG_FILE` / `--config` | Yes | Path to the mounted versioned multi-cluster YAML file. |
| `BROKER_TLS_CERT_FILE` / `--tls-cert-file` | Yes | Broker HTTPS certificate. |
| `BROKER_TLS_KEY_FILE` / `--tls-key-file` | Yes | Broker HTTPS private key. |
| `BROKER_ADMIN_GROUP` / `--admin-group` | No | `sealos.internal.admin`; required OIDC group for internal approval. |
| `BROKER_TRUSTED_PROXY_CIDRS` | Yes | Comma-separated Higress/WAF egress CIDRs. |
| `BROKER_ALLOWED_CLIENT_CIDRS` | Yes | Comma-separated normalized client source CIDRs. |
| `BROKER_TRUSTED_CLIENT_IP_HEADER` | Yes | Defaults to `X-Trusted-Client-IP`. Only the trusted proxy may set this header. |
| `BROKER_REQUEST_RATE_LIMIT` / `--request-rate-limit` | No | Defaults to 10 requests/second. |
| `BROKER_REQUEST_RATE_BURST` / `--request-rate-burst` | No | Defaults to 20 requests. |
| `--listen-address` | No | Defaults to `:8443`; there is no corresponding environment variable. |

## Network requirements

The Broker Pod must be able to:

- resolve and reach the configured OIDC issuer and, when applicable, its
  Discovery and JWKS endpoints over HTTPS;
- reach every target `apiServer` over HTTPS;
- accept HTTPS only from the configured Higress/WAF egress CIDRs and the
  approval adapter namespace/pod selector;
- reach DNS and the Kubernetes APIs allowed by the NetworkPolicy.

The `clusterServer` endpoint is a user-facing value and does not have to be
reachable from the Broker Pod, but it must be reachable from the user's
network. Do not solve an egress problem by allowing unrestricted internet
egress. Add each target and OIDC destination explicitly to the NetworkPolicy.

## API routing

Multi-cluster clients must select only an allowlisted `clusterID`:

| Operation | Route |
| --- | --- |
| Base credential | `POST /v1/clusters/{clusterID}/credentials/base` |
| Elevated redemption | `POST /v1/clusters/{clusterID}/credentials/elevated/redeem` |
| Approval submission | `POST /v1/clusters/{clusterID}/internal/credentials/approve` |
| Lease status | `GET /v1/clusters/{clusterID}/credentials/{leaseID}` |
| Lease revoke | `POST /v1/clusters/{clusterID}/credentials/{leaseID}/revoke` |

The `clusterID` is not a URL or credential selector. The Broker resolves it to
the server-owned target entry. A client cannot override `apiServer`,
`clusterServer`, namespace, audience, CA, or bearer token in a request body.

## RBAC verification

Run these checks against the central cluster and every target cluster after
applying the manifests. Replace the ServiceAccount subject if the deployment
uses a different name.

```sh
SUBJECT='system:serviceaccount:access-broker-system:internal-credential-broker'

kubectl auth can-i get internalusers.access.sealos.io --as "$SUBJECT"
kubectl auth can-i create tokenreviews.authentication.k8s.io --as "$SUBJECT"
kubectl auth can-i get credentialleases.access.sealos.io \
  -n access-broker-system --as "$SUBJECT"
kubectl auth can-i create credentialleases.access.sealos.io \
  -n access-broker-system --as "$SUBJECT"
kubectl auth can-i create serviceaccounts/token \
  -n access-system --as "$SUBJECT"
```

The following must be denied:

```sh
kubectl auth can-i get secrets --all-namespaces --as "$SUBJECT"
kubectl auth can-i create clusterrolebindings.rbac.authorization.k8s.io \
  --as "$SUBJECT"
kubectl auth can-i '*' '*' --all-namespaces --as "$SUBJECT"
```

The exact command results are an environment-specific release gate; the
repository RBAC tests only verify the manifest boundaries.

## Rotation and disablement

- **Target token:** issue a replacement token with the same audience, update
  the corresponding `<id>.token` Secret key, and verify the target API call.
  No Broker restart is required for a token-only replacement.
- **Target CA:** update `<id>.ca.crt` and restart the Broker after the mounted
  Secret has propagated.
- **OIDC CA or YAML:** update the Secret/ConfigMap and restart the Broker.
- **Disable a target:** set `enabled: false`, remove its public route from
  clients, and restart the Broker. Remove the target credential Secret key
  only after outstanding leases are handled according to the incident/runbook
  procedure.
- **Remove a target:** revoke or expire its outstanding leases, delete its
  target RBAC and credential, remove its YAML entry, and verify that requests
  for the old `clusterID` return `404`.

Do not log token contents, kubeconfig contents, Secret data, or private keys.
Treat every target token and returned kubeconfig as a bearer credential.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| `unsupported Broker config version` or unknown field | Confirm `version: 1`, exact field names, and one YAML document. |
| `file is unavailable` | Confirm the Secret key, mount path, and `caFile`/`credentialFile` spelling. |
| `no enabled target backend is available` | Confirm at least one target has `enabled: true` and both referenced files are mounted. |
| OIDC Discovery/JWKS timeout | Check DNS, NetworkPolicy egress, issuer CA, `serverName`, and the exact issuer URL. |
| Target API `401` | Check the target token, its expiration, API CA, endpoint, and the configured audience. |
| Target API `403` | Run the `kubectl auth can-i` checks against the target cluster and verify the target RBAC template was applied. |
| Broker rejects configuration at startup | Inspect `BROKER_CONFIG_FILE`, YAML field names, mounted CA/token files, and enabled targets. |
| `clusterID` returns `404` | Confirm the ID exactly matches an enabled YAML target and that the Broker was restarted after the YAML change. |

## Source manifests

- Central deployment: `./config/broker/`
- Multi-cluster configuration types and validation:
  `./pkg/broker/config.go`
- Target client construction:
  `./cmd/broker/main.go`
- Per-target RBAC template:
  `./config/broker/target-rbac.yaml`
