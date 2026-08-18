# backup-exec-restriction overlay

This optional kustomize overlay deploys a [Kyverno](https://kyverno.io/) `ClusterPolicy` that closes the
gap between the backup CronJob's cluster-wide `pods/exec` RBAC grant and the
application's actual access pattern (only `hive-hosted-*` namespaces).

## Background (issue [#4062](https://github.com/kubestellar/hive/issues/4062))

The `hive-hub-backup` ClusterRole must grant `pods/exec: create` cluster-wide
because Kubernetes RBAC cannot express wildcard-namespace rules. The backup
binary (`pkg/hubbackup/collect.go`) only ever targets `hive-hosted-<id>`
namespaces, but the RBAC permission is broader: a compromised backup pod could
exec into pods in any namespace, including infrastructure workloads.

This overlay enforces the intended restriction at the admission layer.

## Prerequisites

- Kyverno ≥ 1.11 installed and running in the cluster.
- The base k8s manifests already applied
  (`src/deploy/k8s` or another base that includes `backup-cronjob.yaml`).

## Apply

```sh
# Standalone (no kustomize build needed)
kubectl apply -f src/deploy/kustomize/overlays/backup-exec-restriction/kyverno-backup-exec-restriction.yaml

# Via kustomize (includes the base manifests)
kubectl apply -k src/deploy/kustomize/overlays/backup-exec-restriction
```

## What this changes

| Before | After |
|--------|-------|
| `hive-hub-backup` SA can exec into pods in **any** namespace | Exec requests are admitted only when the target namespace matches `hive-hosted-*` |
| Namespace enforcement: application-only (binary, not admission) | Namespace enforcement: application + Kyverno admission webhook |

## Testing

After applying, verify the policy blocks an out-of-scope exec:

```sh
# Should be DENIED (wrong namespace)
kubectl auth can-i create pods/exec \
  --namespace default \
  --as system:serviceaccount:hive-hub:hive-hub-backup

# Should be ALLOWED (correct namespace)
kubectl auth can-i create pods/exec \
  --namespace hive-hosted-example \
  --as system:serviceaccount:hive-hub:hive-hub-backup
```

Note: `kubectl auth can-i` checks RBAC only; Kyverno enforcement is visible
in `kubectl describe clusterpolicy restrict-backup-exec-to-hive-namespaces`
and in Kyverno audit logs.
