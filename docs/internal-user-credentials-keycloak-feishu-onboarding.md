# Internal User Credentials: Keycloak OIDC Onboarding (Feishu upstream)

Status: planning checklist for placing Keycloak between the internal user
credential system and Feishu. The credential system trusts Keycloak; Keycloak
brokers identities to Feishu. No Keycloak- or Feishu-specific implementation
exists in this repository.

Chinese companion:
[internal-user-credentials-keycloak-feishu-onboarding.zh-CN.md](internal-user-credentials-keycloak-feishu-onboarding.zh-CN.md).

## 1. Target architecture

```text
user / CLI
   |  OIDC authorization code + PKCE
   v
Keycloak realm  ---- identity brokering ---->  Feishu (upstream IdP)
   ^
   |  JWKS + issuer (RS256 JWT access token)
   |
internal-credential-broker  <--- Bearer JWT --- CLI
```

The credential system no longer talks to Feishu directly. It trusts one stable
Keycloak realm issuer; Keycloak is free to change the upstream identity source
without touching the Broker, Controller, or `InternalUser` data.

## 2. Why Keycloak fixes the direct-Feishu blockers

| Blocker with direct Feishu | How Keycloak solves it |
| --- | --- |
| Feishu access tokens are usually opaque; the Broker requires a JWKS-verifiable JWT | Keycloak access tokens are RS256 JWTs with `kid` and a realm JWKS |
| No standard OIDC discovery/JWKS from Feishu | Keycloak always exposes discovery and `/protocol/openid-connect/certs` |
| Unstable/opaque issuer and subject | Issuer is your realm URL; `sub` is the stable Keycloak user ID |
| No custom `aud`/`groups` claims | Client-scope mappers add an audience and a `groups` claim |
| Changing upstream IdP breaks identities | Only the Keycloak IdP changes; `InternalUser` identity stays |

What Keycloak does **not** solve:

- the elevated approval integration (Keycloak is not an approval system);
- the Broker source-address policy (`X-Trusted-Client-IP` still required);
- Broker network egress / CA trust (now points at Keycloak, not Feishu);
- the CLI callback-port question (Keycloak wildcard redirects help; see 3.3).

## 3. Contract the system requires

### 3.1 Token

`./pkg/broker/oidc.go` verifies the bearer token as a signed
JWT against JWKS and requires:

- a `kid` header and a supported algorithm (default `RS256`);
- `iss` exactly equal to the configured `oidc-issuer`;
- `aud` containing the configured `oidc-audience` (exact string match);
- a valid `sub`, `exp`, and optional `nbf`;
- an optional `groups` claim (string or array of strings).

The CLI (`./cmd/sealos-access/main.go`) sends the OIDC
**access_token** to the Broker. Keycloak access tokens satisfy this because
they are signed JWTs.

### 3.2 Issuer and endpoints

For a Keycloak realm:

- issuer: `https://<keycloak-host>/realms/<realm>`
- discovery: `<issuer>/.well-known/openid-configuration`
- JWKS: `<issuer>/protocol/openid-connect/certs`

The `issuer` returned by the discovery document must equal the configured
`oidc-issuer`. This value is controlled by Keycloak hostname/frontend URL
configuration, so fix it before wiring the Broker.

### 3.3 CLI callback URI

The CLI currently listens on `http://127.0.0.1:<random-port>/callback`.
Keycloak supports wildcard redirect URIs such as `http://127.0.0.1/*` for
public clients, which covers the random port; confirm on your Keycloak
version, otherwise add a fixed callback port to the CLI.

### 3.4 Subject stability

The Broker uses the token `sub` as the user identity; the
`InternalUser.spec.identity.subject` must equal the Keycloak `sub`, which is
the Keycloak user UUID. Treat realm users and IDs as durable data:

- back up the realm and the Keycloak database;
- do not delete/recreate users to "fix" attributes (the UUID changes);
- if the realm is migrated, preserve user IDs.

If a deterministic subject derived from the upstream identity is required,
configure a Keycloak mapper that writes a stable subject, or keep the upstream
ID as a user attribute and map it into a custom claim.

## 4. Keycloak-side configuration

### 4.1 Realm, hostname, TLS

1. Create a dedicated realm (for example `sealos`).
2. Set a stable frontend URL / `KC_HOSTNAME` so the issuer is exactly
   `https://<keycloak-host>/realms/sealos`, reachable by both the CLI and the
   Broker; behind a proxy enable the appropriate proxy-header setting.
3. Terminate TLS with a CA that the Broker can trust (mount that CA into the
   Broker; the image is built from `scratch`, see 5.2).

### 4.2 Client for the CLI

- Public client, standard flow, **PKCE S256**, no client secret.
- Redirect URIs: `http://127.0.0.1/*` (verify wildcard support) or the fixed
  port used by the CLI.
- The CLI passes `--client-id <client>`; the Broker does not need the client
  ID unless it is mapped into the token audience.

### 4.3 Audience mapper

The Broker compares `aud` with an exact string. Add a client scope with an
**Audience** mapper that adds the Broker audience, for example
`sealos-internal-credential-broker`, to the access token, and set the Broker
`oidc-audience` to the same value.

### 4.4 Groups mapper (optional, for the OIDC admin path)

The Broker uses `groups` with exact string matching (`claimGroups` performs no
path normalization). The Keycloak **Group Membership** mapper emits full group
paths with a leading slash, for example `/sealos.internal.admin`.

Choose one:

- set the Broker admin group to the exact mapped value
  (`/sealos.internal.admin`); or
- add a mapper that emits `sealos.internal.admin` without the leading slash.

Groups are required for the OIDC administrator operations on the Broker HTTP
API, including elevated approval submission. The Broker does not use mTLS for
this path.

### 4.5 Signing and token lifetime

- Keep RS256 (default) so token headers carry `kid` and the JWKS contains the
  matching key; rotation updates the JWKS automatically.
- A short access-token lifespan (5-15 minutes) is sufficient: the CLI uses the
  token immediately for the Broker request. Do not lengthen it to act as a
  credential lifetime; credential TTLs are enforced by the Broker profiles.

### 4.6 Identity provider for Feishu

Create an Identity Provider in the realm and point it at Feishu:

- If Feishu exposes OIDC, use the generic **OpenID Connect v1.0** provider
  (authorization/token/userinfo/JWKS endpoints, client ID/secret, scopes,
  issuer).
- If Feishu exposes SAML, use the **SAML v2.0** provider.
- If Feishu only exposes OAuth 2.0 with opaque tokens, Keycloak has no
  built-in Feishu provider: implement a Keycloak `IdentityProvider` SPI (or
  use a maintained Feishu provider plugin, or put a small token-exchange
  bridge in front of Keycloak).

Configure attribute mappers to import at least the email address and a stable
user identifier; configure the first-broker-login flow so users are created
and linked on first login.

Feishu must be configured with the Keycloak broker redirect URI:

```text
https://<keycloak-host>/realms/<realm>/broker/<idp-alias>/endpoint
```

## 5. Credential-system configuration

Snapshot the objects you are about to change:

```sh
mkdir -p /tmp/keycloak-onboarding-backup
kubectl -n access-broker-system get cm internal-credential-broker-config -o yaml \
  > /tmp/keycloak-onboarding-backup/broker-cm.yaml
kubectl -n access-broker-system get deploy internal-credential-broker -o yaml \
  > /tmp/keycloak-onboarding-backup/broker-deploy.yaml
kubectl -n access-broker-system get netpol internal-credential-broker-controlled-egress -o yaml \
  > /tmp/keycloak-onboarding-backup/broker-netpol.yaml
```

### 5.1 Broker

The Broker accepts only the strict versioned multi-cluster YAML file. Edit the
`oidc` section of `broker.yaml` and keep every target in the `targets` list;
repeat the target entry for each Kubernetes cluster:

```yaml
version: 1
oidc:
  issuer: https://<keycloak-host>/realms/<realm>
  audience: <broker-audience-added-by-the-mapper>
  jwksURL: ""
  caFile: /etc/broker/oidc-ca/ca.crt
  serverName: <keycloak-host>
targets:
- id: <target-id>
  apiServer: https://<target-api>
  clusterServer: https://<target-public-api>
  caFile: /etc/broker/targets/<target-id>.ca.crt
  credentialFile: /etc/broker/targets/<target-id>.token
  kubernetesAudience: https://kubernetes.default.svc.cluster.local
  userNamespace: access-system
  brokerNamespace: access-broker-system
  clusterName: <target-id>
  enabled: true
```

Keep `BROKER_CONFIG_FILE` set to `/etc/broker/config/broker.yaml`. The target
CA and bearer-token files are mounted from the per-target Secret described in
[`internal-user-credentials-broker-config.md`](internal-user-credentials-broker-config.md).
Leave `jwksURL` empty to use discovery, or set the Keycloak JWKS endpoint when
discovery is unavailable or proxied. Then restart the Broker:

```sh
kubectl -n access-broker-system rollout restart deploy/internal-credential-broker
kubectl -n access-broker-system rollout status deploy/internal-credential-broker
```

### 5.2 CA bundle

`./Dockerfile.broker` builds from `scratch` and has no system
CA store. Mount the Keycloak CA (or a CA bundle) at the `caFile` path in
`broker.yaml`; do not rely on system roots.

### 5.3 NetworkPolicy and source-address policy

- Allow Broker egress to Keycloak only: a pod selector if Keycloak runs in the
  cluster, or TCP 443 to its address if external. Feishu egress is now needed
  by Keycloak, not by the Broker.
- Authenticated Broker requests still need a trusted peer plus exactly one
  `X-Trusted-Client-IP` header inside `allowed-client-cidrs`; in production
  Higress injects it. Never expose the Broker directly to clients.

### 5.4 Controller issuer allowlist

```sh
kubectl -n access-controller-system patch cm internal-user-controller-config \
  --type=merge \
  -p '{"data":{"oidc-issuers":"https://<keycloak-host>/realms/<realm>,https://test-oidc-provider.access-mock.svc:8443"}}'

kubectl -n access-controller-system rollout restart deploy/internal-user-controller
kubectl -n access-controller-system rollout status deploy/internal-user-controller
```

Keep the previous issuer in the Controller allowlist only when the migration
requires it; the Broker validates the issuer configured in its strict YAML file.

### 5.5 Create a test identity

```sh
./bin/sealos-access-amd64 admin create \
  --kubeconfig "$KUBECONFIG" \
  --issuer "https://<keycloak-host>/realms/<realm>" \
  --subject "<keycloak-sub-uuid>"

kubectl get internalusers -o wide
kubectl wait --for=jsonpath='{.status.phase}'=Active \
  internaluser/<derived-iu-name> --timeout=120s
```

The name is `iu-<first-24-hex-of-sha256(issuer + NUL + subject)>`.

## 6. Elevated approvals (unchanged by Keycloak)

Keycloak authenticates people; it does not approve elevated access. The
elevated workflow still needs an approval source that submits an authoritative
record to `POST /v1/clusters/{clusterID}/internal/credentials/approve` with an OIDC token carrying
the exact configured administrator group:

1. Implement `approvaladapter.ApprovalSource` for the approval system
   (Feishu approval API: instance lookup and email resolution).
2. Add an event trigger (callback or polling) and expose the adapter through
   an Ingress/route reachable by that system.
3. Wire `BrokerHTTPClient` with an OIDC bearer token and a notifier (SMTP `EmailNotifier` or a
   Feishu message notifier that must be implemented).
4. Keep approval records non-secret: the adapter submits an approval ID,
   target identity, TTL, and reason; the Broker returns the one-time reference.

Until the adapter exists, use the `sealos-access admin approve` path from the
mock e2e runbook (sections 6.4 and 9.3).

## 7. Work required

| Item | Owner | Notes |
| --- | --- | --- |
| Keycloak deployment, DB, TLS, backups | platform | Stable issuer URL and CA |
| Feishu IdP feasibility (OIDC/SAML/custom SPI) | platform + Feishu admin | Decide before wiring mappers |
| Realm, CLI client, PKCE, redirect URI | platform | Wildcard or fixed port |
| Audience mapper | platform | Must match Broker `oidc-audience` exactly |
| Groups mapper (optional) | platform | Exact match with the Broker admin group |
| Broker issuer/audience/CA/egress update | cluster ops | Includes netpol and restart |
| Controller allowlist update | cluster ops | Comma-separated issuers |
| `InternalUser` subject = Keycloak `sub` | cluster ops | UUID stability/backups |
| Approval adapter | app team | Not solved by Keycloak |

## 8. Cutover sequence

1. Stand up Keycloak with a stable issuer and TLS; verify discovery and JWKS.
2. Configure the Feishu IdP and complete one login; decode the Keycloak
   access token and check `iss`, `aud`, `sub`, and `groups`.
3. Add the audience (and optional groups) mappers; re-check the token.
4. Back up the Broker objects, then update issuer/audience/CA/egress and
   restart; confirm the Broker fetches JWKS.
5. Add the Keycloak issuer to the Controller allowlist and restart.
6. Create an `InternalUser` for a Keycloak user and run the base flow.
7. Run the elevated flow (adapter, or `sealos-access admin approve` for the first run).

## 9. Verification checklist

- [ ] Discovery `issuer` equals the configured `oidc-issuer`.
- [ ] `jwks_uri` is `<issuer>/protocol/openid-connect/certs`.
- [ ] Access token header has `kid`; claims contain `iss`, `aud`, `sub`,
      `exp`; `aud` contains the configured audience.
- [ ] `groups` (if used) matches the Broker admin group exactly.
- [ ] Broker audit/log shows a successful `issue_base`.
- [ ] `InternalUser` is `Active` with the derived name.
- [ ] Base kubeconfig: read pods/services/deployments/replicasets; cannot
      create `InternalUser`/`CredentialLease`.
- [ ] Elevated kubeconfig: `events` create/patch allowed; deployment/SA/RBAC
      writes denied.
- [ ] Replay of the same approval reference returns HTTP 409.

## 10. Rollback

Restore the Broker ConfigMap, Deployment, and NetworkPolicy snapshots, remove
temporary CA ConfigMaps/Secrets, and restart the Broker. The mock e2e runbook
(section 10) contains a working restore sequence, including the caveat that
`kubectl apply` does not remove fields added by strategic patches.

## 11. Related documents

- [Internal user credentials overview](internal-user-credentials-overview.md)
- [Architecture](internal-user-credentials-architecture.md)
- [Mock e2e runbook](internal-user-credentials-mock-e2e.md)
- [Approval adapter](internal-user-approval-adapter.md)
- [Pluggable adapter](internal-user-credentials-pluggable-adapter.md)
- [Administrator CLI](internal-user-admin-cli.md)
- [User CLI](internal-user-credentials-cli.md)
