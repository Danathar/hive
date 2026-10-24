package policies

import (
	"bytes"
	"io/fs"
	"path"
	"strings"
	"testing"
)

// Every policy that tells an agent how to park an issue for the maintainer
// must carry ${STANDING_APPROVALS} exactly once, at the end of that
// paragraph, so an owner's project.standing_approvals reach every lane that
// can park. The variable sits on the paragraph's last line with no space or
// newline before it, so a hive with no standing approvals renders the
// paragraph byte for byte as before.
func TestParkingPoliciesCarryStandingApprovals(t *testing.T) {
	t.Parallel()

	park := []byte("**Park an issue that needs the maintainer's call.**")
	want := []byte("Use the flag; do not name the label yourself.${STANDING_APPROVALS}\n")

	entries, err := fs.ReadDir(DefaultPolicies, "defaults")
	if err != nil {
		t.Fatalf("read embedded defaults: %v", err)
	}
	checked := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := DefaultPolicies.ReadFile(path.Join("defaults", e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		n := bytes.Count(data, []byte("${STANDING_APPROVALS}"))
		if !bytes.Contains(data, park) {
			if n != 0 {
				t.Errorf("%s carries ${STANDING_APPROVALS} but has no park paragraph for it to extend", e.Name())
			}
			continue
		}
		checked++
		if n != 1 || !bytes.Contains(data, want) {
			t.Errorf("%s: the park paragraph must end with %q (found the variable %d times)", e.Name(), want, n)
		}
	}
	if checked == 0 {
		t.Fatal("no policy carries the park paragraph; the guard would pass vacuously")
	}
}
