package dashboard

import (
	"strings"
	"testing"
)

// agyLoginPane is a real antigravity (`agy`) login screen, captured verbatim
// from a hive agent's pane on 2026-09-01 at a 211-column terminal — the width a
// browser terminal actually runs at. The URL is 704 characters and agy wrapped
// it ITSELF, so tmux never flagged the breaks and `capture-pane -J` leaves them
// exactly as they are here.
const agyLoginPane = ` Your browser should open automatically. If not:
 https://accounts.google.com/o/oauth2/auth?access_type=offline&client_id=1071006060591-x.apps.googleusercontent.com&code_challenge=VCsnbBttr5DazUcZIn2Yo7kcvuNDV-ZfJJrlOvvXiK0&code
 _challenge_method=S256&prompt=consent&redirect_uri=https%3A%2F%2Fantigravity.google%2Foauth-callback&response_type=code&scope=https%3A%2F%2Fwww.googleapis.com%2Fauth%2Fcloud-platform+https%3A%2F%2Fwww.googleap
 is.com%2Fauth%2Fuserinfo.email+https%3A%2F%2Fwww.googleapis.com%2Fauth%2Fexperimentsandconfigs+https%3
 A%2F%2Fwww.googleapis.com%2Fauth%2Faicode+openid&state=cuog9M96pk-QlnAiRsq_jA
 If you aren't automatically redirected, paste the authorization code below:
 authorization code...
`

// TestTerminalURLsRejoinsCLIHardWrappedURL is the defect in one assertion: the
// endpoint promised a whole login URL and delivered the first line of one.
func TestTerminalURLsRejoinsCLIHardWrappedURL(t *testing.T) {
	urls := prepareTerminalURLs(agyLoginPane)
	if len(urls) == 0 {
		t.Fatal("no URL extracted from a pane that plainly contains one")
	}

	got := urls[0]
	for _, want := range []string{"state=cuog9M96pk-QlnAiRsq_jA", "code_challenge_method=S256", "redirect_uri="} {
		if !strings.Contains(got, want) {
			t.Fatalf("rejoined URL is missing %q — it was truncated at a wrap point.\ngot (%d chars): %s", want, len(got), got)
		}
	}
	if strings.ContainsAny(got, " \n") {
		t.Fatalf("rejoined URL contains whitespace, which invalidates the OAuth exchange: %q", got)
	}
	if !isAuthURL(got) {
		t.Fatalf("rejoined URL no longer classifies as a sign-in link: %s", got)
	}
}

// A URL that was never wrapped must come through untouched.
func TestRejoinLeavesUnwrappedURLsAlone(t *testing.T) {
	pane := "see https://example.com/a?b=c for details\nnext line\n"
	if got := rejoinHardWrappedURLs(pane); got != pane {
		t.Fatalf("rewrote a pane with no wrapped URL:\n got: %q\nwant: %q", got, pane)
	}
}

// Prose after a URL must never be swallowed: a line with spaces is not a
// continuation, and joining it would corrupt the link.
func TestRejoinStopsAtProse(t *testing.T) {
	pane := "https://example.com/auth?a=1\nthis line has spaces\n"
	got := rejoinHardWrappedURLs(pane)
	if strings.Contains(got, "auth?a=1this") {
		t.Fatalf("joined a prose line onto a URL: %q", got)
	}
}

// The join must never bridge a redacted segment. redactTokens is entitled to
// cut a credential out of the middle of a line; reassembling across that cut
// would hand the operator bytes the redactor removed.
func TestRejoinNeverBridgesRedaction(t *testing.T) {
	pane := "https://example.com/authorize?token=\n***REDACTED***\nmore\n"
	got := rejoinHardWrappedURLs(pane)
	if strings.Contains(got, "token=***REDACTED***") {
		t.Fatalf("join bridged a redacted line: %q", got)
	}
}

// A second URL on the following line is a new URL, not a continuation.
func TestRejoinStopsAtANewURL(t *testing.T) {
	pane := "https://example.com/authorize?a=1\nhttps://example.com/other\n"
	got := rejoinHardWrappedURLs(pane)
	if strings.Contains(got, "a=1https://") {
		t.Fatalf("joined two distinct URLs: %q", got)
	}
}
