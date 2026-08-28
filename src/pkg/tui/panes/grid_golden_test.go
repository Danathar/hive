// The full-frame golden test lives HERE, next to the panes, because the
// design doc's testing convention (src/docs/design/tui.md §"Testing
// convention") puts rendering goldens under src/pkg/tui/panes/testdata/ and
// the T3 acceptance criteria name panes/testdata/grid.golden specifically.
// The frame itself is composed by pkg/tui, reached through its exported New —
// an external test package, so no import cycle.
//
// Regenerate after a DELIBERATE layout change with:
//
//	cd src && go test ./pkg/tui/panes/... -update
//
// and review the regenerated file in the diff like any other change — a
// golden file updated without reading it asserts nothing.
package panes_test

import (
	"bytes"
	"flag"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"

	"github.com/kubestellar/hive/pkg/tui"
)

// TestGridGolden pins the complete 100x30 frame — header, all four bordered
// stub panes with the top-left one focused, and the footer — byte for byte.
// The size is pinned explicitly per the design doc: golden files are
// width-sensitive terminal output, and a test that inherited a default size
// would produce a diff on someone else's machine.
func TestGridGolden(t *testing.T) {
	tm := teatest.NewTestModel(t, tui.New(), teatest.WithInitialTermSize(100, 30))

	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	tm.WaitFinished(t, teatest.WithFinalTimeout(5*time.Second))

	out, err := io.ReadAll(tm.FinalOutput(t, teatest.WithFinalTimeout(5*time.Second)))
	if err != nil {
		t.Fatalf("read final output: %v", err)
	}
	requireGolden(t, out, filepath.Join("testdata", "grid.golden"))
}

// requireGolden is golden.RequireEqual with the file name fixed to the path
// the T3 acceptance criteria specify (testdata/grid.golden) instead of the
// package's tb.Name()-derived default, which would be TestGridGolden.golden.
// It honours the SAME -update flag: teatest's import of x/exp/golden has
// already registered it, so the documented regeneration command
// (`go test ./pkg/tui/panes/... -update`) drives this test too, and no second
// flag definition can collide with the panes' future per-pane goldens.
func requireGolden(t *testing.T, out []byte, path string) {
	t.Helper()
	if f := flag.Lookup("update"); f != nil {
		if getter, ok := f.Value.(flag.Getter); ok {
			if update, ok := getter.Get().(bool); ok && update {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, out, 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing golden file (regenerate with -update): %v", err)
	}
	if !bytes.Equal(out, want) {
		t.Fatalf("output does not match %s (regenerate with -update after a DELIBERATE layout change and review the diff)\ngot %d bytes, want %d", path, len(out), len(want))
	}
}
