package dashboard

import "testing"

// Multi-session-per-account (one GitHub account, several concurrent relays —
// e.g. one per CLI backend). identityOf() is the key for task leases,
// assignment cooldowns, failure streaks and ownership fences, so two sessions
// under one account MUST resolve to distinct identities or they collide on a
// single active-task slot. A contributor that declares no session keeps the
// historical bare-ContributorID identity (backward compatibility).

func TestIdentityOfSessionScoping(t *testing.T) {
	profile := &ContributorProfile{GitHubUsername: "hanthor", ContributorID: "c-abc123", TrustTier: "contributor"}

	bare := &ContributorConnection{profile: profile}
	if got := identityOf(bare); got != "c-abc123" {
		t.Fatalf("no session: identityOf = %q, want the bare ContributorID %q", got, "c-abc123")
	}

	claude := &ContributorConnection{profile: profile, session: "claude"}
	agy := &ContributorConnection{profile: profile, session: "agy"}
	if identityOf(claude) == identityOf(agy) {
		t.Fatalf("two sessions under one account share an identity (%q) — they will collide on one task slot", identityOf(claude))
	}
	if got, want := identityOf(claude), "c-abc123#claude"; got != want {
		t.Fatalf("session identity = %q, want %q", got, want)
	}
	if identityOf(bare) == identityOf(claude) {
		t.Fatalf("a sessioned relay collides with the bare identity; existing single-session contributors would be displaced")
	}
}

func TestIdentityOfFallsBackToUsername(t *testing.T) {
	// No ContributorID (a profile created before IDs, or username-only): the
	// session still scopes off the username so the fallback path is not a
	// single-slot regression.
	profile := &ContributorProfile{GitHubUsername: "hanthor"}
	sessioned := &ContributorConnection{profile: profile, session: "pi"}
	if got, want := identityOf(sessioned), "hanthor#pi"; got != want {
		t.Fatalf("username fallback with session = %q, want %q", got, want)
	}
	bare := &ContributorConnection{profile: profile}
	if got, want := identityOf(bare), "hanthor"; got != want {
		t.Fatalf("username fallback no session = %q, want %q", got, want)
	}
}

func TestSanitizeSessionLabel(t *testing.T) {
	cases := map[string]string{
		"":                              "",
		"claude":                        "claude",
		"pi-codex":                      "pi-codex",
		"Agy_1.2":                       "Agy_1.2",
		"bad/label":                     "badlabel",       // path chars stripped
		"a b\tc":                        "abc",            // whitespace stripped
		"../../etc":                     "....etc",        // no traversal survives as separators
		"drop#tables":                   "droptables",     // '#' (our separator) stripped
		"0123456789012345678901234567890123456789": "01234567890123456789012345678901", // capped at 32
	}
	for in, want := range cases {
		if got := sanitizeSessionLabel(in); got != want {
			t.Errorf("sanitizeSessionLabel(%q) = %q, want %q", in, got, want)
		}
	}
	// The cap must never emit more than 32 bytes.
	if got := sanitizeSessionLabel("xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"); len(got) > 32 {
		t.Errorf("sanitizeSessionLabel over-long output len=%d, want <=32", len(got))
	}
}
