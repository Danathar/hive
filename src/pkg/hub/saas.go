package hub

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

var saasUsersDir = "/data/saas/users"

// defaultHubAdminUsername is the compile-time fallback fleet-superuser
// GitHub login, used when HIVE_HUB_ADMIN_USERNAME is not set in the
// environment. Keeping the historical value as the default preserves
// backward compatibility for existing deployments.
const defaultHubAdminUsername = "clubanderson"

// hubAdminUsername is the GitHub login treated as the fleet superuser. It is
// resolved once at package init from HIVE_HUB_ADMIN_USERNAME (falling back to
// defaultHubAdminUsername), rather than being a hardcoded constant (audit F12,
// CWE-798). This lets a deployment override the admin login via config and
// removes the fragility where a GitHub username rename or release would silently
// transfer — or forfeit — fleet-superuser privilege. It is set once at startup
// and treated as read-only thereafter (never mutated at runtime).
var hubAdminUsername = resolveHubAdminUsername()

// resolveHubAdminUsername reads the admin login from the environment, trimming
// surrounding whitespace, and falls back to defaultHubAdminUsername when the
// env var is unset or blank.
func resolveHubAdminUsername() string {
	if v := strings.TrimSpace(os.Getenv("HIVE_HUB_ADMIN_USERNAME")); v != "" {
		return v
	}
	return defaultHubAdminUsername
}

// hubAdminsEnv is the env var (comma-separated canonical ids, e.g.
// "github:clubanderson,google:1078...") that overrides the admin SET for
// multi-provider login. A bare login in the list is accepted and treated as
// github: via the identity shim. When unset, the admin set is the single
// hubAdminUsername above (itself overridable via HIVE_HUB_ADMIN_USERNAME), so
// both existing env contracts keep working. Prefer isHubAdmin()/
// primaryHubAdmin() over comparing against hubAdminUsername directly, so
// multi-provider admins work and so a same-subject identity on a DIFFERENT
// provider can never inherit admin.
const hubAdminsEnv = "HIVE_HUB_ADMINS"

// hubAdminSet returns the canonicalized set of admin identities. Sourced from
// HIVE_HUB_ADMINS when set, else the single hubAdminUsername. Every entry is
// run through canonicalizeLegacy so a bare login becomes github:<login>; this
// is what stops a Google/IBMid user whose subject happens to be the admin's
// login from matching the GitHub admin.
func hubAdminSet() map[string]bool {
	raw := strings.TrimSpace(os.Getenv(hubAdminsEnv))
	entries := []string{hubAdminUsername}
	if raw != "" {
		entries = splitCSV(raw)
	}
	set := make(map[string]bool, len(entries))
	for _, e := range entries {
		if c := canonicalizeLegacy(e); c != "" {
			set[strings.ToLower(c)] = true
		}
	}
	return set
}

// isHubAdmin reports whether the given identity (bare-legacy or canonical) is a
// hub admin. Both the input and the configured admin ids are canonicalized, so
// "clubanderson", "github:clubanderson", and "GitHub:ClubAnderson" all match the
// default admin, while "google:clubanderson" does NOT.
func isHubAdmin(id string) bool {
	if id == "" {
		return false
	}
	return hubAdminSet()[strings.ToLower(canonicalizeLegacy(id))]
}

// primaryHubAdmin returns the canonical identity of the primary hub admin — the
// first entry of HIVE_HUB_ADMINS, else the resolved hubAdminUsername. Used where
// the code needs a concrete admin identity to WRITE (e.g. audit attribution).
func primaryHubAdmin() string {
	raw := strings.TrimSpace(os.Getenv(hubAdminsEnv))
	if raw != "" {
		if list := splitCSV(raw); len(list) > 0 {
			return canonicalizeLegacy(list[0])
		}
	}
	return canonicalizeLegacy(hubAdminUsername)
}

// userCanonicalID returns a user's canonical wire-form identity: the explicit
// CanonicalID when present, else the legacy-shimmed GitHubUsername (a bare login
// becomes github:<login>). This is the single source of truth for "who is this
// record" across the dual-read storage path and the provider badge.
func userCanonicalID(u *SaaSUser) string {
	if u == nil {
		return ""
	}
	if u.CanonicalID != "" {
		return canonicalizeLegacy(u.CanonicalID)
	}
	return canonicalizeLegacy(u.GitHubUsername)
}

// userProvider returns a user's login provider ("github"/"google"/"ibmid"/
// "redhat"/"microsoft"/"custom"), from the stored Provider field when set, else
// parsed from the canonical identity. Legacy records with neither resolve to
// "github" via the shim. Drives the admin Users-table auth-method badge.
func userProvider(u *SaaSUser) string {
	if u == nil {
		return ""
	}
	if u.Provider != "" {
		return strings.ToLower(u.Provider)
	}
	if p, _, ok := parseCanonical(userCanonicalID(u)); ok {
		return p
	}
	return legacyProvider
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func repoTargetForgeHost(baseURL string) string {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return "github.com"
	}
	if u, err := url.Parse(baseURL); err == nil && u.Host != "" {
		return strings.ToLower(u.Host)
	}
	return strings.ToLower(strings.Trim(strings.TrimPrefix(strings.TrimPrefix(baseURL, "https://"), "http://"), "/"))
}

// hubUpgradeDebounce is the minimum gap between hub self-upgrade rollout
// restarts. The behind-latest check runs every SHA-poll cycle, so without this
// the hub could re-trigger a restart before the previous rollout's new pod
// reports the new hash. One cycle plus rollout headroom.
const hubUpgradeDebounce = 4 * time.Minute

// upgradeKubectlTimeout bounds a single auto-upgrade kubectl call. kubectl's own
// default retries an unreachable API server for ~2 minutes before giving up;
// paying that per hive serialized the upgrade loop and starved the hub's own
// upgrade check that runs after it. The heartbeat fallback is the real delivery
// path for unreachable clusters, so failing fast costs nothing.
//
// Retained under nolint despite having no current caller: three other files
// cite it BY NAME as the basis for their own timeouts
// (hosted_namespace_identity.go, netadmin_reconcile.go, saas_bulk.go). Deleting
// it to satisfy the linter would orphan those comments and lose the recorded
// reasoning for the 15s figure, which is the thing worth keeping.
//
//nolint:unused // referenced by name from three sibling timeout comments
const upgradeKubectlTimeout = 15 * time.Second

// clusterUnreachableTTL is how long the hub skips kubectl for a cluster after a
// dial failure. Long enough that one poll cycle probes a firewalled cluster at
// most once, short enough that a cluster coming back online is picked up soon.
const clusterUnreachableTTL = 10 * time.Minute

// beginUpgrade marks the registry entry at index i as upgrading toward target
// and stamps UpgradeStartedAt so the dashboard can show a TRUE elapsed time.
//
// The invariant it enforces: UpgradeStartedAt is the moment an upgrade toward a
// given target FIRST began, and it survives every retry of that same upgrade. A
// hive that is already Upgrading toward the SAME target keeps its original start
// time — it is retrying, not starting over. Only a genuinely new upgrade (the
// hive was not upgrading, or the target actually changed) re-stamps the clock.
//
// This is the fix for the reset-every-retry bug: a crash-looping self-upgrade
// re-enters the arm/retry paths every heartbeat cycle with the SAME target, and
// each re-stamp of UpgradeStartedAt to time.Now() reset the displayed
// "Upgrading Ns" back toward zero. The elapsed time therefore never crossed
// staleUpgradeTimeout, so the row never turned red and the stuck-upgrade alert
// never fired even while the hive thrashed for hours. Routing every site that
// sets Upgrading=true through this one helper makes the invariant impossible to
// violate in one place and not another.
//
// The caller MUST hold s.mu. i must be a valid index into s.registry.Hives.
func (s *HubServer) beginUpgrade(i int, target string) {
	h := &s.registry.Hives[i]
	// (Re)stamp the start ONLY on a genuinely new upgrade: the hive was not
	// already upgrading, or it is now aimed at a different target. A retry
	// toward the same target keeps the original start so the timer is honest.
	if !h.Upgrading || h.UpgradeTarget != target || h.UpgradeStartedAt.IsZero() {
		h.UpgradeStartedAt = time.Now()
	}
	h.Upgrading = true
	h.UpgradeTarget = target
}

// clearUpgradeLatch drops every trace of an in-flight upgrade on the entry at
// index i: the Upgrading flag, its target, AND the start clock. Zeroing
// UpgradeStartedAt on completion/cancel/orphan-clear guarantees the NEXT upgrade
// starts a fresh timer even if some future path forgot to re-stamp — the
// stuck-upgrade signal is only as honest as the clock it reads. Callers that
// clear Upgrading MUST route through this so the invariant (a non-zero
// UpgradeStartedAt implies an upgrade is genuinely in flight) holds everywhere.
//
// The caller MUST hold s.mu. i must be a valid index into s.registry.Hives.
func (s *HubServer) clearUpgradeLatch(i int) {
	h := &s.registry.Hives[i]
	h.Upgrading = false
	h.UpgradeTarget = ""
	h.UpgradeStartedAt = time.Time{}
}

// stampObservedUpgrade extends the beginUpgrade invariant to upgrades the hub
// merely OBSERVES rather than arms: whenever an entry ends up Upgrading=true
// from ANY source, it must carry a non-zero UpgradeStartedAt — the dashboard
// row only renders the elapsed counter, and the stuck-upgrade alert only
// fires, when the clock is non-zero. The spoke-reported path (a heartbeat
// whose payload says Upgrading with no hub-side target) rebuilt the registry
// entry from scratch every beat with a zero clock, so the badge rendered
// without its counter and the alert was blind for the whole upgrade.
//
// prevStart is the previous beat's clock for the same entry (zero when there
// is no previous state, e.g. a first heartbeat). Carrying it forward — never
// re-stamping a live clock — is what keeps this compatible with the #2725
// rule that a retry of the same upgrade preserves its original start time.
// Entries that are not Upgrading, or already carry a clock (the hub-armed
// paths route through beginUpgrade), are left untouched.
func stampObservedUpgrade(entry *RegistryEntry, prevStart time.Time) {
	if !entry.Upgrading || !entry.UpgradeStartedAt.IsZero() {
		return
	}
	if !prevStart.IsZero() {
		entry.UpgradeStartedAt = prevStart
		return
	}
	entry.UpgradeStartedAt = time.Now()
}

type SaaSUser struct {
	GitHubUsername string            `json:"github_username"`
	CreatedAt      string            `json:"created_at"`
	LastLogin      string            `json:"last_login"`
	Hives          map[string]string `json:"hives"`
	// HiveExpiry optionally bounds a grant in Hives: hive ID → RFC3339 UTC
	// instant after which that grant is revoked (#4150). Grants without an
	// entry are permanent — omitempty keeps every pre-expiry record
	// byte-identical on disk. Enforced at read time by loadSaaSUser's prune
	// and persisted/audited by sweepExpiredAccess (access_expiry.go).
	HiveExpiry     map[string]string `json:"hive_expiry,omitempty"`
	SaaSQuota      int               `json:"saas_quota"`
	Blocked        bool              `json:"blocked"`
	EncryptedToken string            `json:"encrypted_token,omitempty"`

	// Multi-provider identity (phase 1d). All omitempty so the thousands of
	// existing GitHub-only records on the PVC round-trip byte-identical until a
	// user first logs in / links after this ships.
	//
	//   CanonicalID  the wire-form primary identity ("google:1078", "github:foo").
	//                Empty on a legacy record → the shim treats GitHubUsername as
	//                the (github:) primary. saveSaaSUser/loadSaaSUser already dual-
	//                read on GitHubUsername; CanonicalID is the explicit form used
	//                by the badge and by Phase 2's OIDC callback when it creates a
	//                non-GitHub user.
	//   Provider     "github" | "google" | "ibmid" | "redhat" | "microsoft" |
	//                "custom" — drives the admin Users-table auth-method badge.
	//                Derivable from CanonicalID but stored so the badge needs no
	//                parse per render.
	//   AvatarURL    stored avatar (Google/IBMid give a picture claim); replaces
	//                the derived github.com/<login>.png where present.
	//   Email        the provider email claim (display only; NEVER the key — subs
	//                are stable, emails are reassignable).
	//   LinkedGitHubLogin  an OPTIONAL attached GitHub identity for a non-GitHub
	//                primary who needs user-scoped GitHub calls (contributor
	//                reissue). Never required to own a hive — the App does the
	//                GitHub work.
	CanonicalID       string `json:"canonical_id,omitempty"`
	Provider          string `json:"provider,omitempty"`
	AvatarURL         string `json:"avatar_url,omitempty"`
	Email             string `json:"email,omitempty"`
	LinkedGitHubLogin string `json:"linked_github_login,omitempty"`

	// DisplayName is the PROVIDER-ASSERTED human name from the OIDC name claim
	// (or userinfo), refreshed on every completed login. Distinct from FullName,
	// which is ADMIN-entered CRM text and must never be clobbered by a login.
	// Display only — the identity key stays provider:sub. omitempty so existing
	// records round-trip byte-identical until the user's next login enriches
	// them (backfill-by-login, no migration).
	DisplayName string `json:"display_name,omitempty"`

	// Contact/CRM fields. Admin-maintained free text used to reach a hub user
	// outside GitHub (and to remember what was said last time). All three are
	// omitempty so the thousands of existing user records already on the PVC
	// stay byte-identical until an admin actually fills one in — a record
	// without them round-trips through load/save unchanged.
	//
	// These are operator-entered free text rendered into the dashboard, so
	// every render path must escape them and every write path must cap them
	// (see maxContactNameLen / maxContactSlackIDLen / maxContactNotesLen).
	FullName string `json:"full_name,omitempty"`
	SlackID  string `json:"slack_id,omitempty"`
	Notes    string `json:"notes,omitempty"`
	// Company is ADMIN-entered CRM free text — the user's company/organization.
	// Like FullName/SlackID/Notes it is operator-maintained (never asserted by a
	// login) and is deliberately NOT collected in the hive request/provision
	// form; the operator fills it in manually from the admin Users table. Same
	// escaping + length-cap discipline as the other contact fields
	// (maxContactCompanyLen); omitempty so existing records round-trip
	// byte-identical until an admin sets it.
	Company string `json:"company,omitempty"`

	// Country is an OPTIONAL ISO 3166-1 alpha-2 code (uppercase, e.g. "GB"),
	// rendered as a small flag beside the user's avatar. Two sources, in
	// priority order: the explicit dropdown in the get-started wizard (copied
	// here on approval, like FullName/SlackID above), else a best-effort
	// inference from the Accept-Language region subtag at login, which only
	// ever fills an EMPTY value. See user_country.go for the full rationale and
	// the privacy posture.
	//
	// Stored as the code, never as the glyph: the flag is derived at render
	// time from regional-indicator code points, so no external image host is
	// involved and an unknown country renders nothing at all.
	//
	// omitempty so the thousands of existing records on the PVC round-trip
	// byte-identical until a user actually picks a country or logs in from a
	// browser that states a region.
	Country string `json:"country,omitempty"`

	// CountrySetByUser records that the country above was chosen DELIBERATELY
	// by the user rather than inferred, and it is what makes an explicit CLEAR
	// stick.
	//
	// Without it, "explicit" is inferred from `Country != ""` (see
	// applyInferredCountry), which is fine for a pick but wrong for a clear: a
	// user who removes their country via the self-service endpoint leaves an
	// empty field, and the very next login's Accept-Language inference would
	// silently put a flag back. "Prefer not to say" would become impossible to
	// express — and impossible to notice failing, since the flag reappears a
	// login later, far from the action that was supposed to remove it.
	//
	// Set only by the user's own writes: the self-service endpoint
	// (handleMyCountry) and the wizard pick copied on approval
	// (applyRequestContactToUser). NEVER set by the login-path inference, which
	// is precisely the distinction this field exists to draw.
	//
	// omitempty bool so every record that has not been through a deliberate
	// pick — which today is all of them — round-trips byte-identical.
	//
	// STILL WRITTEN, not deprecated: CountrySource below is the finer-grained
	// successor, but this boolean is what other readers and every record
	// already on the PVC speak, so every user-chosen write keeps setting it.
	CountrySetByUser bool `json:"country_set_by_user,omitempty"`

	// CountrySource is the PROVENANCE of the country above — who put it there.
	// One of countrySourceInferred / countrySourceAdmin / countrySourceUser, or
	// "" for a record nothing has ever touched.
	//
	// A boolean stopped being enough the moment an ADMIN could assign a country
	// on someone else's behalf, because that is a third kind of claim and it
	// sits BETWEEN the two the boolean can express:
	//
	//   - It is not user-chosen. Stamping CountrySetByUser for an admin edit
	//     would assert the user made a statement about themselves that they
	//     never made, and — since that marker is also what suppresses ever
	//     asking again — would permanently silence the question for them.
	//   - But it must still outrank Accept-Language inference. An admin's
	//     best-effort attribution is a human looking at evidence; the header is
	//     a language preference. Letting the next login overwrite it would
	//     re-introduce, in a new form, exactly the silent-clobber bug #4374 was
	//     opened to fix.
	//
	// Precedence, strongest first: user > admin > inferred > unset. See
	// countryProvenanceRank and mayOverwriteCountry in user_country.go, which
	// are the single arbiters — no caller compares these strings by hand.
	//
	// BACKWARD COMPATIBILITY. Records written before this field exists carry
	// only CountrySetByUser, so an ABSENT source is read through that boolean:
	// CountrySetByUser=true with no source means user-chosen (see
	// effectiveCountrySource). That is why this is omitempty and why nothing
	// backfills it — an untouched record must still serialize byte-identically.
	CountrySource string `json:"country_source,omitempty"`

	// Engagement stats, admin-only (they ride /api/saas/admin/users, which is
	// requireAdmin). Both omitempty ints so existing records round-trip
	// byte-identical until the user first logs in / opens a hive after this ships.
	//
	// LoginCount is the number of completed hub OAuth logins. Incremented in
	// exactly one place — handleOAuthCallback — never in ensureSaaSUser, whose
	// other callers (my-hives poll, admin provisioning) would inflate it.
	LoginCount int `json:"login_count,omitempty"`
	// SessionSeconds is the cumulative time this user has had at least one live
	// session on a hive dashboard, accumulated by the hub from the spoke's
	// per-heartbeat active-session report (see handleHeartbeat). It is a sampled
	// sum of inter-beat intervals, so it is accurate to roughly the beat interval,
	// not to the second.
	SessionSeconds int64 `json:"session_seconds,omitempty"`
	// EngagedSeconds is the honest subset of SessionSeconds: time accumulated
	// only on beats where the user's browser reported ENGAGED presence — tab
	// visible AND input within the idle window (heartbeat EngagedSessionUsers).
	// An idle open tab grows SessionSeconds but never this. omitempty, and
	// absent on records that predate the field or whose spokes don't report
	// presence yet — absence means NO DATA, never "provably unengaged".
	EngagedSeconds int64 `json:"engaged_seconds,omitempty"`
	// LastEngagedAt is the RFC3339 time of the most recent beat that credited
	// EngagedSeconds — when a human was last actually behind this user's
	// session. Feeds the `active` status tier. Same absence semantics as
	// EngagedSeconds.
	LastEngagedAt string `json:"last_engaged_at,omitempty"`
	// LastActionAt is the RFC3339 time of the user's most recent REAL audited
	// action on any of their hives (config save, agent restart, ACMM change,
	// login, …), folded hub-ward from the spoke audit logs (heartbeat
	// UserLastActions) keeping the per-user maximum. Same absence semantics.
	LastActionAt string `json:"last_action_at,omitempty"`
}

// Length caps for the admin-editable contact fields. These are free text
// written straight to the PVC, so each is bounded independently rather than
// relying on the request-body cap alone: name and Slack ID are identifiers and
// stay short, while notes is the running CRM log for a user and gets the most
// room. Values over the cap are truncated (not rejected) so a long paste still
// saves something useful instead of silently failing.
const (
	// maxContactNameLen bounds a person's full name. Generous versus real
	// names so non-Latin scripts and long multi-part names still fit.
	maxContactNameLen = 128
	// maxContactSlackIDLen bounds a Slack member ID or handle. Real Slack IDs
	// are ~11 chars (U01ABCDEF23); the headroom allows an @handle or a
	// workspace-qualified form.
	maxContactSlackIDLen = 64
	// maxContactCompanyLen bounds the company/organization name — an identifier
	// like the name/Slack fields, sized generously for long legal entity names.
	maxContactCompanyLen = 128
	// maxContactNotesLen bounds the free-text notes field — the longest of the
	// three, sized for a few paragraphs of admin scratch notes per user.
	maxContactNotesLen = 8192
	// maxUpdateUserBodyBytes caps the PUT body for the admin user-update
	// endpoint. Comfortably above the sum of the field caps plus JSON
	// overhead/escaping, and small enough that the endpoint can never be used
	// to push a large blob at the PVC.
	maxUpdateUserBodyBytes = 64 * 1024
)

// truncateRunes clips s to at most max runes. It counts runes rather than
// bytes so a cap never splits a multi-byte character (which would write
// invalid UTF-8 into the user record and then into the dashboard HTML).
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

var hmacKeyPath = "/data/saas/hmac.key"

const hmacKeySize = 32

func loadOrCreateHMACKey() ([]byte, error) {
	// Best-effort: a failed mkdir surfaces via the WriteFile error below.
	_ = os.MkdirAll(filepath.Dir(hmacKeyPath), 0o755)
	if data, err := os.ReadFile(hmacKeyPath); err == nil && len(data) == hmacKeySize {
		return data, nil
	}
	key := make([]byte, hmacKeySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, err
	}
	if err := os.WriteFile(hmacKeyPath, key, 0o600); err != nil {
		return nil, fmt.Errorf("write HMAC key: %w", err)
	}
	return key, nil
}

func encryptToken(plaintext string) (string, error) {
	key, err := loadOrCreateHMACKey()
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

func decryptToken(encoded string) (string, error) {
	key, err := loadOrCreateHMACKey()
	if err != nil {
		return "", err
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonceSize := gcm.NonceSize()
	if len(data) < nonceSize {
		return "", fmt.Errorf("ciphertext too short")
	}
	plaintext, err := gcm.Open(nil, data[:nonceSize], data[nonceSize:], nil)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

func (s *HubServer) registerSaaSRoutes() {
	s.mux.HandleFunc("GET /dashboard", s.handleDashboard)
	s.mux.HandleFunc("GET /access-denied", s.handleAccessDenied)
	s.mux.HandleFunc("GET /api/saas/my-hives", s.requireAuth(s.handleMyHives))
	// Per-release image-pulls series, headline (active release line) plus
	// per-line (external adoption gauge). requireAuth,
	// not requireAdmin: any signed-in hub user sees the same public-adoption
	// number, and the underlying data is scraped from the PUBLIC package page.
	s.mux.HandleFunc("GET /api/hub/image-pulls", s.requireAuth(s.handleImagePulls))
	// Token attribution rollups. requireAuth (not requireAdmin) because a
	// non-admin legitimately sees their OWN hives' usage; the handler scopes
	// fleet-wide data to admins itself.
	s.mux.HandleFunc("GET /api/saas/usage", s.requireAuth(s.handleUsage))
	// Self-service country: the ONE field a non-admin may write on their own
	// user record. requireAuth, not requireAdmin — that is the entire point,
	// since every other SaaSUser write is admin-gated and the wizard is a
	// one-time surface. The handler resolves the acting user from the SESSION
	// and the body carries no identity, so this cannot reach anyone else's
	// record. See handleMyCountry in user_country.go.
	//
	// PUT with a JSON body rather than a code in the path: country is personal
	// data and a URL would put it in access logs, Referer headers and history.
	s.mux.HandleFunc("GET /api/saas/me/country", s.requireAuth(s.handleMyCountry))
	s.mux.HandleFunc("PUT /api/saas/me/country", s.requireAuth(s.handleMyCountry))
	s.mux.HandleFunc("POST /api/saas/lite/enroll", s.requireAuth(s.handleLiteEnroll))
	s.mux.HandleFunc("POST /api/saas/hives", s.requireAuth(s.handleCreateHive))
	s.mux.HandleFunc("GET /api/saas/hives/{id}/status", s.requireAuth(s.handleHiveStatus))
	// /open is a browser NAVIGATION endpoint (the SSO handoff), not an API call.
	// It is registered WITHOUT requireAuth so an unauthenticated visit redirects
	// to the hub login (and back) instead of dumping a raw {"error":...} JSON.
	// handleOpenHive does its own auth check + login redirect.
	s.mux.HandleFunc("GET /api/saas/hives/{id}/open", s.handleOpenHive)
	s.mux.HandleFunc("DELETE /api/saas/hives/{id}", s.requireAuth(s.handleDeleteHive))
	s.mux.HandleFunc("POST /api/saas/hives/{id}/upgrade", s.requireAuthOrSpokeSelfService(s.handleUpgradeHive))
	s.mux.HandleFunc("POST /api/saas/hives/{id}/switch-branch", s.requireAuthOrSpokeSelfService(s.handleSwitchBranch))
	// Digest pin: rollback as a first-class run-state (#6290). Owner-only inside
	// the handlers, like switch-branch; the re-arm and every upgrade path
	// honour the pin until unpin lifts it.
	s.mux.HandleFunc("POST /api/saas/hives/{id}/pin-digest", s.requireAuth(s.handlePinDigest))
	s.mux.HandleFunc("POST /api/saas/hives/{id}/unpin-digest", s.requireAuth(s.handleUnpinDigest))
	s.mux.HandleFunc("GET /api/saas/hives/{id}/digest-pin", s.requireAuth(s.handleGetDigestPin))
	s.mux.HandleFunc("PUT /api/saas/hives/{id}/visibility", s.requireAuth(s.handleToggleVisibility))
	s.mux.HandleFunc("PUT /api/saas/hives/{id}/auto-upgrade", s.requireAuth(s.handleToggleAutoUpgrade))
	// Rename a hive's display name (its ProjectName). requireAuth plus an inner
	// owner-or-admin check, exactly like visibility/auto-upgrade above — the
	// gate is the security boundary, not just the hidden UI affordance.
	s.mux.HandleFunc("PUT /api/saas/hives/{id}/name", s.requireAuth(s.handleRenameHive))
	s.mux.HandleFunc("POST /api/saas/hives/{id}/agents/{agent}/restarts/reset", s.requireAuth(s.handleResetAgentRestarts))
	// Move a hive between forges (github.com <-> a GitHub Enterprise host).
	// requireAuth plus an inner owner-or-admin check, exactly like
	// switch-branch and auto-upgrade above.
	s.mux.HandleFunc("POST /api/saas/hives/{id}/forge", s.requireAuth(s.handleSwitchForge))
	s.mux.HandleFunc("POST /api/saas/hives/{id}/reset-app", s.requireAuth(s.handleResetApp))
	// Assigns a hive its OPTIONAL second GitHub App (#4815). requireAuth is the
	// same outer gate reset-app uses; the handler itself re-checks isHubAdmin,
	// which is the authoritative check for both.
	s.mux.HandleFunc("PUT /api/saas/hives/{id}/secondary-app", s.requireAuth(s.handleSetHiveSecondaryApp))
	s.mux.HandleFunc("POST /api/saas/hives/{id}/restart-spoke", s.requireAuth(s.handleRestartSpoke))
	s.mux.HandleFunc("GET /api/saas/hive-config/{hiveID}", s.requireAuth(s.handleProxyHiveConfig))
	s.mux.HandleFunc("GET /api/saas/latest-sha", s.handleLatestSHA)
	s.mux.HandleFunc("POST /api/saas/hub/upgrade", s.requireAdmin(s.handleHubSelfUpgrade))
	s.mux.HandleFunc("PUT /api/saas/hub/auto-upgrade", s.requireAdmin(s.handleHubAutoUpgrade))
	// Admin upgrade kill switch (upgrade_pause.go): pause hub self-upgrades
	// and/or ALL automatic spoke image changes, fleet-wide.
	s.mux.HandleFunc("GET /api/saas/upgrade-pause", s.requireAdmin(s.handleGetUpgradePause))
	s.mux.HandleFunc("POST /api/saas/upgrade-pause", s.requireAdmin(s.handleSetUpgradePause))
	s.mux.HandleFunc("GET /api/saas/auth-check", s.handleSaaSAuthCheck)
	// Sibling-product identity bridge (#4171): dibs.kubestellar.io forwards the
	// browser's hive_hub_user cookie here server-to-server to resolve the
	// session. Registered GET-only via the method pattern, and WITHOUT
	// requireAuth so the unauthenticated answer is the exact 401 JSON shape the
	// dibs bridge expects rather than the generic middleware error.
	s.mux.HandleFunc("GET /api/saas/whoami", s.handleSaaSWhoami)
	// Sibling-product repo registry (#4193): dibs polls this server-to-server
	// (no session) every ~5 minutes to learn which repos hives manage. Public
	// by design, so it returns ONLY already-public data — see handleDibsRepos.
	s.mux.HandleFunc("GET /api/saas/dibs/repos", s.handleDibsRepos)
	s.mux.HandleFunc("POST /api/saas/user-token", s.requireAuth(s.handleUserToken))
	s.mux.HandleFunc("GET /api/saas/hives/{id}/access", s.requireAuth(s.handleAccessList))
	s.mux.HandleFunc("GET /api/saas/grantable-users", s.requireAuth(s.handleGrantableUsers))
	s.mux.HandleFunc("POST /api/saas/hives/{id}/access", s.requireAuth(s.handleAccessAdd))
	s.mux.HandleFunc("DELETE /api/saas/hives/{id}/access/{username}", s.requireAuth(s.handleAccessRemove))
	s.mux.HandleFunc("POST /api/saas/hives/{id}/request-access", s.requireAuth(s.handleRequestAccess))
	s.mux.HandleFunc("GET /api/saas/hives/{id}/requests", s.requireAuth(s.handleGetRequests))
	s.mux.HandleFunc("GET /api/saas/hives/{id}/timeline", s.requireAuth(s.handleHiveTimeline))
	s.mux.HandleFunc("GET /api/saas/hives/{id}/access-log", s.requireAuth(s.handleAccessLog))
	s.mux.HandleFunc("POST /api/saas/hives/{id}/requests/{username}/approve", s.requireAuth(s.handleApproveRequest))
	s.mux.HandleFunc("POST /api/saas/hives/{id}/requests/{username}/deny", s.requireAuth(s.handleDenyRequest))
	s.mux.HandleFunc("PUT /api/saas/hives/{id}/approve-access/{username}", s.requireAuth(s.handleApproveAccess))
	s.mux.HandleFunc("DELETE /api/saas/hives/{id}/deny-access/{username}", s.requireAuth(s.handleDenyAccess))
	s.mux.HandleFunc("GET /api/saas/access-status", s.handleAccessStatus)
	// GET /api/saas/repos is deliberately gone. Repo discovery ran on the
	// requester's github.com OAuth token, so it could only ever see public
	// GitHub — invisible to GitHub Enterprise, and structurally unable to list
	// a GitLab or Gitea repo when those forges arrive. The request flow now
	// takes a typed repository URL, which works for every forge without new
	// code, and that is what let the login drop to an empty OAuth scope.
	// Restoring this endpoint would also require restoring that scope.
	s.mux.HandleFunc("POST /api/saas/request-provision", s.requireAuth(s.handleRequestProvision))
	s.mux.HandleFunc("PUT /api/saas/approve-provision/{username}", s.requireAdmin(s.handleApproveProvision))
	s.mux.HandleFunc("DELETE /api/saas/deny-provision/{username}", s.requireAdmin(s.handleDenyProvision))
	s.mux.HandleFunc("GET /api/saas/admin/available-placeholders", s.requireAdmin(s.handleAvailablePlaceholders))
	s.mux.HandleFunc("GET /api/saas/admin/scale-settings", s.requireAdmin(s.handleGetScaleSettings))
	s.mux.HandleFunc("POST /api/saas/admin/scale-settings", s.requireAdmin(s.handleSetScaleSettings))
	s.mux.HandleFunc("GET /api/saas/admin/users", s.requireAdmin(s.handleAdminUsers))
	// Aggregate geographic rollup of the user base (counts only, no usernames).
	// Admin-gated like the rest of the CRM/Users surface: country is personal
	// data, so even the aggregate stays behind requireAdmin. Takes no query
	// parameters — no country ever appears in a URL. See user_country_rollup.go.
	s.mux.HandleFunc("GET /api/saas/admin/user-countries", s.requireAdmin(s.handleAdminUserCountries))
	// #3234: fleet readiness for removing the N1/N2 legacy compatibility lanes.
	s.mux.HandleFunc("GET /api/saas/admin/auth-rollout", s.requireAdmin(s.handleAuthRollout))
	// Master-secret rotation (src/docs/design/master-key-rotation.md). Both are
	// requireAdmin, which enforces isCSRFSafe BEFORE resolving identity — an
	// ambient hub session cookie would otherwise make a cross-site POST able to
	// rotate the fleet's master key. The rotate route is a POST for that reason
	// too: isCSRFSafe exempts safe methods.
	s.mux.HandleFunc("GET /api/saas/admin/key-generations", s.requireAdmin(s.handleKeyGenerations))
	s.mux.HandleFunc("POST /api/saas/admin/rotate-master-key", s.requireAdmin(s.handleRotateMasterKey))
	s.mux.HandleFunc("PUT /api/saas/admin/users/{username}", s.requireAdmin(s.handleAdminUpdateUser))
	s.mux.HandleFunc("DELETE /api/saas/admin/users/{username}", s.requireAdmin(s.handleAdminDeleteUser))
	// Admin read-only "View as user" impersonation. Enter is admin-only and
	// sets the short-lived signed hive_hub_impersonate cookie; exit clears it
	// and is exempt from the impersonation write-block (see impersonateExitPath)
	// so the admin can always get back out. Status folds into /api/auth/user for
	// the banner, but a dedicated read is offered too.
	s.mux.HandleFunc("POST /api/saas/admin/impersonate/exit", s.requireAdmin(s.handleImpersonateExit))
	s.mux.HandleFunc("POST /api/saas/admin/impersonate/{username}", s.requireAdmin(s.handleImpersonateStart))
	s.mux.HandleFunc("GET /api/saas/impersonation-status", s.requireAuth(s.handleImpersonationStatus))
	s.mux.HandleFunc("POST /api/saas/hives/{id}/assign", s.requireAuth(s.handleAssignHive))
	// Escape hatch: return an assigned-but-unclaimed placeholder to the available
	// pool so it can be re-armed. Admin-only, and guarded to the wedged middle
	// state (assigned && !claim_delivered) inside the handler.
	s.mux.HandleFunc("POST /api/saas/hives/{id}/reset-assignment", s.requireAdmin(s.handleResetAssignment))
	s.mux.HandleFunc("GET /api/saas/cluster-health", s.requireAdmin(s.handleClusterHealth))
	// PR reach telemetry (#3994): the read-only join of merged PRs against
	// the commits/components the fleet reports actually running. The payload
	// names hives fleet-wide — the same exposure class as cluster-health
	// directly above, so the same requireAdmin gate.
	s.mux.HandleFunc("GET /api/reach", s.requireAdmin(s.handleReach))
	// Advisory-staleness diagnostics (#4167): the read-only fleet view of WHICH
	// gate decided each hive's advisory verdict, and how many stale digests no
	// pill is reporting. Names hives fleet-wide with their App state, so the
	// same requireAdmin gate as cluster-health and /api/reach above.
	s.mux.HandleFunc("GET /api/saas/admin/advisory-diagnostics", s.requireAdmin(s.handleAdvisoryDiagnostics))
	// Acknowledging a fleet alert is an operator action on the operator's own
	// view, so it is admin-only (see alerts.go).
	s.mux.HandleFunc("POST /api/saas/admin/alert-ack", s.requireAdmin(s.handleAlertAck))
	s.mux.HandleFunc("GET /api/hub/clusters", s.requireAuth(s.handleListClusters))
	// Per-cluster GitHub App key store. The GET is fingerprints only (never key
	// material); the PUT is the single write-only entry point for a key.
	s.mux.HandleFunc("GET /api/saas/admin/cluster-app-keys", s.requireAdmin(s.handleGetClusterAppKeys))
	s.mux.HandleFunc("PUT /api/saas/admin/cluster-app-keys/{clusterID}", s.requireAdmin(s.handlePutClusterAppKey))
	s.mux.HandleFunc("POST /api/saas/admin/hub-banner", s.requireAdmin(s.handleSendHubBanner))
	s.mux.HandleFunc("DELETE /api/saas/admin/hub-banner", s.requireAdmin(s.handleClearHubBanner))
	s.mux.HandleFunc("GET /api/saas/admin/hub-banner", s.requireAdmin(s.handleGetHubBanner))
	s.registerBulkRoutes()
	// Slack messaging. The single-user and hive-owner routes are admin-or-owner
	// (checked inside each handler, like switch-branch); the BROADCAST is
	// admin-only, because it reaches every user with a slack_id and cannot be
	// recalled. It additionally requires a typed confirmation and offers a dry
	// run — see slack.go.
	s.mux.HandleFunc("POST /api/saas/slack/user/{username}", s.requireAuth(s.handleSlackMessageUser))
	s.mux.HandleFunc("POST /api/saas/hives/{id}/slack", s.requireAuth(s.handleSlackMessageHiveOwner))
	s.mux.HandleFunc("POST /api/saas/admin/slack/broadcast", s.requireAdmin(s.handleSlackBroadcast))
	s.mux.HandleFunc("POST /api/saas/admin/journey-snooze", s.requireAdmin(s.handleJourneySnooze))
	s.mux.HandleFunc("GET /api/saas/admin/journey-status", s.requireAdmin(s.handleJourneyStatus))
}

// clusterIDForSaaSHive returns the cluster ID for a SaaS hive, defaulting to
// the default cluster when the field is empty (backward compatibility).
func clusterIDForSaaSHive(sh SaaSHive) string {
	if sh.ClusterID != "" {
		return sh.ClusterID
	}
	return defaultClusterID
}

// ensureClusterIDForClaim stamps a non-blank cluster_id onto a hive that is
// about to be persisted by a claim/assign. This is the data-integrity guard
// for the observed bug where CLAIMED hives lost cluster_id in their meta.json
// and then fell back to defaultClusterID (the hub-reachable cluster) in clusterForHive — which
// mis-routed App/host resolution for hives that actually run on the heartbeat-only cluster.
//
// Precedence, most-trusted first:
//  1. The hive's OWN non-blank ClusterID (the placeholder already belongs to a
//     cluster — always the most authoritative source; never override it).
//  2. poolFallback — the pool the claim was drawn from, when the caller knows
//     it (e.g. handleApproveProvision picks a pool by auth_method), but only
//     when it names a cluster the hub actually has.
//  3. defaultClusterID — last resort, matching clusterForHive's own fallback.
//
// The result is always non-blank, so with omitempty on the json tag it still
// serializes to a concrete "cluster_id" value rather than vanishing.
func (s *HubServer) ensureClusterIDForClaim(h *SaaSHive, poolFallback string) {
	if h.ClusterID != "" {
		return
	}
	if poolFallback != "" {
		if _, ok := s.clusters[poolFallback]; ok {
			h.ClusterID = poolFallback
			return
		}
	}
	h.ClusterID = defaultClusterID
}

// clusterNameForID returns the human-readable name for a cluster ID.
// Returns empty string when the cluster is not found.
func (s *HubServer) clusterNameForID(clusterID string) string {
	if c, ok := s.clusters[clusterID]; ok {
		return c.Name
	}
	return ""
}

// ClusterListEntry is the JSON response for the clusters list endpoint.
type ClusterListEntry struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	HasGPU bool   `json:"has_gpu"`
	Arch   string `json:"arch"`
	// GitHubHost is the bare hostname of the GitHub instance hives on this
	// cluster default to ("github.com" for public GitHub). Shown in the
	// create-hive modal so an admin can see which GitHub a hive will target.
	GitHubHost string `json:"github_host,omitempty"`
	// AppInstallURL is the GitHub App install link for THIS cluster's GitHub
	// host and app slug. A GitHub Enterprise cluster must never be handed a
	// public github.com link: the install request would land on the wrong
	// GitHub and the GHE org admin would never see it.
	AppInstallURL string `json:"app_install_url,omitempty"`
}

// clusterGitHubConfig projects a cluster's GitHub settings onto the
// config.GitHubConfig that owns URL construction, so the hub and the spoke
// build install links from exactly one implementation.
func clusterGitHubConfig(c *ClusterConfig) config.GitHubConfig {
	if c == nil {
		return config.GitHubConfig{}
	}
	base := c.GitHubBaseURL
	// The cluster stores "" for public GitHub; config.GitHubConfig uses the
	// same convention, so pass it through untouched. Carry the api_url too so the
	// derived config's HostLabel()/IsGHE()/AppInstallURL() resolve the forge
	// base-or-api: a GHE cluster that records only an api_url (blank base_url —
	// the common state) is still recognised as GHE, not mislabelled github.com.
	return config.GitHubConfig{BaseURL: base, APIURL: c.GitHubAPIURL, AppSlug: c.GitHubAppSlug}
}

// githubHostLabel renders a GitHub base URL as a bare hostname for display.
// Empty (public GitHub) becomes "github.com" rather than an empty chip.
func githubHostLabel(baseURL string) string {
	h := strings.TrimSpace(baseURL)
	if h == "" {
		return "github.com"
	}
	h = strings.TrimPrefix(strings.TrimPrefix(h, "https://"), "http://")
	return strings.TrimRight(h, "/")
}

// GitHubHostLabel is the exported form of githubHostLabel, for the spoke to
// normalize its own configured GitHub base URL before reporting it over the
// heartbeat. Sharing one implementation keeps the value the spoke sends and
// the value the hub renders from drifting into two different spellings.
func GitHubHostLabel(baseURL string) string { return githubHostLabel(baseURL) }

func (s *HubServer) handleListClusters(w http.ResponseWriter, r *http.Request) {
	var entries []ClusterListEntry
	for _, c := range s.clusters {
		gh := clusterGitHubConfig(&c)
		entries = append(entries, ClusterListEntry{
			ID:            c.ID,
			Name:          c.Name,
			HasGPU:        c.HasGPU,
			Arch:          c.Arch,
			GitHubHost:    gh.HostLabel(),
			AppInstallURL: gh.AppInstallURL(),
		})
	}
	// Sort for deterministic API output.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].ID < entries[j].ID
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(entries)
}

var hubAutoUpgradePath = "/data/saas/hub-auto-upgrade"

// The hub's own auto-upgrade is deliberately NOT given the daily schedule that
// per-hive auto-upgrade has. The two look symmetric but carry opposite risk:
//
//   - A spoke hive is a workload. Restarting it interrupts running agents, so
//     deferring to an after-hours window is a clear win — that is exactly the
//     "don't disturb a working hive" motivation for the daily mode.
//   - The hub is the control plane. It is what DELIVERS every spoke upgrade,
//     serves the dashboard, and receives every heartbeat. Holding a hub fix for
//     up to 24 hours means holding back fixes to the upgrade machinery itself,
//     including any fix to this scheduler. A hub restart is also cheap: it is a
//     single stateless pod whose state lives on the PVC, and spokes tolerate a
//     missed heartbeat cycle by design.
//
// Deferring hub upgrades would therefore add real risk (a known-bad hub stays
// up all day) to avoid a disruption the hub does not really suffer. It stays a
// plain on/off toggle. Revisit only if hub restarts are ever shown to disrupt
// in-flight spoke work.
func isHubAutoUpgrade() bool {
	data, err := os.ReadFile(hubAutoUpgradePath)
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(data)) == "true"
}

func (s *HubServer) handleHubAutoUpgrade(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AutoUpgrade bool `json:"auto_upgrade"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
		return
	}
	val := "false"
	if body.AutoUpgrade {
		val = "true"
	}
	if err := os.WriteFile(hubAutoUpgradePath, []byte(val), 0644); err != nil {
		s.logger.Error("hub auto-upgrade toggle save failed", "enabled", body.AutoUpgrade, "error", err)
		http.Error(w, `{"error":"failed to save preference"}`, http.StatusInternalServerError)
		return
	}
	s.logger.Info("audit: hub auto-upgrade toggled", "enabled", body.AutoUpgrade, "by", s.getAuthUser(r))

	// If enabling and hub is behind, trigger immediately. The kill switch does
	// NOT block saving the preference — only the immediate rollout: with hub
	// upgrades paused the poller stays suppressed too, and the preference takes
	// effect when an admin resumes.
	if body.AutoUpgrade {
		if sw, paused := s.hubUpgradesPaused(); paused {
			s.logger.Info("hub auto-upgrade initial trigger suppressed — hub upgrades are paused",
				"paused_by", sw.By, "paused_at", sw.At)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"ok":true,"auto_upgrade":%t}`, body.AutoUpgrade)
			return
		}
		latestSHA := getLatestHubSHAForBranch(s.hubGitBranch)
		if latestSHA != "" && !sameCommit(latestSHA, s.hubGitHash) {
			s.logger.Info("audit: hub auto-upgrade initial trigger", "from", s.hubGitHash, "to", latestSHA)
			// Route through rolloutHubToSHA so this shares the hub-image gate and
			// the SHA-pin (avoids a stale cached v2-latest) with the poller path.
			if err := s.rolloutHubToSHA(latestSHA); err != nil {
				s.logger.Warn("hub auto-upgrade skipped", "to", latestSHA, "reason", err)
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"ok":true,"auto_upgrade":%t}`, body.AutoUpgrade)
}

// rolloutHubToSHA upgrades the hub deployment to a specific v2 SHA. It first
// verifies the hub's OWN image (ghcrRepoHub) exists for that SHA on GHCR — a
// separate build job from the spoke image, so a failed hive-hub build must not
// trigger a doomed rollout — then pins the deployment to the immutable
// hive-hub:<sha> tag via `kubectl set image`. Pinning (rather than
// `rollout restart` of the mutable v2-latest tag) forces the node to pull that
// exact image and stops a stale cached v2-latest digest from coming back up
// (the "rolled but still on the old hash" failure). On success it records the
// in-flight state so the dashboard shows "Upgrading". Returns an error the
// caller surfaces; safe to call from both the poller and the admin handler.
func (s *HubServer) rolloutHubToSHA(sha string) error {
	if sha == "" {
		return fmt.Errorf("empty target SHA")
	}
	// Shape-check the tag BEFORE it can reach a live Deployment. Writing an
	// unresolvable tag (the `target1` incident — a test fixture SHA that escaped
	// to the real cluster) leaves the new ReplicaSet in ImagePullBackOff while
	// the old one keeps serving: the hub stays "up" but silently runs stale
	// code. Refusing here leaves the last good image running and makes the
	// failure loud instead of invisible.
	if err := validateImageTag(sha); err != nil {
		s.logger.Error("hub self-upgrade REFUSED: invalid image tag",
			"tag", sha,
			"deployment", hubDeploymentName,
			"namespace", hubNamespace,
			"error", err)
		s.setHubUpgradeFault(fmt.Sprintf("refused invalid image tag %q: %v", sha, err))
		return err
	}
	if !hubImageExists(sha, s.logger) {
		return fmt.Errorf("hub image %s:%s not published yet", ghcrRepoHub, sha)
	}
	image := fmt.Sprintf("ghcr.io/%s:%s", ghcrRepoHub, sha)
	cmd := kubectlForCluster(s.hubCluster(), "set", "image",
		"deployment/"+hubDeploymentName, hubContainerName+"="+image, "-n", hubNamespace)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("kubectl set image failed: %s", strings.TrimSpace(string(out)))
	}
	s.hubUpgradeMu.Lock()
	s.lastHubUpgradeTrigger = time.Now()
	s.hubUpgradeTarget = sha
	s.hubUpgradeFault = "" // a fresh, validated rollout clears any prior refusal
	s.hubUpgradeMu.Unlock()
	// Watch the rollout land. Detect-and-report only: an auto-rollback would
	// race the upgrade poller (which re-triggers the same target every cycle),
	// so a rollback could fight the upgrade loop and flap the deployment. See
	// watchHubRollout.
	go s.watchHubRollout(sha, image)
	return nil
}

// hubRolloutWatchTimeout bounds how long we wait for a self-upgrade we just
// triggered to become Ready before flagging it as stuck. The hub's own image is
// a few hundred MB and a cold node has to pull it, so this is generous enough
// to avoid false alarms while still catching an ImagePullBackOff — which
// back-off-retries indefinitely and would otherwise never surface.
const hubRolloutWatchTimeout = 5 * time.Minute

// hubRolloutPollInterval is how often watchHubRollout re-checks rollout status.
const hubRolloutPollInterval = 15 * time.Second

// watchHubRollout confirms a self-upgrade actually became Ready, and records a
// loud, user-visible fault if it did not.
//
// It deliberately does NOT roll back. The auto-upgrade poller re-evaluates the
// same target every cycle, so an automatic rollback would immediately be undone
// and re-applied, flapping the deployment during an already-degraded window.
// Detect and report is the safe half of the loop: the operator sees the stuck
// upgrade in the UI and in the logs, and decides.
func (s *HubServer) watchHubRollout(sha, image string) {
	s.watchHubRolloutWithInterval(sha, image, hubRolloutWatchTimeout, hubRolloutPollInterval, s.hubRolloutReady)
}

// hubRolloutReady reports whether the rollout we triggered has fully succeeded.
//
// `rollout status --timeout=0s` returns non-zero while a rollout is still
// progressing and zero once it has fully succeeded.
func (s *HubServer) hubRolloutReady() bool {
	cmd := kubectlForCluster(s.hubCluster(), "rollout", "status",
		"deployment/"+hubDeploymentName, "-n", hubNamespace, "--timeout=0s")
	return cmd.Run() == nil
}

// watchHubRolloutWithInterval is watchHubRollout with its clock and its kubectl
// dependency passed in (#7220).
//
// The production wrapper above supplies the real constants and runner. The
// parameters exist so the SUCCESS path is testable: with the interval hardcoded
// at 15s and readiness hardcoded to a kubectl exec, confirming that a completed
// rollout actually clears hubUpgradeFault took a live cluster and five minutes,
// so in practice it was never confirmed at all. That is the silent half of an
// upgrade-safety check — a regression here leaves a stale fault on the
// dashboard, or exits the loop early, and nothing notices until a real hub
// upgrade.
//
// Mirrors the seam runPermissionsWatcher already uses in pkg/agent: interval as
// a parameter, thin production wrapper.
func (s *HubServer) watchHubRolloutWithInterval(sha, image string, timeout, poll time.Duration, rolloutReady func() bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(poll)
		if rolloutReady() {
			s.hubUpgradeMu.Lock()
			s.hubUpgradeFault = ""
			s.hubUpgradeMu.Unlock()
			s.logger.Info("hub self-upgrade rollout completed", "sha", sha, "image", image)
			return
		}
	}
	// Still not Ready. Pull the pod-level reason so the log names the actual
	// cause (ImagePullBackOff / ErrImagePull / CrashLoopBackOff) rather than
	// just "timed out".
	reason := s.hubRolloutFailureReason()
	s.logger.Error("hub self-upgrade STUCK: new ReplicaSet not Ready — hub is still serving the OLD image",
		"sha", sha,
		"image", image,
		"deployment", hubDeploymentName,
		"namespace", hubNamespace,
		"waited", timeout.String(),
		"reason", reason)
	s.setHubUpgradeFault(fmt.Sprintf("upgrade to %s stuck after %s: %s (still serving the previous image)",
		image, timeout, reason))
}

// hubRolloutFailureReason best-effort extracts why the hub's pods are not
// Ready, so the stuck-rollout log/status names the real cause.
func (s *HubServer) hubRolloutFailureReason() string {
	const reasonJSONPath = `{range .items[*].status.containerStatuses[*]}{.state.waiting.reason}{" "}{.state.waiting.message}{"\n"}{end}`
	cmd := kubectlForCluster(s.hubCluster(), "get", "pods",
		"-n", hubNamespace, "-l", "app="+hubDeploymentName,
		"-o", "jsonpath="+reasonJSONPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "unknown (could not read pod status)"
	}
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return "unknown (no waiting container state reported)"
}

// setHubUpgradeFault records a self-upgrade failure for the dashboard so a
// stuck or refused upgrade is visible in the UI, not only in `kubectl describe`.
func (s *HubServer) setHubUpgradeFault(msg string) {
	s.hubUpgradeMu.Lock()
	s.hubUpgradeFault = msg
	s.hubUpgradeMu.Unlock()
}

// HubUpgradeFault returns the current self-upgrade fault message, or "" when
// the last upgrade attempt was healthy.
func (s *HubServer) HubUpgradeFault() string {
	s.hubUpgradeMu.Lock()
	defer s.hubUpgradeMu.Unlock()
	return s.hubUpgradeFault
}

// hubUpgradeState reports the hub's own upgrade status for the dashboard badge:
//   - "current"   — running the latest v2 SHA
//   - "upgrading" — a rollout we triggered is in flight (within the debounce
//     window after the trigger, before the new pod reports the new hash)
//   - "queued"    — behind latest, auto-upgrade ON, no rollout in flight yet
//     (the poller will trigger one shortly)
//   - "behind"    — behind latest, auto-upgrade OFF (admin must click Upgrade)
//   - "failed"    — an upgrade was REFUSED (malformed image tag) or its rollout
//     never became Ready. The hub is behind and cannot self-heal;
//     an operator must look. HubUpgradeFault() carries the reason.
//   - "unknown"   — latest SHA not resolved yet
//
// This is the field the badge needs: previously the frontend could only tell an
// admin-clicked rollout ("upgrading") from everything else ("queued"), so an
// AUTO rollout in progress showed a misleading "queued".
func (s *HubServer) hubUpgradeState() string {
	// Gate the badge on the HUB image, matching what rolloutHubToSHA can
	// actually roll to — otherwise the UI shows "behind"/"queued" against a
	// target the hub is incapable of reaching.
	latest := getLatestHubSHAForBranch(s.hubGitBranch)
	if latest == "" {
		return "unknown"
	}
	if sameCommit(latest, s.hubGitHash) {
		return "current"
	}
	s.hubUpgradeMu.Lock()
	inFlight := s.hubUpgradeTarget != "" &&
		time.Since(s.lastHubUpgradeTrigger) < hubUpgradeDebounce
	fault := s.hubUpgradeFault
	s.hubUpgradeMu.Unlock()
	// A refused or stuck upgrade outranks "upgrading"/"queued": the hub is
	// behind AND cannot get there on its own, which needs an operator. Without
	// this the badge showed a reassuring "queued" while the rollout was wedged.
	if fault != "" {
		return hubUpgradeStateFailed
	}
	if inFlight {
		return "upgrading"
	}
	if isHubAutoUpgrade() {
		return "queued"
	}
	return "behind"
}

func (s *HubServer) handleHubSelfUpgrade(w http.ResponseWriter, r *http.Request) {
	username := s.getAuthUser(r)
	// Admin kill switch: while hub upgrades are paused, a manual trigger is
	// refused loudly — never queued — so the operator learns the state and who
	// set it instead of waiting on a rollout that will not come.
	if sw, paused := s.hubUpgradesPaused(); paused {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": upgradePauseRefusal("hub", sw)})
		return
	}
	target := getLatestHubSHAForBranch(s.hubGitBranch)
	s.logger.Info("audit: hub self-upgrade triggered", "by", username, "to", target)
	if err := s.rolloutHubToSHA(target); err != nil {
		s.logger.Warn("hub self-upgrade failed", "error", err)
		http.Error(w, `{"error":"hub upgrade failed — check logs"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"upgrading"}`))
}

func (s *HubServer) handleUpgradeHive(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if isSameOriginAsHub(origin) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	}
	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	id := r.PathValue("id")
	username := s.getAuthUser(r)
	if username == "" {
		username, _ = s.trustedSpokeSelfServiceUser(r, id)
	}
	h := loadSaaSHive(id)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}
	if !userIsHiveOwner(username, h) {
		http.Error(w, `{"error":"only the owner can upgrade"}`, http.StatusForbidden)
		return
	}
	// Digest pin: an upgrade would re-tag the Deployment off the pinned
	// digest. Refused with the pin's provenance; lift it first (#6290).
	if h.DigestPinned() {
		writeDigestPinRefusal(w, h)
		return
	}
	// Admin kill switch: refuse loudly rather than arm anything — a request
	// accepted here would either restart the pod now or sit silently in
	// heartbeatUpgrade, both of which the pause exists to prevent.
	if sw, paused := s.spokeUpgradesPaused(); paused {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": upgradePauseRefusal("spoke", sw)})
		return
	}
	cluster := s.clusterForHive(h)
	if cluster == nil {
		http.Error(w, `{"error":"no cluster config for this hive"}`, http.StatusInternalServerError)
		return
	}

	// PULL ONLY — no kubectl push. The manual Upgrade button now does exactly
	// what auto-upgrade does: record the target and arm the heartbeat. The
	// spoke collects it on its next beat and patches its own Deployment.
	//
	// UX CONSEQUENCE, DELIBERATE: the click no longer produces an immediate
	// pod roll. Delivery is bounded by the heartbeat interval, so "clicked
	// Upgrade, nothing visible yet" is now normal and must not be mistaken for
	// the wedge this PR fixes. The hive is latched Upgrading with a target the
	// moment the request returns, which is what the dashboard renders.

	// Do not ARM an upgrade this hive cannot COLLECT — the same predicate
	// triggerAutoUpgrades() applies, for the same reason. Delivery is PULL on
	// BOTH paths: the hub only records a target and arms the heartbeat, and the
	// spoke patches its own Deployment when it next beats. A hive that never
	// heartbeats (or is silent past staleRemoveAge) therefore never collects
	// the instruction, while Upgrading=true latches on the hub and the
	// stale-upgrade sweep re-arms it every staleUpgradeTimeout — an unbounded
	// loop the orphan sweep's retry budget cannot break, because such a hive
	// fails evaluateOrphanedUpgrade()'s liveness test. See pullonly_upgrade.go.
	//
	// Without this, the manual button was strictly WORSE than the auto path it
	// diverged from: auto-upgrade refuses and records the refusal on the
	// timeline, whereas the click reported {"status":"upgrading"} and a success
	// toast for an upgrade that could never land. Worse, the asymmetry read as
	// a workaround — the same spoke auto-upgrade had declined would accept a
	// manual click, appearing to fix the problem while only hiding it.
	//
	// lastHeartbeat comes from the REGISTRY entry, which is the only record
	// that carries it; SaaSHive (the loadSaaSHive record `h` above) has no such
	// field. This is the identical source triggerAutoUpgrades() reads.
	//
	// Refused with 409, matching the pause-switch refusal above. The reason is
	// operator-facing by construction and documented to carry no kubeconfig
	// paths or credentials, so it is safe in the body.
	//
	// The target is the spoke's REACHABLE latest (reachableUpgradeTarget), not
	// the branch tip: a spoke pinned to a release channel can only ever land
	// on the commit its channel tag points at, so arming the v4 tip for a
	// :stable spoke wedges it — it re-pulls :stable, comes back on the same
	// commit, and stays "Upgrading" against a target it cannot reach (#6294).
	// Resolved OUTSIDE s.mu: the channel lookup may consult GHCR.
	s.mu.RLock()
	var regBranch, regImageRef string
	for i := range s.registry.Hives {
		if s.registry.Hives[i].ID == id {
			regBranch = s.registry.Hives[i].GitBranch
			regImageRef = s.registry.Hives[i].ImageRef
			break
		}
	}
	s.mu.RUnlock()
	reach := s.reachableUpgradeTarget(s.upgradeBranchOrDefault(regBranch), regImageRef, h.TrackedChannel)
	if !reach.Resolved {
		s.logger.Warn("manual upgrade not armed — the spoke's release channel did not resolve to a commit",
			"hive_id", id, "by", username, "channel", reach.Channel, "image_ref", regImageRef)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "release channel " + reach.Channel + " did not resolve to a commit — try again shortly"})
		return
	}
	s.mu.Lock()
	var latestSHA, lastHeartbeat string
	var found bool
	for i := range s.registry.Hives {
		if s.registry.Hives[i].ID == id {
			found = true
			lastHeartbeat = s.registry.Hives[i].LastHeartbeat
			latestSHA = reach.SHA
			break
		}
	}
	// A hive with no registry entry has never checked in at all, so it is
	// uncollectible for exactly the reason the empty-heartbeat case is.
	if !found || !upgradeCollectible(lastHeartbeat, time.Now()) {
		s.mu.Unlock()
		reason := uncollectibleUpgradeReason(lastHeartbeat)
		s.logger.Warn("manual upgrade not armed — hive cannot collect the instruction",
			"hive_id", id, "by", username, "cluster", cluster.ID,
			"would_have_targeted", latestSHA, "last_heartbeat", orDash(lastHeartbeat),
			"reason", reason)
		s.noteUncollectibleUpgrade(id, latestSHA, reason)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": reason})
		return
	}
	for i := range s.registry.Hives {
		if s.registry.Hives[i].ID == id {
			s.beginUpgrade(i, latestSHA)
			break
		}
	}
	if latestSHA != "" {
		// Arm delivery: the spoke self-restarts onto the target when its next
		// heartbeat carries UpgradeTo. The stale-upgrade sweep re-arms this if
		// the spoke misses it, so the request cannot be lost.
		if s.heartbeatUpgrade == nil {
			s.heartbeatUpgrade = make(map[string]string)
		}
		s.heartbeatUpgrade[id] = latestSHA
	}
	s.mu.Unlock()

	if latestSHA == "" {
		// No build target is known for this hive's branch, so there is nothing
		// the heartbeat could carry. This is the only hard-failure case left.
		s.logger.Warn("upgrade failed: no build target known for this hive's branch",
			"hive", id, "cluster", cluster.ID)
		http.Error(w, `{"error":"upgrade failed — no build target known for this hive's branch"}`, http.StatusBadGateway)
		return
	}

	// Always "heartbeat" now: the push path is retired, so every upgrade is
	// collected by the spoke on its next beat. The UI uses this to say "queued"
	// rather than implying an immediate roll.
	const mode = "heartbeat"
	// Armed successfully, so the uncollectible condition has genuinely cleared:
	// drop the de-duplication memory (as the auto path does on its own successful
	// arm) so a LATER refusal for this same target is reported afresh rather than
	// suppressed by a stale entry.
	s.forgetUncollectibleUpgrade(id)
	s.logger.Info("audit: hosted hive upgrade requested",
		"hive_id", id, "by", username, "cluster", cluster.ID, "mode", mode)
	s.recordTimeline(id, TimelineUpgradeStarted, "upgrade requested from the hub dashboard ("+mode+")", username)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"upgrading","mode":"` + mode + `"}`))
}

// branchToTag converts a git branch name into a valid Docker image tag.
// Branch names may contain '/' (e.g. feat/x) which is illegal in a tag; the
// docker.yml build sanitizes the same way (feat/x -> feat-x-latest), so the
// hub must match to find the image.
func branchToTag(branch string) string {
	return strings.ReplaceAll(branch, "/", "-")
}

func (s *HubServer) handleSwitchBranch(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if isSameOriginAsHub(origin) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	}
	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	id := r.PathValue("id")
	username := s.getAuthUser(r)
	if username == "" {
		username, _ = s.trustedSpokeSelfServiceUser(r, id)
	}
	h := loadSaaSHive(id)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}
	if !userIsHiveOwner(username, h) {
		http.Error(w, `{"error":"only the owner can switch branches"}`, http.StatusForbidden)
		return
	}
	// Digest pin: a branch/channel switch writes a moving tag over the pinned
	// digest. Refused with the pin's provenance; unpin first (#6290).
	if h.DigestPinned() {
		writeDigestPinRefusal(w, h)
		return
	}
	// Admin kill switch: a branch/channel switch while spoke upgrades are
	// paused gets an explicit 409 naming who paused and when — never a silent
	// queue into heartbeatSwitchTag.
	if sw, paused := s.spokeUpgradesPaused(); paused {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": upgradePauseRefusal("spoke", sw)})
		return
	}
	var body struct {
		Branch string `json:"branch"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Branch == "" {
		http.Error(w, `{"error":"branch is required"}`, http.StatusBadRequest)
		return
	}
	// A release channel is a moving TAG, not a git ref, so the branch-existence
	// checks below would reject it ("stable" is not a branch on the hive repo).
	// Accept it here and let the shared publish check further down be the real
	// gate — the tag still has to exist on GHCR before we point a hive at it.
	isChannel := isReleaseChannel(body.Branch)
	validBranch := isChannel
	for _, b := range s.trackedBranchList() {
		if b == body.Branch {
			validBranch = true
			break
		}
	}
	// Bootstrap case: trackedBranchList only includes branches already
	// assigned to some hive, so the FIRST hive moved to a new dev branch
	// would never validate. Accept any branch that actually exists on the
	// hive repo — a live one-shot check (fetchBranchSHA populates the SHA
	// cache as a side effect, so the branch is tracked from here on).
	if !validBranch {
		fetchBranchSHA(s.logger, body.Branch)
		if getLatestSHAForBranch(body.Branch) != "" {
			validBranch = true
		}
	}
	if !validBranch {
		http.Error(w, `{"error":"unknown branch (does not exist on the hive repo)"}`, http.StatusBadRequest)
		return
	}
	cluster := s.clusterForHive(h)
	if cluster == nil {
		http.Error(w, `{"error":"no cluster config for this hive"}`, http.StatusInternalServerError)
		return
	}
	ns := "hive-hosted-" + id
	// A channel IS the tag ("stable"); a branch's moving tag is "<branch>-latest".
	imageTag := upgradeTargetTag(body.Branch)
	image := "ghcr.io/hivecommons/hive:" + imageTag
	// Refuse a branch name that sanitizes into something that is not a valid
	// channel tag, rather than stranding the spoke on ImagePullBackOff behind a
	// still-serving old ReplicaSet.
	if err := validateImageTag(imageTag); err != nil {
		s.logger.Error("branch switch REFUSED: invalid image tag",
			"hive", id, "branch", body.Branch, "tag", imageTag, "error", err)
		http.Error(w, `{"error":"branch does not map to a valid image tag"}`, http.StatusBadRequest)
		return
	}
	// Shape is not existence. A DEPRECATED branch (v3 was retired while hives
	// were still pointed at it) keeps a perfectly well-formed "<branch>-latest"
	// tag that CI no longer publishes, so validateImageTag passes and the pull
	// then fails with an opaque "manifest unknown". Kubernetes keeps the old
	// ReplicaSet serving, so the hive looks alive while silently running stale
	// code. Verify the tag is actually pullable before writing it.
	if !spokeImageExists(imageTag, s.logger) {
		s.logger.Error("branch switch REFUSED: image tag not published on GHCR",
			"hive", id, "branch", body.Branch, "tag", imageTag,
			"hint", "branch may be deprecated or its CI image build never completed")
		http.Error(w, `{"error":"no published image for that branch (deprecated branch, or its image build has not completed)"}`, http.StatusBadRequest)
		return
	}
	// Persist WHAT the operator selected before delivering it, on the hub-owned
	// hive record. The registry cannot remember a channel selection: the spoke
	// heartbeats the image's baked-in branch (a "stable" retag of a v4 build
	// reports git_branch="v4") and overwrites GitBranch every beat, so within
	// one beat of the switch the dashboard's pill fell back to the branch. Set
	// on a channel switch, cleared on a plain-branch switch, written by no
	// other path — heartbeats never touch it. Done after every validation gate
	// above (so a refused switch records nothing) and before the two delivery
	// paths below (so kubectl-vs-heartbeat delivery cannot diverge on it). A
	// save failure only downgrades the pill to the reported branch; it must
	// not block the switch itself.
	if isChannel {
		h.TrackedChannel = body.Branch
	} else {
		h.TrackedChannel = ""
	}
	if err := saveSaaSHive(h); err != nil {
		s.logger.Warn("branch switch: failed to persist tracked channel — the version pill will fall back to the spoke-reported branch",
			"hive", id, "target", body.Branch, "error", err)
	}
	// "*=" updates every container including init containers (copy-config,
	// init-permissions) — pinning only "hive" left inits on the old branch tag.
	cmd := kubectlForCluster(cluster, "set", "image", "deployment/hive", "*="+image, "-n", ns)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// The hub can't reach this hive's cluster over kubectl (e.g. the heartbeat-only cluster
		// from the hub-reachable-cluster hub). Fall back to the heartbeat path: record the
		// target tag; the spoke — which has in-cluster RBAC (hive-self-upgrade
		// role) to patch its own deployment — applies it on its next
		// heartbeat. This is the ONLY path that works for unreachable
		// clusters, so it's not an error.
		s.logger.Warn("branch switch kubectl failed, using heartbeat fallback",
			"hive", id, "branch", body.Branch, "output", string(out))
		s.mu.Lock()
		s.heartbeatSwitchTag[id] = imageTag
		for i := range s.registry.Hives {
			if s.registry.Hives[i].ID == id {
				s.beginUpgrade(i, imageTag)
				break
			}
		}
		s.mu.Unlock()
		s.logger.Info("audit: hive branch switch queued via heartbeat", "hive_id", id, "branch", body.Branch, "image", image, "by", username)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "switching", "branch": body.Branch, "image": image, "via": "heartbeat"})
		return
	}
	// Restart the deployment to pull the new image
	restartCmd := kubectlForCluster(cluster, "rollout", "restart", "deployment/hive", "-n", ns)
	restartOut, restartErr := restartCmd.CombinedOutput()
	if restartErr != nil {
		s.logger.Warn("rollout restart after branch switch failed", "hive", id, "output", string(restartOut))
	}
	s.logger.Info("audit: hive branch switched", "hive_id", id, "branch", body.Branch, "image", image, "by", username)
	s.recordTimeline(id, TimelineBranchChanged,
		fmt.Sprintf("branch switch to %s requested (image %s)", body.Branch, image), username)
	s.mu.Lock()
	for i := range s.registry.Hives {
		if s.registry.Hives[i].ID == id {
			s.beginUpgrade(i, imageTag)
			break
		}
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status": "switching",
		"branch": body.Branch,
		"image":  image,
	})
}

func pendingAgentRestartResetsForHeartbeat(hiveID string) []string {
	h := loadSaaSHive(hiveID)
	if h == nil || len(h.AgentRestartResets) == 0 {
		return nil
	}
	var names []string
	changed := false
	for name, reset := range h.AgentRestartResets {
		if !reset.Pending {
			continue
		}
		names = append(names, name)
		reset.Pending = false
		reset.TotalBaseline = 0
		h.AgentRestartResets[name] = reset
		changed = true
	}
	if changed {
		_ = saveSaaSHive(h)
	}
	sort.Strings(names)
	return names
}

func applyAgentRestartResetBaselines(agents []AgentSummary, resets map[string]AgentRestartReset, now time.Time) {
	if len(resets) == 0 {
		return
	}
	cutoff := now.Add(-24 * time.Hour)
	for i := range agents {
		reset, ok := resets[agents[i].Name]
		if !ok {
			continue
		}
		agents[i].Restarts.ResetAt = reset.ResetAt
		agents[i].Restarts.ResetBy = reset.By
		resetAt, err := time.Parse(time.RFC3339, reset.ResetAt)
		if err != nil || resetAt.Before(cutoff) {
			continue
		}
		delta := agents[i].Restarts.Total - reset.TotalBaseline
		if delta < 0 {
			delta = 0
		}
		agents[i].Restarts.Last24h = delta
	}
}

func appendDriftSignal(report *DriftReport, kind string, sev DriftSeverity, reason string) {
	if report == nil {
		return
	}
	report.Signals = append(report.Signals, DriftSignal{Kind: kind, Severity: sev, Reason: reason})
	report.Count = len(report.Signals)
	if driftSeverityRank[sev] > driftSeverityRank[report.WorstSeverity] {
		report.WorstSeverity = sev
	}
}

func (s *HubServer) handleToggleAutoUpgrade(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if isSameOriginAsHub(origin) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Allow-Methods", "PUT, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	}
	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	id := r.PathValue("id")
	username := s.getAuthUser(r)
	h := loadSaaSHive(id)
	if h == nil {
		// Hive may exist in registry via heartbeat but have no SaaS entry yet.
		// Create a minimal entry so the auto-upgrade preference can be stored
		// and delivered via heartbeat response.
		s.mu.RLock()
		var regEntry *RegistryEntry
		for i := range s.registry.Hives {
			if s.registry.Hives[i].ID == id {
				regEntry = &s.registry.Hives[i]
				break
			}
		}
		s.mu.RUnlock()
		if regEntry == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprint(w, `{"error":"hive not found"}`)
			return
		}
		h = &SaaSHive{
			ID:    id,
			Owner: regEntry.Owner,
			Org:   regEntry.Org,
			Repos: regEntry.Repos,
		}
	}
	if !userIsHiveOwner(username, h) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(w, `{"error":"only the owner can change auto-upgrade"}`)
		return
	}
	// The mode rides on the EXISTING endpoint rather than a second one: it is
	// the same preference, the same owner-or-admin authorization checked above,
	// and the same persistence. A separate endpoint would let the two settings
	// drift apart across two requests. Older clients that send only
	// auto_upgrade omit the field, which reads as "" = instant, preserving
	// their behaviour exactly.
	var body struct {
		AutoUpgrade bool   `json:"auto_upgrade"`
		Mode        string `json:"auto_upgrade_mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error":"invalid request body"}`)
		return
	}
	// Reject unknown modes instead of defaulting — a typo must not silently
	// change how often a hive restarts.
	if !isValidAutoUpgradeMode(body.Mode) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error":"invalid auto_upgrade_mode (expected \"instant\", \"daily\" or \"weekly\")"}`)
		return
	}
	h.AutoUpgrade = body.AutoUpgrade
	h.AutoUpgradeMode = body.Mode
	// Switching modes clears the day's fire record. Otherwise a hive flipped to
	// daily after an instant upgrade earlier today would inherit a stale date
	// and skip tonight's window.
	h.AutoUpgradeLastFired = ""
	if err := saveSaaSHive(h); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprint(w, `{"error":"failed to save"}`)
		return
	}
	s.logger.Info("audit: auto-upgrade toggled", "hive_id", id, "auto_upgrade", body.AutoUpgrade, "mode", normalizeAutoUpgradeMode(body.Mode), "by", username)

	// If enabling auto-upgrade and hive is behind, trigger immediately via kubectl
	// for hosted hives. For heartbeat-connected hives, the upgrade instruction
	// is delivered via the heartbeat response.
	// Daily mode deliberately does NOT kick an upgrade here: the whole point of
	// choosing it is that turning auto-upgrade on should not immediately
	// restart a working hive. It will roll at the next daily ET window
	// (autoUpgradeDailyHour, currently 13:00 / 1pm ET). The
	// operator who wants it now still has the explicit Upgrade button, which
	// goes through handleUpgradeHive and is never gated by mode.
	// The kill switch does not block saving the PREFERENCE (auto-upgrade
	// on/off is configuration, not delivery) — only the immediate trigger
	// below. While paused, triggerAutoUpgrades stays suppressed anyway, and
	// the hive upgrades after an admin resumes.
	spokePauseSw, spokesPaused := s.spokeUpgradesPaused()
	if body.AutoUpgrade && spokesPaused {
		s.logger.Info("auto-upgrade initial trigger suppressed — spoke upgrades are paused",
			"hive_id", id, "paused_by", spokePauseSw.By, "paused_at", spokePauseSw.At)
	}
	if body.AutoUpgrade && !spokesPaused && normalizeAutoUpgradeMode(body.Mode) == AutoUpgradeModeInstant {
		s.mu.RLock()
		var currentSHA, branch string
		for _, reg := range s.registry.Hives {
			if reg.ID == id {
				currentSHA = reg.GitHash
				branch = reg.GitBranch
				break
			}
		}
		s.mu.RUnlock()
		branch = s.upgradeBranchOrDefault(branch)
		latestSHA := getLatestSHAForBranch(branch)
		if latestSHA != "" && currentSHA != "" && !sameCommit(currentSHA, latestSHA) {
			s.logger.Info("audit: auto-upgrade initial trigger", "hive_id", id, "from", currentSHA, "to", latestSHA)
			s.mu.Lock()
			for i := range s.registry.Hives {
				if s.registry.Hives[i].ID == id {
					s.beginUpgrade(i, latestSHA)
					break
				}
			}
			s.mu.Unlock()
			hiveCluster := s.clusterForHive(h)
			if hiveCluster != nil {
				ns := "hive-hosted-" + id
				cmd := kubectlForCluster(hiveCluster, "rollout", "restart", "deployment/hive", "-n", ns)
				if out, err := cmd.CombinedOutput(); err != nil {
					s.logger.Warn("auto-upgrade initial trigger failed (will retry via heartbeat)", "hive", id, "cluster", hiveCluster.ID, "output", string(out))
				}
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"ok":true,"auto_upgrade":%t,"auto_upgrade_mode":%q}`, body.AutoUpgrade, normalizeAutoUpgradeMode(body.Mode))
}

// provisionWG tracks async hive-provisioning goroutines so tests (which swap
// the package-level saas*Dir path variables) can wait for them to drain
// before mutating shared state.
var provisionWG sync.WaitGroup

// clusterRecentlyUnreachable reports whether the hub failed to reach this
// cluster recently enough that another kubectl attempt would just burn the
// timeout again. Callers should go straight to the heartbeat fallback instead.
func (s *HubServer) clusterRecentlyUnreachable(clusterID string) bool {
	if clusterID == "" {
		return false
	}
	// A pull-only cluster is unreachable BY DECLARATION and permanently, so it
	// never has to be learned the expensive way. This breaker exists because
	// discovering unreachability costs a full dial timeout per hive per cycle —
	// ~90s a call, with a pool of hives serialising into tens of minutes of
	// blocking. Saying so in clusters.json means that price is never paid even
	// once, and every caller that already consults this breaker is covered
	// without needing its own pull-only check.
	if c, ok := s.clusters[clusterID]; ok && c.PullOnly {
		return true
	}
	s.clusterUnreachableMu.Lock()
	defer s.clusterUnreachableMu.Unlock()
	until, ok := s.clusterUnreachableUntil[clusterID]
	return ok && time.Now().Before(until)
}

// markClusterUnreachable starts (or extends) the kubectl suppression window for
// a cluster the hub just failed to dial.
func (s *HubServer) markClusterUnreachable(clusterID string) {
	if clusterID == "" {
		return
	}
	s.clusterUnreachableMu.Lock()
	defer s.clusterUnreachableMu.Unlock()
	if s.clusterUnreachableUntil == nil {
		s.clusterUnreachableUntil = make(map[string]time.Time)
	}
	s.clusterUnreachableUntil[clusterID] = time.Now().Add(clusterUnreachableTTL)
}

// markClusterReachable clears any suppression after a kubectl call succeeds, so
// a cluster that recovers is used immediately rather than waiting out the TTL.
func (s *HubServer) markClusterReachable(clusterID string) {
	if clusterID == "" {
		return
	}
	s.clusterUnreachableMu.Lock()
	defer s.clusterUnreachableMu.Unlock()
	delete(s.clusterUnreachableUntil, clusterID)
}

func (s *HubServer) triggerAutoUpgrades() {
	// Admin kill switch: while spoke upgrades are paused this entire reconciler
	// is a no-op — it must start no new upgrades, issue no kubectl restarts,
	// and (critically) not re-arm heartbeatUpgrade through its stale-recovery
	// path, which otherwise re-delivers in-flight targets every cycle. The
	// registry latches and armed targets are left untouched so resuming picks
	// up exactly where the fleet paused.
	if sw, paused := s.spokeUpgradesPaused(); paused {
		s.logger.Debug("auto-upgrade reconciler suppressed — spoke upgrades are paused",
			"paused_by", sw.By, "paused_at", sw.At)
		return
	}
	hives := listSaaSHives()
	// Upgrade waves: bound how many hives may be UPGRADING per cluster at
	// once. A merge used to roll every behind hive simultaneously — observed
	// live as a fleet-wide restart inside minutes, an image-pull + PVC IO
	// storm. Count the in-flight upgrades per cluster first; the arming gate
	// below starts new upgrades only while a cluster is under its wave size.
	// Recovery/latch-clearing paths are deliberately NOT bounded (corrective,
	// not disruptive), and wait-healthy is implicit: Upgrading clears when a
	// spoke reports the target reached, freeing wave slots for the next tick.
	upgradingByCluster := make(map[string]int)
	s.mu.RLock()
	upgradingIDs := make(map[string]bool)
	for _, reg := range s.registry.Hives {
		if reg.Upgrading {
			upgradingIDs[reg.ID] = true
		}
	}
	s.mu.RUnlock()
	for i := range hives {
		if upgradingIDs[hives[i].ID] {
			upgradingByCluster[clusterIDForHive(&hives[i])]++
		}
	}
	waveSize := upgradeWaveSize()
	for _, h := range hives {
		s.mu.RLock()
		var currentSHA, branch, upgradeTarget, imageRef, lastHeartbeat string
		var alreadyUpgrading bool
		var upgradeStartedAt time.Time
		for _, reg := range s.registry.Hives {
			if reg.ID == h.ID {
				currentSHA = reg.GitHash
				branch = reg.GitBranch
				alreadyUpgrading = reg.Upgrading
				upgradeTarget = reg.UpgradeTarget
				upgradeStartedAt = reg.UpgradeStartedAt
				imageRef = reg.ImageRef
				lastHeartbeat = reg.LastHeartbeat
				break
			}
		}
		s.mu.RUnlock()
		// Resolve against the hub's own branch when the hive has not reported
		// one, never a hardcoded "v2" — see upgradeBranchOrDefault. A hardcoded
		// v2 is what armed 0b78dc0 (a v2-only commit) at placeholders on a v4
		// hub.
		branch = s.upgradeBranchOrDefault(branch)
		if alreadyUpgrading {
			// Floating-tag convergence. A hive whose Deployment tracks a MUTABLE
			// tag (…-latest) has no stable target commit: a restart re-pulls the
			// tag and lands on whatever CI last published, so its reported GitHash
			// chases an ever-moving branch HEAD and can never equal the SPECIFIC
			// commit the hub armed. Left to the stale-recovery path below, that
			// hive is re-armed and rolled every staleUpgradeTimeout forever —
			// restarting the spoke pod each cycle for no benefit. Once such a hive
			// reports it is running the image-verified latest for its branch, it IS
			// up to date; clear the latch here instead of advancing the target.
			// Commit-pinned hives are unaffected — their tag resolves to exactly
			// one build, so the specific-target check still governs them.
			registryLatestSHA := getLatestSHAForBranch(branch)
			if imageTagIsMutable(imageRef) && registryLatestSHA != "" &&
				sameCommit(currentSHA, registryLatestSHA) {
				s.mu.Lock()
				for i := range s.registry.Hives {
					if s.registry.Hives[i].ID == h.ID {
						s.clearUpgradeLatch(i)
						break
					}
				}
				delete(s.heartbeatUpgrade, h.ID)
				s.mu.Unlock()
				s.logger.Info("clearing upgrade latch — floating-tag hive is at latest",
					"hive", h.ID, "branch", branch, "sha", currentSHA, "image_ref", imageRef)
				continue
			}
			// Target reached or SURPASSED. The equality checks above cannot see
			// a spoke that landed AHEAD of the armed target: a floating-tag
			// re-pull delivers whatever CI last published, so when the branch
			// advanced between arming and pulling — and again before this cycle
			// — the reported hash equals neither the target nor the current
			// latest. Without this ancestry check the stale-recovery below
			// re-arms the ORIGINAL stale pin forever (manual upgrades never
			// advance their target), re-stamping the registry latch and
			// re-instructing a commit the spoke can never report — the
			// vllmd-13 wedge. Cache-only + background resolve, so this loop
			// never blocks on the network; an unresolved pair clears on a
			// later cycle.
			if upgradeTarget != "" && currentSHA != "" &&
				commitAtOrAheadOfTarget(currentSHA, upgradeTarget, s.logger) {
				s.mu.Lock()
				for i := range s.registry.Hives {
					if s.registry.Hives[i].ID == h.ID {
						s.clearUpgradeLatch(i)
						break
					}
				}
				delete(s.heartbeatUpgrade, h.ID)
				s.mu.Unlock()
				s.logger.Info("clearing upgrade latch — hive is at or ahead of its armed target",
					"hive", h.ID, "branch", branch, "sha", currentSHA, "target", upgradeTarget)
				continue
			}
			// Latched-upgrade recovery runs for EVERY hive, deliberately BEFORE
			// the AutoUpgrade gate below (#2476). The registry latch
			// (Upgrading/UpgradeTarget/UpgradeStartedAt) is durable, but
			// heartbeatUpgrade — the map that actually delivers the instruction
			// — is in-memory; when this recovery lived behind
			// `if !h.AutoUpgrade { continue }`, a hub restart orphaned every
			// manually upgraded hive forever: latched "Upgrading" in the
			// registry, zero instructions on the wire.
			upgradeAge := time.Since(upgradeStartedAt)
			// Zero UpgradeStartedAt means the timestamp was lost (heartbeats
			// used to wipe it on rebuild) — treat as stale so already-stuck
			// hives self-heal instead of upgrading forever.
			isStale := upgradeStartedAt.IsZero() || upgradeAge > staleUpgradeTimeout

			if isStale {
				// UNCOLLECTIBLE: abandon, do not re-arm. This is the branch that
				// produced the measured wedge — 26 hives re-arming, stale_minutes
				// climbing past 146 and still rising — because re-arming an
				// instruction nothing will ever pick up reproduces the identical
				// no-op every staleUpgradeTimeout, forever, while beginUpgrade()
				// preserves the original start clock so the elapsed only ever
				// grows. The retry budget in sweepOrphanedUpgrades() cannot bound
				// it: these hives never heartbeated, so evaluateOrphanedUpgrade()
				// bails on the liveness test and the budget is never spent.
				//
				// Clearing the latch outright is what stops staleness
				// accumulating. The hive stays on its old SHA — truthful and
				// visible — instead of misreporting as perpetually "Upgrading"
				// while reading offline. Nothing is lost: the instruction was
				// never being collected, so dropping it forfeits nothing. If the
				// spoke later starts heartbeating, the normal arming path picks
				// it up on the next poll.
				if !upgradeCollectible(lastHeartbeat, time.Now()) {
					reason := uncollectibleUpgradeReason(lastHeartbeat)
					s.logger.Warn("abandoning stale upgrade — hive cannot collect it, not re-arming",
						"hive", h.ID, "stale_minutes", int(upgradeAge.Minutes()),
						"target", upgradeTarget, "last_heartbeat", orDash(lastHeartbeat),
						"reason", reason)
					s.mu.Lock()
					for i := range s.registry.Hives {
						if s.registry.Hives[i].ID == h.ID {
							s.clearUpgradeLatch(i)
							break
						}
					}
					delete(s.heartbeatUpgrade, h.ID)
					s.mu.Unlock()
					s.noteUncollectibleUpgrade(h.ID, upgradeTarget, reason)
					continue
				}
				// Upgrade has been stuck longer than staleUpgradeTimeout.
				// Recover it. Two things can be wrong: (a) the target SHA
				// contains a crashing bug a newer commit fixes — advance the
				// target to latest; (b) the kubectl rollout never reached the
				// spoke (e.g. the hub can't route to the hive's cluster API),
				// so the upgrade was never actually delivered.
				//
				// The heartbeat fallback (heartbeatUpgrade → the spoke
				// self-restarts on its next heartbeat) is the ONLY path that
				// works when kubectl can't reach the cluster, so re-arm it
				// unconditionally for a stale upgrade — not only when the
				// target advances. Previously, when the target already equalled
				// latest, this branch was skipped entirely and the hive stayed
				// latched-upgrading forever behind an unreachable kubectl.
				recoverTarget := upgradeTarget
				if h.AutoUpgrade {
					// Target advancement is an auto-upgrade behaviour: those
					// hives always chase latest. A manual upgrade keeps exactly
					// the target that was requested — with AutoUpgrade off,
					// silently delivering a newer build than the one the owner
					// clicked would override their setting.
					//
					// Advance to the spoke's REACHABLE latest, never the raw
					// branch tip: a :stable spoke re-armed at the v4 tip can
					// only re-pull :stable, so it would wedge on an unreachable
					// target forever (#6294). An unresolved channel keeps the
					// old target rather than inventing one.
					if reach := s.reachableUpgradeTarget(branch, imageRef, h.TrackedChannel); reach.Resolved && reach.SHA != "" && reach.SHA != upgradeTarget {
						recoverTarget = reach.SHA
					}
				}
				if recoverTarget != upgradeTarget {
					s.logger.Warn("advancing upgrade target for stale upgrade",
						"hive", h.ID, "stale_minutes", int(upgradeAge.Minutes()),
						"old_target", upgradeTarget, "new_target", recoverTarget)
				} else {
					s.logger.Warn("re-arming heartbeat fallback for stale upgrade",
						"hive", h.ID, "stale_minutes", int(upgradeAge.Minutes()),
						"target", recoverTarget)
				}
				if recoverTarget != "" {
					s.mu.Lock()
					for i := range s.registry.Hives {
						if s.registry.Hives[i].ID == h.ID {
							// Route through beginUpgrade so the start time
							// SURVIVES a same-target re-arm (the crash-loop retry
							// case): re-arming delivery for the same target must
							// not reset the elapsed clock, or a thrashing upgrade
							// never crosses staleUpgradeTimeout and the stuck-
							// upgrade alert never fires. A genuinely advanced
							// target (recoverTarget != old UpgradeTarget) is a new
							// upgrade and DOES get a fresh clock. A lost/zero start
							// is re-stamped either way, which self-heals the
							// ibm-alchemy zero-timestamp wedge.
							s.beginUpgrade(i, recoverTarget)
							break
						}
					}
					s.heartbeatUpgrade[h.ID] = recoverTarget
					s.mu.Unlock()
					// PULL ONLY — the heartbeat armed above IS the delivery, as
					// the previous comment here already conceded ("the heartbeat
					// fallback armed above is what actually delivers the
					// upgrade"). The kubectl push that followed it was pure
					// latency optimisation and is deliberately gone; see the
					// push-path retirement note in pullonly_upgrade.go.
					continue
				}
			}

			// Not stale — keep the original target so the hive can satisfy it.
			// Re-populate the heartbeatUpgrade map in case the hub restarted.
			//
			// SAME COLLECTIBILITY GATE AS THE STALE BRANCH ABOVE. Arming is
			// arming: re-populating the map for a hive that cannot collect
			// reproduces the wedge the stale branch just abandoned, only
			// sooner. Because this branch runs on EVERY poll while the hive is
			// latched, it re-arms roughly every 2 minutes, whereas abandonment
			// waits out staleUpgradeTimeout — so without this check the fix
			// merely races the timeout and the uncollectible hive stays armed.
			// The predicate is upgradeCollectible(), reused rather than
			// restated, so there is one definition of "can collect".
			if upgradeTarget != "" && !upgradeCollectible(lastHeartbeat, time.Now()) {
				s.logger.Debug("not re-arming in-progress upgrade — hive cannot collect it",
					"hive", h.ID, "target", upgradeTarget,
					"last_heartbeat", orDash(lastHeartbeat))
				continue
			}
			hiveCluster := s.clusterForHive(&h)
			if hiveCluster != nil && !hiveCluster.InCluster {
				if upgradeTarget != "" {
					s.mu.Lock()
					s.heartbeatUpgrade[h.ID] = upgradeTarget
					s.mu.Unlock()
				}
			}
			s.logger.Debug("skipping target advance — upgrade still in progress",
				"hive", h.ID, "current", currentSHA)
			continue
		}
		// Everything below STARTS a new upgrade, which only auto-upgrade hives
		// opt into. The recovery above must stay ahead of this gate — see #2476.
		if !h.AutoUpgrade {
			continue
		}
		// Digest pin (#6290): a pinned hive is deliberately outside the upgrade
		// train. Arming it would put UpgradeTo on the wire and the spoke would
		// roll off the digest an operator chose. Skipped silently at Debug: the
		// dashboard's PINNED pill already says why this hive is not moving.
		if h.DigestPinned() {
			s.logger.Debug("auto-upgrade skipped - hive is pinned to a digest",
				"hive_id", h.ID, "digest", h.DigestPin.Digest, "pinned_by", h.DigestPin.By)
			continue
		}
		// Claim-in-flight latch (#95). A placeholder that has just been ASSIGNED
		// but whose claim has not yet been delivered (Status==assigned &&
		// !ClaimDelivered) is mid-wiring: the spoke is receiving its org/repos/
		// ACMM over successive heartbeats. Rolling its pod onto a new image now
		// can wedge it — the classic EPM dead-end (task #94). DEFER (do not
		// cancel) the auto-upgrade until the claim lands. This is self-limiting:
		// it releases the moment ClaimDelivered flips true, and if the claim
		// never completes, sweepStuckAssignments returns the slot to available
		// after assignStuckResetTimeout — either way this latch clears and the
		// next cycle upgrades normally. A hard image pin is unaffected: pins are
		// delivered via UpgradeTarget through the recovery path ABOVE this gate,
		// not started here, so a pin still wins.
		if assignmentInFlight(&h) {
			s.logger.Debug("auto-upgrade deferred — claim in flight",
				"hive_id", h.ID, "status", h.Status, "assigned_at", h.AssignedAt)
			continue
		}
		// Scheduling gate. Instant-mode hives (and every legacy record, whose
		// mode is empty) pass straight through, so this changes nothing for the
		// existing fleet. Daily-mode hives are held until the first cycle at or
		// after autoUpgradeDailyHour ET and released only once per ET day.
		// Evaluated BEFORE any of the work below so a held hive costs nothing.
		decision := shouldAutoUpgradeNow(h.AutoUpgradeMode, h.AutoUpgradeLastFired, time.Now())
		if !decision.Allowed {
			s.logger.Debug("auto-upgrade held by schedule",
				"hive_id", h.ID, "mode", h.AutoUpgradeMode, "reason", decision.Reason)
			continue
		}
		// Skip hives that are actively provisioning or in error state.
		// Empty status means the hive predates the provisioning system — treat as eligible.
		if h.Status == "provisioning" || h.Status == "error" {
			continue
		}
		if currentSHA == "" {
			continue
		}
		latestSHA := getLatestSHAForBranch(branch)
		if latestSHA == "" || sameCommit(currentSHA, latestSHA) {
			continue
		}
		// Merge-driven debounce (#5391). Reached ONLY on the automatic
		// chase-latest path: everything that starts an upgrade for an operator
		// — a manual "Upgrade now" (upgradeHiveHandler), a bulk upgrade
		// (saas_bulk.go), and a hard image pin (delivered as UpgradeTarget
		// through the stale-recovery branch ABOVE the `if !h.AutoUpgrade` gate)
		// — arms s.heartbeatUpgrade directly and never enters this loop body.
		// So an operator's upgrade and a pin stay IMMEDIATE by construction,
		// and only the merge-frequency-driven roll is held.
		//
		// Placed after every eligibility gate above so a hive that would not
		// upgrade anyway never arms a window, and before the wave gate and the
		// fire-date persistence below so a debounced hive costs no wave slot and
		// keeps its daily/weekly window open.
		debounce := shouldDebounceAutoUpgrade(
			autoUpgradeDebounceState{
				Target:       h.AutoUpgradePendingTarget,
				ArmedAt:      h.AutoUpgradePendingSince,
				FirstArmedAt: h.AutoUpgradePendingFirst,
				Collapsed:    h.AutoUpgradeCollapsed,
			},
			latestSHA, autoUpgradeDebounceInterval(), autoUpgradeMaxHold(), time.Now())
		if !debounce.Allowed {
			// Persist the (possibly just-replaced) pending target so a hub
			// restart inside the window resumes it rather than dropping it.
			s.persistUpgradeDebounceState(&h, debounce.State)
			s.logger.Info("auto-upgrade debounced — holding for a quiet branch",
				"hive_id", h.ID, "branch", branch,
				"target", debounce.State.Target, "current", currentSHA,
				"collapsed", debounce.State.Collapsed,
				"debounce", autoUpgradeDebounceInterval(),
				"reason", debounce.Reason)
			continue
		}
		if debounce.Collapsed > 0 {
			// Report the collapse. Silent batching would trade one invisible
			// problem for another: without this line, N merges producing one
			// roll is indistinguishable from N-1 upgrades having been lost.
			s.logger.Info("auto-upgrade debounce collapsed a merge burst into one roll",
				"hive_id", h.ID, "branch", branch,
				"merges_collapsed", debounce.Collapsed+1,
				"final_target", latestSHA, "current", currentSHA,
				"debounce", autoUpgradeDebounceInterval())
		}
		hiveCluster := s.clusterForHive(&h)
		if hiveCluster == nil {
			s.logger.Warn("auto-upgrade skipped — no cluster config", "hive_id", h.ID, "cluster_id", h.ClusterID)
			continue
		}
		// Do not ARM an upgrade this hive cannot COLLECT. Delivery is PULL: the
		// spoke reads UpgradeTo off its own outbound heartbeat response and then
		// patches its own Deployment with its own ServiceAccount. A hive that
		// never heartbeats therefore never picks the instruction up, while
		// Upgrading=true latches on the hub: the stale-recovery branch above
		// re-arms every staleUpgradeTimeout, beginUpgrade() preserves the
		// original start clock, and the elapsed grows without bound. The orphan
		// sweep's retry budget cannot rescue it — a hive that never heartbeated
		// fails evaluateOrphanedUpgrade()'s liveness test, so the budget is never
		// spent and exhaustion never converts it to a visible failure. See
		// pullonly_upgrade.go for the full measured loop.
		//
		// This is deliberately NOT gated on cluster reachability. The hub's
		// kubectl path is only a fast-path optimisation, so a pull-only cluster
		// is irrelevant here; gating on it would silently disable auto-upgrade
		// for the 40+ pull-only spokes that heartbeat perfectly well.
		//
		// Refused LOUDLY, never silently: a hive with auto_upgrade=true that
		// simply never upgrades is indistinguishable from one already at latest,
		// which is how this stayed unnoticed.
		if !upgradeCollectible(lastHeartbeat, time.Now()) {
			reason := uncollectibleUpgradeReason(lastHeartbeat)
			s.logger.Warn("auto-upgrade not armed — hive cannot collect the instruction",
				"hive_id", h.ID, "cluster", hiveCluster.ID, "branch", branch,
				"from", currentSHA, "would_have_targeted", latestSHA,
				"last_heartbeat", orDash(lastHeartbeat), "reason", reason)
			s.noteUncollectibleUpgrade(h.ID, latestSHA, reason)
			continue
		}
		// Wave gate — evaluated AFTER every eligibility check so a slot is
		// only ever spent on a hive that would actually arm, and BEFORE the
		// fire-date persistence so a deferred daily/weekly hive keeps its
		// window open and simply boards a later wave this same day.
		if waveSize > 0 && upgradingByCluster[hiveCluster.ID] >= waveSize {
			s.logger.Debug("auto-upgrade deferred — cluster upgrade wave is full",
				"hive_id", h.ID, "cluster", hiveCluster.ID,
				"in_flight", upgradingByCluster[hiveCluster.ID], "wave_size", waveSize)
			continue
		}
		upgradingByCluster[hiveCluster.ID]++
		// Record the day's fire BEFORE kicking the rollout. Persisting first
		// means a hub crash between here and the restart cannot cause a second
		// upgrade for the same ET day; at worst the hive waits for tomorrow's
		// window, which is the conservative direction for a "don't disturb it"
		// mode. Only daily-mode hives carry a fire date (decision.FireDate is
		// empty for instant), and a save failure is logged but not fatal — the
		// upgrade itself still proceeds.
		if decision.FireDate != "" {
			stored := loadSaaSHive(h.ID)
			if stored == nil {
				stored = &h
			}
			stored.AutoUpgradeLastFired = decision.FireDate
			if err := saveSaaSHive(stored); err != nil {
				s.logger.Warn("failed to persist auto-upgrade fire date — a hub restart today could re-fire",
					"hive_id", h.ID, "date", decision.FireDate, "error", err)
			}
		}
		// Clear the debounce record now the roll is actually going out. Cleared
		// HERE, after every gate that could still `continue`, so a hive turned
		// away by the wave gate keeps its pending target and simply boards a
		// later wave instead of re-arming a fresh window each cycle. Clearing
		// before the rollout (like the fire date above) means a hub crash in
		// between costs at most a re-armed window, never a duplicate roll.
		s.persistUpgradeDebounceState(&h, autoUpgradeDebounceState{})
		// The hive is deliverable again — drop any suppressed-refusal memory so a
		// future undeliverable episode is reported afresh rather than swallowed.
		s.forgetUncollectibleUpgrade(h.ID)
		s.logger.Info("audit: auto-upgrade triggered", "hive_id", h.ID, "branch", branch, "from", currentSHA, "to", latestSHA, "cluster", hiveCluster.ID, "mode", normalizeAutoUpgradeMode(h.AutoUpgradeMode))
		s.recordTimeline(h.ID, TimelineUpgradeStarted,
			fmt.Sprintf("auto-upgrade triggered on %s: %s → %s", branch, orDash(currentSHA), latestSHA), "auto-upgrade")
		s.mu.Lock()
		for i := range s.registry.Hives {
			if s.registry.Hives[i].ID == h.ID {
				s.beginUpgrade(i, latestSHA)
				break
			}
		}
		s.mu.Unlock()
		// PULL ONLY — no kubectl push. Arming the heartbeat is the delivery:
		// the spoke reads UpgradeTo off its next heartbeat response and patches
		// its own Deployment with its own ServiceAccount (cmd/hive/main.go →
		// self_upgrade.go). The former `rolloutRestartHive` call here was only a
		// latency optimisation, and it is deliberately gone: keeping it would
		// require the hub to hold write-capable kubeconfigs into every spoke
		// cluster, which is a large standing blast radius for a few seconds of
		// speed. See the push-path retirement note in pullonly_upgrade.go.
		//
		// The trade is real and accepted: delivery is now bounded by the
		// heartbeat interval rather than being immediate.
		s.mu.Lock()
		s.heartbeatUpgrade[h.ID] = latestSHA
		// Keep Upgrading=true so the dashboard shows the correct state.
		s.mu.Unlock()
	}
}

var provisionRequestsDir = "/data/saas/provision-requests"

const (
	provisionStatusPending  = "pending"
	provisionStatusApproved = "approved"
	provisionStatusDenied   = "denied"
)

const maxProvisionRequestBodyBytes = 4 * 1024

type ProvisionRequest struct {
	Username string `json:"username"`
	// UserID is the human-facing identifier admins should use when reviewing
	// the request. Username remains the stable auth key/native provider subject
	// (for example "ibmid:695000VVZ9"); UserID captures the meaningful login or
	// identity available at request time so the review queue does not headline
	// opaque SSO subjects. Empty on older records; enrichProvisionRequests fills
	// it from the user record when possible, and the UI falls back to Username.
	UserID       string `json:"user_id,omitempty"`
	UserIDSource string `json:"user_id_source,omitempty"`
	// GitHubHost is the GitHub instance the org lives on — empty means public
	// github.com, otherwise a GitHub Enterprise host (github.ibm.com,
	// github.cisco.com, …). Captured so an admin can see which instance a
	// request targets before deciding where to place the hive.
	GitHubHost  string `json:"github_host,omitempty"`
	Org         string `json:"org"`
	Repos       string `json:"repos"`
	PrimaryRepo string `json:"primary_repo"`
	ACMMLevel   int    `json:"acmm_level"`
	AuthMethod  string `json:"auth_method"`
	RequestedAt string `json:"requested_at"`
	Status      string `json:"status"`

	// Who is asking, as a person rather than as a GitHub login. Both reuse the
	// SaaSUser contact fields of the same name and the same caps
	// (maxContactNameLen / maxContactSlackIDLen) — deliberately NOT new keys.
	// A second Slack key would split one fact across two names, with the
	// request form writing one and the admin users panel reading the other.
	//
	// These are captured here and copied onto the SaaSUser record on approval
	// (handleApproveProvision); asking and then dropping the answer would be
	// worse than not asking. Both are omitempty so requests filed before these
	// fields existed round-trip unchanged.
	FullName string `json:"full_name,omitempty"`
	SlackID  string `json:"slack_id,omitempty"`

	// Country is the requester's OPTIONAL self-declared ISO 3166-1 alpha-2
	// code, picked from the wizard's dropdown. Like the two fields above it
	// reuses the SaaSUser key of the same name and is copied onto the user
	// record on approval (applyRequestContactToUser) — asking and then dropping
	// the answer would be worse than not asking.
	//
	// This is the AUTHORITATIVE source of a user's country: they chose it about
	// themselves. The Accept-Language inference on the login path is only a
	// fallback for records that never got one. omitempty so requests filed
	// before this field existed round-trip unchanged.
	Country string `json:"country,omitempty"`

	// Decision audit. Previously a request only carried its final Status, so
	// once it left "pending" there was no record of WHO decided, WHEN, or —
	// for an approval — which hive the requester actually got. That made the
	// history unauditable: an approved request and a denied one looked equally
	// anonymous. Empty on records decided before these fields existed.
	DecidedBy string `json:"decided_by,omitempty"`
	// DecidedByName is the display-only label for DecidedBy, resolved on read.
	DecidedByName string `json:"decided_by_name,omitempty"`
	DecidedAt     string `json:"decided_at,omitempty"`
	AssignedHive  string `json:"assigned_hive,omitempty"`
	// DenyReason is the optional free-text explanation shown back to the
	// requester when a request is turned down.
	DenyReason string `json:"deny_reason,omitempty"`

	// --- Derived, never persisted ---
	// These are filled in on read (see enrichProvisionRequests) so the admin
	// Past Requests table can show the requester's role on the hive they were
	// given, plus the rest of their fleet, without one API call per row. They
	// are omitempty so records written before they existed stay valid and so a
	// round-trip through saveProvisionRequest never bakes stale roles onto disk.
	//
	// AssignedRole is the requester's role on AssignedHive, read from their
	// SaaSUser.Hives map — the authoritative grant (see accessForHive). Empty
	// when the request was denied, when no hive was assigned, or when the grant
	// was since revoked.
	AssignedRole string `json:"assigned_role,omitempty"`
	// OtherHives is every OTHER hive the requester can sign in to, with their
	// role on each — the person's footprint beyond this one request. Sorted
	// owners-first then by hive ID for a stable render.
	OtherHives []UserHiveRole `json:"other_hives,omitempty"`
}

// roleOwner is the role string stored in SaaSUser.Hives for the hive's owner.
// Named so the owners-first sort below does not repeat a bare literal.
const roleOwner = "owner"

// UserHiveRole is one hive a user can sign in to and the role they hold on it.
// The mirror image of HiveAccessEntry: that answers "who is on this hive", this
// answers "which hives is this user on".
type UserHiveRole struct {
	HiveID string `json:"hive_id"`
	Role   string `json:"role"`
}

// hivesForUser returns every hive the named user can sign in to, with their
// role on each, optionally excluding one hive ID (the one already shown in its
// own column). Access comes from SaaSUser.Hives — the authoritative grant that
// handleApproveProvision / handleAssignHive write — not from hive.Owner, which
// can name someone whose grant was revoked.
//
// users is passed in rather than read here so a caller enriching many rows can
// read the roster ONCE: listAllSaaSUsers hits the filesystem per user record.
func hivesForUser(username string, excludeHiveID string, users []SaaSUser) []UserHiveRole {
	if username == "" {
		return nil
	}
	out := make([]UserHiveRole, 0)
	for _, u := range users {
		if !strings.EqualFold(u.GitHubUsername, username) {
			continue
		}
		for id, role := range u.Hives {
			if id == "" || id == excludeHiveID {
				continue
			}
			out = append(out, UserHiveRole{HiveID: id, Role: role})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		// Owned hives first — the useful line to read when scanning someone's
		// footprint — then alphabetical by hive ID for a stable order.
		if (out[i].Role == roleOwner) != (out[j].Role == roleOwner) {
			return out[i].Role == roleOwner
		}
		return out[i].HiveID < out[j].HiveID
	})
	if len(out) == 0 {
		return nil // omitempty: no cell rather than an empty array
	}
	return out
}

// roleForUserOnHive returns the named user's role on the named hive, or "" if
// they hold no grant on it. Same roster-passed-in contract as hivesForUser.
func roleForUserOnHive(username, hiveID string, users []SaaSUser) string {
	if username == "" || hiveID == "" {
		return ""
	}
	for _, u := range users {
		if !strings.EqualFold(u.GitHubUsername, username) {
			continue
		}
		if role, ok := u.Hives[hiveID]; ok {
			return role
		}
	}
	return ""
}

// provisionRequestUserIdentity chooses the operator-facing identifier for a
// request, plus its source so the UI only links identifiers known to be GitHub
// logins.
// The raw request Username is the auth key and can be an opaque provider subject
// ("ibmid:…"). Prefer a linked GitHub/GHE login when the user has one, then a
// recognizable GitHub login, then email/name claims, and fall back to the auth
// key only when no friendlier identity exists.
func provisionRequestUserIdentity(username string, u *SaaSUser, requestFullName string) (id, source string) {
	if u != nil {
		if id := strings.TrimSpace(u.LinkedGitHubLogin); id != "" {
			return id, "github"
		}
		if provider, subject := splitIdentityKey(strings.TrimSpace(u.GitHubUsername)); provider == "" || normalizeIdentityProvider(provider) == legacyProvider {
			if subject != "" {
				return subject, "github"
			}
		}
		if id := strings.TrimSpace(u.Email); id != "" {
			return id, "email"
		}
		if id := strings.TrimSpace(u.DisplayName); id != "" {
			return id, "name"
		}
		if id := strings.TrimSpace(u.FullName); id != "" {
			return id, "name"
		}
	}
	if id := strings.TrimSpace(requestFullName); id != "" {
		return id, "name"
	}
	if provider, subject := splitIdentityKey(strings.TrimSpace(username)); normalizeIdentityProvider(provider) == legacyProvider && subject != "" {
		return subject, "github"
	}
	return strings.TrimSpace(username), "native"
}

func provisionRequestUserID(username string, u *SaaSUser, requestFullName string) string {
	id, _ := provisionRequestUserIdentity(username, u, requestFullName)
	return id
}

func provisionRequestUserFromRoster(username string, users []SaaSUser) *SaaSUser {
	for i := range users {
		if strings.EqualFold(users[i].GitHubUsername, username) {
			return &users[i]
		}
	}
	return nil
}

// enrichProvisionRequests fills in the derived AssignedRole / OtherHives fields
// on every request in place.
//
// ADMIN-ONLY: this exposes other people's hive memberships, so it must only be
// called on a payload already gated behind requireAdmin (or the isAdmin branch
// of the dashboard handler). Do not call it on a per-user response.
//
// The roster is read once here — O(1) filesystem sweeps for the whole table
// rather than O(rows) — because listAllSaaSUsers walks and unmarshals every
// user record on disk.
func enrichProvisionRequests(requests []ProvisionRequest) []ProvisionRequest {
	if len(requests) == 0 {
		return requests
	}
	users := listAllSaaSUsers()
	label := (&HubServer{}).identityLabeler()
	for i := range requests {
		if requests[i].UserID == "" {
			requests[i].UserID, requests[i].UserIDSource = provisionRequestUserIdentity(requests[i].Username, provisionRequestUserFromRoster(requests[i].Username, users), requests[i].FullName)
		} else if requests[i].UserIDSource == "" {
			if requests[i].UserID == requests[i].Username {
				requests[i].UserIDSource = "native"
			}
		}
		if l := label(requests[i].DecidedBy); l != requests[i].DecidedBy {
			requests[i].DecidedByName = l
		}
		requests[i].AssignedRole = roleForUserOnHive(requests[i].Username, requests[i].AssignedHive, users)
		requests[i].OtherHives = hivesForUser(requests[i].Username, requests[i].AssignedHive, users)
	}
	return requests
}

// applyRequestContactToUser carries the requester's contact details from an
// approved provision request onto their SaaSUser record.
//
// This is what makes asking for them worth anything. The admin users panel and
// the Slack sender both read SaaSUser, NOT ProvisionRequest — a value that
// stops at the request file is invisible to every consumer of it. Slack
// messaging shipped with no user having a slack_id, settable only by hand one
// user at a time; approval is the natural point of capture.
//
// It only ever FILLS A BLANK. An admin who has already curated these fields (or
// a user who corrected them later) outranks whatever was typed into a request
// form, and a re-approval must not silently revert that.
//
// Nil-safe on both sides: it runs on the approval path beside other work that
// can legitimately leave either side absent, and must not panic there.
func applyRequestContactToUser(user *SaaSUser, pr *ProvisionRequest) {
	if user == nil || pr == nil {
		return
	}
	if user.FullName == "" && pr.FullName != "" {
		user.FullName = truncateRunes(strings.TrimSpace(pr.FullName), maxContactNameLen)
	}
	if user.SlackID == "" && pr.SlackID != "" {
		user.SlackID = truncateRunes(strings.TrimSpace(pr.SlackID), maxContactSlackIDLen)
	}
	// The explicit pick outranks anything Accept-Language inferred at login, so
	// unlike the two fields above this one overwrites a value already on the
	// record — but only when the request actually carries a choice. Re-normalize
	// rather than trusting the stored request: it may predate the validation.
	if code := normalizeCountryCode(pr.Country); code != "" {
		// The wizard pick is a deliberate statement BY THE USER, so it carries
		// the same provenance the self-service endpoint stamps. Without this,
		// an approval would leave the record looking "inferred", and the
		// priority rule would hold only by the accident of the value being
		// non-empty.
		setUserCountry(user, code, countrySourceUser)
	}
}

func loadProvisionRequest(username string) *ProvisionRequest {
	if strings.Contains(username, "..") || strings.Contains(username, "/") || strings.Contains(username, "\\") {
		return nil
	}
	path := filepath.Join(provisionRequestsDir, username+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var pr ProvisionRequest
	if json.Unmarshal(data, &pr) != nil {
		return nil
	}
	return &pr
}

func saveProvisionRequest(pr *ProvisionRequest) error {
	// Same traversal guard as loadProvisionRequest/deleteProvisionRequest (and
	// SaveUser): the username becomes a filename, so a value carrying "..", "/"
	// or "\" must never reach filepath.Join. Auth'd usernames cannot normally
	// contain these (makeCanonical rejects them), but the write path fails
	// closed rather than trusting every future caller to have checked.
	if strings.Contains(pr.Username, "..") || strings.Contains(pr.Username, "/") || strings.Contains(pr.Username, "\\") {
		return fmt.Errorf("invalid username for provision request: %q", pr.Username)
	}
	// Best-effort: a failed mkdir surfaces via the WriteFile error below.
	_ = os.MkdirAll(provisionRequestsDir, 0o755)
	data, err := json.MarshalIndent(pr, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(provisionRequestsDir, pr.Username+".json")
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func deleteProvisionRequest(username string) {
	if strings.Contains(username, "..") || strings.Contains(username, "/") || strings.Contains(username, "\\") {
		return
	}
	if err := os.Remove(filepath.Join(provisionRequestsDir, username+".json")); err != nil && !os.IsNotExist(err) {
		slog.Warn("deleteProvisionRequest: remove failed", "user", username, "error", err)
	}
}

func listProvisionRequests() []ProvisionRequest {
	// Best-effort: a failed mkdir surfaces via the ReadDir error below.
	_ = os.MkdirAll(provisionRequestsDir, 0o755)
	entries, err := os.ReadDir(provisionRequestsDir)
	if err != nil {
		return nil
	}
	var result []ProvisionRequest
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		uname := strings.TrimSuffix(e.Name(), ".json")
		pr := loadProvisionRequest(uname)
		// Return decided requests too, not just pending ones — the admin view
		// splits them into a pending action queue and a decision-history table,
		// and filtering here made the history permanently empty.
		if pr != nil {
			result = append(result, *pr)
		}
	}
	return result
}

func (s *HubServer) handleRequestProvision(w http.ResponseWriter, r *http.Request) {
	username := s.getAuthUser(r)
	if username == "" {
		http.Error(w, `{"error":"not authenticated"}`, http.StatusUnauthorized)
		return
	}

	existing := loadProvisionRequest(username)
	if existing != nil && existing.Status == provisionStatusPending {
		http.Error(w, `{"error":"you already have a pending provision request"}`, http.StatusBadRequest)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxProvisionRequestBodyBytes)
	var body struct {
		Org         string `json:"org"`
		GitHubHost  string `json:"github_host"`
		Repos       string `json:"repos"`
		PrimaryRepo string `json:"primary_repo"`
		ACMMLevel   int    `json:"acmm_level"`
		AuthMethod  string `json:"auth_method"`
		FullName    string `json:"full_name"`
		SlackID     string `json:"slack_id"`
		Country     string `json:"country"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if body.Org == "" || body.Repos == "" {
		http.Error(w, `{"error":"org and repos are required"}`, http.StatusBadRequest)
		return
	}
	// Contact details for the person asking.
	//
	// No charset validation on purpose: isValidName() is for identifiers
	// (org/repo/host names) and would reject spaces, apostrophes and every
	// non-ASCII script. These are display strings — trimmed, rune-capped and
	// escaped at every render site, exactly as the admin contact editor
	// (handleAdminUpdateUser) already treats these same two fields. We also do
	// NOT try to detect a "real" name: any pattern for that (two words,
	// capitalised, Latin script) is wrong for a large fraction of the world's
	// names, and a determined user types junk regardless. Human review is the
	// check, not a regex.
	body.FullName = truncateRunes(strings.TrimSpace(body.FullName), maxContactNameLen)
	body.SlackID = truncateRunes(strings.TrimSpace(body.SlackID), maxContactSlackIDLen)
	// Country is OPTIONAL and normalized rather than rejected: a malformed or
	// absent code stores "", which renders no flag at all. Validating here (the
	// last point before the PVC) means the render sites can trust that a stored
	// country is two uppercase letters, and a client that never sends the field
	// behaves exactly as before this shipped.
	body.Country = normalizeCountryCode(body.Country)
	// Accept a pasted org/repo URL, not just a bare name. Users read
	// "GitHub Organization" and paste the org's URL; the old validator rejected
	// ":" and "/" and returned a bare "invalid org name" that explained nothing.
	// The host is kept so the admin can see whether a request is for github.com
	// or a GitHub Enterprise instance before placing the hive.
	ghHost, orgName := normalizeOrgRef(body.Org)
	body.Org = orgName
	if ghHost != "" {
		body.GitHubHost = ghHost
	}
	// The forge is REQUIRED. A bare org name ("z-innersource") does not say
	// whether the org lives on github.com or on a GitHub Enterprise instance,
	// and the hub cannot guess: the wrong choice provisions the hive against
	// the wrong GitHub and points the App-install link at a forge the org
	// admin never sees. Normalize FIRST — the field accepts
	// "https://github.ibm.com/" and isValidName rejects ":" and "/", so
	// validating the raw value would reject a form the form itself advertises.
	forgeHost, ok := normalizeForgeHost(body.GitHubHost)
	if !ok {
		http.Error(w, `{"error":"a GitHub forge is required — enter the host your org lives on (e.g. github.com or github.ibm.com); without it we cannot tell which GitHub to provision against"}`, http.StatusBadRequest)
		return
	}
	body.GitHubHost = forgeHost
	// Single-host-per-spoke: every repo (and the primary) must be on the same
	// GitHub host as the org. Check BEFORE normalizeRepoRef strips the host off
	// each pasted repo. Reject a mixed request up front with a clear message —
	// a spoke that mixed github.com and a GHE instance would silently fail to
	// authenticate against half its repos, the onboarding footgun this removes.
	if err := validateSingleRepoHost(body.GitHubHost, body.PrimaryRepo, strings.Split(body.Repos, ",")); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
		return
	}
	{
		var cleaned []string
		for _, repo := range strings.Split(body.Repos, ",") {
			if r := normalizeRepoRef(repo); r != "" {
				cleaned = append(cleaned, r)
			}
		}
		body.Repos = strings.Join(cleaned, ",")
		body.PrimaryRepo = normalizeRepoRef(body.PrimaryRepo)
	}
	if !isValidName(body.Org) {
		http.Error(w, fmt.Sprintf(`{"error":"invalid org name %q — use the org name or its URL (e.g. github.ibm.com/my-org)"}`, body.Org), http.StatusBadRequest)
		return
	}
	// Defence in depth: normalizeForgeHost above already guaranteed this, but
	// keep the check so a future edit that reorders the handler cannot let an
	// unvalidated host reach the stored request.
	if !isValidName(body.GitHubHost) {
		http.Error(w, `{"error":"invalid github host"}`, http.StatusBadRequest)
		return
	}
	for _, repo := range strings.Split(body.Repos, ",") {
		if !isValidRepoRef(strings.TrimSpace(repo)) {
			http.Error(w, `{"error":"invalid repo name"}`, http.StatusBadRequest)
			return
		}
	}

	// Name is REQUIRED; Slack ID is optional.
	//
	// The name exists to map a GitHub login to a person an operator can talk
	// to, which only works if it is actually populated — a field that is
	// usually blank cannot be relied on and stops being read. Requesting a hive
	// is a deliberate, one-per-user action already reviewed by a human, so one
	// short field is negligible friction at the moment the requester is most
	// motivated to answer.
	//
	// Checked AFTER the org/forge/repo validation above so the more specific
	// "which GitHub is this org on?" errors keep their precedence — a request
	// missing both should be told about the forge, not sent round the loop one
	// field at a time.
	//
	// This does tighten an existing endpoint: a caller that posted no full_name
	// now gets a 400 where it used to get a 200. That is intended — the whole
	// point is that the field is reliably present.
	//
	// The get-started wizard (static/get-started.html) is now the only in-tree
	// caller, but do not read that as "it always was". When this check landed
	// (#2369) this comment claimed the wizard was the only caller and it was
	// simply wrong: the hub dashboard had its own Request-a-Hive modal posting
	// here, added later than the wizard, reachable by every logged-in user, and
	// it went un-updated — so the button 400'd with no field on screen that
	// could satisfy it. The modal has since been removed deliberately (the
	// wizard is the single supported request path), which is what makes this
	// sentence true today rather than aspirational.
	//
	// The modal was invisible to CI because it was inline JS inside the
	// dashboardHTML raw string with no test naming any of its symbols. Before
	// adding another required field here, re-run the caller audit rather than
	// trusting this comment: TestRequestProvisionInTreeCallersSendRequiredFields
	// greps the embedded JS and the static wizard for callers of this endpoint
	// and fails on one that omits a required field.
	if body.FullName == "" {
		http.Error(w, `{"error":"your name is required — we use it to know who the request is from"}`, http.StatusBadRequest)
		return
	}

	// A hive is never REQUESTED above L3 Quality-Gated. L4-L6 are real levels, but
	// they are reached after provisioning, from the hive's own dashboard, once
	// the project has the coverage and CI history to justify them. The
	// get-started wizard only offers L1-L3, but the wizard is one client: clamp
	// here so a crafted request cannot provision straight into auto-merge.
	// Admin paths (assign/provision) keep the full 0..6 range on purpose — an
	// operator setting a level deliberately is not the case being guarded.
	acmm := body.ACMMLevel
	if acmm < minRequestACMMLevel || acmm > maxRequestACMMLevel {
		acmm = minRequestACMMLevel
	}

	primaryRepo := body.PrimaryRepo
	if primaryRepo == "" {
		repos := strings.Split(body.Repos, ",")
		if len(repos) > 0 {
			primaryRepo = strings.TrimSpace(repos[0])
		}
	}

	userID, userIDSource := provisionRequestUserIdentity(username, loadSaaSUser(username), body.FullName)
	pr := &ProvisionRequest{
		Username:     username,
		UserID:       userID,
		UserIDSource: userIDSource,
		GitHubHost:   body.GitHubHost,
		Org:          body.Org,
		Repos:        body.Repos,
		PrimaryRepo:  primaryRepo,
		ACMMLevel:    acmm,
		AuthMethod:   body.AuthMethod,
		FullName:     body.FullName,
		SlackID:      body.SlackID,
		Country:      body.Country,
		RequestedAt:  time.Now().UTC().Format(time.RFC3339),
		Status:       provisionStatusPending,
	}
	if err := saveProvisionRequest(pr); err != nil {
		http.Error(w, `{"error":"failed to save provision request"}`, http.StatusInternalServerError)
		return
	}

	s.logger.Info("audit: provision request created", "user", username, "org", body.Org, "repos", body.Repos)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "status": provisionStatusPending})
}

// ApproveProvisionRequest is the OPTIONAL body of PUT
// /api/saas/approve-provision/{username}. HiveID lets the admin pick the exact
// available placeholder to assign (from the approve-picker modal); an empty or
// absent HiveID preserves the historical auto-pick behavior.
type ApproveProvisionRequest struct {
	HiveID string `json:"hive_id"`
	// GitHubHost is the admin's explicit choice of GitHub instance for this
	// hive, overriding the one on the provision request. "public" forces public
	// github.com even on a GitHub Enterprise cluster; empty means "use the
	// request's host, else the cluster default".
	GitHubHost string `json:"github_host,omitempty"`
}

func (s *HubServer) handleApproveProvision(w http.ResponseWriter, r *http.Request) {
	targetUsername := r.PathValue("username")
	approver := s.getAuthUser(r)

	pr := loadProvisionRequest(targetUsername)
	if pr == nil || pr.Status != provisionStatusPending {
		http.Error(w, `{"error":"no pending provision request for this user"}`, http.StatusNotFound)
		return
	}

	// Optionally the admin picks the EXACT placeholder to assign (from the
	// approve-picker modal) instead of letting the hub auto-pick. The body is
	// tolerated as absent/empty — an empty hive_id preserves the historical
	// auto-pick behavior. The body is tiny (a single id), so cap it small.
	const maxApproveRequestBodyBytes = 1 * 1024
	var approveBody ApproveProvisionRequest
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, maxApproveRequestBodyBytes)
		// An empty body is valid (auto-pick); ignore EOF/empty decode errors and
		// fall through to auto-pick. Any non-empty malformed body is rejected.
		if err := json.NewDecoder(r.Body).Decode(&approveBody); err != nil && err != io.EOF {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
	}

	// Approving now ASSIGNS an available placeholder instead of just bumping
	// quota for a manual provision. Pick the pool from the request's auth_method
	// (private → the heartbeat-only cluster / GPU pool, otherwise → the hub-reachable cluster / public pool). If no
	// placeholder is available in that pool, tell the admin to provision more.
	pool := poolClusterForAuthMethod(pr.AuthMethod)
	hiveID := strings.TrimSpace(approveBody.HiveID)
	if hiveID != "" {
		// Admin chose a specific placeholder — validate it is an available
		// placeholder (same check the assign path uses) before using it. The
		// full status recheck under loadSaaSHive below still guards the race.
		if sel := loadSaaSHive(hiveID); sel == nil || sel.Status != statusAvailable || !isHubAdmin(sel.Owner) {
			http.Error(w, `{"error":"selected placeholder is not available"}`, http.StatusConflict)
			return
		}
	} else {
		hiveID = findAvailablePlaceholder(pool)
	}
	if hiveID == "" {
		http.Error(w, fmt.Sprintf(`{"error":"no available placeholder hive in pool %q — provision more placeholders"}`, pool), http.StatusConflict)
		return
	}

	h := loadSaaSHive(hiveID)
	if h == nil || h.Status != statusAvailable {
		// Raced with another assignment between selection and load.
		http.Error(w, `{"error":"selected placeholder became unavailable — retry"}`, http.StatusConflict)
		return
	}

	// Reuse the request's org/repos/primary_repo/acmm as the assignment inputs.
	var repos []string
	for _, repo := range strings.Split(pr.Repos, ",") {
		if repo = strings.TrimSpace(repo); repo != "" {
			repos = append(repos, repo)
		}
	}
	primaryRepo := pr.PrimaryRepo
	if primaryRepo == "" && len(repos) > 0 {
		primaryRepo = repos[0]
	}
	acmm := pr.ACMMLevel
	if acmm == 0 {
		acmm = defaultAssignACMMLevel
	}
	if acmm < minAssignACMMLevel || acmm > maxAssignACMMLevel {
		acmm = defaultAssignACMMLevel
	}

	// Rewrite the placeholder's meta.json to the requesting user's real project.
	// Flipping status to statusAssigned makes it show under the new owner in My
	// Hives AND marks it as no-longer-available for the fleet counters; the
	// project config reaches the spoke via the heartbeat channel
	// (projectConfigForHiveID).
	h.Owner = targetUsername
	h.Org = pr.Org
	h.Repos = repos
	h.PrimaryRepo = primaryRepo
	h.ACMMLevel = acmm
	// Record the level as REQUESTED, not merely as current. ACMMLevel is
	// overwritten the moment the spoke reports the level it was minted at, so it
	// cannot be the source of truth for "what did the owner ask for". Keeping the
	// requested value in its own field is what lets the delivery reconcile below
	// remain correct — and idempotent — across any number of heartbeats.
	h.RequestedACMMLevel = acmm
	// Re-arm the level handshake for this claim. A placeholder is reused across
	// assignments, so a flag left true by a previous tenancy would suppress
	// delivery for the new owner.
	h.ACMMDelivered = false
	// Re-arm the org/repos claim handshake too (#2372). ClaimDelivered gates
	// both halves of adoptSpokeProjectConfig: the org/repos PUSH fires only
	// while !ClaimDelivered, and the spoke's report is ADOPTED only once it is
	// true. A RECYCLED placeholder (previously claimed, returned to the pool)
	// carries the prior tenant's ClaimDelivered=true, so without this reset the
	// hub never pushes the new owner's org/repos AND adopts the spoke's stale
	// self-report — hub and spoke silently agree on the PREVIOUS tenant's
	// project. The heartbeat is the only hub->spoke write channel, so the claim
	// must re-arm on (re)assignment for exactly the same reason ACMMDelivered
	// does above. (The assign path — handleAssignHive — already does this.)
	h.ClaimDelivered = false
	h.Status = statusAssigned
	// Stamp when this claim began so the self-heal sweep can age it out if the
	// spoke never reports the project back (ClaimDelivered stuck false).
	h.AssignedAt = time.Now().UTC().Format(time.RFC3339)
	h.Error = ""
	// Preserve the placeholder's real cluster before ANY cluster-derived
	// resolution below (host backfill uses s.clusterForHive(h), which silently
	// returns the hub-reachable cluster when ClusterID is blank). The placeholder was picked
	// from `pool`, so a blank cluster_id here can only mean the placeholder was
	// created without one — stamp the pool it came from rather than leaving a
	// blank that later resolves to the wrong (default) cluster's App/host.
	s.ensureClusterIDForClaim(h, pool)
	// The requester already told us which GitHub their org lives on (parsed
	// from the org URL they pasted, or picked explicitly), so honour it. Before
	// this, approve dropped github_host entirely — only the manual assign path
	// ever set it — and a GHE request approved through this path produced a
	// hive with a blank host talking to api.github.com. An override the admin
	// chose in the approve modal arrives on the request body below and wins.
	if host := strings.TrimSpace(approveBody.GitHubHost); host != "" {
		if !isValidName(host) && !strings.EqualFold(host, githubHostPublic) {
			http.Error(w, `{"error":"invalid github host"}`, http.StatusBadRequest)
			return
		}
		// An explicit "public" choice means public github.com. Record it as a
		// blank host so forgeAPIURLForHost pushes nothing and the spoke keeps its
		// own api.github.com default — and so the cluster backfill below, which
		// only ever fills a blank, does not silently re-GHE it.
		if strings.EqualFold(host, githubHostPublic) {
			// Record public github.com EXPLICITLY. This used to store a blank
			// host plus the "public" sentinel, because a blank was the only way
			// to stop backfillGitHubHostFromCluster re-GHE-ing the hive on the
			// next line. Storing the real host achieves the same thing — that
			// backfill only ever fills a value that is EMPTY — without leaving
			// an absent field that means "public" by implication.
			//
			// An absent field is what hid the 2026-07-31 incident and what left
			// 25 of 50 hub records with no github_host at all. github_host is
			// the single stored input for a hive's identity (#2386); it should
			// never be the one field we deliberately leave blank.
			h.GitHubHost = publicForgeHost
		} else {
			h.GitHubHost = host
		}
	} else if pr.GitHubHost != "" {
		// The request itself named a host. Honour a "public" sentinel here the SAME
		// way the admin override does: the self-service onboarding form now sends
		// "public" for an explicit github.com choice (never a blank), so a
		// github.com request must be pinned public — NOT stored as the literal host
		// "public", and NOT left blank for the cluster backfill below to re-GHE.
		if strings.EqualFold(pr.GitHubHost, githubHostPublic) {
			// Same as the admin-override branch above: store the real host, not
			// a blank plus the sentinel. The literal string "public" must never
			// be stored as a host — it is a request-time marker, not a hostname,
			// and a hive naming it resolves to no forge at all.
			h.GitHubHost = publicForgeHost
		} else {
			h.GitHubHost = pr.GitHubHost
		}
	}
	// Same cluster backfill the manual assign path does: when neither the admin
	// nor the request named a host, inherit the cluster's GHE default rather
	// than leaving a blank that pushes an empty API URL forever.
	if host := backfillGitHubHostFromCluster(h, s.clusterForHive(h)); host != "" {
		h.GitHubHost = host
		s.logger.Info("backfilled hive github host from cluster defaults",
			"hive", hiveID, "github_host", host)
	}
	if err := saveSaaSHive(h); err != nil {
		http.Error(w, `{"error":"failed to assign placeholder hive"}`, http.StatusInternalServerError)
		return
	}

	// Entry point 2/3 for namespace identity: this is the moment a placeholder
	// gets a real owner/org — the hive's identity is known here for the first
	// time, so the namespace's labels/annotations must be (re)written. That
	// stamp is kubectl against the hive's cluster, so it runs in the BACKGROUND
	// via kickClaimClusterWorkAsync below — inline it held the approve dialog's
	// fetch behind up to 2×15s of dial timeouts against an unreachable cluster
	// (the same request-path disease #2730 cured on the heartbeat path).

	// Ensure the user record exists, grant them owner access, and count this
	// owned hive against a quota.
	//
	// Granting Hives[hiveID] is what actually puts the requester on the hive's
	// permissions: handleAccessList builds the access list by scanning every
	// user record for Hives[hiveID], NOT from h.Owner. Without this the
	// assignment set h.Owner correctly but the new owner never appeared under
	// Manage Access — only the admin who provisioned the placeholder did — and
	// on a heartbeat-only cluster (the heartbeat-only cluster) that stale list is what gets
	// delivered to the spoke.
	user := loadSaaSUser(targetUsername)
	if user == nil {
		user = ensureSaaSUser(targetUsername)
	}
	if user.Hives == nil {
		user.Hives = map[string]string{}
	}
	user.Hives[hiveID] = "owner"
	user.SaaSQuota++
	applyRequestContactToUser(user, pr)
	if err := saveSaaSUser(user); err != nil {
		s.logger.Warn("assigned placeholder but failed to grant owner access", "user", targetUsername, "hive", hiveID, "error", err)
	}

	// Mark the request fulfilled, recording who approved it and which hive the
	// requester actually received — that pairing is the whole point of the
	// history table, and it is unrecoverable after the fact if not stored now.
	pr.Status = provisionStatusApproved
	pr.DecidedBy = approver
	pr.DecidedAt = time.Now().UTC().Format(time.RFC3339)
	pr.AssignedHive = hiveID
	if err := saveProvisionRequest(pr); err != nil {
		s.logger.Warn("assigned placeholder but failed to update provision request", "user", targetUsername, "error", err)
	}

	s.logger.Info("audit: provision request approved via placeholder assignment",
		"target", targetUsername, "by", approver, "org", pr.Org, "repos", pr.Repos,
		"hive_id", hiveID, "cluster", clusterIDForHive(h))

	// Everything the response depends on is persisted above — all fast local
	// ops. The cluster-facing side effects (namespace identity stamp; vanity
	// mint, which this path previously left to the heartbeat repair) run in the
	// background so the approve dialog gets its ack immediately; the claim
	// itself reaches the spoke over the heartbeat channel regardless.
	s.kickClaimClusterWorkAsync(hiveID)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"status":  provisionStatusApproved,
		"hive_id": hiveID,
	})
}

func (s *HubServer) handleDenyProvision(w http.ResponseWriter, r *http.Request) {
	targetUsername := r.PathValue("username")
	denier := s.getAuthUser(r)

	pr := loadProvisionRequest(targetUsername)
	if pr == nil || pr.Status != provisionStatusPending {
		http.Error(w, `{"error":"no pending provision request for this user"}`, http.StatusNotFound)
		return
	}

	// Retain the record instead of deleting it. Deleting made a denial
	// indistinguishable from a request that was never made — the history table
	// could only ever show approvals, and an admin had no way to answer "did we
	// already turn this person down, and why?". The retained record is what
	// makes a denial re-requestable-but-accountable.
	const maxDenyRequestBodyBytes = 1 * 1024
	var denyBody struct {
		Reason string `json:"reason"`
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, maxDenyRequestBodyBytes)
		_ = json.NewDecoder(r.Body).Decode(&denyBody) // absent body is fine
	}
	pr.Status = provisionStatusDenied
	pr.DecidedBy = denier
	pr.DecidedAt = time.Now().UTC().Format(time.RFC3339)
	pr.DenyReason = strings.TrimSpace(denyBody.Reason)
	if err := saveProvisionRequest(pr); err != nil {
		s.logger.Warn("failed to record provision denial", "target", targetUsername, "error", err)
	}

	s.logger.Info("audit: provision request denied", "target", targetUsername, "by", denier)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// authMethodPrivate is the request auth_method value that routes a provision
// request to the private/GPU placeholder pool (the heartbeat-only cluster). Any other value (the
// default) routes to the public pool (the hub-reachable cluster).
const authMethodPrivate = "private"

// poolClusterForAuthMethod maps a provision request's auth_method to the
// placeholder pool it should draw from: private methods → the GPU pool
// (the heartbeat-only cluster); everything else → the public pool (the hub-reachable cluster).
func poolClusterForAuthMethod(authMethod string) string {
	if strings.EqualFold(authMethod, authMethodPrivate) || strings.EqualFold(authMethod, gpuClusterID) {
		return gpuClusterID
	}
	return defaultClusterID
}

// clusterIDForHive returns the effective cluster ID of a SaaS hive, treating an
// empty cluster_id as the default (the hub-reachable cluster) — matching clusterForHive's own
// fallback so pool matching agrees with cluster resolution.
func clusterIDForHive(h *SaaSHive) string {
	if h.ClusterID == "" {
		return defaultClusterID
	}
	return h.ClusterID
}

// findAvailablePlaceholder returns the ID of an available placeholder hive in
// the given pool (cluster), or "" if none exists. A placeholder is a SaaS hive
// owned by the hub admin, sitting at statusAvailable, on the target cluster.
func findAvailablePlaceholder(clusterID string) string {
	for _, h := range listSaaSHives() {
		if h.Status != statusAvailable {
			continue
		}
		if !isHubAdmin(h.Owner) {
			continue
		}
		if clusterIDForHive(&h) != clusterID {
			continue
		}
		return h.ID
	}
	return ""
}

// AvailablePlaceholder is one row of the approve-picker dropdown: an available
// placeholder hive the admin can assign a provision request to.
type AvailablePlaceholder struct {
	ID          string `json:"id"`
	ClusterID   string `json:"cluster_id"`
	ProjectName string `json:"project_name"`
}

// listAvailablePlaceholders returns every available placeholder (admin-owned,
// statusAvailable), optionally filtered to a single pool (cluster). An empty
// pool returns placeholders across all pools. It mirrors findAvailablePlaceholder's
// availability predicate so the picker and the assign path agree on what's usable.
func listAvailablePlaceholders(pool string) []AvailablePlaceholder {
	var result []AvailablePlaceholder
	for _, h := range listSaaSHives() {
		if h.Status != statusAvailable {
			continue
		}
		if !isHubAdmin(h.Owner) {
			continue
		}
		cluster := clusterIDForHive(&h)
		if pool != "" && cluster != pool {
			continue
		}
		result = append(result, AvailablePlaceholder{
			ID:          h.ID,
			ClusterID:   cluster,
			ProjectName: h.ProjectName,
		})
	}
	return result
}

// handleAvailablePlaceholders (admin-only) returns the available placeholders
// the approve-picker modal populates its dropdown from. An optional ?pool=
// filters to a single cluster; the default is all available placeholders.
func (s *HubServer) handleAvailablePlaceholders(w http.ResponseWriter, r *http.Request) {
	pool := strings.TrimSpace(r.URL.Query().Get("pool"))
	placeholders := listAvailablePlaceholders(pool)
	if placeholders == nil {
		placeholders = []AvailablePlaceholder{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"placeholders": placeholders})
}

// projectConfigForHiveID returns the claimed project's real org/repos/ACMM for
// delivery to the spoke in its heartbeat response, or nil when no reconcile is
// needed. It mirrors authorizedUsersForHiveID: nil when the hive has no SaaS
// record, is still an unclaimed placeholder (statusAvailable), or the spoke
// already reports the recorded project. The spoke's currently-reported values
// (curOrg/curRepos/curPrimary/curACMM) let the hub stop sending once matched.
// adoptSpokeProjectConfig makes the hub's meta.json track what a CLAIMED hive's
// spoke reports for its OPERATOR-CONTROLLED runtime settings: org, repos,
// primary_repo, and ACMM level. Once a placeholder is claimed, the spoke
// dashboard is the source of truth for these — an operator can re-point repos or
// change the level there. Without adopting them, the reconcile in
// projectConfigForHiveID keeps re-pushing meta's old values and silently reverts
// the operator's edit every heartbeat (the spyre / Joe Runde bug — originally
// just ACMM, but org/repos have the identical failure mode).
//
// Guardrails:
//   - No-op for a hive with no SaaS record or an unclaimed placeholder
//     (statusAvailable) — assign owns those until claimed.
//   - Never adopt EMPTY/zero values: a spoke reporting empty org/repos or level 0
//     (e.g. mid-boot, before its config loads) must not wipe/downgrade meta.
//   - Only writes when something actually changed.
func (s *HubServer) adoptSpokeProjectConfig(hiveID, org string, repos []string, primary string, level int) {
	h := loadSaaSHive(hiveID)
	if h == nil || h.Status == statusAvailable {
		return // no record, or an unclaimed placeholder — assign controls it
	}
	changed := false
	prevLevel := h.ACMMLevel

	// ACMM runs its OWN delivery handshake (ACMMDelivered), not the org/repos
	// one (ClaimDelivered).
	//
	// #2061 made ACMM "operator-owned from the start — always adopt", which fixed
	// the spyre revert but left a gap on the ASSIGN path: a freshly-claimed
	// placeholder keeps reporting the level it was MINTED at, and adopting that
	// pre-delivery report overwrote the level the requester asked for. #2333
	// closed that by gating on ClaimDelivered — correct for hives claimed after
	// it shipped, but a no-op for every hive claimed BEFORE, whose
	// ClaimDelivered was already true from the old org/repos-only rule. Those
	// hives adopt the stale report on the very first beat after upgrade and the
	// push stays disabled, which is why the live oke-11 hive is still L2 against
	// an approved L3 request.
	//
	// The dedicated flag defaults to false on those hives, so the level is
	// delivered exactly once, then ownership passes to the spoke as #2061
	// intended.

	// Backfill the requested level for hives assigned before the field existed.
	// Without this the reconcile has no target on exactly the hives that need
	// it. ACMMLevel is the best available record of the assignment at this
	// point, and using it is safe: if the spoke already agrees, the delivery is
	// a no-op that simply marks itself done.
	if h.RequestedACMMLevel == 0 && h.ACMMLevel > 0 {
		h.RequestedACMMLevel = h.ACMMLevel
		changed = true
	}

	// Delivery is complete once the spoke reports the level the hub asked for.
	// A level-0 report means "too old / mid-boot to say", never a mismatch.
	if !h.ACMMDelivered && h.RequestedACMMLevel > 0 && level == h.RequestedACMMLevel {
		h.ACMMDelivered = true
		changed = true
		s.logger.Info("acmm level delivered to spoke",
			"hive_id", hiveID, "acmm_level", level)
	}

	switch {
	case level <= 0:
		// Nothing reported — say nothing, change nothing.
	case !h.ACMMDelivered && h.RequestedACMMLevel > 0:
		// Pre-delivery the REQUESTED level is authoritative. Hold meta at it so
		// the stale spoke report cannot overwrite the target the push below is
		// still working toward — the exact loop that lost the L3.
		if h.ACMMLevel != h.RequestedACMMLevel {
			h.ACMMLevel = h.RequestedACMMLevel
			changed = true
		}
		if level != h.RequestedACMMLevel {
			s.logger.Info("acmm level not yet applied on spoke; holding requested level",
				"hive_id", hiveID,
				"spoke_reports", level,
				"requested", h.RequestedACMMLevel)
		}
	case level != h.ACMMLevel:
		// Post-delivery the spoke's dashboard owns the level: adopt operator
		// edits, and keep the requested level in step so a later re-delivery
		// never reverts the operator's choice.
		h.ACMMLevel = level
		h.RequestedACMMLevel = level
		changed = true
	}

	// Org/repos: while the claim hasn't been delivered yet, the hub is still
	// PUSHING them down and the spoke may report its OLD placeholder project —
	// do NOT adopt that (it would clobber the real claim). Mark the claim
	// delivered once the spoke reports the matching org/repos, and only AFTER
	// delivery treat the spoke as the source of truth for repo edits.
	orgMatches := org != "" && strings.EqualFold(org, h.Org)
	reposMatch := len(repos) > 0 && sameStringSliceFold(repos, h.Repos)
	primaryMatches := primary == "" || strings.EqualFold(primary, h.PrimaryRepo)
	// ACMM is NO LONGER part of this condition. #2333 added it here so the claim
	// could not be declared delivered while the level was outstanding, but that
	// coupled two independent deliveries: a hive whose level lagged would also
	// have its org/repos pushed forever, and — worse — the level had no way to
	// re-arm on a hive whose ClaimDelivered was already true. ACMMDelivered
	// above now tracks the level on its own, so this returns to being purely
	// about the project payload.
	if !h.ClaimDelivered {
		if orgMatches && reposMatch && primaryMatches {
			h.ClaimDelivered = true
			changed = true
		}
	} else {
		if org != "" && !strings.EqualFold(org, h.Org) {
			h.Org = org
			changed = true
		}
		if len(repos) > 0 && !sameStringSliceFold(repos, h.Repos) {
			h.Repos = repos
			changed = true
		}
		if primary != "" && !strings.EqualFold(primary, h.PrimaryRepo) {
			h.PrimaryRepo = primary
			changed = true
		}
	}
	if !changed {
		return
	}
	if err := saveSaaSHive(h); err != nil {
		s.logger.Warn("failed to persist spoke-reported project config to meta",
			"hive_id", hiveID, "error", err)
		return
	}
	// Keep the in-memory registry consistent so the UI and the next
	// projectConfigForHiveID comparison see the adopted values immediately.
	s.mu.Lock()
	for i := range s.registry.Hives {
		if s.registry.Hives[i].ID == hiveID {
			s.registry.Hives[i].Org = h.Org
			s.registry.Hives[i].Repos = h.Repos
			s.registry.Hives[i].PrimaryRepo = h.PrimaryRepo
			s.registry.Hives[i].ACMMLevel = h.ACMMLevel
			break
		}
	}
	s.mu.Unlock()
	s.logger.Info("adopted dashboard-set project config from spoke heartbeat",
		"hive_id", hiveID, "org", h.Org, "primary_repo", h.PrimaryRepo,
		"acmm_was", prevLevel, "acmm_now", h.ACMMLevel)
}

// claimedVanityURL returns the vanity dashboard URL the hub should SHOW and LINK
// for a hive (My Hives, the SSO /open handoff, config proxy), or "" to fall back
// to the spoke-reported placeholder host.
//
// The rule is deliberately narrow so it never resurrects the 503 bug:
//   - Unclaimed placeholder (statusAvailable): "" — it has no project yet, so its
//     placeholder host is the only correct URL. Leave it.
//   - Claimed hive with a non-empty meta VanityURL: return it. A vanity URL is
//     only non-empty because it was VALIDATED as servable at provision/assign
//     time (addVanityHostToIngress succeeded, or a cluster wildcard/OpenShift
//     route already serves it, e.g. the heartbeat-only cluster's hosted OpenShift-route host).
//     Trusting it here means the hub shows/links the friendly host the instant a
//     hive is claimed, instead of waiting for the spoke to adopt+report it back.
//   - Claimed hive with an empty VanityURL: "" — never mint an unvalidated host
//     here; the placeholder still works.
func claimedVanityURL(h *SaaSHive) string {
	if h == nil || h.Status == statusAvailable {
		return ""
	}
	return h.VanityURL
}

// placeholderHostURL builds the "<hiveID>.<domain>" placeholder URL for a hive
// that has not yet reported a dashboard URL, using the domain of the cluster
// the hive actually lives on.
//
// The domain MUST come from the hive's own cluster rather than the hub's
// hardcoded hive.kubestellar.io. That constant is the wildcard fronting the
// HUB's router, so using it for a spoke on any other cluster produces a name
// that resolves to the hub and 503s — the exact defect this path exhibited on
// the OpenShift pool. Deriving it per-cluster is also what keeps this correct
// for clusters added in future, without naming any of them here.
//
// Returns "" when the cluster (or its domain) is unknown, so the caller reports
// "no reachable dashboard URL yet" instead of inventing an unreachable host.
func (s *HubServer) placeholderHostURL(hiveID string) string {
	// A hive with no meta record yet (mid-provision, or a registry-only entry)
	// still resolves through clusterForHive, which falls back to the default
	// cluster. That is the correct answer for it: with nothing recorded about
	// where it lives, the hub's own pool is the only defensible guess, and it
	// is the pool such a hive is in fact provisioned into.
	cluster := s.clusterForHive(&SaaSHive{ID: hiveID})
	if h := loadSaaSHive(hiveID); h != nil {
		cluster = s.clusterForHive(h)
	}
	if cluster == nil || cluster.Domain == "" {
		return ""
	}
	return "https://" + hiveID + "." + strings.Trim(strings.TrimSpace(cluster.Domain), ".")
}

// curAPIURL is the GitHub API base URL the spoke reports it is CURRENTLY using
// (HeartbeatPayload.GitHubAPIURL). Empty means the spoke is too old to report
// it — treated as UNKNOWN, never as a mismatch.
func projectConfigForHiveID(hiveID, curOrg string, curRepos []string, curPrimary string, curACMM int, curURL, curAPIURL string) *HeartbeatProjectConfig {
	h := loadSaaSHive(hiveID)
	if h == nil {
		return nil
	}
	// This reconcile exists ONLY to push a freshly-CLAIMED placeholder's project
	// down to its spoke. It must NEVER touch a pre-existing hive, whose meta.json
	// predates the claim feature and carries stale/empty fields (empty
	// primary_repo, acmm_level: 0) even though the spoke runs a real project at a
	// real ACMM. Reconciling from that stale record silently wiped org/repos and
	// DOWNGRADED live hives to L0. So we only reconcile a record that looks like a
	// genuine claim — a complete project (org + repos + primary_repo) AND a real
	// non-zero ACMM — and even then we never send a value that would blank/lower
	// what the spoke already has.
	if h.Status == statusAvailable { // still an unclaimed placeholder
		return nil
	}
	primary := h.PrimaryRepo
	if primary == "" && len(h.Repos) > 0 {
		primary = h.Repos[0]
	}
	claimComplete := h.Org != "" && len(h.Repos) > 0 && primary != "" && h.ACMMLevel > 0
	if !claimComplete {
		// Incomplete/stale record (a pre-claim hive) — leave the spoke's PROJECT
		// (org/repos/ACMM) alone; reconciling from a stale record wiped/downgraded
		// live hives. BUT the vanity URL is independent of project completeness: a
		// claimed hive can carry a validated meta vanity_url (set at provision,
		// e.g. a hive's hosted OpenShift-route on the heartbeat-only cluster) while its meta's
		// org/repos/ACMM are still stale/empty. Without pushing it, the spoke never
		// adopts the vanity URL and the hub keeps showing the raw placeholder host
		// forever (the placeholder-URL-persists bug). Push the URL alone — never
		// the stale project — until the spoke reports the vanity back.
		//
		// Safety: only a NON-EMPTY VanityURL is ever pushed. A vanity URL is only
		// non-empty because it was validated/served at provision or assign time
		// (addVanityHostToIngress succeeded, or a cluster wildcard route already
		// serves it), so this never pushes an unserved host and never reintroduces
		// the 503.
		//
		// A pending FORGE SWITCH is likewise independent of project
		// completeness — a hive can be moved between forges whether or not its
		// meta carries a complete claim, and the switch is precisely the
		// operator action that must not be silently dropped. Push the API URL
		// alone (never the stale project) until the spoke reports the requested
		// host back.
		if apiURL := pendingForgeAPIURL(h, curAPIURL); apiURL != "" {
			return &HeartbeatProjectConfig{GitHubAPIURL: apiURL}
		}
		if h.VanityURL != "" && curURL != h.VanityURL {
			return &HeartbeatProjectConfig{DashboardURL: h.VanityURL}
		}
		return nil
	}
	// What the hub still PUSHES to the spoke, and until when:
	//   - org/repos/primary_repo: pushed only until the claim is DELIVERED (the
	//     spoke first reports the assigned project). After delivery the spoke's
	//     dashboard owns them and the caller adopts operator edits instead.
	//   - vanity URL: pushed until the spoke first reports it back.
	//   - ACMM level: pushed on its OWN handshake (ACMMDelivered), until the
	//     spoke reports the requested level back. After that it is never pushed
	//     again — the spoke's dashboard owns it and the caller adopts operator
	//     edits, exactly as #2061 intended.
	//
	//     #2061 removed the ACMM push entirely because pushing it FOREVER
	//     reverted every dashboard level change (the spyre bug). Dropping it
	//     altogether meant a freshly-assigned placeholder was never told the
	//     level its owner requested. #2333 bounded the push by ClaimDelivered,
	//     which is right in principle but dead in practice for every hive
	//     claimed before it shipped: their ClaimDelivered was already true, so
	//     the push never fires. Bounding by the level's own flag is what makes
	//     the delivery reachable for those hives — and it is self-limiting, so
	//     re-running it is harmless.
	needClaimPush := !h.ClaimDelivered &&
		(!strings.EqualFold(curOrg, h.Org) ||
			!sameStringSliceFold(curRepos, h.Repos) ||
			!strings.EqualFold(curPrimary, primary))
	// Independent of the project claim: deliver the requested level until the
	// spoke confirms it. curACMM == 0 means the spoke did not report a level, so
	// there is nothing to correct yet.
	needACMMPush := !h.ACMMDelivered && h.RequestedACMMLevel > 0 && curACMM != h.RequestedACMMLevel
	// Vanity URL: the spoke always reports its dashboard URL, so observed is
	// known and an empty one is a real "I have none" rather than silence.
	needURLPush := needsPush(h.VanityURL, curURL, true)
	// A spoke reporting a repo that fails isValidRepoRef is wedged: the hub 400s
	// its every heartbeat ("invalid repo name"), /api/livez then fails on the
	// stale heartbeat and the kubelet crash-loops the pod. Push a corrected
	// project even when the claim was already delivered — otherwise the guard
	// above ("nothing left to push") leaves the hive broken forever, since a
	// wedged spoke can never report anything the hub will accept.
	needRepoRepair := false
	for _, r := range append(append([]string{}, curRepos...), curPrimary) {
		if r != "" && !isValidRepoRef(r) {
			needRepoRepair = true
			break
		}
	}
	// A hive whose GitHubHost was filled in AFTER its claim was delivered —
	// the retroactive repair, or an admin editing the host later — has
	// ClaimDelivered == true and a matching vanity URL, so every gate above is
	// false and the reconcile returns nil forever. The spoke would then keep
	// talking to api.github.com against a GitHub Enterprise org (the heartbeat-only cluster /
	// hosted-available-vllmd-01 failure). Push whenever we have a GHE API URL
	// to deliver and the spoke reports a DIFFERENT one.
	//
	// Deliberately conservative: an empty curAPIURL means the spoke is too old
	// to report its API URL, which is UNKNOWN, not a mismatch — pushing on it
	// would re-send on every beat with no read-back to ever stop it.
	wantAPIURL := forgeAPIURLForHost(h.Forge, h.GitHubHost)
	// The api_url is the field where unknown-vs-mismatch actually bites: a spoke
	// too old to report it sends "", which is NOT "I am on api.github.com". The
	// observedKnown argument carries that distinction explicitly instead of
	// hiding it in a curAPIURL != "" conjunct that a later edit could drop.
	needGHEAPIPush := needsPush(wantAPIURL, curAPIURL, curAPIURL != "")
	// A pending FORGE SWITCH pushes on its own handshake. It cannot ride
	// needGHEAPIPush: that gate is deliberately conservative about an empty
	// curAPIURL (unknown, not a mismatch) and — more importantly — it can never
	// deliver a switch TO public github.com, whose wantAPIURL is "" by
	// definition. An operator moving a hive back to github.com must be able to,
	// so the switch carries its own target and its own read-back.
	forgeAPIURL := pendingForgeAPIURL(h, curAPIURL)
	needForgePush := forgeAPIURL != ""
	if !needClaimPush && !needURLPush && !needRepoRepair && !needGHEAPIPush && !needACMMPush && !needForgePush {
		return nil // nothing left to push
	}
	// Sanitize before pushing. A repo pasted as a URL
	// ("github.ibm.com/enricom-ibm/jackrabbit") has two slashes, which
	// isValidRepoRef rejects — so the hub 400s the spoke's every heartbeat
	// ("invalid repo name"), /api/livez then fails on the stale heartbeat, and
	// the kubelet restarts the pod in a loop. Normalizing here repairs an
	// already-broken hive over the heartbeat, which is the only channel that
	// reaches a firewalled cluster (the heartbeat-only cluster).
	pushRepos := make([]string, 0, len(h.Repos))
	for _, r := range h.Repos {
		if rr := sanitizeRepoEntry(r); rr != "" {
			pushRepos = append(pushRepos, rr)
		}
	}
	if len(pushRepos) == 0 {
		pushRepos = h.Repos
	}
	pushPrimary := sanitizeRepoEntry(primary)
	if pushPrimary == "" {
		pushPrimary = primary
	}

	// Pre-delivery the REQUESTED level is what goes down the wire. The adopt path
	// also holds h.ACMMLevel at that value, so the two normally agree — but
	// stating it here means a push cannot deliver a stale level if this function
	// ever runs against a record the adopt path has not touched yet.
	pushACMM := h.ACMMLevel
	if !h.ACMMDelivered && h.RequestedACMMLevel > 0 {
		pushACMM = h.RequestedACMMLevel
	}

	return &HeartbeatProjectConfig{
		Org:          h.Org,
		Repos:        pushRepos,
		PrimaryRepo:  pushPrimary,
		ACMMLevel:    pushACMM,
		DashboardURL: h.VanityURL,
		// IssueFilter rides the claim push only when the record carries one.
		// nil (the ordinary case) tells the spoke "keep your own filter" — the
		// echo of this struct on later beats must never blank an operator's
		// locally configured project.issue_filter.
		IssueFilter: h.IssueFilter,
		// Point a GHE hive at its enterprise API. jjs-world
		// (hosted-open-source-osscar) is the working reference: a bare
		// primary_repo plus github.api_url = https://<host>/api/v3. Empty host
		// pushes nothing, so a github.com hive keeps the spoke's own default.
		// A pending forge switch wins: forgeAPIURL is the host the operator
		// asked for and is only non-empty while that delivery is outstanding.
		// Once the spoke reports the requested host, ForgeDelivered latches and
		// this falls back to the ordinary host-derived value — which by then
		// derives from the SAME host, so the two agree and nothing flaps.
		GitHubAPIURL: func() string {
			if forgeAPIURL != "" {
				return forgeAPIURL
			}
			return forgeAPIURLForHost(h.Forge, h.GitHubHost)
		}(),
		// AIAuthor is deliberately left empty here. Provisioning state never
		// knows the agents' GitHub account — the spoke owns it — and the spoke
		// treats an empty author as "leave mine alone". Setting it from this
		// struct would reintroduce the blanking bug.
	}
}

// sameStringSliceFold reports whether two string slices contain the same
// entries in the same order, case-insensitively (org/repo names are compared
// case-insensitively throughout the hub).
func sameStringSliceFold(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !strings.EqualFold(a[i], b[i]) {
			return false
		}
	}
	return true
}

// AssignHiveRequest is the body of POST /api/saas/hives/{id}/assign. It carries
// the real project the placeholder is being claimed for, plus optional GitHub
// App credentials to deliver to the spoke via the heartbeat channel.
type AssignHiveRequest struct {
	Owner string `json:"owner"`
	Org   string `json:"org"`
	// GitHubHost is the GitHub instance the org lives on ("" = public
	// github.com, otherwise a GHE host). Parsed from a pasted org URL when the
	// caller does not send it explicitly.
	GitHubHost     string `json:"github_host,omitempty"`
	Repos          string `json:"repos"`
	PrimaryRepo    string `json:"primary_repo"`
	ProjectName    string `json:"project_name"`
	ACMMLevel      int    `json:"acmm_level"`
	IsPublic       bool   `json:"is_public"`
	AppID          string `json:"app_id"`
	InstallationID string `json:"installation_id"`
	AppPrivateKey  string `json:"app_private_key"`
}

// handleAssignHive assigns an available placeholder hive to a real owner/project
// (admin-only). It rewrites the hive's meta.json to the real project and clears
// its "available" status, then delivers the new project config — and any GitHub
// App creds — to the spoke via the heartbeat response. This works uniformly for
// both reachable (the hub-reachable cluster) and heartbeat-only (the heartbeat-only cluster) clusters: NO hub→spoke
// push or kubectl is used, so a heartbeat-only-cluster claim is delivered entirely by heartbeat.
func (s *HubServer) handleAssignHive(w http.ResponseWriter, r *http.Request) {
	if !isHubAdmin(s.getAuthUser(r)) {
		http.Error(w, `{"error":"admin access required"}`, http.StatusForbidden)
		return
	}
	hiveID := r.PathValue("id")

	// A GitHub App private key PEM can be a few KB, so allow more headroom than
	// the provision-request body limit.
	const maxAssignRequestBodyBytes = 16 * 1024
	r.Body = http.MaxBytesReader(w, r.Body, maxAssignRequestBodyBytes)
	var body AssignHiveRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	h := loadSaaSHive(hiveID)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}
	if h.Status != statusAvailable {
		http.Error(w, `{"error":"hive is not an available placeholder"}`, http.StatusConflict)
		return
	}

	// Validate the claimed project inputs (reuse the shared validators).
	if body.Owner == "" || !isValidName(body.Owner) {
		http.Error(w, `{"error":"invalid owner"}`, http.StatusBadRequest)
		return
	}
	// Accept a pasted org/repo URL here too — an admin assigning a hive reaches
	// for the same paste the requester did. normalizeOrgRef returns a non-empty
	// host with an EMPTY org when the field held only a forge host
	// ("github.ibm.com") and no org — clear body.Org in that case so the
	// isValidName check below rejects it, instead of the raw hostname (which
	// isValidName accepts, dots and all) silently becoming the org. That
	// host-as-org bug produced the two broken github.ibm.com claims on the heartbeat-only cluster.
	if h, o := normalizeOrgRef(body.Org); h != "" {
		body.Org = o // may be "" for a bare-host paste — rejected just below
		if body.GitHubHost == "" {
			body.GitHubHost = h
		}
	}
	if body.Org == "" || !isValidName(body.Org) {
		http.Error(w, fmt.Sprintf(`{"error":"invalid org name %q — use the org name or its URL (e.g. github.ibm.com/my-org)"}`, body.Org), http.StatusBadRequest)
		return
	}
	// "public" is the sentinel that forces public github.com even on a GHE
	// cluster; it is not a hostname, so exempt it from the hostname validator.
	if body.GitHubHost != "" && !isValidName(body.GitHubHost) && !strings.EqualFold(body.GitHubHost, githubHostPublic) {
		http.Error(w, `{"error":"invalid github host"}`, http.StatusBadRequest)
		return
	}
	if body.Repos == "" {
		http.Error(w, `{"error":"repos are required"}`, http.StatusBadRequest)
		return
	}
	// Single-host-per-spoke (assign path — mirrors the request path). Every repo
	// and the primary must share the spoke's host, checked on the raw pasted
	// values before normalizeRepoRef strips the host. The "public" sentinel means
	// github.com, so pass "" to the validator for it.
	{
		spokeHost := body.GitHubHost
		if strings.EqualFold(spokeHost, githubHostPublic) {
			spokeHost = ""
		}
		if err := validateSingleRepoHost(spokeHost, body.PrimaryRepo, strings.Split(body.Repos, ",")); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
			return
		}
	}
	{
		var cleaned []string
		for _, r := range strings.Split(body.Repos, ",") {
			if rr := normalizeRepoRef(r); rr != "" {
				cleaned = append(cleaned, rr)
			}
		}
		body.Repos = strings.Join(cleaned, ",")
		if body.PrimaryRepo != "" {
			body.PrimaryRepo = normalizeRepoRef(body.PrimaryRepo)
		}
	}
	var repos []string
	for _, repo := range strings.Split(body.Repos, ",") {
		repo = strings.TrimSpace(repo)
		if repo == "" {
			continue
		}
		if !isValidRepoRef(repo) {
			http.Error(w, `{"error":"invalid repo name"}`, http.StatusBadRequest)
			return
		}
		repos = append(repos, repo)
	}
	if len(repos) == 0 {
		http.Error(w, `{"error":"repos are required"}`, http.StatusBadRequest)
		return
	}
	primaryRepo := strings.TrimSpace(body.PrimaryRepo)
	if primaryRepo == "" {
		primaryRepo = repos[0]
	} else if !isValidRepoRef(primaryRepo) {
		http.Error(w, `{"error":"invalid primary repo"}`, http.StatusBadRequest)
		return
	}
	forgeHost := body.GitHubHost
	if strings.EqualFold(forgeHost, githubHostPublic) || forgeHost == "" {
		forgeHost = "github.com"
	}
	if issue := config.ValidateProjectRepoTargets(body.Org, repos, primaryRepo, forgeHost); issue != nil {
		writeJSONError(w, http.StatusBadRequest, issue.Message)
		return
	}

	acmm := body.ACMMLevel
	if acmm == 0 {
		acmm = defaultAssignACMMLevel
	}
	if acmm < minAssignACMMLevel || acmm > maxAssignACMMLevel {
		http.Error(w, `{"error":"acmm_level must be between 0 and 6"}`, http.StatusBadRequest)
		return
	}

	// Rewrite the placeholder's meta.json to the real project. Clearing status
	// (and any stale error) alone makes it show under the new owner in My Hives.
	h.Owner = body.Owner
	h.Org = body.Org
	// Preserve the placeholder's real cluster before the host backfill below,
	// which resolves via s.clusterForHive(h) and would fall back to the hub-reachable cluster on
	// a blank cluster_id. The admin assigns a specific placeholder, so its own
	// ClusterID is authoritative; only a placeholder created without one lands
	// on the default here (never a silent blank that mis-routes to the hub-reachable cluster).
	s.ensureClusterIDForClaim(h, "")
	// Record the GHE host (if any) so the heartbeat can point this spoke at the
	// right GitHub API. Never blank an existing value with an empty one.
	//
	// "public" is an explicit choice of public github.com on a cluster whose
	// defaults point at GHE. Record it as a blank host (so forgeAPIURLForHost
	// pushes nothing and the spoke keeps api.github.com) PLUS the
	// GitHubBaseURL sentinel, which is what makes effectiveGitHubBaseURL
	// resolve to "" and therefore makes the cluster backfill below decline to
	// re-GHE the hive. Without the sentinel a blank host would simply be
	// refilled from the cluster on the very next line.
	if strings.EqualFold(body.GitHubHost, githubHostPublic) {
		// Explicit public github.com, stored as the real host rather than a
		// blank + sentinel. The cluster backfill below only fills an EMPTY
		// host, so a stated value blocks it just as the blank did — and does
		// not leave a field whose absence has to be interpreted.
		h.GitHubHost = publicForgeHost
	} else if body.GitHubHost != "" {
		h.GitHubHost = body.GitHubHost
	}
	// Backfill the host from the hive's cluster when neither the request nor
	// the placeholder carries one. Placeholders provisioned BEFORE their
	// cluster gained github_base_url/github_api_url have GitHubHost == "", and
	// nothing else ever fills it in: projectConfigForHiveID pushes
	// forgeAPIURLForHost(h.Forge, h.GitHubHost), which is empty for those hives, so the
	// spoke keeps api.github.com and the public app_id even though the cluster
	// is a GHE cluster (observed on the heartbeat-only cluster: hosted-available-vllmd-01 has
	// base_url: "" / api_url: "" against a github.ibm.com cluster). The hive's
	// own value always wins; this only fills a blank.
	if host := backfillGitHubHostFromCluster(h, s.clusterForHive(h)); host != "" {
		h.GitHubHost = host
		s.logger.Info("backfilled hive github host from cluster defaults",
			"hive", hiveID, "github_host", host)
	}
	h.Repos = repos
	h.PrimaryRepo = primaryRepo
	if body.ProjectName != "" {
		h.ProjectName = body.ProjectName
	}
	h.ACMMLevel = acmm
	// Same as the approve-provision path: the admin-assigned level is what the
	// hub must deliver, and it needs its own field because the spoke will
	// overwrite ACMMLevel with whatever it is currently running.
	h.RequestedACMMLevel = acmm
	h.ACMMDelivered = false
	h.IsPublic = body.IsPublic
	h.Status = statusAssigned
	// Stamp when this claim began so the self-heal sweep can age it out if the
	// spoke never reports the project back (ClaimDelivered stuck false).
	h.AssignedAt = time.Now().UTC().Format(time.RFC3339)
	h.Error = ""
	// A (re)assignment is a new claim payload: reset delivery so the hub pushes
	// this project to the spoke until it reports the new org/repos back, before
	// letting the spoke's dashboard own them.
	h.ClaimDelivered = false
	// The vanity URL is NOT minted here anymore. It used to be derived and made
	// servable inline (makeVanityHostServable → kubectl against the hive's
	// cluster) between this save and the HTTP response, which held the admin's
	// assign dialog hostage for ~a minute whenever the cluster was slow or
	// unreachable (each kubectl call eats a ~45s TCP dial timeout on the
	// heartbeat-only cluster pool — the same disease #2730 cured on the
	// heartbeat path). The mint — same host preference (name-bearing Option B
	// host, org/repo fallback), same servability seam, same "never adopt an
	// unservable host" rule — now runs in the background via
	// kickClaimClusterWorkAsync below, after the response is written. Nothing
	// about the response depends on it: the claim reaches the spoke over the
	// heartbeat channel regardless, and a failed mint is retried by the
	// heartbeat-kicked repair exactly as before.
	if err := saveSaaSHive(h); err != nil {
		http.Error(w, `{"error":"failed to save hive assignment"}`, http.StatusInternalServerError)
		return
	}

	// Grant the assignee owner access. handleAccessList builds a hive's access
	// list by scanning every user record for Hives[hiveID], NOT from h.Owner —
	// so without this the assignment set h.Owner correctly while Manage Access
	// still showed only the admin who provisioned the placeholder. On a
	// heartbeat-only cluster (the heartbeat-only cluster) that stale list is what reaches the spoke.
	assignee := loadSaaSUser(body.Owner)
	if assignee == nil {
		assignee = ensureSaaSUser(body.Owner)
	}
	if assignee.Hives == nil {
		assignee.Hives = map[string]string{}
	}
	if assignee.Hives[hiveID] != "owner" {
		assignee.Hives[hiveID] = "owner"
		assignee.SaaSQuota++
		if err := saveSaaSUser(assignee); err != nil {
			s.logger.Warn("assigned hive but failed to grant owner access", "user", body.Owner, "hive", hiveID, "error", err)
		}
	}

	// Deliver GitHub App creds (if supplied) via the SAME heartbeat channel the
	// webhook path uses — storePendingGitHubAppConfig queues them for the next
	// heartbeat response (consumePendingGitHubAppConfig in handleHeartbeat). We
	// deliberately do NOT call pushGitHubConfigToSpoke here: it requires a
	// reachable dashboardURL and would fail for the heartbeat-only cluster. The heartbeat path
	// covers both clusters uniformly.
	appDelivered := false
	if body.AppID != "" && body.InstallationID != "" && strings.TrimSpace(body.AppPrivateKey) != "" {
		appID, err1 := strconv.ParseInt(strings.TrimSpace(body.AppID), 10, 64)
		installID, err2 := strconv.ParseInt(strings.TrimSpace(body.InstallationID), 10, 64)
		if err1 != nil || err2 != nil {
			http.Error(w, `{"error":"app_id and installation_id must be numeric"}`, http.StatusBadRequest)
			return
		}
		if !strings.HasPrefix(strings.TrimSpace(body.AppPrivateKey), "-----BEGIN") {
			http.Error(w, `{"error":"app_private_key must be a PEM private key"}`, http.StatusBadRequest)
			return
		}
		s.storePendingGitHubAppConfig(hiveID, &HeartbeatGitHubAppConfig{
			AppID:          appID,
			InstallationID: installID,
			PrivateKey:     strings.TrimSpace(body.AppPrivateKey),
		})
		appDelivered = true
	}

	// NO CREDS PASTED: derive the identity from the forge we already know.
	//
	// The three-way AND above is an ADMIN OVERRIDE, not the normal path — it
	// fires only when someone hand-carries an app_id, an installation_id and a
	// PEM into the dialog. Every other assignment fell through it silently and
	// left the hive on config.PlaceholderAppID, even though h.GitHubHost was
	// resolved a hundred lines earlier and the hub holds that forge's App key.
	// Deriving here is what makes "assign knows the forge, so assign sets the
	// forge identity" true in code rather than only in intent.
	if !appDelivered {
		if appCfg := s.assignTimeAppIdentity(h); appCfg != nil {
			s.storePendingGitHubAppConfig(hiveID, appCfg)
			appDelivered = true
			s.logger.Info("assign: derived github app identity from the hive's forge",
				"hive_id", hiveID,
				"forge", h.GitHubHost,
				"app_id", appCfg.AppID,
				"app_slug", appCfg.AppSlug,
				"api_url", appCfg.APIURL,
				"key_delivered", appCfg.PrivateKey != "",
			)
		} else {
			s.logger.Warn("assign: no github app identity for this hive's forge — spoke keeps the placeholder app_id and starts in dashboard-only mode",
				"hive_id", hiveID,
				"forge", h.GitHubHost,
				"cluster", clusterIDForHive(h),
				"remedy", "name an App for this forge in clusters.json, or supply app_id/installation_id/app_private_key on the assign request",
			)
		}
	}

	// The project config itself is delivered by handleHeartbeat via
	// projectConfigForHiveID on the next beat — it keeps sending until the spoke
	// reports the matching project. No hub→spoke push or kubectl is needed, so
	// this works for the heartbeat-only cluster pool as well as the hub-reachable cluster.

	s.logger.Info("audit: placeholder hive assigned",
		"hive_id", hiveID,
		"owner", h.Owner,
		"org", h.Org,
		"primary_repo", h.PrimaryRepo,
		"acmm_level", h.ACMMLevel,
		"cluster", clusterIDForHive(h),
		"app_creds_delivered", appDelivered,
	)
	s.recordTimeline(hiveID, TimelineOwnership,
		fmt.Sprintf("hive assigned to %s (%s, ACMM %d)", h.Owner, repoDisplayLine(h.Org, h.PrimaryRepo), h.ACMMLevel),
		s.getAuthUser(r))

	// Everything the response depends on is persisted above (meta.json, owner
	// grant, pending App creds, audit/timeline) — all fast local ops. The
	// cluster-facing work (namespace identity stamp + vanity-host mint, both
	// kubectl against the hive's cluster) runs in the background so the assign
	// dialog gets its ack immediately; the row's "claim pending" indicators
	// track actual delivery, which happens over the heartbeat channel anyway.
	s.kickClaimClusterWorkAsync(hiveID)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":       "assigned",
		"id":           hiveID,
		"owner":        h.Owner,
		"org":          h.Org,
		"primary_repo": h.PrimaryRepo,
		"acmm_level":   h.ACMMLevel,
	})
}

func isUnfurlBot(ua string) bool {
	bots := []string{"Slackbot", "Slack-ImgProxy", "Discordbot", "Twitterbot", "facebookexternalhit", "LinkedInBot", "WhatsApp", "TelegramBot"}
	for _, b := range bots {
		if strings.Contains(ua, b) {
			return true
		}
	}
	return false
}

const ogFallbackHTML = `<!DOCTYPE html><html><head>
<meta charset="utf-8">
<meta property="og:title" content="My Hives — Hive Hub">
<meta property="og:description" content="AI Agent Orchestration for Open Source. Manage your hive instances — monitor agents, governor mode, issues, PRs, and contributor activity.">
<meta property="og:type" content="website">
<meta property="og:site_name" content="Hive Hub">
<meta property="og:url" content="https://hive.hivecommons.dev/dashboard">
<link rel="icon" href="data:image/svg+xml,<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 100 100'><text y='.9em' font-size='90'>🍯</text></svg>">
<title>My Hives — Hive Hub</title>
</head><body></body></html>`

func (s *HubServer) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if isUnfurlBot(r.UserAgent()) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(ogFallbackHTML))
		return
	}
	cookie, err := r.Cookie("hive_hub_user")
	if err != nil || cookie.Value == "" {
		http.Redirect(w, r, "/login", http.StatusTemporaryRedirect)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprint(w, dashboardHTML)
}

func (s *HubServer) handleAccessDenied(w http.ResponseWriter, r *http.Request) {
	hiveID := sanitize(r.URL.Query().Get("hive"))

	ownerLink := ""
	s.mu.RLock()
	for _, h := range s.registry.Hives {
		if h.ID == hiveID && h.Owner != "" {
			safeOwner := sanitize(h.Owner)
			if safeOwner != "" {
				ownerLink = fmt.Sprintf(`<a href="https://github.com/%s" target="_blank" style="color:#58a6ff;text-decoration:underline">the hive owner</a>`, safeOwner)
			}
			break
		}
	}
	s.mu.RUnlock()
	if ownerLink == "" {
		ownerLink = "the hive owner"
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	_, _ = fmt.Fprintf(w, `<!DOCTYPE html>
<html><head><meta charset="UTF-8"><title>Access Denied — Hive Hub</title>
<script async src="https://www.googletagmanager.com/gtag/js?id=G-4707R797K3"></script><script>window.dataLayer=window.dataLayer||[];function gtag(){dataLayer.push(arguments)}gtag("js",new Date());gtag("config","G-4707R797K3");gtag("event","access_denied",{hive_id:"%s"});</script>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;background:#0d1117;color:#e6edf3;display:flex;justify-content:center;align-items:center;min-height:100vh}
.card{background:#161b22;border:1px solid #30363d;border-radius:12px;padding:48px;max-width:520px;text-align:center}
h1{font-size:2rem;margin-bottom:8px}
.bee{font-size:3rem;margin-bottom:16px}
.msg{color:#8b949e;margin-bottom:24px;line-height:1.6}
.hive-name{color:#f0883e;font-family:monospace;font-weight:600}
.btn{display:inline-block;padding:10px 24px;border-radius:8px;text-decoration:none;font-weight:600;font-size:0.9rem;margin:6px}
.btn-primary{background:#238636;color:#fff}
.btn-secondary{background:transparent;color:#58a6ff;border:1px solid #30363d}
.help{color:#8b949e;font-size:0.8rem;margin-top:24px}
</style></head><body>
<div class="card">
<div class="bee">🐝</div>
<h1>Access Denied</h1>
<p class="msg">
You don't have access to
<span class="hive-name">%s</span>.<br><br>
Ask %s to grant you access from their
<a href="/dashboard" style="color:#58a6ff">My Hives</a> dashboard.
</p>
<a href="/dashboard" class="btn btn-primary">Go to My Hives</a>
<a href="/" class="btn btn-secondary">Browse Public Hives</a>
<p class="help">If you believe this is an error, <a href="https://github.com/hivecommons/hive/issues" style="color:#58a6ff">file an issue</a>.</p>
</div>
</body></html>`, hiveID, hiveID, ownerLink)
}

const (
	bannerIDPrefix       = "hub-banner-"
	maxBannerMessageLen  = 500
	maxBannerTargetHives = 100
)

var validBannerColors = map[string]bool{
	"green": true,
	"blue":  true,
	"amber": true,
	"gray":  true,
}

func (s *HubServer) handleSendHubBanner(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Message string   `json:"message"`
		Color   string   `json:"color"`
		HiveIDs []string `json:"hive_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	body.Message = strings.TrimSpace(body.Message)
	if body.Message == "" {
		http.Error(w, `{"error":"message is required"}`, http.StatusBadRequest)
		return
	}
	if len([]rune(body.Message)) > maxBannerMessageLen {
		http.Error(w, fmt.Sprintf(`{"error":"message exceeds %d characters"}`, maxBannerMessageLen), http.StatusBadRequest)
		return
	}
	if body.Color == "" {
		body.Color = "green"
	}
	if !validBannerColors[body.Color] {
		http.Error(w, `{"error":"invalid color; must be green, blue, amber, or gray"}`, http.StatusBadRequest)
		return
	}
	if len(body.HiveIDs) == 0 {
		http.Error(w, `{"error":"at least one hive must be selected"}`, http.StatusBadRequest)
		return
	}
	if len(body.HiveIDs) > maxBannerTargetHives {
		http.Error(w, fmt.Sprintf(`{"error":"too many hives (max %d)"}`, maxBannerTargetHives), http.StatusBadRequest)
		return
	}

	bannerID := fmt.Sprintf("%s%d", bannerIDPrefix, time.Now().UnixMilli())
	now := time.Now().UTC().Format(time.RFC3339)
	entry := &HubBannerEntry{
		ID:      bannerID,
		Message: body.Message,
		Color:   body.Color,
		SentAt:  now,
	}

	s.hubBannersMu.Lock()
	for _, hiveID := range body.HiveIDs {
		s.hubBanners[hiveID] = entry
	}
	s.hubBannersMu.Unlock()
	// Persist so the banner survives a hub restart/upgrade (the pod roll would
	// otherwise wipe the in-memory map and silently drop it).
	s.saveHubBanners()

	username := s.getAuthUser(r)
	s.logger.Info("hub banner sent",
		"banner_id", bannerID,
		"message", body.Message,
		"color", body.Color,
		"hive_count", len(body.HiveIDs),
		"by", username,
	)

	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"ok":true,"banner_id":%q,"hive_count":%d}`, bannerID, len(body.HiveIDs))
}

func (s *HubServer) handleClearHubBanner(w http.ResponseWriter, r *http.Request) {
	s.hubBannersMu.Lock()
	count := len(s.hubBanners)
	s.hubBanners = make(map[string]*HubBannerEntry)
	s.hubBannersMu.Unlock()
	// Persist the cleared (empty) state so banners stay gone across a restart.
	s.saveHubBanners()

	username := s.getAuthUser(r)
	s.logger.Info("hub banners cleared", "cleared_count", count, "by", username)

	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"ok":true,"cleared":%d}`, count)
}

func (s *HubServer) handleGetHubBanner(w http.ResponseWriter, r *http.Request) {
	s.hubBannersMu.RLock()
	defer s.hubBannersMu.RUnlock()

	type bannerStatus struct {
		HiveID  string `json:"hive_id"`
		ID      string `json:"id"`
		Message string `json:"message"`
		Color   string `json:"color"`
		SentAt  string `json:"sent_at"`
	}
	var banners []bannerStatus
	for hiveID, entry := range s.hubBanners {
		banners = append(banners, bannerStatus{
			HiveID:  hiveID,
			ID:      entry.ID,
			Message: entry.Message,
			Color:   entry.Color,
			SentAt:  entry.SentAt,
		})
	}
	if banners == nil {
		banners = []bannerStatus{}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"banners": banners})
}

const (
	imageBuildStaleAfterEnv     = "HIVE_IMAGE_BUILD_STALE_AFTER"
	defaultImageBuildStaleAfter = 30 * time.Minute
)

//go:embed assets/dashboard.html
var dashboardHTML string
