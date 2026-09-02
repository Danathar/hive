// Operator-surface configuration: notifications (ntfy/Slack/Discord),
// HubConfig (contribute controls), DashboardConfig (public URL, snapshot
// frame ancestors, authorized users and role tiers).
package config

import (
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

type NotificationsConfig struct {
	Ntfy    *NtfyConfig    `yaml:"ntfy,omitempty"`
	Slack   *SlackConfig   `yaml:"slack,omitempty"`
	Discord *DiscordConfig `yaml:"discord,omitempty"`
}

type NtfyConfig struct {
	Server string `yaml:"server"`
	Topic  string `yaml:"topic"`
}

type SlackConfig struct {
	Webhook string `yaml:"webhook"`
}

type DiscordConfig struct {
	Webhook   string `yaml:"webhook"`
	BotToken  string `yaml:"bot_token"`
	ChannelID string `yaml:"channel_id"`
	// AllowedUsers is an allowlist of Discord user IDs permitted to issue bot
	// COMMANDS (!kick, !pause, agent actions — anything that drives an agent).
	// SECURITY: without it, any member of the guild who can post in the channel
	// can inject prompts into the agents. When empty, command handling is
	// DISABLED (fail closed) — the bot still posts status but accepts no
	// commands — so an operator must opt in by listing the trusted user IDs.
	AllowedUsers []string `yaml:"allowed_users,omitempty"`
}

type HubConfig struct {
	Enabled             bool   `yaml:"enabled"`
	URL                 string `yaml:"url"`
	IsPublic            bool   `yaml:"is_public"`
	SnapshotURL         string `yaml:"snapshot_url"`
	DashboardURL        string `yaml:"dashboard_url"`
	HiveType            string `yaml:"hive_type"`
	ClusterID           string `yaml:"cluster_id"`
	AutoSnapshot        bool   `yaml:"auto_snapshot"`
	AutoUpgrade         bool   `yaml:"auto_upgrade"`
	ContributeSuspended bool   `yaml:"contribute_suspended"`
	// Contribute title/author/label filters use a single list plus a mode:
	//   - FilterModeAllow ("allow"): allowlist — an item passes ONLY if it
	//     matches the list (a non-empty list is required for the filter to gate;
	//     an empty allow list means "no items pass" is intentionally avoided —
	//     see passesContributeFilter, where an empty allow list is treated as
	//     "filter off" so a half-configured filter never silently blocks all).
	//   - FilterModeDeny ("deny", default): denylist — an item is skipped if it
	//     matches the list; everything else passes.
	// The *DenyTitles/*DenyAuthors/*DenyLabels fields hold the LIST for each
	// filter regardless of mode (names kept for backward compatibility with
	// existing on-disk config; the mode decides allow vs deny). ContributeAllowLabels
	// is retained only for one-time migration into DenyLabels+LabelsMode.
	ContributeTitlesMode          string   `yaml:"contribute_titles_mode,omitempty"`
	ContributeAuthorsMode         string   `yaml:"contribute_authors_mode,omitempty"`
	ContributeLabelsMode          string   `yaml:"contribute_labels_mode,omitempty"`
	ContributeAllowLabels         []string `yaml:"contribute_allow_labels"`
	ContributeDenyLabels          []string `yaml:"contribute_deny_labels"`
	ContributeDenyTitles          []string `yaml:"contribute_deny_titles"`
	ContributeDenyAuthors         []string `yaml:"contribute_deny_authors"`
	ContributeAllowModels         []string `yaml:"contribute_allow_models"`
	ContributeRejectUnknownModels bool     `yaml:"contribute_reject_unknown_models"`
	// ContributeSkipAssignedToOthers, when true, makes the /contribute queue
	// skip any issue that is already assigned to someone OTHER than the
	// contributor requesting work. An issue assigned to the contributor
	// themselves (or unassigned) is still eligible. Default false preserves the
	// prior behavior of handing out issues regardless of assignment (#2357).
	ContributeSkipAssignedToOthers bool `yaml:"contribute_skip_assigned_to_others"`
	// ContributeCooldownEnabled toggles the POST-COMPLETION cooldown that keeps a
	// just-worked issue out of the /contribute queue for a while (see
	// contribute_ws.go markTaskCompleted / isTaskInCooldown). It is a POINTER so
	// that an absent value (older on-disk config that predates this toggle) means
	// "unset" and defaults to ENABLED — the prior, backward-compatible behavior.
	// A non-nil false explicitly DISABLES cooldown gating (no completed issue is
	// ever excluded from the queue for cooldown; failure quarantine is unaffected
	// and stays on). Use IsContributeCooldownEnabled() to resolve the effective
	// value rather than reading the pointer directly.
	ContributeCooldownEnabled *bool `yaml:"contribute_cooldown_enabled,omitempty"`
	// ContributeCooldownHours is the WITH-PR completion cooldown period in hours —
	// the operator-tunable replacement for the hardcoded default of
	// contributeCooldownDefaultHours (168h / one week). 0 or unset means "use the
	// default"; any positive value is clamped to
	// [contributeCooldownMinHours, contributeCooldownMaxHours] in Normalize.
	// Resolve with ContributeCooldownHoursOrDefault(), never by reading the raw
	// field (which may legitimately be 0 == default). The short NO-PR cooldown is
	// left as its own const and is not tuned here (the operator specifically asked
	// for the week-long period to be adjustable).
	ContributeCooldownHours int `yaml:"contribute_cooldown_hours,omitempty"`
	// ContributeQueueOrder is the OPERATOR PRIORITY OVERRIDE for the ready-work
	// queue: an ordered list of "owner/repo#number" keys the operator dragged to
	// the front on the Operations tab. When set, these issues are OFFERED FIRST —
	// both in the queue display (ReadyQueue) and in selectTask's candidate ordering
	// — in exactly this order; everything else follows in the established default
	// order. It only reorders OFFER PRIORITY: a key here that is filtered out by
	// admission / cooldown / disabled-repo / in-flight rules is still excluded, and
	// a stale key (no longer actionable) is simply skipped. Persisted through the
	// same Config.Hub.* mechanism as the other admission settings so it survives
	// restart, and edited only through the authenticated PUT /api/contribute/queue/order
	// endpoint (owner/read-write only).
	ContributeQueueOrder []string `yaml:"contribute_queue_order,omitempty"`
	// ContributeQueueHold is the OPERATOR HOLD set for the ready-work queue: an
	// unordered list of "owner/repo#number" keys the operator parked from the
	// Operations tab. A held issue is NEVER offered — it is excluded from BOTH the
	// queue display's offer-eligible set (ReadyQueue) and selectTask's candidate
	// selection — and it stays parked INDEFINITELY until the operator Resumes it.
	// This is DISTINCT from cooldown (time-based, self-clearing): a hold is a
	// manual, persistent operator decision. Held rows remain VISIBLE on the
	// Operations tab (rendered greyed with an "on hold" badge) so the operator can
	// always see and Resume them. Persisted through the same Config.Hub.* mechanism
	// as ContributeQueueOrder so it survives restart, and edited only through the
	// authenticated POST /api/contribute/queue/hold endpoint (owner/read-write only).
	ContributeQueueHold []string `yaml:"contribute_queue_hold,omitempty"`
	// ContributeQueueHoldReasons is an OPTIONAL parallel map (canonical
	// "owner/repo#number" key -> short operator note) annotating why an issue in
	// ContributeQueueHold was parked. It is a companion to — not a replacement for —
	// ContributeQueueHold: the []string set above remains the authoritative source of
	// truth for WHICH issues are held (every admission check reads it); this map only
	// carries the human-facing REASON, surfaced in the on-hold badge tooltip. A hold
	// with no reason simply has no entry here (the badge falls back to its generic
	// text), so holding without a note works exactly as before. Kept as a parallel
	// map, rather than folding the reason into ContributeQueueHold, so the many
	// read sites of the []string set stay untouched. Written only by the same
	// authenticated POST /api/contribute/queue/hold endpoint that maintains the set,
	// and pruned to the held keys on every write so it never leaks stale reasons.
	ContributeQueueHoldReasons map[string]string `yaml:"contribute_queue_hold_reasons,omitempty"`
	// ContributeRequireExplicitAccept gates HOW a contributor's scoped GitHub
	// credential is delivered relative to task acceptance (kubestellar/hive#2537).
	// The credential is ALWAYS delivered only AFTER an acceptance decision — it no
	// longer travels bundled in the task_assign message. This toggle only chooses
	// WHO makes that decision:
	//   - nil / false (DEFAULT): trusted-source AUTO-ACCEPT. A task that already
	//     passed admission (the title/author/label filters, disabled-repo/tier
	//     gates, cooldown, and the per-tier trust gate in selectTask) is
	//     auto-accepted the instant it is assigned, and the scoped credential is
	//     delivered immediately after — no human in the loop. This keeps an
	//     unattended fleet running exactly as before: the only observable change is
	//     that the credential arrives in a distinct message right after task_assign
	//     rather than inside it.
	//   - true: EXPLICIT (manual/human) acceptance. The hub withholds the credential
	//     until the client sends a task_accepted for the assigned task; a task that
	//     is never accepted (declined, timed out, or reconnected away) never
	//     receives a credential. This is the opt-in "mandatory acceptance" mode for
	//     operators who want a wait state.
	// A POINTER so an absent value (older on-disk config) resolves to the
	// backward-compatible auto-accept default via IsContributeRequireExplicitAccept().
	ContributeRequireExplicitAccept *bool `yaml:"contribute_require_explicit_accept,omitempty"`
	// ContributeDelegatableRoles is the hive-wide allow-list of spoke agent roles
	// a contributor relay may request via HIVE_AGENT_ROLE / auth_response.role.
	// Empty means the safe default set: scanner, quality, outreach. Privileged roles
	// (ci-maintainer, sec-check, architect) must be explicitly listed here AND
	// granted on the contributor profile; supervisor is never delegatable.
	ContributeDelegatableRoles []string            `yaml:"contribute_delegatable_roles,omitempty"`
	DisabledRepos              []string            `yaml:"disabled_repos"`
	DisabledTiers              []string            `yaml:"disabled_tiers"`
	TierLimits                 map[string]TierRate `yaml:"tier_limits"`
	SnapshotIntervalMin        int                 `yaml:"snapshot_interval_min"`
}

// Contribute completion-cooldown defaults and clamp bounds. These live in the
// config package because both the resolver methods below and the Normalize path
// reference them; the dashboard keeps its own equal DEFAULT const
// (completedTaskCooldownHours) as the runtime fallback for hubs built without a
// Config (e.g. direct-in-test construction).
const (
	// contributeCooldownDefaultHours is the with-PR completion cooldown used when
	// ContributeCooldownHours is unset/0 — one week, matching the historical
	// hardcoded default.
	contributeCooldownDefaultHours = 168
	// contributeCooldownMinHours / contributeCooldownMaxHours clamp an
	// operator-supplied period to a sane range (one hour .. one year) so a stray
	// value cannot park an issue effectively forever or disable the cooldown by
	// rounding to zero.
	contributeCooldownMinHours = 1
	contributeCooldownMaxHours = 8760
)

// IsContributeCooldownEnabled resolves the effective on/off state of the
// post-completion cooldown. A nil pointer (unset, older config) defaults to
// ENABLED for backward compatibility; an explicit false disables it.
func (h HubConfig) IsContributeCooldownEnabled() bool {
	return h.ContributeCooldownEnabled == nil || *h.ContributeCooldownEnabled
}

// IsContributeRequireExplicitAccept resolves the effective acceptance mode for
// contributor credential delivery (kubestellar/hive#2537). A nil pointer (unset,
// older config) resolves to FALSE — trusted-source auto-accept — so an existing
// deployment keeps handing credentials to admitted tasks without a wait state; an
// explicit true opts into mandatory (human/manual) acceptance where the credential
// is withheld until the client accepts the assigned task.
func (h HubConfig) IsContributeRequireExplicitAccept() bool {
	return h.ContributeRequireExplicitAccept != nil && *h.ContributeRequireExplicitAccept
}

var defaultContributeDelegatableRoles = []string{"scanner", "quality", "outreach"}

// ContributeDelegatableRoleSet resolves the hive-wide allow-list of spoke roles
// a clanker may claim. Empty config preserves the safe v1 default; supervisor is
// deliberately removed even if an operator lists it because it manages the fleet.
func (h HubConfig) ContributeDelegatableRoleSet() map[string]bool {
	out := make(map[string]bool, len(defaultContributeDelegatableRoles)+len(h.ContributeDelegatableRoles))
	for _, role := range defaultContributeDelegatableRoles {
		out[role] = true
	}
	roles := h.ContributeDelegatableRoles
	for _, role := range roles {
		role = strings.ToLower(strings.TrimSpace(role))
		if role == "" || role == "supervisor" {
			continue
		}
		out[role] = true
	}
	return out
}

// IsContributeRoleDelegatable reports whether role is enabled hive-wide for
// clanker delegation. It does not make any per-contributor or trust-tier decision.
func (h HubConfig) IsContributeRoleDelegatable(role string) bool {
	return h.ContributeDelegatableRoleSet()[strings.ToLower(strings.TrimSpace(role))]
}

// ContributeCooldownHoursOrDefault resolves the with-PR cooldown PERIOD in
// hours. A value <= 0 (unset) yields the default (contributeCooldownDefaultHours);
// a positive value is returned as-is (Normalize has already clamped any stored
// value to the valid range, and this method re-clamps defensively for callers
// that build a Hub without running Normalize, e.g. tests).
func (h HubConfig) ContributeCooldownHoursOrDefault() int {
	if h.ContributeCooldownHours <= 0 {
		return contributeCooldownDefaultHours
	}
	if h.ContributeCooldownHours < contributeCooldownMinHours {
		return contributeCooldownMinHours
	}
	if h.ContributeCooldownHours > contributeCooldownMaxHours {
		return contributeCooldownMaxHours
	}
	return h.ContributeCooldownHours
}

type TierRate struct {
	MaxPerHour    int `yaml:"max_per_hour" json:"max_per_hour"`
	MaxPerDay     int `yaml:"max_per_day" json:"max_per_day"`
	MaxConcurrent int `yaml:"max_concurrent" json:"max_concurrent"`
}

type DashboardConfig struct {
	Port               int    `yaml:"port"`
	SnapshotDir        string `yaml:"snapshot_dir"`
	AuthToken          string `yaml:"auth_token"`
	AgentPollIntervalS int    `yaml:"agent_poll_interval_s"`
	// SnapshotFrameAncestors is the explicit set of HTTPS origins allowed to
	// embed the public, read-only /snapshot document. Empty keeps the historical
	// fail-closed framing policy (X-Frame-Options: DENY plus CSP
	// frame-ancestors 'none'). Wildcards and paths are deliberately rejected so
	// the configured value is a small, auditable origin allowlist.
	SnapshotFrameAncestors []string `yaml:"snapshot_frame_ancestors,omitempty" json:"snapshot_frame_ancestors,omitempty"`
	// AuthorizedUsers is the allowlist of GitHub usernames permitted to log in
	// to a direct-route (non-hub-proxied) spoke via the device flow. The first
	// entry is treated as the owner (read-write); the rest are granted viewers
	// (read-only) unless an explicit "username:role" suffix is given. On the
	// hub-proxied path, nginx injects X-Hive-User/X-Hive-Role and this list is
	// consulted only by the read-only Access tab, NOT for gating logins.
	// Populated on both hub-proxied hosted hives (for the Access view + hub
	// Manage Access) and standalone direct-route spokes (for device-flow authz).
	AuthorizedUsers []string `yaml:"authorized_users"`
	// AuthorizedUserNames is an OPTIONAL, purely cosmetic map from an
	// AuthorizedUsers entry's raw identity key (the same string before any
	// ":role" suffix, e.g. "ibmid:5500087VJB" or a plain GitHub login) to a
	// human-readable display name. It rides alongside AuthorizedUsers — never
	// inside it — so the identity key used for allowlist matching and grants
	// (AuthorizedRole, IsDirectRouteAuthzEnabled) is completely unaffected by
	// this field's presence, absence, or content. Delivered by the hub in the
	// same heartbeat beat as AuthorizedUsers (mirrors it 1:1) so the read-only
	// Access tab can render "Jane Doe" instead of a raw IBMid/Google/Microsoft
	// subject. A key with no entry here (or an empty value) simply has no known
	// human name yet — the UI falls back to the raw key. omitempty/nil-safe:
	// existing configs and hubs that never send this round-trip unchanged.
	AuthorizedUserNames map[string]string `yaml:"authorized_user_names,omitempty"`
	// HubProxied is true when this hive sits behind the hub's nginx auth-proxy,
	// which authenticates every request and injects trusted X-Hive-User/X-Hive-Role
	// headers (hub-reachable-cluster hosted hives). When true the hive TRUSTS those headers and
	// keeps the shared-token path enabled even if AuthorizedUsers is non-empty —
	// nginx is the gate, and the allowlist is informational (Access tab) only.
	//
	// When false (the default) a non-empty AuthorizedUsers list means this is a
	// STANDALONE direct-route spoke with no nginx in front (the heartbeat-only cluster), so it must
	// strip client-supplied identity headers and enforce per-user device-flow
	// authz itself. Decoupling these two meanings fixes hub-proxied hives being
	// wrongly forced into direct-route mode (which broke their dashboard link and
	// snapshot preview) the moment they were granted an authorized_users list.
	HubProxied bool `yaml:"hub_proxied"`
	// PublicURL is the externally reachable origin of THIS dashboard
	// (scheme + host[:port], no path), used to build OAuth redirect URIs —
	// the Linear agent install and the OpenRouter funding flow — when it
	// differs from the host the request arrived on. Precedence when a
	// callback URL is built: dashboard.public_url, then hub.dashboard_url
	// (kept for hub-hosted spokes, whose hub already knows their public
	// name), then the X-Forwarded-Proto/X-Forwarded-Host/Host of the request.
	// Set it on a hub-less hive whose dashboard is private but whose OAuth
	// callback path is published on a different public hostname, or behind
	// an ingress that rewrites the Host header on the way in (Traefik with a
	// fixed upstream Host, a Cloudflare Tunnel "HTTP Host Header"): with
	// nothing configured, the install leg and the callback leg can derive
	// different origins and the provider rejects the code exchange with
	// "redirect_uri is invalid". Validated at load time by
	// ValidateDashboardPublicURL.
	PublicURL string `yaml:"public_url,omitempty" json:"public_url,omitempty"`
}

// ValidateDashboardPublicURL validates and normalizes dashboard.public_url:
// an absolute http(s) URL naming an origin only — no path, query, fragment or
// credentials — returned with any trailing slash removed. Empty is valid and
// means "unset".
func ValidateDashboardPublicURL(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", nil
	}
	u, err := url.Parse(v)
	if err != nil {
		return "", fmt.Errorf("dashboard.public_url %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("dashboard.public_url %q: must be an absolute http:// or https:// URL", raw)
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("dashboard.public_url %q: missing host", raw)
	}
	if u.User != nil {
		return "", fmt.Errorf("dashboard.public_url %q: credentials are not allowed", raw)
	}
	if strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return "", fmt.Errorf("dashboard.public_url %q: must be an origin only (scheme://host[:port]) with no path, query or fragment", raw)
	}
	return u.Scheme + "://" + u.Host, nil
}

var snapshotFrameAncestorHostPattern = regexp.MustCompile(`(?i)^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*\.?$`)

// ValidateSnapshotFrameAncestors validates and normalizes the /snapshot framing
// allowlist. Only exact HTTPS origins are accepted: scheme + host with optional
// port, and no path, query, fragment, credentials, or wildcard hostnames.
func ValidateSnapshotFrameAncestors(origins []string) ([]string, error) {
	normalized := make([]string, 0, len(origins))
	seen := map[string]struct{}{}
	for _, raw := range origins {
		origin := strings.TrimSpace(raw)
		if origin == "" {
			continue
		}
		u, err := url.Parse(origin)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return nil, fmt.Errorf("snapshot_frame_ancestors entry %q must be an https origin", raw)
		}
		if u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || strings.Contains(u.Hostname(), "*") {
			return nil, fmt.Errorf("snapshot_frame_ancestors entry %q must be an exact https origin with no path, credentials, query, fragment, or wildcard", raw)
		}
		if !validSnapshotFrameAncestorHost(u.Hostname()) {
			return nil, fmt.Errorf("snapshot_frame_ancestors entry %q has an invalid host", raw)
		}
		if port := u.Port(); port != "" {
			n, err := strconv.Atoi(port)
			if err != nil || n < 1 || n > 65535 {
				return nil, fmt.Errorf("snapshot_frame_ancestors entry %q has an invalid port", raw)
			}
		}
		origin = "https://" + u.Host
		if _, ok := seen[origin]; ok {
			continue
		}
		seen[origin] = struct{}{}
		normalized = append(normalized, origin)
	}
	return normalized, nil
}

func validSnapshotFrameAncestorHost(host string) bool {
	if host == "" {
		return false
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	return snapshotFrameAncestorHostPattern.MatchString(host)
}

// SnapshotFrameAncestorsCSP returns the CSP source list for /snapshot framing.
func (d DashboardConfig) SnapshotFrameAncestorsCSP() string {
	if len(d.SnapshotFrameAncestors) == 0 {
		return "'none'"
	}
	return strings.Join(d.SnapshotFrameAncestors, " ")
}

// Role strings used for direct-route spoke authorization. These mirror the
// roles the hub injects via X-Hive-Role on the proxied path so the read-only
// gating in the dashboard behaves identically on both paths.
const (
	RoleOwner = "owner"
	RoleRead  = "read"
	// RoleReadWrite is granted by the hub's Manage Access screen (see the role
	// validation in hub.handleAccessAdd, which accepts read / read-write /
	// owner). It was missing here, so splitAuthorizedEntry treated
	// "user:read-write" as an unknown suffix and folded the whole string into
	// the username — the allowlist then contained a user literally named
	// "cbrooker27:read-write", no lookup could ever match, and every login was
	// rejected as unauthorized despite the grant being present and correct.
	RoleReadWrite = "read-write"
	// RoleMerger can do everything read-write can, plus approve/queue other
	// people's PRs for Hive's auto-merge-on-green sweep. Both the dashboard
	// queue endpoint and the sweep enforce the "never your own PR" rule
	// server-side.
	RoleMerger = "merger"
)

var roleRanks = map[string]int{
	RoleRead:      1,
	RoleReadWrite: 2,
	RoleMerger:    3,
	RoleOwner:     4,
}

// ValidRole reports whether role is one of the hive access tiers.
func ValidRole(role string) bool {
	_, ok := roleRanks[strings.ToLower(strings.TrimSpace(role))]
	return ok
}

// RoleAtLeast reports whether role includes all capabilities of tier.
func RoleAtLeast(role, tier string) bool {
	roleRank, okRole := roleRanks[strings.ToLower(strings.TrimSpace(role))]
	tierRank, okTier := roleRanks[strings.ToLower(strings.TrimSpace(tier))]
	return okRole && okTier && roleRank >= tierRank
}

// AuthorizedRole resolves a username against the spoke's authorized-users
// allowlist and returns the user's role and whether they are authorized.
//
// Each entry is either "username" or "username:role" (role = "owner" or "read").
// An entry without an explicit role defaults to "owner" for the first entry
// (the hive owner) and "read" for the rest (granted viewers). Matching is
// case-insensitive because GitHub usernames are case-insensitive, and tolerant
// of the hub's canonical identity form: a bare login and its "github:<login>"
// wire form denote the SAME user (the hub's legacy shim), so an allowlist entry
// in either form matches a session username in either form. Without this, a
// hub-delivered "github:alice:owner" entry silently failed to authorize the
// device-flow login "alice" — one more variant of the recurring
// "granted owner still gets 'owner access required'" class.
func (d DashboardConfig) AuthorizedRole(username string) (string, bool) {
	if username == "" {
		return "", false
	}
	want := identityMatchKey(username)
	for i, entry := range d.AuthorizedUsers {
		name, role := splitAuthorizedEntry(entry)
		if name == "" {
			continue
		}
		if identityMatchKey(name) != want {
			continue
		}
		if role == "" {
			if i == 0 {
				role = RoleOwner
			} else {
				role = RoleRead
			}
		}
		return role, true
	}
	return "", false
}

// identityMatchKey normalizes an identity string for allowlist comparison:
// lower-cased (GitHub logins are case-insensitive, and the pre-existing
// allowlist match always folded case), with the legacy-GitHub provider prefix
// stripped so "github:alice" and "alice" compare equal. Other providers'
// prefixes ("ibmid:", "google:", ...) are kept — those subjects are only ever
// delivered and presented in their full canonical form.
func identityMatchKey(id string) string {
	key := strings.ToLower(strings.TrimSpace(id))
	return strings.TrimPrefix(key, "github:")
}

// IsDirectRouteAuthzEnabled reports whether this hive must enforce per-user
// authorization on device-flow logins ITSELF because there is no hub nginx in
// front of it. True only for a STANDALONE spoke: it has an authorized-users
// allowlist AND is not hub-proxied.
//
// A hub-proxied hive (HubProxied=true) can also carry an allowlist — for the
// read-only Access tab and the hub's Manage Access screen — but nginx is the
// gate there, so it must NOT flip into direct-route mode (which would strip the
// trusted X-Hive-User/X-Hive-Role headers nginx injects and disable the shared
// token, breaking the dashboard link and the snapshot preview).
func (d DashboardConfig) IsDirectRouteAuthzEnabled() bool {
	return !d.HubProxied && len(d.AuthorizedUsers) > 0
}

// splitAuthorizedEntry parses a "username" or "username:role" allowlist entry.
func splitAuthorizedEntry(entry string) (name, role string) {
	entry = strings.TrimSpace(entry)
	if idx := strings.LastIndex(entry, ":"); idx >= 0 {
		name = strings.TrimSpace(entry[:idx])
		role = strings.ToLower(strings.TrimSpace(entry[idx+1:]))
		if !ValidRole(role) {
			// Unknown role suffix — treat the whole thing as a bare username so
			// a stray colon can never silently downgrade or escalate access.
			return strings.TrimSpace(entry), ""
		}
		return name, role
	}
	return entry, ""
}
