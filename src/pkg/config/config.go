package config

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kubestellar/hive/pkg/resolve"
	"gopkg.in/yaml.v3"
)

// saveMu serializes all Config.Save() calls process-wide. Multiple goroutines
// persist config concurrently — the mutex-guarded pause callback, the async
// dashboard PersistFunc, the ACMM-level saver, HTTP mutation handlers. Each
// does yaml.Marshal(c) + write. Without serialization, two Save() calls race:
// the one that finishes writing LAST wins the file, and if it marshaled a
// staler snapshot (e.g. before a later pause committed to c.Agents), that
// pause is silently lost. Pausing 7 agents in quick succession reliably left
// only the last 1-2 on the PVC. Serializing every Save() closes the race.
var saveMu sync.Mutex

type Config struct {
	Project       ProjectConfig          `yaml:"project"`
	Policies      PoliciesConfig         `yaml:"policies"`
	Agents        map[string]AgentConfig `yaml:"agents"`
	Governor      GovernorConfig         `yaml:"governor"`
	GitHub        GitHubConfig           `yaml:"github"`
	GitLab        GitLabConfig           `yaml:"gitlab,omitempty"`
	Gitea         GiteaConfig            `yaml:"gitea,omitempty"`
	Notifications NotificationsConfig    `yaml:"notifications"`
	Dashboard     DashboardConfig        `yaml:"dashboard"`
	Data          DataConfig             `yaml:"data"`
	Knowledge     KnowledgeConfig        `yaml:"knowledge"`
	Hub           HubConfig              `yaml:"hub"`
	HiveID        string                 `yaml:"hive_id"`
	ACMMLevel     *int                   `yaml:"acmm_level,omitempty" json:"acmm_level"`
	Variables     VariablesConfig        `yaml:"variables,omitempty"`
	// OTel configures standards-based OTLP trace export. It is the preferred
	// operator-facing block; Tracing is retained as a legacy alias.
	OTel    OTelConfig `yaml:"otel,omitempty" json:"otel,omitempty"`
	Tracing OTelConfig `yaml:"tracing,omitempty"`
	// Triggers is an additive list of CEL-based declarative agent triggers.
	// Default empty → existing label/governor triggering is unchanged.
	Triggers []TriggerRule `yaml:"triggers,omitempty" json:"triggers,omitempty"`
	// Hooks is an additive list of operator-declared state-triggered hooks
	// (RFC #4001): `on: <transition>` → `action: <vetted action>`. Default
	// empty → no hooks fire and behavior is byte-identical to before.
	//
	// This list is OPERATOR-ONLY by construction: it is config, so writing it
	// requires the same authz and carries the same layer provenance as any
	// other config write, and there is deliberately no runtime registration
	// API. Nothing agent-writable can reach it — an agent able to register
	// hooks on its own transitions would have an escalation path.
	Hooks []HookRule `yaml:"hooks,omitempty" json:"hooks,omitempty"`
	// ToolApproval configures the approval desk (RFC #4000): the single
	// decision point every approval-shaped request resolves through, plus the
	// operator rules that steer it. Additive and DEFAULT-OFF — an absent block
	// leaves every existing gate in charge and behavior byte-identical.
	ToolApproval ToolApprovalConfig `yaml:"tool_approval,omitempty" json:"tool_approval,omitempty"`
	Mint         MintConfig         `yaml:"mint,omitempty"`
	Ioscan       IoscanConfig       `yaml:"ioscan,omitempty" json:"ioscan,omitempty"`
	Classifier   ClassifierConfig   `yaml:"classifier,omitempty" json:"classifier,omitempty"`
	Planning     PlanningConfig     `yaml:"planning,omitempty" json:"planning,omitempty"`
	Quality      QualityConfig      `yaml:"quality,omitempty" json:"quality,omitempty"`
	Intent       IntentConfig       `yaml:"intent,omitempty" json:"intent,omitempty"`
	Escalation   EscalationConfig   `yaml:"escalation,omitempty" json:"escalation,omitempty"`
	Retro        RetroConfig        `yaml:"retro,omitempty" json:"retro,omitempty"`
	Review       ReviewConfig       `yaml:"review,omitempty" json:"review,omitempty"`
	AutoMerge    AutoMergeConfig    `yaml:"auto_merge,omitempty" json:"auto_merge,omitempty"`
	// AgentSandbox configures the phase-1 credential-free sandbox runner. It is
	// disabled by default and agents must opt in individually.
	AgentSandbox AgentSandboxConfig `yaml:"agent_sandbox,omitempty" json:"agent_sandbox,omitempty"`
	// Convergence toggles the convergence-driven admission surfaces
	// (kubestellar/hive#3845 follow-ons). Default off → zero behaviour change.
	Convergence ConvergenceConfig `yaml:"convergence,omitempty" json:"convergence,omitempty"`

	// RemovedAgents are agent names an operator deliberately deleted. It is a
	// TOMBSTONE list, and it exists because deletion had no durable record
	// anywhere: the delete handlers dropped the agent from the in-memory map
	// and re-saved the overlay, but an agent lives in THREE places — the
	// ConfigMap seed, /data/agent-configs/<name>.yaml, and the dashboard
	// overlay — and Load() UNIONS all three via MergeAgentOverrides, which
	// only ever adds. So the next config reload (fsnotify, observed ~36s after
	// the delete on a live hive) re-materialized the agent, and even after
	// that the next ApplyPack re-created it from the ACMM pack. That is the
	// reported "I deleted brainstorm and guide and they always come back".
	//
	// A tombstone is scoped to the agent NAME and persists indefinitely,
	// including across ACMM level changes. The alternative — clearing
	// tombstones on a level change so a higher pack can reintroduce the agent
	// — was rejected: an operator who deletes `guide` is expressing "I do not
	// want this agent", not "I do not want it at this level", and silently
	// resurrecting it during an unrelated level bump is the same class of
	// silent revert as the bug itself. Re-adding the agent explicitly (the
	// Governor grid's add, the agent CRUD create, or an import) clears the
	// tombstone, which is the one unambiguous signal that the operator changed
	// their mind. A genuinely NEW pack agent — one never deleted here — is
	// unaffected and is still added on a level increase.
	RemovedAgents []string `yaml:"removed_agents,omitempty" json:"removed_agents,omitempty"`

	SourcePath string `yaml:"-" json:"-"`
}

// DefaultOTelServiceName is the OTLP resource service.name used when the
// operator does not set otel.service_name.
const DefaultOTelServiceName = "hive"

// OTelConfig configures OpenTelemetry trace export. It is additive and OFF by
// default: a config with no `otel:` block (or enabled:false) yields a
// zero-overhead no-op tracer. When enabled, spans are exported over OTLP/HTTP
// to Endpoint (or the standard OTEL_EXPORTER_OTLP_ENDPOINT env var when
// Endpoint is empty).
type OTelConfig struct {
	// Enabled turns OTLP trace export on. Default false — the zero value keeps
	// every existing config a no-op with no exporter and no network activity.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Endpoint is the OTLP/HTTP collector endpoint (host:port or full URL).
	// When empty, the exporter falls back to OTEL_EXPORTER_OTLP_ENDPOINT.
	Endpoint string `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`
	// Headers are optional static OTLP/HTTP headers, commonly used for collector
	// authentication. Values should come from env interpolation, not literals.
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
	// ServiceName is recorded as resource service.name. Empty defaults to "hive".
	ServiceName string `yaml:"service_name,omitempty" json:"service_name,omitempty"`
	// Insecure disables TLS for OTLP/HTTP. Leave false for https collectors.
	Insecure bool `yaml:"insecure,omitempty" json:"insecure,omitempty"`
	// SampleRatio is the head-based sampling ratio in [0.0, 1.0]. The zero
	// value is treated as "sample everything" (1.0) so an operator who only
	// sets enabled:true gets full traces; set explicitly to sample less.
	SampleRatio float64 `yaml:"sample_ratio,omitempty" json:"sample_ratio,omitempty"`
}

// ServiceNameOrDefault returns a valid resource service.name.
func (o OTelConfig) ServiceNameOrDefault() string {
	if name := strings.TrimSpace(o.ServiceName); name != "" {
		return name
	}
	return DefaultOTelServiceName
}

// IsZero reports whether the block contains no operator-provided settings.
func (o OTelConfig) IsZero() bool {
	return !o.Enabled && o.Endpoint == "" && len(o.Headers) == 0 && o.ServiceName == "" && !o.Insecure && o.SampleRatio == 0
}

// EffectiveOTel returns the preferred otel block, falling back to the legacy
// tracing block so existing configs continue to work unchanged.
func (c *Config) EffectiveOTel() OTelConfig {
	if c == nil {
		return OTelConfig{}
	}
	if !c.OTel.IsZero() {
		return c.OTel
	}
	return c.Tracing
}

// MintConfig configures the OIDC token mint service (pkg/mint). It is additive
// and DISABLED by default: an absent `mint:` block, or Enabled=false, leaves
// existing behavior byte-identical. When enabled, the mint issues short-lived
// scoped JWTs (a Workload Identity Federation broker) that downstream cloud/
// registry WIF providers can trust via Issuer + JWKS.
type MintConfig struct {
	// Enabled turns the mint service on. Default false (deny).
	Enabled bool `yaml:"enabled,omitempty"`
	// KeyPath is the PEM path of the signing key. If the file is absent the
	// mint generates one and persists it with 0600 perms. Required when enabled.
	KeyPath string `yaml:"key_path,omitempty"`
	// Issuer is the `iss` claim and the identity WIF providers are configured to
	// trust (typically the mint's public URL). Required when enabled.
	Issuer string `yaml:"issuer,omitempty"`
	// MaxTTLSeconds bounds a minted token's lifetime. 0 uses the package default
	// (15m). The value is clamped to the package hard cap (1h) regardless.
	MaxTTLSeconds int `yaml:"max_ttl_seconds,omitempty"`
}

type PoliciesConfig struct {
	Repo         string        `yaml:"repo"`
	Branch       string        `yaml:"branch"`
	Path         string        `yaml:"path"`
	PollInterval time.Duration `yaml:"poll_interval"`
	LocalDir     string        `yaml:"local_dir"`
}

// StatsDisplayEntry defines a single metric to show in the agent's sidebar/detail view.
type StatsDisplayEntry struct {
	Key        string `yaml:"key" json:"key"`
	Label      string `yaml:"label" json:"label"`
	Source     string `yaml:"source" json:"source"`
	Field      string `yaml:"field" json:"field"`
	Style      string `yaml:"style" json:"style"`
	TrendField string `yaml:"trend_field,omitempty" json:"trendField,omitempty"`
	Target     int    `yaml:"target,omitempty" json:"target,omitempty"`
	// Desc is a one-line explanation of what the stat verifies, rendered
	// as a hover tooltip in the dashboard (health checks especially).
	Desc string `yaml:"desc,omitempty" json:"desc,omitempty"`
}

type LoggingConfig struct {
	Dir        string `yaml:"dir"`
	MaxSizeMB  int    `yaml:"max_size_mb"`
	MaxAgeDays int    `yaml:"max_age_days"`
	MaxBackups int    `yaml:"max_backups"`
	Compress   bool   `yaml:"compress"`
	Level      string `yaml:"level"`
}

type LabelsConfig struct {
	Exempt []string `yaml:"exempt"`
	// AutoMerge is the label a merger/owner queue action applies to a PR.
	// Configurable because the label has to live in someone else's
	// repository, where the local convention already exists: Prow-style
	// projects have used `lgtm` for exactly this decision for years, and a
	// hive that hard-codes its own name either collides with that or forces
	// every managed repo to grow a second label meaning the same thing.
	// Defaults to DefaultAutoMergeLabel.
	AutoMerge string `yaml:"automerge"`
}

type SensingConfig struct {
	GHRatePatterns     []string `yaml:"gh_rate_patterns"`
	CLIExcludePatterns []string `yaml:"cli_exclude_patterns"`
	LoginPatterns      []string `yaml:"login_patterns"`
	TTLSeconds         int      `yaml:"ttl_seconds"`
	PullbackSeconds    int      `yaml:"pullback_seconds"`
}

// defaultLoginPatterns is the built-in login-detector pattern set (#3959):
// every entry matches a CLI's own login CHROME, never ordinary English, so an
// agent is not paused for merely reading or discussing an auth error. It is a
// named var (not an inline literal in applyDefaults) so persistence can
// recognize "this list IS the default" and skip writing it — see
// redactedForPersist, which is what keeps the default set from being
// materialized into saved configs and pinned there forever (#4041).
var defaultLoginPatterns = []string{
	// claude: exact prompt strings from Claude Code's login screens.
	"Please run /login",
	"Not logged in",
	// gh / copilot / gemini: the commands their CLIs tell the user to run.
	"gh auth login",
	"claude login",
	// "copilot auth login" is the Copilot CLI's full logged-out instruction.
	// The bare 2-word fragment "copilot auth" false-positived: it also matched
	// `copilot auth status` (an auth CHECK) and incidental doc/comment mentions
	// (e.g. bin/copilot-models.mjs), pausing logged-IN agents — a live quality
	// agent flapped on `(?i)copilot auth` for days (restart_count 83). Tightening
	// to the full command matches the specificity of its `gh auth login` /
	// `gemini auth login` siblings and still catches genuine Copilot logouts.
	"copilot auth login",
	"gemini auth login",
	// bob: its API-key entry prompts.
	"Enter Bob-Shell API Key",
	"Paste your API key here",
}

// legacyDefaultLoginPatterns is the pre-#3959 default login-detector list,
// frozen verbatim (values AND order) as it shipped from the day the field
// existed until da9f6ff2. It exists only for migration: every hive that saved
// its config in that window has this exact list materialized as explicit
// values (Save() marshals defaults along with everything else), which
// permanently defeated the #3959 defaults fix because defaults only apply to
// an empty list (#4041). Never extend or reorder it — a byte-identical match
// is the evidence that the list carries no operator intent.
var legacyDefaultLoginPatterns = []string{
	"please log in",
	"authentication required",
	"not logged in",
	"login required",
	"session expired",
	"token expired",
	"unauthorized.*401",
	"gh auth login",
	"claude login",
	"copilot auth",
}

// IsLegacyDefaultLoginPatterns reports whether list is byte-identical (same
// entries, same order) to the pre-#3959 default login-pattern set. Exported
// because cmd/hive's legacy state-overrides migration replays a persisted
// sensing_login list over the loaded config, which is the same materialized-
// defaults hazard applyDefaults migrates in the config file itself (#4041).
func IsLegacyDefaultLoginPatterns(list []string) bool {
	return stringSlicesEqual(list, legacyDefaultLoginPatterns)
}

// stringSlicesEqual is an exact element-wise comparison (no normalization —
// migration must only ever fire on a byte-identical match).
func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

type HealthConfig struct {
	HealthcheckInterval int  `yaml:"healthcheck_interval"`
	RestartCooldown     int  `yaml:"restart_cooldown"`
	ModelLock           bool `yaml:"model_lock"`
}

type BudgetConfig struct {
	TotalTokens int64 `yaml:"total_tokens"`
	PeriodDays  int   `yaml:"period_days"`
	CriticalPct int   `yaml:"critical_pct"`
}

type ModeConfig struct {
	Threshold int                `yaml:"threshold"`
	Cadences  map[string]Cadence `yaml:"cadences"`
}

// UnmarshalYAML implements custom unmarshaling for ModeConfig.
// The YAML format has threshold and agent cadences as sibling keys:
//
//	idle:
//	  threshold: 0
//	  scanner: 15m
//	  ci-maintainer: 15m
//
// This method separates "threshold" into the Threshold field and collects
// all other keys into the Cadences map.
func (m *ModeConfig) UnmarshalYAML(value *yaml.Node) error {
	var raw map[string]Cadence
	if err := value.Decode(&raw); err != nil {
		return err
	}

	m.Cadences = make(map[string]Cadence)

	const thresholdKey = "threshold"
	if v, ok := raw[thresholdKey]; ok {
		var t int
		if _, err := fmt.Sscanf(v.Interval(), "%d", &t); err != nil {
			return fmt.Errorf("invalid threshold value %q: %w", v.Interval(), err)
		}
		m.Threshold = t
	}

	for k, v := range raw {
		if k == thresholdKey {
			continue
		}
		m.Cadences[k] = v
	}

	return nil
}

// MarshalYAML produces the flat format expected by UnmarshalYAML:
// threshold as a sibling key alongside agent cadences.
func (m ModeConfig) MarshalYAML() (interface{}, error) {
	out := make(map[string]interface{})
	out["threshold"] = m.Threshold
	for k, v := range m.Cadences {
		out[k] = v
	}
	return out, nil
}

type DataConfig struct {
	MetricsDir         string `yaml:"metrics_dir"`
	LogsDir            string `yaml:"logs_dir"`
	ClaudeSessionsDir  string `yaml:"claude_sessions_dir"`
	CopilotSessionsDir string `yaml:"copilot_sessions_dir"`
	BobSessionsDir     string `yaml:"bob_sessions_dir"`
	AgentsDir          string `yaml:"agents_dir"`
}

// Load reads hive.yaml, then applies config.env overrides if present.
// Precedence: hive.yaml < config.env < explicit env vars (via ${} interpolation).
func Load(path string) (*Config, error) {
	return LoadWithOverrides(path, "")
}

// LoadWithOverrides reads hive.yaml and applies a config.env override file.
// If envPath is empty, it looks for config.env next to hive.yaml, then at
// /etc/hive/config.env. Pass "-" to skip config.env entirely.
func LoadWithOverrides(path, envPath string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	expanded := expandEnvVars(string(data))

	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	if envPath != "-" {
		if envPath == "" {
			envPath = findConfigEnv(path)
		}
		if envPath != "" {
			if err := cfg.applyConfigEnv(envPath); err != nil {
				return nil, fmt.Errorf("applying config.env %s: %w", envPath, err)
			}
		}
	}

	cfg.SourcePath = path
	cfg.applyBootstrapEnv()
	cfg.applyDefaults()

	// Merge per-agent overlay files from the agents directory.
	if cfg.Data.AgentsDir != "" {
		overlays, err := LoadAgentOverrides(cfg.Data.AgentsDir)
		if err != nil {
			return nil, fmt.Errorf("loading agent overlays: %w", err)
		}
		cfg.MergeAgentOverrides(overlays)
		// Re-apply defaults for overlay agents.
		for name := range overlays {
			cfg.ApplyAgentDefaults(name)
		}
	}

	if err := cfg.ExpandAgentReplicas(); err != nil {
		return nil, fmt.Errorf("expanding agent replicas: %w", err)
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("validating config: %w", err)
	}

	return &cfg, nil
}

// LoadWithDashboardOverlay loads the config from path, then — in Kubernetes
// mode — re-applies the dashboard overlay's agent configs on top, mirroring
// the entrypoint's boot-time seed+overlay merge.
//
// Why this exists: the ConfigMap seed at path carries only provision-time
// agent fields. Runtime reconciliation (ApplyPack raising a hive's ACMM level
// updates kick_template/mode/model) is persisted to the dashboard overlay, NOT
// the seed. The entrypoint merges the overlay over the seed once at boot, but a
// live ConfigMap remount rewrites the seed back to its stale values and fires
// the config watcher. If the watcher reloaded the raw seed it would silently
// revert every reconciled agent field (observed: a hive raised to L5 dropped
// its scanner back to the L2/L3 advisory template at runtime). Applying the
// overlay here keeps the reload consistent with boot.
func LoadWithDashboardOverlay(path string) (*Config, error) {
	cfg, err := Load(path)
	if err != nil {
		return nil, err
	}
	if !IsKubernetesPod() {
		return cfg, nil
	}
	data, err := os.ReadFile(DashboardOverlayFile)
	if err != nil {
		// No overlay (or unreadable) — the seed is authoritative, as at boot.
		return cfg, nil
	}
	var overlay Config
	if err := yaml.Unmarshal([]byte(expandEnvVars(string(data))), &overlay); err != nil {
		return cfg, nil // malformed overlay: fall back to seed, don't fail the reload
	}
	// Tombstones live in the dashboard overlay because that is the only agent
	// source the dashboard can write. Adopt them BEFORE the fullness guard below
	// so a short/empty overlay (one that has no agents yet, or only carries the
	// removed_agents list) still yields the tombstone. Previously this ran AFTER
	// the guard, so on a reload the guard's early return dropped RemovedAgents to
	// empty; the ~2-min saver then rewrote every layer tombstone-free and the
	// deleted agents reappeared on an interval (#2439). Merge already skips and
	// prunes tombstoned agents, so adopting them early is safe even when we bail.
	if !overlay.OTel.IsZero() {
		cfg.OTel = overlay.OTel
		cfg.Tracing = overlay.OTel
	} else if !overlay.Tracing.IsZero() {
		cfg.Tracing = overlay.Tracing
		cfg.OTel = mergeOTelOverride(cfg.OTel, overlay.Tracing)
	}
	// Governor work source: the dashboard's PUT /api/config/governor/work-source
	// writes the whole config to the overlay, but the reload only adopted
	// OTel/Tracing, RemovedAgents and Agents from it — so a work source set
	// from the dashboard was lost on every pod restart and GET returned
	// type "" again. Adopt it here, BEFORE the fullness guard, so a short
	// overlay (no agents yet) still carries it. The whole block is copied so
	// per-adapter settings (teams, hold labels, assigned_only, …) survive with
	// the type. Nothing here touches overlay.Variables — see the security
	// invariant at the end of this function.
	if !overlay.Governor.WorkSource.IsZero() {
		cfg.Governor.WorkSource = overlay.Governor.WorkSource
	}
	if len(overlay.RemovedAgents) > 0 {
		cfg.RemovedAgents = overlay.RemovedAgents
		cfg.PruneRemovedAgents()
		// Observability (#2439): this runs on boot AND on every ~2-min config reload,
		// so keep it at DEBUG. It confirms the reload adopted the overlay's tombstones
		// BEFORE the fullness guard below — the exact ordering whose absence let the
		// deleted agents reappear on an interval.
		slog.Default().Debug("reload: adopted removed-agents from overlay",
			"hive_id", cfg.HiveID,
			"count", len(cfg.RemovedAgents),
			"agents", cfg.RemovedAgents,
		)
	}
	// Guard: the overlay must look like a full hive config (same check the
	// entrypoint and validateSaveGuard apply) before we trust its agents.
	if overlay.Project.Org == "" || len(overlay.Agents) == 0 {
		return cfg, nil
	}
	// Overlay agents win — they carry the reconciled pack-behavior fields.
	cfg.MergeAgentOverrides(overlay.Agents)
	for name := range overlay.Agents {
		cfg.ApplyAgentDefaults(name)
	}
	if err := cfg.ExpandAgentReplicas(); err != nil {
		return cfg, err
	}
	// Security invariant: cfg.Variables (resolver defs + the exec/http trust
	// policy) comes ONLY from the seed loaded above — the overlay's Variables
	// block is intentionally NOT merged. The dashboard overlay is user-writable,
	// so honoring its resolver policy would let a compromised overlay enable
	// script/http execution. Keep this true if overlay merging is ever expanded.
	return cfg, nil
}

// findConfigEnv returns the path to a config.env file, or "" if none found.
func findConfigEnv(yamlPath string) string {
	candidates := []string{
		strings.TrimSuffix(yamlPath, "hive.yaml") + "config.env",
		"/etc/hive/config.env",
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// ParseEnvFile reads a flat KEY=VALUE file (# comments, blank lines skipped).
func ParseEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // read-only fd; nothing to lose on close error

	result := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		val = strings.Trim(val, `"'`)
		result[key] = val
	}
	return result, scanner.Err()
}

// applyConfigEnv merges flat KEY=VALUE overrides into the loaded config.
func (c *Config) applyConfigEnv(path string) error {
	env, err := ParseEnvFile(path)
	if err != nil {
		return err
	}

	if v, ok := env["PROJECT_ORG"]; ok {
		c.Project.Org = v
	}
	if v, ok := env["PROJECT_REPOS"]; ok {
		c.Project.Repos = strings.Fields(v)
	}
	if v, ok := env["PROJECT_AI_AUTHOR"]; ok {
		c.Project.AIAuthor = v
	}
	if v, ok := env["PROJECT_PRIMARY_REPO"]; ok {
		c.Project.PrimaryRepo = v
	}
	if v, ok := env["PROJECT_OPEN_PRS"]; ok {
		b := v == "true" || v == "1" || v == "yes"
		c.Project.OpenPRs = &b
	}
	if v, ok := env["AGENTS_ENABLED"]; ok {
		for _, name := range strings.Fields(v) {
			if agent, exists := c.Agents[name]; exists {
				agent.Enabled = true
				c.Agents[name] = agent
			}
		}
	}
	if v, ok := env["DASHBOARD_PORT"]; ok {
		var port int
		if _, err := fmt.Sscanf(v, "%d", &port); err == nil && port > 0 {
			c.Dashboard.Port = port
		}
	}
	if v, ok := env["DASHBOARD_AUTH_TOKEN"]; ok {
		c.Dashboard.AuthToken = v
	}
	if c.Dashboard.AuthToken == "" {
		if v, ok := env["HIVE_DASHBOARD_TOKEN"]; ok {
			c.Dashboard.AuthToken = v
		}
	}

	return nil
}

func (c *Config) applyBootstrapEnv() {
	if repo := os.Getenv("HIVE_REPO"); repo != "" {
		parts := strings.SplitN(repo, "/", 2)
		if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
			if c.Project.Org == "" {
				c.Project.Org = parts[0]
			}
			if len(c.Project.Repos) == 0 {
				c.Project.Repos = []string{parts[1]}
			}
			if c.Project.PrimaryRepo == "" {
				c.Project.PrimaryRepo = parts[1]
			}
		}
	}
	// K8s deployments pass the auth token as an OS env var from a Secret.
	// applyConfigEnv only reads file-based config.env, so without this
	// the token is silently ignored and the dashboard is unauthenticated.
	if c.Dashboard.AuthToken == "" {
		if v := os.Getenv("DASHBOARD_AUTH_TOKEN"); v != "" {
			c.Dashboard.AuthToken = v
		}
	}
	if c.Dashboard.AuthToken == "" {
		if v := os.Getenv("HIVE_DASHBOARD_TOKEN"); v != "" {
			c.Dashboard.AuthToken = v
		}
	}
	// K8s-provisioned spokes receive their per-hive authorized GitHub users as a
	// comma-separated env var (owner first). This is what lets a direct-route
	// spoke reject unauthorized device-flow logins without the hub proxy.
	if len(c.Dashboard.AuthorizedUsers) == 0 {
		if v := os.Getenv("HIVE_AUTHORIZED_USERS"); v != "" {
			c.Dashboard.AuthorizedUsers = parseAuthorizedUsers(v)
		}
	}
}

// parseAuthorizedUsers splits a comma-separated authorized-users list, trimming
// whitespace and dropping empty entries. Order is preserved so the first entry
// remains the owner.
func parseAuthorizedUsers(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if u := strings.TrimSpace(p); u != "" {
			out = append(out, u)
		}
	}
	return out
}

// expandEnvVars substitutes ${VAR} references in the raw config text. It runs
// BEFORE the YAML is parsed, so to honor an operator `variables:` block it
// first bootstrap-parses just that block from the same text, builds a
// config-scoped resolve.Registry, and delegates. With no `variables:` block the
// registry is env-only and the result is byte-identical to the legacy behavior
// (${NAME} -> os.LookupEnv(NAME), unset left literal).
func expandEnvVars(s string) string {
	reg := configRegistryFromText(s)
	return reg.Expand(context.Background(), s, resolve.ScopeConfig, nil)
}

const (
	MaxAgentReplicas = 5

	defaultDashboardPort          = 3002
	defaultAgentPollIntervalS     = 10
	defaultEvalIntervalS          = 300
	defaultPollIntervalMins       = 5
	defaultKnowledgeMaxFacts      = 25
	defaultKnowledgeEngine        = "llm-wiki"
	defaultCuratorSchedule        = "daily"
	defaultPromoteThreshold       = 0.9
	defaultSensingTTLSeconds      = 900
	defaultSensingPullbackSeconds = 900
	defaultHealthcheckIntervalS   = 300
	defaultRestartCooldownS       = 60
	defaultBudgetPeriodDays       = 7
	defaultBudgetCriticalPct      = 90
	defaultLogMaxSizeMB           = 50
	defaultLogMaxAgeDays          = 7
	defaultLogMaxBackups          = 10
	defaultLogLevel               = "info"

	// defaultAdvisoryMaxFindings is the digest's default top-N cap. Ten fits
	// in a screenful, which is the point: the digest is a "what should I look
	// at next" list, not an inventory.
	defaultAdvisoryMaxFindings = 10
	// defaultAdvisoryStalenessDays is how long an advisory bead survives
	// without being re-reported. Advisory agents re-scan far more often than
	// weekly, so a finding untouched for a week is one no agent still sees.
	defaultAdvisoryStalenessDays = 7
)

func (c *Config) applyDefaults() {
	// Repo targets are built as org + "/" + repo, so an entry that already
	// carries the org resolves to "org/org/repo" and every agent fails. Strip a
	// matching org prefix off both primary_repo and every repos entry on load.
	// primary_repo has been normalized here for a long time; repos was not,
	// which is why live configs are seen with a correct bare primary_repo next
	// to an org-qualified repos list. A mismatched org is deliberately left
	// alone so ValidateRepoTargets still rejects it rather than silently
	// retargeting the hive at a repository the owner never named.
	if c.Project.PrimaryRepo != "" && c.Project.Org != "" {
		c.Project.PrimaryRepo, _ = NormalizeRepoForOrg(c.Project.Org, c.Project.PrimaryRepo)
	}
	if len(c.Project.Repos) > 0 && c.Project.Org != "" {
		c.Project.Repos, _ = NormalizeProjectRepos(c.Project.Org, c.Project.Repos)
	}
	if c.Dashboard.Port == 0 {
		c.Dashboard.Port = defaultDashboardPort
	}
	if c.Dashboard.AgentPollIntervalS == 0 {
		c.Dashboard.AgentPollIntervalS = defaultAgentPollIntervalS
	}
	if normalized, err := ValidateSnapshotFrameAncestors(c.Dashboard.SnapshotFrameAncestors); err == nil {
		c.Dashboard.SnapshotFrameAncestors = normalized
	}
	if c.Governor.EvalIntervalS == 0 {
		c.Governor.EvalIntervalS = defaultEvalIntervalS
	}
	if c.Governor.Trajectory.IsEnabled() {
		if c.Governor.Trajectory.OnDivergence == "" {
			c.Governor.Trajectory.OnDivergence = "pause"
		}
	}
	if c.Policies.PollInterval == 0 {
		c.Policies.PollInterval = time.Duration(defaultPollIntervalMins) * time.Minute
	}
	if c.Data.MetricsDir == "" {
		c.Data.MetricsDir = "/data/metrics"
	}
	if c.Data.LogsDir == "" {
		c.Data.LogsDir = "/data/logs"
	}
	if c.Data.ClaudeSessionsDir == "" {
		c.Data.ClaudeSessionsDir = "/data/home/.claude/projects"
	}
	if c.Data.CopilotSessionsDir == "" {
		c.Data.CopilotSessionsDir = "/data/home/.copilot/session-state"
	}
	if c.Data.BobSessionsDir == "" {
		c.Data.BobSessionsDir = "/data/home/.bob"
	}
	if c.Data.AgentsDir == "" {
		c.Data.AgentsDir = "/data/agent-configs"
	}
	if c.Hub.URL == "" {
		c.Hub.URL = "https://hive.kubestellar.io"
		c.Hub.IsPublic = true
	}
	for name, agent := range c.Agents {
		agent.name = name
		if agent.ID == "" {
			agent.ID = name
		}
		if agent.Replicas == 0 {
			agent.Replicas = 1
		}
		if agent.BeadsDir == "" {
			agent.BeadsDir = fmt.Sprintf("/data/beads/%s", name)
		}
		// Default to enabled unless the user explicitly set enabled: false.
		if !agent.Enabled && !agent.enabledSet {
			agent.Enabled = true
		}
		if !agent.clearOnKickSet {
			agent.ClearOnKick = true
		}
		if agent.Role == "" {
			agent.Role = name
		}
		applyKnownAgentDefaults(name, &agent)
		c.Agents[name] = agent
	}

	if len(c.Hub.ContributeDenyTitles) == 0 {
		c.Hub.ContributeDenyTitles = []string{
			"*dependency dashboard*",
			"*renovate dashboard*",
			"epic:*",
			"epic(*",
		}
	}
	if len(c.Hub.ContributeDenyAuthors) == 0 {
		c.Hub.ContributeDenyAuthors = []string{
			"renovate[bot]",
			"dependabot[bot]",
			"mergeraptor[bot]",
		}
	}

	// Contribute filter modes: default to deny (the pre-mode behavior — the
	// *Deny* lists were always deny lists). Normalize any stored value.
	c.Hub.ContributeTitlesMode = NormalizeFilterMode(c.Hub.ContributeTitlesMode)
	c.Hub.ContributeAuthorsMode = NormalizeFilterMode(c.Hub.ContributeAuthorsMode)
	c.Hub.ContributeLabelsMode = NormalizeFilterMode(c.Hub.ContributeLabelsMode)

	// Contribute completion-cooldown period: leave 0 (== "use default") alone, but
	// clamp any explicitly-set value to [min,max] so a stray input cannot park an
	// issue effectively forever or round down to disable the cooldown.
	if c.Hub.ContributeCooldownHours != 0 {
		if c.Hub.ContributeCooldownHours < contributeCooldownMinHours {
			c.Hub.ContributeCooldownHours = contributeCooldownMinHours
		} else if c.Hub.ContributeCooldownHours > contributeCooldownMaxHours {
			c.Hub.ContributeCooldownHours = contributeCooldownMaxHours
		}
	}

	// One-time migration of the old dual label lists into the single list+mode.
	// If a legacy allow list was set (and no new label list/mode has been chosen
	// yet), adopt it as an allow filter. Otherwise the deny list (if any) stands.
	// After migration the allow list is cleared so it isn't re-applied.
	if len(c.Hub.ContributeAllowLabels) > 0 && len(c.Hub.ContributeDenyLabels) == 0 &&
		c.Hub.ContributeLabelsMode == FilterModeDeny {
		c.Hub.ContributeDenyLabels = c.Hub.ContributeAllowLabels
		c.Hub.ContributeLabelsMode = FilterModeAllow
		c.Hub.ContributeAllowLabels = nil
	}

	defaultTierLimits := map[string]TierRate{
		"newcomer":    {MaxPerHour: 3, MaxPerDay: 10, MaxConcurrent: 1},
		"contributor": {MaxPerHour: 10, MaxPerDay: 50, MaxConcurrent: 2},
		"trusted":     {MaxPerHour: 30, MaxPerDay: 200, MaxConcurrent: 5},
		"merger":      {MaxPerHour: 30, MaxPerDay: 200, MaxConcurrent: 5},
		"advisor":     {MaxPerHour: 0, MaxPerDay: 0, MaxConcurrent: 0},
	}
	if c.Hub.TierLimits == nil {
		c.Hub.TierLimits = map[string]TierRate{}
	}
	for tier, limits := range defaultTierLimits {
		if _, ok := c.Hub.TierLimits[tier]; !ok {
			c.Hub.TierLimits[tier] = limits
		}
	}

	if len(c.Governor.Labels.Exempt) == 0 {
		c.Governor.Labels.Exempt = []string{
			"nightly-tests", "LFX", "meta-tracker",
			"auto-qa-tuning-report", "adopters",
			"changes-requested", "waiting-on-author",
		}
	}
	if strings.TrimSpace(c.Governor.Labels.AutoMerge) == "" {
		c.Governor.Labels.AutoMerge = DefaultAutoMergeLabel
	}
	if len(c.Governor.Sensing.GHRatePatterns) == 0 {
		c.Governor.Sensing.GHRatePatterns = []string{
			"API rate limit exceeded",
			"secondary rate limit",
			"403.*rate limit",
			"You have exceeded a secondary rate",
			"retry-after:[[:space:]]*[0-9]",
			"gh: Resource not accessible",
			"abuse detection mechanism",
		}
	}
	if len(c.Governor.Sensing.CLIExcludePatterns) == 0 {
		c.Governor.Sensing.CLIExcludePatterns = []string{
			"You.re out of extra usage",
			"out of extra usage",
			"extra usage.*resets",
			"resets [0-9]+(:[0-9]+)?[aApP][mM]",
		}
	}
	// #4041: hives that saved their config while the pre-#3959 defaults were
	// in effect have that generic list MATERIALIZED as explicit values in
	// their persisted config (Save() marshals the whole struct, defaults
	// included), and "defaults only apply to an empty list" meant the #3959
	// fix could never reach them — a live hosted hive's quality agent flapped
	// on `(?i)copilot auth` for days with restart_count 83. A list that is
	// byte-identical to the old default set expresses no operator intent, so
	// treat it as "default": drop it and let the corrected defaults apply. A
	// list that differs in ANY way is an operator's, and stays verbatim.
	if IsLegacyDefaultLoginPatterns(c.Governor.Sensing.LoginPatterns) {
		log.Printf("[config] migrating login_patterns: persisted list matches the pre-#3959 defaults verbatim — dropping it so the corrected defaults apply (customized lists are never touched)")
		c.Governor.Sensing.LoginPatterns = nil
	}
	if len(c.Governor.Sensing.LoginPatterns) == 0 {
		// Each pattern must match the CLI's own login CHROME, never ordinary
		// English. The login-detector matches these against the agent's PANE —
		// which contains the issue bodies, PR diffs and CI logs the agent is
		// READING — so generic phrases pause an agent for merely discussing an
		// auth error. The previous defaults ("authentication required",
		// "unauthorized.*401", "session expired", "login required", "please
		// log in", "token expired") did exactly that on a live hive: a
		// ci-maintainer reading CI logs full of 401s and a supervisor
		// summarising issues across 39 repos were paused repeatedly, and the
		// two HIGHEST-cadence agents are hit hardest because the detector runs
		// at kick time, so exposure scales with kick frequency. Reproduced on
		// demand by typing "authentication required" into a healthy agent's
		// input box (unsubmitted — just rendered on the pane) and kicking it.
		c.Governor.Sensing.LoginPatterns = append([]string(nil), defaultLoginPatterns...)
	}
	if c.Governor.Sensing.TTLSeconds == 0 {
		c.Governor.Sensing.TTLSeconds = defaultSensingTTLSeconds
	}
	if c.Governor.Sensing.PullbackSeconds == 0 {
		c.Governor.Sensing.PullbackSeconds = defaultSensingPullbackSeconds
	}
	if c.Governor.Health.HealthcheckInterval == 0 {
		c.Governor.Health.HealthcheckInterval = defaultHealthcheckIntervalS
	}
	if c.Governor.Health.RestartCooldown == 0 {
		c.Governor.Health.RestartCooldown = defaultRestartCooldownS
	}
	if c.Governor.Budget.PeriodDays == 0 {
		c.Governor.Budget.PeriodDays = defaultBudgetPeriodDays
	}
	if c.Governor.Budget.CriticalPct == 0 {
		c.Governor.Budget.CriticalPct = defaultBudgetCriticalPct
	}
	if c.Governor.Logging.Dir == "" {
		c.Governor.Logging.Dir = c.Data.LogsDir
	}
	if c.Governor.Logging.MaxSizeMB == 0 {
		c.Governor.Logging.MaxSizeMB = defaultLogMaxSizeMB
	}
	if c.Governor.Logging.MaxAgeDays == 0 {
		c.Governor.Logging.MaxAgeDays = defaultLogMaxAgeDays
	}
	if c.Governor.Logging.MaxBackups == 0 {
		c.Governor.Logging.MaxBackups = defaultLogMaxBackups
	}
	if !c.Governor.Logging.Compress {
		c.Governor.Logging.Compress = true
	}
	if c.Governor.Logging.Level == "" {
		c.Governor.Logging.Level = defaultLogLevel
	}
	if c.Governor.Advisory.MaxFindings == 0 {
		c.Governor.Advisory.MaxFindings = defaultAdvisoryMaxFindings
	}
	if c.Governor.Advisory.StalenessDays == 0 {
		c.Governor.Advisory.StalenessDays = defaultAdvisoryStalenessDays
	}
	if c.Governor.Advisory.PRAutoClose == nil {
		on := true
		c.Governor.Advisory.PRAutoClose = &on
	}

	if c.Knowledge.Enabled {
		if c.Knowledge.Engine == "" {
			c.Knowledge.Engine = defaultKnowledgeEngine
		}
		if c.Knowledge.Primer.MaxFacts == 0 {
			c.Knowledge.Primer.MaxFacts = defaultKnowledgeMaxFacts
		}
		if c.Knowledge.Primer.MergeStrategy == "" {
			c.Knowledge.Primer.MergeStrategy = "precedence"
		}
		if len(c.Knowledge.Primer.Priority) == 0 {
			c.Knowledge.Primer.Priority = []string{"regression", "gotcha", "test_scaffold", "pattern", "decision"}
		}
		if c.Knowledge.Curator.Schedule == "" {
			c.Knowledge.Curator.Schedule = defaultCuratorSchedule
		}
		if c.Knowledge.Curator.AutoPromoteThreshold == 0 {
			c.Knowledge.Curator.AutoPromoteThreshold = defaultPromoteThreshold
		}
		if c.Knowledge.BeadSynthesizer.Schedule == "" {
			c.Knowledge.BeadSynthesizer.Schedule = "hourly"
		}
		if c.Knowledge.BeadSynthesizer.MinConfidence == 0 {
			c.Knowledge.BeadSynthesizer.MinConfidence = 0.5
		}
		if c.Knowledge.BeadSynthesizer.TargetLayer == "" {
			c.Knowledge.BeadSynthesizer.TargetLayer = "project"
		}
		if c.Knowledge.BeadSynthesizer.MaxFactsPerCycle == 0 {
			c.Knowledge.BeadSynthesizer.MaxFactsPerCycle = 20
		}
		if c.Knowledge.BeadSynthesizer.VaultPath == "" {
			c.Knowledge.BeadSynthesizer.VaultPath = "/data/vaults/bead-synth-wiki"
		}
	}
}

// applyKnownAgentDefaults populates metadata fields for well-known agent names
// when those fields are not explicitly set in YAML. This bridges existing configs.
func applyKnownAgentDefaults(name string, agent *AgentConfig) {
	type knownAgent struct {
		Emoji          string
		Color          string
		Aliases        []string
		LaneKeywords   []string
		DetectKeywords []string
		BeadRole       string
		SortOrder      int
		IncludeRepos   bool
	}

	known := map[string]knownAgent{
		"scanner": {
			Emoji: "🔍", Color: "#3498db", Aliases: []string{"sc"},
			LaneKeywords:   []string{"bug", "triage", "typo", "fix"},
			DetectKeywords: []string{"scanner", "triage", "issue", "bug"},
			BeadRole:       "worker", SortOrder: 20, IncludeRepos: true,
		},
		"ci-maintainer": {
			Emoji: "🔧", Color: "#2ecc71", Aliases: []string{"ci"},
			LaneKeywords:   []string{"workflow-failure", "ci-failure", "nightly", "coverage", "regression", "ga4", "analytics"},
			DetectKeywords: []string{"ci-maintainer", "review", "ci", "coverage", "ga4"},
			BeadRole:       "worker", SortOrder: 30, IncludeRepos: true,
		},
		"architect": {
			Emoji: "🏗", Color: "#9b59b6", Aliases: []string{"ar"},
			LaneKeywords:   []string{"rfc", "architecture", "refactor", "redesign", "migration", "breaking change", "protocol", "api design"},
			DetectKeywords: []string{"architect", "rfc", "refactor"},
			BeadRole:       "worker", SortOrder: 40, IncludeRepos: true,
		},
		"outreach": {
			Emoji: "🌐", Color: "#e67e22", Aliases: []string{"ou"},
			LaneKeywords:   []string{"adopters", "outreach", "community", "engagement"},
			DetectKeywords: []string{"outreach", "adopters", "community"},
			BeadRole:       "worker", SortOrder: 50, IncludeRepos: false,
		},
		"supervisor": {
			Emoji: "👑", Color: "#e74c3c", Aliases: []string{"su"},
			DetectKeywords: []string{"supervisor", "sweep", "monitor"},
			BeadRole:       "supervisor", SortOrder: 10, IncludeRepos: true,
		},
		"sec-check": {
			Emoji: "🛡", Color: "#1abc9c", Aliases: []string{"se"},
			DetectKeywords: []string{"security", "sec-check", "vulnerability"},
			BeadRole:       "worker", SortOrder: 60, IncludeRepos: true,
		},
		"telemetry": {
			Emoji: "📡", Color: "#00a8cc", Aliases: []string{"tm"},
			LaneKeywords:   []string{"observability", "opentelemetry", "prometheus", "grafana", "tracing", "metrics", "structured-logging", "servicemonitor", "podmonitor"},
			DetectKeywords: []string{"telemetry", "observability", "opentelemetry", "prometheus"},
			BeadRole:       "worker", SortOrder: 65, IncludeRepos: true,
		},
		"operations": {
			Emoji: "🚨", Color: "#d35400", Aliases: []string{"op"},
			LaneKeywords:   []string{"healthz", "readyz", "readiness", "slo-", "sli-", "service-level-objective", "service-level-indicator", "error-budget", "runbook", "incident-response", "rollback", "alerting"},
			DetectKeywords: []string{"operations", "operability", "healthz", "runbook"},
			BeadRole:       "worker", SortOrder: 66, IncludeRepos: true,
		},
		"quality": {
			Emoji: "🧪", Color: "#3498db", Aliases: []string{"te", "qa"},
			LaneKeywords:   []string{"test-gap", "test-strategy", "test-coverage", "test-scaffold", "untested", "missing-tests"},
			DetectKeywords: []string{"quality", "test", "coverage"},
			BeadRole:       "worker", SortOrder: 35, IncludeRepos: true,
		},
		"strategist": {
			Emoji: "🧠", Color: "#f39c12", Aliases: []string{"sg"},
			DetectKeywords: []string{"strategist", "strategy"},
			BeadRole:       "worker", SortOrder: 70, IncludeRepos: true,
		},
		"guide": {
			Emoji: "📖", Color: "#8e44ad", Aliases: []string{"gu"},
			LaneKeywords:   []string{"docs", "documentation", "readme", "guide", "tutorial", "onboarding"},
			DetectKeywords: []string{"guide", "docs", "documentation"},
			BeadRole:       "worker", SortOrder: 45, IncludeRepos: true,
		},
	}

	k, ok := known[name]
	if !ok {
		return
	}

	if agent.Emoji == "" {
		agent.Emoji = k.Emoji
	}
	if agent.Color == "" {
		agent.Color = k.Color
	}
	if len(agent.Aliases) == 0 && len(k.Aliases) > 0 {
		agent.Aliases = k.Aliases
	}
	if len(agent.LaneKeywords) == 0 && len(k.LaneKeywords) > 0 {
		agent.LaneKeywords = k.LaneKeywords
	}
	if len(agent.DetectKeywords) == 0 && len(k.DetectKeywords) > 0 {
		agent.DetectKeywords = k.DetectKeywords
	}
	if agent.BeadRole == "" {
		agent.BeadRole = k.BeadRole
	}
	if agent.SortOrder == 0 {
		agent.SortOrder = k.SortOrder
	}
	if agent.IncludeRepos == nil {
		v := k.IncludeRepos
		agent.IncludeRepos = &v
	}
}

func (c *Config) validate() error {
	if c.Project.Org == "" {
		return fmt.Errorf("project.org is required")
	}
	// Repos can be empty — L1 inception starts with just an idea, no repo.
	if len(c.Agents) == 0 {
		return fmt.Errorf("at least one agent must be configured")
	}
	// Deliberately a bare zero-test, NOT HasApp(): PlaceholderAppID exists
	// precisely so a hive awaiting its real App can satisfy this check and boot
	// into dashboard-only mode. Everywhere else, use HasApp().
	// A hive described by `forge:` alone satisfies this too: ResolvedAppID()
	// derives a real App ID from a known forge, so the identity is present even
	// though app_id is not written down. Without this, the end state of this
	// design — one field naming the forge, the rest derived — fails validation
	// and the spoke will not boot.
	// An EXPLICIT `forge:` satisfies this too: ResolvedAppID() derives a real
	// App ID from a known forge, so the identity is present even though app_id
	// is not written down. Deliberately keyed on Forge_ (the raw field) and not
	// Forge() — Forge() INFERS public for a blank config, which would make an
	// empty github block validate and silently boot a hive with no credentials
	// at all. Only a forge the operator actually wrote counts.
	if c.GitHub.Token == "" && c.GitHub.AppID == 0 &&
		(strings.TrimSpace(c.GitHub.Forge_) == "" || c.GitHub.ResolvedAppID() == 0) {
		return fmt.Errorf("github.token, github.app_id or github.forge is required")
	}
	if err := c.Governor.LiteLLM.Validate(); err != nil {
		return err
	}
	if normalized, err := ValidateSnapshotFrameAncestors(c.Dashboard.SnapshotFrameAncestors); err != nil {
		return err
	} else {
		c.Dashboard.SnapshotFrameAncestors = normalized
	}
	if normalized, err := ValidateDashboardPublicURL(c.Dashboard.PublicURL); err != nil {
		return err
	} else {
		c.Dashboard.PublicURL = normalized
	}
	if !ValidateThresholdScaling(c.Governor.ThresholdScaling) {
		return fmt.Errorf("governor: invalid threshold_scaling %q (must be linear, sqrt, or none)", c.Governor.ThresholdScaling)
	}
	for modeName, mode := range c.Governor.Modes {
		for agentName, cadence := range mode.Cadences {
			if err := cadence.Validate(); err != nil {
				return fmt.Errorf("governor mode %s cadence for %s: %w", modeName, agentName, err)
			}
		}
	}
	// The hive-wide default goes through the SAME gate as the per-agent field.
	// Without this a bad value here is silently normalized to off by
	// ResolveExplainModeDefault, so an operator who typed "verbose" in the
	// dashboard would see explanation stay off with nothing saying why.
	if !ValidateExplainMode(strings.TrimSpace(c.Governor.ExplainMode)) {
		return fmt.Errorf("governor: invalid explain_mode %q (must be off, brief, or full, or empty to inherit %s)", c.Governor.ExplainMode, ExplainModeEnvVar)
	}
	if !ValidateACMMIssueTracker(strings.TrimSpace(c.Governor.ACMM.IssueTracker)) {
		return fmt.Errorf("governor: invalid acmm.issue_tracker %q (must be %s or %s, or empty for %s)", c.Governor.ACMM.IssueTracker, ACMMIssueTrackerGitHub, ACMMIssueTrackerWorkSource, ACMMIssueTrackerGitHub)
	}
	for name, agent := range c.Agents {
		// One gate, shared with the config write path (dashboard agent-config
		// save) and agreeing with what the launcher can actually dispatch. A
		// configured gateway name is valid too: naming a gateway as the backend
		// routes that agent through it, matched case-insensitively to mirror
		// ResolveGateway.
		if err := c.Governor.ValidateBackend(agent.Backend); err != nil {
			return fmt.Errorf("agent %s: %w", name, err)
		}
		if !ValidateCavemanMode(agent.CavemanMode) {
			return fmt.Errorf("agent %s: invalid caveman_mode %q (must be lite, full, ultra, or wenyan)", name, agent.CavemanMode)
		}
		if !ValidateExplainMode(agent.ExplainMode) {
			return fmt.Errorf("agent %s: invalid explain_mode %q (must be off, brief, or full, or empty to inherit %s)", name, agent.ExplainMode, ExplainModeEnvVar)
		}
		if err := validateChannels(name, agent.Channels); err != nil {
			return err
		}
		if err := validateTools(name, agent.Tools); err != nil {
			return err
		}
		if err := validateConnections(name, agent.Connections); err != nil {
			return err
		}
	}
	return nil
}

func validateChannels(agentName string, channels []ChannelConfig) error {
	validTypes := map[string]bool{"kick": true, "webhook": true, "discord": true, "schedule": true, "bead": true}
	for i, ch := range channels {
		if !validTypes[ch.Type] {
			return fmt.Errorf("agent %s: channel[%d]: invalid type %q", agentName, i, ch.Type)
		}
		switch ch.Type {
		case "webhook":
			if len(ch.Events) == 0 {
				return fmt.Errorf("agent %s: channel[%d]: webhook requires at least one event", agentName, i)
			}
		case "discord":
			if len(ch.Patterns) == 0 {
				return fmt.Errorf("agent %s: channel[%d]: discord requires at least one pattern", agentName, i)
			}
		case "schedule":
			if ch.Schedule == "" {
				return fmt.Errorf("agent %s: channel[%d]: schedule requires a cron expression", agentName, i)
			}
		case "bead":
			if len(ch.Match) == 0 {
				return fmt.Errorf("agent %s: channel[%d]: bead requires at least one match criterion", agentName, i)
			}
		}
	}
	return nil
}

func validateTools(agentName string, tools *ToolsConfig) error {
	if tools == nil {
		return nil
	}
	validPresets := map[string]bool{"": true, "advisory": true, "issues-only": true, "issues-prs": true, "full": true}
	if !validPresets[tools.Preset] {
		return fmt.Errorf("agent %s: tools.preset %q is invalid (must be advisory, issues-only, issues-prs, or full)", agentName, tools.Preset)
	}
	validActions := map[string]bool{"allow": true, "deny": true}
	for i, rule := range tools.Rules {
		if rule.Pattern == "" {
			return fmt.Errorf("agent %s: tools.rules[%d]: pattern is required", agentName, i)
		}
		if !validActions[rule.Action] {
			return fmt.Errorf("agent %s: tools.rules[%d]: action must be allow or deny, got %q", agentName, i, rule.Action)
		}
	}
	return nil
}

func validateConnections(agentName string, conns []ConnectionConfig) error {
	validTypes := map[string]bool{"mcp": true, "api": true, "knowledge": true}
	seen := map[string]bool{}
	for i, conn := range conns {
		if conn.Name == "" {
			return fmt.Errorf("agent %s: connections[%d]: name is required", agentName, i)
		}
		if seen[conn.Name] {
			return fmt.Errorf("agent %s: connections[%d]: duplicate name %q", agentName, i, conn.Name)
		}
		seen[conn.Name] = true
		if !validTypes[conn.Type] {
			return fmt.Errorf("agent %s: connections[%d]: invalid type %q (must be mcp, api, or knowledge)", agentName, i, conn.Type)
		}
		if (conn.Type == "mcp" || conn.Type == "api") && conn.URI == "" {
			return fmt.Errorf("agent %s: connections[%d]: %s requires a uri", agentName, i, conn.Type)
		}
		if conn.Auth != nil {
			validAuthTypes := map[string]bool{"env": true, "file": true}
			if !validAuthTypes[conn.Auth.Type] {
				return fmt.Errorf("agent %s: connections[%d]: auth.type must be env or file, got %q", agentName, i, conn.Auth.Type)
			}
			if conn.Auth.Type == "env" && conn.Auth.EnvVar == "" {
				return fmt.Errorf("agent %s: connections[%d]: auth.env_var is required when auth.type is env", agentName, i)
			}
			if conn.Auth.Type == "file" && conn.Auth.File == "" {
				return fmt.Errorf("agent %s: connections[%d]: auth.file is required when auth.type is file", agentName, i)
			}
		}
	}
	return nil
}

// MarshalYAML persists only declared agents. Runtime-derived replicas are
// re-created by ExpandAgentReplicas on the next load so they never collide with
// their base agent's replicas setting after a save.
func (c Config) MarshalYAML() (interface{}, error) {
	type plain Config
	out := plain(c)
	if c.Agents != nil {
		out.Agents = make(map[string]AgentConfig, len(c.Agents))
		for name, agent := range c.Agents {
			if agent.ReplicaOf != "" {
				continue
			}
			agent.ReplicaIndex = 0
			agent.ReplicaCount = 0
			out.Agents[name] = agent
		}
	}
	return out, nil
}

// validateSaveGuard checks that essential fields are present before allowing
// a config write. This prevents docker compose down -v (or similar) from
// causing Save() to overwrite hive.yaml with an empty/minimal config that
// would crash-loop on next startup.
func (c *Config) validateSaveGuard() error {
	if c.Project.Org == "" {
		log.Printf("WARNING: config.Save() blocked — project.org is empty, would corrupt hive.yaml")
		return fmt.Errorf("project.org is empty")
	}
	// Zero agents is a legitimate state when the operator deliberately deleted
	// them all: #2361's tombstones (RemovedAgents) are the durable record of
	// that intent. Blocking the save here would make the last deletion
	// unpersistable — the in-memory roster empties, the write is refused, and
	// the next reload restores the agents from the seed, silently undoing the
	// operator's action. That is precisely the "they always come back" bug
	// #2361 fixed, reintroduced through the save path.
	//
	// An empty roster with NO tombstones is still refused: that is the
	// truncated/uninitialised case this guard exists to catch. The two states
	// are distinguishable, so distinguish them rather than rejecting both.
	if len(c.Agents) == 0 && len(c.RemovedAgents) == 0 {
		log.Printf("WARNING: config.Save() blocked — no agents configured and no tombstones, would corrupt hive.yaml")
		return fmt.Errorf("no agents configured")
	}
	return nil
}

// Save marshals the current config back to its source YAML file using an
// inode-preserving write (open → truncate → write → sync). This is critical
// for Docker bind-mounted files: an atomic rename (temp + rename) replaces
// the inode, which silently breaks the bind mount — the host file is never
// updated, so changes are lost on container restart.
//
// As a safety measure, Save refuses to write if essential fields are missing
// (project.org, at least one agent). This prevents an empty or minimal config
// from overwriting the bind-mounted hive.yaml — a scenario that causes
// crash-loops on the next startup ("project.org is required").
func (c *Config) Save() error {
	saveMu.Lock()
	defer saveMu.Unlock()
	return c.saveLocked()
}

// SetAgentPausedAndSave atomically updates one agent's Paused field and
// persists the config, all under saveMu. This is the pause-callback path
// (AgentMgr.Pause/Resume). Doing the c.Agents read-modify-write and the Save
// under the SAME lock as every other saver eliminates both the map-mutation
// race (two goroutines writing c.Agents) and the file-level lost-write race.
// Returns whether a change was made (false when already at the target state).
func (c *Config) SetAgentPausedAndSave(name string, paused bool) (bool, error) {
	saveMu.Lock()
	defer saveMu.Unlock()
	ac, ok := c.Agents[name]
	if !ok || ac.Paused == paused {
		return false, nil
	}
	ac.Paused = paused
	c.Agents[name] = ac
	return true, c.saveLocked()
}

// ReconcilePausedAndSave sets each named agent's Paused field to the given
// live value and persists, all under saveMu. This is the async PersistFunc
// path (persistState): it carries the authoritative live paused set from the
// agent manager, so its write is a correcting one rather than a stale snapshot
// that could clobber a concurrent pause. Serializing it with SetAgentPausedAndSave
// under saveMu is what closes the race that dropped pauses when many agents
// were paused in quick succession.
func (c *Config) ReconcilePausedAndSave(livePaused map[string]bool) error {
	saveMu.Lock()
	defer saveMu.Unlock()
	for name, paused := range livePaused {
		if ac, ok := c.Agents[name]; ok && ac.Paused != paused {
			ac.Paused = paused
			c.Agents[name] = ac
		}
	}
	return c.saveLocked()
}

// saveLocked performs the actual marshal-and-write. Callers MUST hold saveMu.
func (c *Config) saveLocked() error {
	if c.SourcePath == "" {
		return fmt.Errorf("config has no source path")
	}
	if err := c.validateSaveGuard(); err != nil {
		return fmt.Errorf("refusing to save invalid config: %w", err)
	}
	data, err := yaml.Marshal(c.redactedForPersist())
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}

	// Open the existing file (preserving its inode) rather than creating a
	// temp file and renaming. Rename breaks Docker bind mounts because it
	// replaces the inode — the host file is never updated, so acmm_level
	// and other runtime changes are lost on container restart.
	//
	// #3961: a source-path failure must NOT abort the save. On deployments
	// that mount the config read-only (a ConfigMap mounted straight at
	// /etc/hive/hive.yaml — the issue's k3s case), this write can NEVER
	// succeed, and returning here skipped exactly the two layers that DO
	// survive a pod restart: the PVC runtime config (which the entrypoint
	// boots from in both K8s steady state and Docker/LXC) and the dashboard
	// overlay (the K8s first-boot/reprovision merge input). The old early
	// return therefore made every runtime change — pause state, operator
	// model/backend ownership, ACMM level, gateway saves — evaporate on
	// every restart, while spamming "failed to persist" on every save.
	// Record the failure, keep writing the durable layers, and report
	// success iff the state will actually survive a restart.
	var srcErr error
	f, err := os.OpenFile(c.SourcePath, os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		// File may not exist yet — fall back to create. Continue below so
		// the PVC backup and dashboard overlay are still written.
		if writeErr := os.WriteFile(c.SourcePath, data, 0o644); writeErr != nil {
			srcErr = fmt.Errorf("writing config (create fallback): %w", writeErr)
		}
	} else {
		if _, err := f.Write(data); err != nil {
			_ = f.Close() // best-effort cleanup; the write error is what's recorded
			srcErr = fmt.Errorf("writing config: %w", err)
		} else if err := f.Sync(); err != nil {
			_ = f.Close() // best-effort cleanup; the sync error is what's recorded
			srcErr = fmt.Errorf("syncing config: %w", err)
		} else if err := f.Close(); err != nil {
			srcErr = fmt.Errorf("closing config: %w", err)
		}
	}

	// Persist the runtime config to the PVC. In K8s this is a recovery copy
	// (the ConfigMap seed plus the overlay is authoritative); in Docker/LXC
	// it IS the boot-time source of truth, since there is no ConfigMap and
	// no overlay there. The entrypoint decides which applies.
	//
	// Always written under the new name. The legacy file is never written,
	// renamed or removed here — see RuntimeConfigFileLegacy.
	runtimePath := RuntimeConfigFile
	var runtimeErr error
	// 0600, not 0644: the marshaled config carries dashboard.auth_token (and
	// github.token in PAT mode), and /data is world-traversable on hive
	// hosts, so a group/world-readable runtime config hands the dashboard
	// owner credential to every unprivileged agent user (#5331).
	if err := os.WriteFile(runtimePath, data, 0o600); err != nil {
		// Common cause: init container created the file as root, runtime user
		// can't overwrite. Remove and retry so runtime state is not silently lost.
		_ = os.Remove(runtimePath) // best-effort; the retry's own WriteFile error is what's recorded below
		if retryErr := os.WriteFile(runtimePath, data, 0o600); retryErr != nil {
			runtimeErr = retryErr
			log.Printf("[config] warning: failed to write PVC runtime config to %s (even after remove): %v", runtimePath, retryErr)
		} else {
			log.Printf("[config] PVC runtime config written to %s (recovered from permission error)", runtimePath)
		}
	} else {
		log.Printf("[config] PVC runtime config written to %s", runtimePath)
		// os.WriteFile's mode only applies when it CREATES the file; a
		// pre-existing world-readable inode (every hive deployed before
		// this fix) keeps its old 0644 bits, so tighten explicitly.
		if chmodErr := os.Chmod(runtimePath, 0o600); chmodErr != nil {
			log.Printf("[config] warning: failed to tighten permissions on %s: %v", runtimePath, chmodErr)
		}
	}

	overlayErr := c.saveDashboardOverlay()

	if srcErr == nil {
		return nil
	}
	// The primary config path failed — a read-only mount, not a transient
	// error, in every observed case. When the boot-durable layers were both
	// written (the overlay write is a no-op outside Kubernetes), the state
	// WILL survive a restart, so this save has done its job: say so once per
	// failure mode instead of letting every caller raise a false
	// "will be lost on restart" alert on every save.
	if runtimeErr == nil && overlayErr == nil {
		log.Printf("[config] primary config path %s is not writable (%v) — state persisted to the PVC layers instead and will survive restarts (see RuntimeConfigFile)", c.SourcePath, srcErr)
		return nil
	}
	return srcErr
}

// RuntimeConfigFile is where Save() persists the full runtime config on the
// PVC. Its role differs by environment, which is exactly why the old
// hive.yaml.bak name was misleading enough to cost debugging time:
//
//   - Kubernetes: a post-merge SNAPSHOT. The entrypoint writes it after
//     merging the dashboard overlay over the ConfigMap seed, and reads it
//     back only in the disaster fallback (ConfigMap missing or empty).
//   - Docker/LXC: a live boot INPUT and the source of truth. There is no
//     ConfigMap and no overlay, so the entrypoint restores this file over
//     the config path on every boot. It is the only reason a dashboard save
//     survives a container recreation — see saveDashboardOverlay, which
//     early-returns outside Kubernetes for that reason.
//
// ".runtime" is accurate for both; ".bak" implied "the restorable backup",
// which is true only of the Kubernetes half.
// A package var (not const) only so tests can point it at a temp dir; it
// never changes at runtime in production (same convention as
// DashboardOverlayFile below).
var RuntimeConfigFile = "/data/hive.yaml.runtime"

// RuntimeConfigFileLegacy is the pre-rename name of RuntimeConfigFile.
//
// It is READ as a fallback and never written, renamed or removed: ~51 live
// hives carry only this file, and on Docker/LXC it is the single copy of
// their live configuration. Mutating it at boot could lose owner
// customisations with no warning, so the migration is copy-forward only —
// readers prefer RuntimeConfigFile and fall back to this one.
//
// Removable one release after every live hive has written the new name.
const RuntimeConfigFileLegacy = "/data/hive.yaml.bak"

// DashboardOverlayFile is where Save() persists a secret-free copy of the
// dashboard-edited config on the PVC in Kubernetes mode. The copy-config
// init container re-seeds /etc/hive/hive.yaml FROM THE CONFIGMAP on every
// pod boot, so without this overlay every dashboard save (LiteLLM
// endpoint, notifications, agent tweaks, ...) silently vanished on the
// next restart or upgrade. The entrypoint merges this file over the
// ConfigMap seed at boot; the ConfigMap stays authoritative for the
// hub/admin-managed keys (acmm_level, hub.is_public).
//
// A package var (not const) only so tests can point it at a temp dir; it
// never changes at runtime in production.
var DashboardOverlayFile = "/data/hive.yaml.dashboard"

// saTokenFile is the Kubernetes serviceaccount token path IsKubernetesPod
// probes. It is a var (not a const) only so tests can point it at a
// non-existent path and stay hermetic on hosts that really are pods;
// production always uses the fixed in-cluster path.
var saTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// SetSATokenFileForTest points IsKubernetesPod's serviceaccount-token probe
// at path and returns a restore func. Out-of-package tests that need the
// non-Kubernetes branch call this with a non-existent path (alongside
// clearing KUBERNETES_SERVICE_HOST) so they stay hermetic on hosts that
// really are pods — in-cluster CI runners and dev hives.
func SetSATokenFileForTest(path string) func() {
	orig := saTokenFile
	saTokenFile = path
	return func() { saTokenFile = orig }
}

// IsKubernetesPod reports whether the process is running inside a
// Kubernetes pod (mirrors the entrypoint's IS_KUBERNETES detection).
func IsKubernetesPod() bool {
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		return true
	}
	_, err := os.Stat(saTokenFile)
	return err == nil
}

// saveDashboardOverlay writes the secret-free PVC overlay in Kubernetes
// mode. Failures are logged, never fatal — but they ARE returned (#3961):
// when the primary config path is unwritable (read-only ConfigMap mount)
// the overlay and the runtime config are the only layers that survive a pod
// restart, so saveLocked needs to know whether this write landed before it
// can report the save as durable. Outside Kubernetes it returns nil (the
// overlay is not part of the boot path there).
//
// The write MUST be atomic (temp file + rename), unlike saveLocked()'s
// inode-preserving write to the bind-mounted primary config. DashboardOverlayFile
// lives on the PVC (not a bind mount), so rename is safe here, and it is the
// only way to avoid a truncated/partial overlay if the pod is killed mid-write
// (a redeploy sends SIGTERM/SIGKILL at an arbitrary instant). A truncate-in-place
// write (os.WriteFile) can leave the file cut off partway through — GitHubConfig
// marshals AFTER Agents/Project in the Config struct field order (see the
// struct tags above), so a truncated overlay can silently keep valid
// project/agents blocks while losing app_id/installation_id/key_file entirely.
// The entrypoint's merge script only sanity-checks project.org and agents
// before trusting the overlay wholesale, so that truncated-but-plausible file
// would pass the guard and revert a dashboard-installed GitHub App to the
// placeholder ConfigMap seed on the next restart — exactly the durability bug
// this atomic write prevents.
func (c *Config) saveDashboardOverlay() error {
	if !IsKubernetesPod() {
		// Docker/LXC mode: RuntimeConfigFile is already the boot-time
		// source of truth there, so dashboard saves persist without an
		// overlay.
		return nil
	}
	data, err := c.dashboardOverlayBytes()
	if err != nil {
		log.Printf("[config] warning: failed to marshal dashboard overlay: %v", err)
		return err
	}
	tmpPath := DashboardOverlayFile + ".tmp"
	// 0600, not 0644: dashboardOverlayBytes only folds the dashboard auth
	// token back to its env form when it matches a bootstrap env var — a
	// dashboard-minted token is persisted verbatim, so the overlay is not
	// reliably secret-free (#5331).
	const overlayFileMode = 0o600
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, overlayFileMode)
	if err != nil {
		log.Printf("[config] warning: failed to open dashboard overlay temp file %s (dashboard saves will not survive pod restarts): %v", tmpPath, err)
		return err
	}
	// OpenFile's mode only applies on create; a leftover 0644 tmp file from a
	// crash before this fix would otherwise carry its old bits through the
	// rename. Best-effort: the rename below installs whatever mode f has.
	_ = f.Chmod(overlayFileMode)
	if _, err := f.Write(data); err != nil {
		_ = f.Close() // best-effort cleanup; the write error is what's returned
		log.Printf("[config] warning: failed to write dashboard overlay temp file %s (dashboard saves will not survive pod restarts): %v", tmpPath, err)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close() // best-effort cleanup; the sync error is what's returned
		log.Printf("[config] warning: failed to fsync dashboard overlay temp file %s (dashboard saves will not survive pod restarts): %v", tmpPath, err)
		return err
	}
	if err := f.Close(); err != nil {
		log.Printf("[config] warning: failed to close dashboard overlay temp file %s (dashboard saves will not survive pod restarts): %v", tmpPath, err)
		return err
	}
	if err := os.Rename(tmpPath, DashboardOverlayFile); err != nil {
		log.Printf("[config] warning: failed to rename dashboard overlay into place %s (dashboard saves will not survive pod restarts): %v", DashboardOverlayFile, err)
		return err
	}
	log.Printf("[config] dashboard overlay written to %s (merged over the ConfigMap seed at next boot)", DashboardOverlayFile)
	return nil
}

// dashboardOverlayBytes marshals the config with env-derived secret VALUES
// collapsed back to their env-var forms, so the PVC overlay stays
// secret-free. Load() re-expands ${VAR} references and applyBootstrapEnv
// re-fills the dashboard auth token from the pod env, so nothing is lost.
func (c *Config) dashboardOverlayBytes() ([]byte, error) {
	// Shallow copy: top-level fields are struct values, so mutating the
	// copy's GitHub/Dashboard sections leaves the live config untouched
	// (the shared Agents map is not modified).
	cp := *c
	if tok := os.Getenv("HIVE_GITHUB_TOKEN"); tok != "" && cp.GitHub.Token == tok {
		cp.GitHub.Token = "${HIVE_GITHUB_TOKEN}"
	}
	for _, env := range []string{"DASHBOARD_AUTH_TOKEN", "HIVE_DASHBOARD_TOKEN"} {
		if v := os.Getenv(env); v != "" && cp.Dashboard.AuthToken == v {
			cp.Dashboard.AuthToken = ""
			break
		}
	}
	cp = *cp.redactedForPersist()
	return yaml.Marshal(&cp)
}

func (c *Config) redactedForPersist() *Config {
	if c == nil {
		return nil
	}
	cp := *c
	cp.OTel.Headers = envRedactedHeaders(cp.OTel.Headers)
	cp.Tracing.Headers = envRedactedHeaders(cp.Tracing.Headers)
	// Work-source credentials are persisted verbatim. The dashboard PUT stores
	// the operator's literal `${LINEAR_API_KEY}` reference (API saves are not
	// env-expanded) and worksource.FromConfig resolves it at the point of use,
	// so the reference round-trips through the overlay unchanged. Do NOT try
	// to "fold" a value back into ${VAR} by scanning the environment: any env
	// value that is a substring of the key (CI's ACCEPT_EULA=Y rewrote the
	// trailing Y of the literal reference) corrupts it.
	// #4041: never write the built-in login-pattern defaults as explicit
	// values. applyDefaults fills LoginPatterns on load, so by save time the
	// in-memory list always LOOKS explicit; marshaling it pins today's
	// defaults into the persisted config, where "defaults only apply to an
	// empty list" freezes them forever — exactly how every pre-#3959 hive
	// ended up stuck with the false-positive-prone generic list. A list equal
	// to the current defaults expresses no operator intent: persist it as
	// absent so future default fixes reach existing hives. An operator-
	// customized list differs from the defaults and is persisted verbatim.
	if stringSlicesEqual(cp.Governor.Sensing.LoginPatterns, defaultLoginPatterns) {
		cp.Governor.Sensing.LoginPatterns = nil
	}
	return &cp
}

func mergeOTelOverride(base, override OTelConfig) OTelConfig {
	merged := base
	if override.Enabled {
		merged.Enabled = true
	}
	if override.Endpoint != "" {
		merged.Endpoint = override.Endpoint
	}
	if len(override.Headers) > 0 {
		merged.Headers = override.Headers
	}
	if override.ServiceName != "" {
		merged.ServiceName = override.ServiceName
	}
	if override.Insecure {
		merged.Insecure = true
	}
	if override.SampleRatio != 0 {
		merged.SampleRatio = override.SampleRatio
	}
	return merged
}

func envRedactedHeaders(headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return headers
	}
	out := make(map[string]string, len(headers))
	for k, value := range headers {
		out[k] = redactEnvExpandedValue(value)
	}
	return out
}

func redactEnvExpandedValue(value string) string {
	type envValue struct {
		name  string
		value string
	}
	values := make([]envValue, 0)
	for _, pair := range os.Environ() {
		name, val, ok := strings.Cut(pair, "=")
		if !ok || val == "" {
			continue
		}
		values = append(values, envValue{name: name, value: val})
	}
	sort.SliceStable(values, func(i, j int) bool {
		return len(values[i].value) > len(values[j].value)
	})

	redacted := value
	for _, item := range values {
		redacted = strings.ReplaceAll(redacted, item.value, "${"+item.name+"}")
	}
	return redacted
}
