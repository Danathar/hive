package automerge

import (
	"strings"
	"sync"
	"time"
)

// MergeQueueBases remembers base branches whose ruleset refused a direct
// merge as merge-queue-only (hivecommons/hive#14068). The hive merges over
// REST and cannot enqueue, so no sweep can merge a PR into such a base. The
// sweeps read it to skip the merge call; the merge-eligibility classifier
// reads it so the dashboard does not call such a PR merge-eligible.
//
// An entry expires after mergeQueueBaseRecheck, and the next merge attempt
// on that base finds out again, so removing the queue rule is picked up
// without a restart. The memory is in-process only: after a restart a base
// is unknown until a sweep tries to merge into it once.
type MergeQueueBases struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

// NewMergeQueueBases returns an empty memory.
func NewMergeQueueBases() *MergeQueueBases {
	return &MergeQueueBases{seen: make(map[string]time.Time)}
}

func mergeQueueBaseKey(repo, base string) string {
	return strings.ToLower(strings.TrimSpace(repo)) + "#" + strings.TrimSpace(base)
}

// Note records that base of repo ("owner/name") refused a direct merge at now.
func (m *MergeQueueBases) Note(repo, base string, now time.Time) {
	if m == nil || strings.TrimSpace(base) == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.seen == nil {
		m.seen = make(map[string]time.Time)
	}
	m.seen[mergeQueueBaseKey(repo, base)] = now
}

// Required reports whether base of repo ("owner/name") refused a direct merge
// as merge-queue-only within mergeQueueBaseRecheck of now.
func (m *MergeQueueBases) Required(repo, base string, now time.Time) bool {
	if m == nil || strings.TrimSpace(base) == "" {
		return false
	}
	key := mergeQueueBaseKey(repo, base)
	m.mu.Lock()
	defer m.mu.Unlock()
	seen, ok := m.seen[key]
	if !ok {
		return false
	}
	if now.Sub(seen) >= mergeQueueBaseRecheck {
		delete(m.seen, key)
		return false
	}
	return true
}
