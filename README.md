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

## Build and Test

```sh
make build
make test
```

Kubernetes manifests are under `config/`. The two CRDs are rendered from
`config/crd`, while each runtime component has its own kustomization.
