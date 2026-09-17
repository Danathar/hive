package hub

import (
	"context"
	"log/slog"
	"strings"
	"time"
)

// provisionRollbackTimeout bounds each kubectl call in the rollback path.
//
// Matches stampNamespaceIdentityTimeout: both are single synchronous kubectl
// calls on the provisioning path, and both must fail fast against an
// unreachable cluster rather than hold a queued provision slot (or, in tests
// with no kubectl on PATH, hold the test) for kubectl's own default timeout.
const provisionRollbackTimeout = 15 * time.Second

// namespacePresence is the tri-state result of the pre-apply existence check.
// A boolean cannot carry it: "absent" and "unknown" must drive different
// behaviour, and collapsing them is precisely what would turn an unreachable
// cluster into a namespace delete.
type namespacePresence int

const (
	// namespacePresenceUnknown means the check itself failed — no kubectl, an
	// unreachable API server, an RBAC denial. Never roll back on this.
	namespacePresenceUnknown namespacePresence = iota
	// namespacePresenceAbsent means kubectl succeeded and reported nothing: the
	// namespace did not exist before the apply.
	namespacePresenceAbsent
	// namespacePresentBeforeApply means the namespace already existed and is
	// therefore not ours to delete on a failed apply.
	namespacePresentBeforeApply
)

// hostedNamespaceExistedBeforeApply reports whether a hosted namespace exists
// on the cluster right now, distinguishing "no" from "could not tell".
//
// Uses `get namespace <ns> --ignore-not-found -o name`: with --ignore-not-found
// kubectl exits 0 and prints NOTHING when the namespace is absent, and prints
// "namespace/<ns>" when it is present. A non-zero exit therefore means the
// check failed rather than that the namespace is missing — the distinction the
// whole rollback decision rests on.
func hostedNamespaceExistedBeforeApply(cluster *ClusterConfig, namespace string) namespacePresence {
	if cluster == nil || strings.TrimSpace(namespace) == "" {
		return namespacePresenceUnknown
	}
	if !cluster.KubectlReachable() {
		return namespacePresenceUnknown
	}
	ctx, cancel := context.WithTimeout(context.Background(), provisionRollbackTimeout)
	defer cancel()
	out, err := kubectlForClusterContext(ctx, cluster,
		"--request-timeout", provisionRollbackTimeout.String(),
		"get", "namespace", namespace, "--ignore-not-found", "-o", "name").Output()
	if err != nil {
		return namespacePresenceUnknown
	}
	if strings.TrimSpace(string(out)) == "" {
		return namespacePresenceAbsent
	}
	return namespacePresentBeforeApply
}

// rollbackProvisionNamespace deletes the hosted namespace a failed provision
// just created — and ONLY that case.
//
// before is the presence recorded by hostedNamespaceExistedBeforeApply BEFORE
// the apply ran. The three cases and why each behaves as it does are argued in
// the file comment above; the short version:
//
//	absent  -> delete (we created it, nothing else can be using it)
//	present -> keep   (pre-existing; deleting it destroys a live spoke)
//	unknown -> keep   (never resolve "could not tell" into a delete)
//
// The delete uses --wait=false so a namespace whose termination is slow (a
// finalizer on a PVC can hold one for minutes) does not pin the provisioning
// queue slot; the API server proceeds with the cascade on its own. It uses
// --ignore-not-found so the common case — the apply failed before creating
// anything at all — is a clean no-op rather than a spurious error line.
//
// Returns whether a delete was issued, purely so tests can assert the decision
// rather than infer it from a log line. Callers on the provisioning path ignore
// it: a failed rollback must not change the error the admin sees.
func rollbackProvisionNamespace(cluster *ClusterConfig, namespace string, before namespacePresence, logger *slog.Logger) bool {
	if cluster == nil || strings.TrimSpace(namespace) == "" {
		return false
	}

	switch before {
	case namespacePresentBeforeApply:
		if logger != nil {
			logger.Info("provision rollback skipped: namespace existed before this apply — not ours to delete",
				"namespace", namespace, "cluster", cluster.ID)
		}
		return false
	case namespacePresenceUnknown:
		if logger != nil {
			// Loud, because this is the branch that leaves a namespace behind.
			// The read-only detector in leaked_hosted_namespace.go is what
			// eventually surfaces it; saying so here is what connects the two
			// for whoever reads this line.
			logger.Warn("provision rollback skipped: could not determine whether the namespace pre-existed — leaving it in place for the leaked-namespace report",
				"namespace", namespace, "cluster", cluster.ID)
		}
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), provisionRollbackTimeout)
	defer cancel()
	out, err := kubectlForClusterContext(ctx, cluster,
		"--request-timeout", provisionRollbackTimeout.String(),
		"delete", "namespace", namespace, "--ignore-not-found", "--wait=false").CombinedOutput()
	if err != nil {
		if logger != nil {
			logger.Warn("provision rollback: namespace delete failed — namespace may be leaked",
				"namespace", namespace, "cluster", cluster.ID,
				"output", strings.TrimSpace(string(out)), "error", err)
		}
		return true
	}
	if logger != nil {
		logger.Info("provision rollback: deleted the namespace created by a failed provision",
			"namespace", namespace, "cluster", cluster.ID)
	}
	return true
}
