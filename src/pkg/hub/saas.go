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
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
