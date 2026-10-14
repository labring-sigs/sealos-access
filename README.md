# Sealos Access

Standalone internal-user credential access components for Sealos.

The repository contains the `InternalUser` and `CredentialLease` APIs, their
controller and admission components, the multi-cluster Broker, approval
adapter interfaces, and the `sealos-access` CLI.

## Components

- `cmd/internal-user-controller`: reconciles `InternalUser` and `CredentialLease` resources.
- `cmd/internal-user-admission`: validates controller-owned RBAC bindings.
- `cmd/broker`: authenticates OIDC users and issues target-cluster credentials.
- `cmd/internal-user-approval-adapter`: integrates external approval sources.
- `cmd/sealos-access`: user and administrator CLI.

## Helm charts

The deployment is intentionally split into two charts. Install the control-plane
chart once in the control cluster, and install the target-cluster chart once in
each production cluster. Do not install both charts in the same cluster unless
that cluster is intentionally both a control and a target cluster.

### Control cluster

`charts/sealos-access-control-plane` installs the central Broker, approval
adapter, Services, cert-manager certificates, and restrictive NetworkPolicies.
It does not install CRDs, the InternalUser Controller, or target-cluster RBAC.

```sh
helm upgrade --install sealos-access-control-plane \
  ./charts/sealos-access-control-plane \
  --namespace access-broker-system \
  --create-namespace \
  --values examples/control-plane-values.yaml
```

Set `broker.targets` for every production cluster. The Broker reads each
target's CA and bearer token from the existing Secret named by
`broker.targetCredentialsSecretName`; the keys must be `<target-id>.ca.crt` and
`<target-id>.token`. Do not put those values in Helm values or Git.

The approval adapter also expects the existing Secrets named by
`adapter.broker.caSecretName` and `adapter.broker.bearerTokenSecretName`. The
latter must contain the configured bearer-token key and may also contain
`smtp-username` and `smtp-password`.

### Production/target cluster

`charts/sealos-access-target-cluster` installs the two CRDs, InternalUser
Controller, controller webhooks, RBAC admission service, reviewed permission
profiles, and the least-privilege Broker target ServiceAccount/RBAC.

```sh
helm upgrade --install sealos-access-target-cluster \
  ./charts/sealos-access-target-cluster \
  --namespace access-controller-system \
  --create-namespace \
  --values examples/target-cluster-values.yaml
```

Install this chart separately against every production cluster referenced by
`broker.targets`. The target chart creates the `access-system` and
`access-broker-system` namespaces required by the target-side resources.

## Prerequisites and safety

- cert-manager is required by default for certificate issuance and webhook CA
  injection; disabling `*.tls.createCertificate` still requires an equivalent
  CA injection strategy for admission webhooks.
- The charts create their fixed namespaces by default and skip the Helm release
  namespace when `--create-namespace` creates it first; set `namespaces.create: false`
  when namespace lifecycle is managed outside Helm.
- A NetworkPolicy-capable CNI is required. Default-deny policies are enabled by
  default; add exact API, OIDC, SMTP, and approval-system CIDRs or selectors via
  `broker.networkPolicy.additionalEgress` and
  `adapter.networkPolicy.additionalEgress` before production rollout.
- Create the target credentials, OIDC CA, Broker CA, and adapter credentials
  through an external secret-management process or `kubectl create secret`.
- Replace all example OIDC URLs, target endpoints, cluster IDs, SMTP settings,
  and source-IP CIDRs before enabling a target.
- Review the generated RBAC and run `kubectl auth can-i` checks in every target
  cluster before issuing credentials.

## Build and Test

```sh
make build
make test
```

Kubernetes manifests are under `config/`. The two CRDs are rendered from
`config/crd`, while each runtime component has its own kustomization.
