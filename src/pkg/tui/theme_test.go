package tui

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// colorConstructors are the lipgloss spellings that turn a literal into a
// color. A theme is only single-source if none of them appears outside
// theme.go, so this list is what the ratchet below looks for.
var colorConstructors = map[string]bool{
	"Color":                 true,
	"ANSIColor":             true,
	"AdaptiveColor":         true,
	"CompleteColor":         true,
	"CompleteAdaptiveColor": true,
}

// hexLiteral matches a CSS-style color string, the other way a color reaches
// a call site — as a bare "#ff5fd7" handed to a Style method.
var hexLiteral = regexp.MustCompile(`^"#[0-9a-fA-F]{3,8}"$`)

// themedPackages are the directories the ratchet covers, relative to this
// package: the app itself, where T25 found the two literals it migrated, and
// panes/, whose help overlay borders its box in the frame's emphasis color.
//
// theme/ is deliberately NOT listed. It is the one place a color may be named,
// so the ratchet works by not scanning it rather than by exempting a filename
// — a file added to this package called theme.go gets caught like any other.
var themedPackages = []string{".", "panes"}

// TestNoRawColorLiteralsOutsideTheme is the ratchet the acceptance criteria
// ask for: pkg/tui/theme is the only place allowed to name a color.
//
// It parses the sources rather than grepping them, so a color mentioned in a
// comment (this package explains its palette at length) cannot fail the test,
// and a color written as lipgloss.Color("240") cannot pass it by being spelled
// across two lines.
//
// It scans app.go as well as panes/, because app.go is where the literals
// actually were — a panes-only test would have passed on the day it was
// written, against a package with a deliberate no-color policy, while the two
// colors it was meant to catch sat one directory up.
func TestNoRawColorLiteralsOutsideTheme(t *testing.T) {
	for _, dir := range themedPackages {
		paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatalf("glob %s: %v", dir, err)
		}
		if len(paths) == 0 {
			t.Fatalf("no Go files under %s — the ratchet is scanning nothing", dir)
		}
		for _, path := range paths {
			if strings.HasSuffix(filepath.Base(path), "_test.go") {
				continue
			}
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			ast.Inspect(file, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.SelectorExpr:
					pkg, ok := n.X.(*ast.Ident)
					if ok && pkg.Name == "lipgloss" && colorConstructors[n.Sel.Name] {
						t.Errorf("%s: lipgloss.%s names a color outside pkg/tui/theme; add a theme token and reference it instead",
							fset.Position(n.Pos()), n.Sel.Name)
					}
				case *ast.BasicLit:
					if n.Kind == token.STRING && hexLiteral.MatchString(n.Value) {
						t.Errorf("%s: raw color literal %s outside pkg/tui/theme; add a theme token and reference it instead",
							fset.Position(n.Pos()), n.Value)
					}
				}
				return true
			})
		}
	}
}

// TestFocusBorderAdaptsToBackground is the behavioural half: the focused
// pane's border must actually resolve to a DIFFERENT color on a light
// terminal than on a dark one.
//
// Asserting on theme's field values would pass on a theme nothing reads. This
// renders the real frame through a color-capable profile and compares the
// bytes, so it also covers the wiring — a style that kept a hardcoded color,
// or a token assigned to the wrong border, fails here.
func TestFocusBorderAdaptsToBackground(t *testing.T) {
	restoreProfile(t, termenv.ANSI256)

	dark := frameWithBackground(t, true)
	light := frameWithBackground(t, false)

	if dark == light {
		t.Fatal("the frame renders identically on light and dark backgrounds; the palette is not adaptive in practice")
	}
	for _, f := range []struct {
		name  string
		frame string
	}{{"dark", dark}, {"light", light}} {
		if !strings.Contains(f.frame, "\x1b[") {
			t.Fatalf("%s-background frame carries no color at all under ANSI256:\n%q", f.name, f.frame)
		}
	}
}

// TestFrameIsBackgroundIndependentUnderTestProfile guards the golden files.
//
// `go test` renders through termenv's Ascii profile, where color is stripped
// before an AdaptiveColor's two halves can differ — so background detection,
// which depends on the terminal a machine happens to have, must not reach the
// captured bytes. If it ever could, testdata/grid.golden would pass or fail
// depending on whose terminal CI inherited: the #5131 flake's shape, one
// vector over.
func TestFrameIsBackgroundIndependentUnderTestProfile(t *testing.T) {
	restoreProfile(t, termenv.Ascii)

	if dark, light := frameWithBackground(t, true), frameWithBackground(t, false); dark != light {
		t.Fatalf("under the Ascii profile the frame differs by background, so the golden files depend on the machine's terminal:\ndark:\n%q\nlight:\n%q", dark, light)
	}
}

// frameWithBackground renders a sized frame with the reported terminal
// background pinned, restoring the previous setting when the test ends.
func frameWithBackground(t *testing.T, dark bool) string {
	t.Helper()
	had := lipgloss.HasDarkBackground()
	t.Cleanup(func() { lipgloss.SetHasDarkBackground(had) })
	lipgloss.SetHasDarkBackground(dark)

	m, _ := newModel().Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	return m.(model).View()
}

// restoreProfile pins lipgloss's color profile for one test and puts the
// previous one back afterwards. The profile is process-global state on the
// default renderer that every style in this package shares, so a test that
// changed it and walked away would silently retint every test after it.
func restoreProfile(t *testing.T, p termenv.Profile) {
	t.Helper()
	had := lipgloss.ColorProfile()
	t.Cleanup(func() { lipgloss.SetColorProfile(had) })
	lipgloss.SetColorProfile(p)
}
