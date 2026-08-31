package hub

import (
	"context"
	"sort"
	"strings"
	"time"
)

// Orphaned Terminating-pod VISIBILITY (issue #5328, item 3).
//
// WHY THIS EXISTS SEPARATELY FROM THE REAPER. orphaned_pod_reaper.go removes
// orphaned pods. This file REPORTS them. Those are deliberately different
// lanes, because a reaper alone trades one invisible problem for another:
//
// The measured incident was 27 orphaned pods across 15 hive namespaces
// accumulating for THREE WEEKS with nothing reporting it. The reaper now
// clears them. But orphan PRODUCTION — a node disappearing without draining —
// is issue #5328 item 1 and is NOT fixed by the reaper. If the reaper quietly
// deletes 30 pods every sweep forever, the fleet looks healthy and the
// underlying node-lifecycle fault stays invisible exactly as it did for those
// three weeks. A count on the fleet-health surface is what makes a SPIKE in
// orphan volume legible as the upstream signal it is.
//
// WHY IT IS COUNTED HERE AND NOT DERIVED FROM THE REAPER'S DELETIONS. Counting
// what the reaper deleted would report only what the hub could reach and had
// permission to remove — a cluster whose credential lacks pod-delete, or a pod
// inside the orphanedPodMinAge window, would silently read as zero. Counting
// the LIVE state instead means the number is what is actually stuck right now,
// independent of whether anything succeeded in cleaning it up. That is the
// property the fleet-health surface needs.
//
// THE PREDICATE IS NOT RESTATED. Selection goes through
// podIsOrphanedTerminating from orphaned_pod_reaper.go — the exact function the
// reaper uses, unmodified. There is deliberately no second copy of the rule to
// drift out of agreement with the first: if the predicate is ever changed, the
// count and the reaping change together or not at all.
//
// READ-ONLY. This lane issues `kubectl get pods` and NOTHING else. It adds no
// credential, no verb and no cluster-write of any kind beyond the read the
// cluster-health collector already performs on this same path (saas.go runs
// `get pods --all-namespaces` there today). The hub's blast radius is
// unchanged.
//
// SCOPED TO HIVE NAMESPACES. Only namespaces carrying hiveHostedNamespacePrefix
// are counted. A stuck pod in kube-system is somebody else's problem and
// reporting it here would be noise the fleet view cannot act on.

// orphanedPodStuckReportLimit caps how many per-namespace entries the report
// carries.
//
// The count itself is always exact and fleet-wide — the cap applies ONLY to the
// per-namespace breakdown that accompanies it. The breakdown exists so an
// operator can see WHERE orphans are concentrated; past a couple of dozen
// namespaces that list has stopped being diagnostic and started being a wall of
// text, and the total plus the worst offenders already says everything the
// operator needs to act on. The measured incident spanned 15 namespaces, so
// this is comfortably above the shape the signal was designed around.
const orphanedPodStuckReportLimit = 25

// OrphanedNamespaceCount is one namespace's stuck-pod count.
type OrphanedNamespaceCount struct {
	Namespace string `json:"namespace"`
	Count     int    `json:"count"`
}

// StuckPodReport is the per-cluster orphaned-pod signal surfaced in fleet
// health.
//
// Total is the authoritative number and is exact. Namespaces is the
// (possibly truncated, see orphanedPodStuckReportLimit) breakdown, ordered
// worst-first so the head of the list is the useful part when it is cut.
// NamespacesAffected is the exact count of distinct namespaces holding at
// least one orphan, so a truncated breakdown never understates the spread.
type StuckPodReport struct {
	Total              int                      `json:"total"`
	NamespacesAffected int                      `json:"namespaces_affected"`
	Namespaces         []OrphanedNamespaceCount `json:"namespaces,omitempty"`
	// Truncated marks that Namespaces holds fewer entries than
	// NamespacesAffected, so a reader can tell a short list from a complete
	// one rather than silently believing it saw everything.
	Truncated bool `json:"truncated,omitempty"`
}

// summarizeStuckPods builds the report from already-parsed candidates.
//
// PURE. No kubectl, no cluster, no clock of its own — now is passed in. This is
// what makes the count testable against real predicate matches rather than
// against the mere existence of a field.
//
// Namespaces not carrying hiveHostedNamespacePrefix are skipped before the
// predicate ever runs, so the report cannot include pods this fleet does not
// own.
func summarizeStuckPods(candidates []orphanedPodCandidate, now time.Time, minAge time.Duration) StuckPodReport {
	perNS := make(map[string]int)
	total := 0
	for _, c := range candidates {
		if !strings.HasPrefix(c.Namespace, hiveHostedNamespacePrefix) {
			continue
		}
		// The reaper's predicate, called not copied.
		if !podIsOrphanedTerminating(c, now, minAge) {
			continue
		}
		perNS[c.Namespace]++
		total++
	}

	rep := StuckPodReport{
		Total:              total,
		NamespacesAffected: len(perNS),
	}
	if total == 0 {
		return rep
	}

	rep.Namespaces = make([]OrphanedNamespaceCount, 0, len(perNS))
	for ns, n := range perNS {
		rep.Namespaces = append(rep.Namespaces, OrphanedNamespaceCount{Namespace: ns, Count: n})
	}
	// Worst-first, then by name so equal counts have a stable, reproducible
	// order rather than Go's randomized map iteration. Determinism matters:
	// this list is truncated, and a nondeterministic order would change WHICH
	// namespaces survive the cut between two reads of the same cluster state.
	sort.Slice(rep.Namespaces, func(i, j int) bool {
		if rep.Namespaces[i].Count != rep.Namespaces[j].Count {
			return rep.Namespaces[i].Count > rep.Namespaces[j].Count
		}
		return rep.Namespaces[i].Namespace < rep.Namespaces[j].Namespace
	})
	if len(rep.Namespaces) > orphanedPodStuckReportLimit {
		rep.Namespaces = rep.Namespaces[:orphanedPodStuckReportLimit]
		rep.Truncated = true
	}
	return rep
}

// collectStuckPods lists pods across the cluster and summarizes the orphans.
//
// READ-ONLY and best-effort: on any error it returns nil, and the caller omits
// the signal entirely rather than reporting a false zero. That distinction is
// the whole point — "no orphans" and "could not tell" must not render alike,
// which is the same rule the disk fields on this surface already follow.
//
// The list is NOT field-selector-filtered to a phase. The existing pod query in
// buildSingleClusterHealth uses --field-selector=status.phase=Running, which by
// construction can never see an orphan (the predicate requires phase !=
// Running) — so this needs its own, separate listing. It is scoped to the
// hive-hosted namespaces at summarize time.
func collectStuckPods(ctx context.Context, cluster *ClusterConfig, timeout time.Duration, now time.Time) *StuckPodReport {
	out, err := kubectlForClusterContext(ctx, cluster,
		"--request-timeout", timeout.String(),
		"get", "pods", "--all-namespaces", "-o", "json").Output()
	if err != nil {
		return nil
	}
	candidates, perr := parseOrphanCandidates(out)
	if perr != nil {
		return nil
	}
	rep := summarizeStuckPods(candidates, now, orphanedPodMinAge)
	return &rep
}
