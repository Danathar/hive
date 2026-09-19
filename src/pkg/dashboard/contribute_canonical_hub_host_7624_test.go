package dashboard

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The hosted hub moved from hive.kubestellar.io to hive.hivecommons.dev on
// 2026-09-04 (src/docs/hivecommons-migration.md, P5). The old host is a
// legacy-redirect deployment that answers EVERY path with a 301
// (src/deploy/legacy-redirect/README.md). That is fine for a browser and
// fatal for the two ways the contributor Justfile used it (#7624):
//
//   - `curl -sf` with no -L treats a 301 as failure, so the registry lookup
//     in `just contribute-setup` reported "Could not reach" on a hub that
//     was up;
//   - a WebSocket handshake does not follow redirects, so the default
//     HIVE_HUB of wss://hive.kubestellar.io/contribute could never connect.
//
// The dashboard's own "Hub Enabled" tooltip named the old host too. These are
// source-level pins, like contribute_hub_resolution_test.go: there is no Go
// code path behind a Justfile default or an HTML tooltip, and the regression
// is a hostname string, so the file is what is held.

const canonicalHubHost = "hive.hivecommons.dev"
const legacyHubHost = "hive.kubestellar.io"

func readJustfile(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", "..", "Justfile")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

// justfileCodeLines returns the Justfile with comment lines blanked (not
// dropped, so an index is still a file line number), so a comment that
// explains why the legacy host is gone cannot read as a use.
func justfileCodeLines(src string) []string {
	code := strings.Split(src, "\n")
	for i, line := range code {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			code[i] = ""
		}
	}
	return code
}

func TestContributeJustfileDefaultsToTheCanonicalHub(t *testing.T) {
	src := readJustfile(t)
	want := `hive_hub := env("HIVE_HUB", "wss://` + canonicalHubHost + `/contribute")`
	if !strings.Contains(src, want) {
		t.Fatalf("Justfile hive_hub default is not the canonical hub; want %s", want)
	}
}

// Every hub API the contributor recipes call and every hosted-spoke hostname
// they compose must use the canonical host. The legacy host may appear in
// code only as the legacy_hive_hub sentinel that recognises a stale export.
func TestContributeJustfileReachesOnlyTheCanonicalHubHost(t *testing.T) {
	code := justfileCodeLines(readJustfile(t))
	sentinel := `legacy_hive_hub := "wss://` + legacyHubHost + `/contribute"`
	sawSentinel := false
	for i, line := range code {
		if !strings.Contains(line, legacyHubHost) {
			continue
		}
		if strings.TrimSpace(line) == sentinel {
			sawSentinel = true
			continue
		}
		t.Errorf("Justfile line %d still reaches the legacy hub host, which answers 301 to everything: %s", i+1, strings.TrimSpace(line))
	}
	if !sawSentinel {
		t.Errorf("Justfile no longer defines %s, so a contributor with the old default exported gets a dead connection instead of the hive lookup", sentinel)
	}

	joined := strings.Join(code, "\n")
	for _, want := range []string{
		`"https://` + canonicalHubHost + `/api/saas/my-hives"`,
		`"https://` + canonicalHubHost + `/api/registry"`,
		`wss://${SELECTED}.` + canonicalHubHost + `/contribute`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("Justfile contribute-setup no longer uses %s", want)
		}
	}
	// The "not set" test must accept both the current default and the legacy
	// one, or a stale export of the old value skips the lookup and dials a
	// host that redirects.
	if !regexp.MustCompile(`\{\{hive_hub\}\}" == "wss://` + regexp.QuoteMeta(canonicalHubHost) + `/contribute" \|\| "\{\{hive_hub\}\}" == "\{\{legacy_hive_hub\}\}"`).MatchString(joined) {
		t.Error("contribute-setup's HIVE_HUB-not-set check does not accept both the canonical default and the legacy value")
	}
}

func TestStaticHubEnabledTooltipNamesTheCanonicalHub(t *testing.T) {
	body, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	if !strings.Contains(html, "Register this hive with the central hub at "+canonicalHubHost) {
		t.Fatal("the Hub Enabled tooltip no longer names the canonical hub")
	}
	if strings.Contains(html, "central hub at "+legacyHubHost) {
		t.Fatal("the Hub Enabled tooltip still advertises the legacy hub host")
	}
}
