package mutation

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Persist must not stage through the guessable fixed name path+".tmp": that
// pattern let any other writer on the same path clobber an in-flight commit
// (the beads #4742 failure). A pre-planted file at the fixed name must be
// left untouched, the commit must land, and no temp staging file may remain.
func TestPersistUsesUniqueTempNameAndLeavesNoResidue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claims.json")

	fixed := path + ".tmp"
	if err := os.WriteFile(fixed, []byte("hostile"), 0o600); err != nil {
		t.Fatalf("planting fixed-name tmp: %v", err)
	}

	l, err := OpenLedger(path, 0)
	if err != nil {
		t.Fatalf("OpenLedger: %v", err)
	}
	if _, err := l.Acquire(TaskClaim("acme/widgets", "acme/widgets#7"), "alice", ttl, time.Now()); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	got, err := os.ReadFile(fixed)
	if err != nil || string(got) != "hostile" {
		t.Fatalf("fixed-name tmp must be untouched by persist, got %q, %v", got, err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.Name() == filepath.Base(path) || e.Name() == filepath.Base(fixed) {
			continue
		}
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("persist left temp residue %q", e.Name())
		}
	}

	reopened, err := OpenLedger(path, 0)
	if err != nil {
		t.Fatalf("reopening persisted ledger: %v", err)
	}
	if _, ok := reopened.Get(TaskClaim("acme/widgets", "acme/widgets#7").Key()); !ok {
		t.Fatal("persisted acquisition must survive reopen")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != ledgerFileMode {
		t.Fatalf("ledger file mode = %o, want %o", info.Mode().Perm(), ledgerFileMode)
	}
}
