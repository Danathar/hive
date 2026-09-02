// Work-shaping concern blocks: ioscan (untrusted-input scanning), planning,
// quality, retro, classifier, intent, trajectory, and replan configuration.
package config

import (
	"strings"
)

// IoscanConfig gates the pkg/ioscan input/output security scanner (prompt-
// injection + secret/dangerous-directive detection). It is additive and, per
// audit rec #7 (F11, CWE-693), now ENABLED by default: untrusted external text
// (issue/PR titles, labels, bodies) is scanned before it is injected into an
// agent kick, and only text the input block policy trips (Critical, or High-
// severity injection) is redacted/annotated — benign text passes through
// byte-identically. The scanner is pure (no I/O, no network) and enforcement
// fails safe: a scan is only ever advisory to the caller, never a reason to
// crash a kick. Defaulting on is therefore safe — a running hive sees no change
// for ordinary titles, only genuine injection payloads are withheld.
type IoscanConfig struct {
	// Enabled turns input/output scanning on. Pointer so an omitted `enabled:`
	// key (nil) is distinguishable from an explicit false: nil DEFAULTS ON
	// (fail-safe — scan by default), while an operator can still opt out with an
	// explicit `enabled: false`. Read via IsEnabled(), never dereferenced raw.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// FailMode controls what the kick path does with Critical injection
	// findings. Empty/"open" preserves the historical behavior: redact the
	// offending text and continue the kick. "closed" blocks the kick and records
	// an ioscan_fail_closed audit entry. Read via FailClosed().
	FailMode string `yaml:"fail_mode,omitempty" json:"fail_mode,omitempty"`
	// Canaries plants per-kick exfiltration markers in agent prompts and scans
	// agent-visible egress for leaks. Default false: existing hives see no
	// canary behavior until explicitly enabled.
	Canaries bool `yaml:"canaries,omitempty" json:"canaries,omitempty"`
	// Classifier enables the optional LLM-judge semantic prompt-injection
	// classifier. It defaults off so hives that only use deterministic rules
	// make no model calls and see zero behavior change.
	Classifier IoscanClassifierConfig `yaml:"classifier,omitempty" json:"classifier,omitempty"`
}

// IoscanClassifierConfig controls the optional model-based semantic injection
// classifier that runs after deterministic ioscan redaction.
type IoscanClassifierConfig struct {
	Enabled        bool    `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Model          string  `yaml:"model,omitempty" json:"model,omitempty"`
	WarnThreshold  float64 `yaml:"warn_threshold,omitempty" json:"warn_threshold,omitempty"`
	BlockThreshold float64 `yaml:"block_threshold,omitempty" json:"block_threshold,omitempty"`
}

// IsEnabled reports whether input/output scanning is active. Absent (nil)
// defaults to true (fail-safe): scanning is on unless an operator explicitly
// sets `enabled: false`.
func (c IoscanConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

const ioscanFailModeClosed = "closed"

// FailClosed reports whether Critical injection findings should block the
// whole kick instead of only redacting the offending untrusted text. The default
// is fail-open for backward compatibility.
func (c IoscanConfig) FailClosed() bool {
	return strings.EqualFold(c.FailMode, ioscanFailModeClosed)
}

// PlanningConfig gates the Phase 4 planning entry points that fire automatically
// (as opposed to the explicit dashboard "Plan this issue" click, which is always
// available). Today that is the `plan`/`epic` label trigger: an actionable issue
// carrying one of those labels auto-mints an epic and requests decomposition.
type PlanningConfig struct {
	// PlanFromLabel enables the label trigger. Pointer so an omitted key is
	// distinguishable from an explicit false: when nil, the trigger falls back to
	// an ACMM-level gate (on at L5+), so mature hives get it without extra config
	// while low-maturity hives stay advisory-only. An explicit value overrides the
	// ACMM gate in either direction — but see the note below: even an explicit
	// true is a no-op below L5, because the architect that decomposes the minted
	// epics is not scheduled there.
	PlanFromLabel *bool `yaml:"plan_from_label,omitempty" json:"plan_from_label,omitempty"`
}

// FormalQualityMinACMMLevel is the first maturity level at which the quality
// lane may author and maintain formal models. Below L5, quality is either
// advisory or restricted to narrower testing work, so enabling the capability
// there would grant behavior the active ACMM pack does not permit.
const FormalQualityMinACMMLevel = 5

// QualityConfig controls opt-in capabilities of the quality lane. The zero
// value preserves the existing test/coverage-only behavior.
type QualityConfig struct {
	// Formal lets the quality agent identify protocol-shaped code and add a
	// Spin/Promela model, its executable verification contract, and reporting-
	// only CI. It is deliberately opt-in and is effective only at ACMM L5+.
	Formal bool `yaml:"formal,omitempty" json:"formal,omitempty"`
}

// FormalEnabled reports whether the operator opt-in and ACMM floor both allow
// formal-model work. Keeping the floor in this effective-value method means a
// level downgrade safely disables the capability without making the persisted
// config invalid or forgetting the operator's preference.
func (q QualityConfig) FormalEnabled(acmmLevel int) bool {
	return q.Formal && acmmLevel >= FormalQualityMinACMMLevel
}

// RetroConfig gates the post-completion retro lane. It is off by
// default: an absent `retro:` block or `enabled: false` yields zero behavior
// change. When enabled, the lane periodically scans done/closed beads that have
// a closed/merged PR association, reconstructs a compact trajectory from local
// ledgers, and files rule-based findings as advisory beads. analysis_model is
// additionally opt-in; when empty, no model is called and only deterministic
// phase-1 behavior runs.
type RetroConfig struct {
	Enabled             bool   `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	ScanIntervalS       int    `yaml:"scan_interval_s,omitempty" json:"scan_interval_s,omitempty"`
	MaxFixAttempts      int    `yaml:"max_fix_attempts,omitempty" json:"max_fix_attempts,omitempty"`
	MaxKicks            int    `yaml:"max_kicks,omitempty" json:"max_kicks,omitempty"`
	LongStallDays       int    `yaml:"long_stall_days,omitempty" json:"long_stall_days,omitempty"`
	RecentClosedWindowS int    `yaml:"recent_closed_window_s,omitempty" json:"recent_closed_window_s,omitempty"`
	AnalysisModel       string `yaml:"analysis_model,omitempty" json:"analysis_model,omitempty"`
}

// planFromLabelMinACMM is the lowest ACMM level at which the label trigger fires
// by default. It matches planning.PlanningMinACMMLevel: the architect agent that
// decomposes the minted epics only has a cadence at L5 (4h) and L6 (15m), so
// below L5 a minted epic would sit in decompose_pending forever. It is duplicated
// here (rather than imported) to avoid a config→planning import cycle.
const planFromLabelMinACMM = 5

// PlanFromLabelEnabled reports whether the label trigger should fire, given the
// hive's ACMM level. The trigger is OFF by default and must be explicitly
// enabled (`plan_from_label: true`): the label path pipes a raw issue body into
// the architect's kick prompt with no per-kick review, so a maintainer merely
// labeling an attacker's issue would otherwise auto-fire attacker-controlled
// text into the highest-autonomy agent. Making it opt-in forces an operator to
// consciously accept that. When explicitly enabled, planning's own
// PlanIssuesFromLabels still applies a hard L5+ no-op (the architect that
// decomposes epics only has a cadence at L5/L6), so enabling it below L5 is
// inert — defense in depth that cannot be overridden away.
func (p PlanningConfig) PlanFromLabelEnabled(acmmLevel int) bool {
	if p.PlanFromLabel != nil {
		return *p.PlanFromLabel && acmmLevel >= planFromLabelMinACMM
	}
	return false
}

// ClassifierConfig makes the tier-classification keyword lists (pkg/classify)
// config-driven and dashboard-visible, mirroring how per-agent lane_keywords
// drive lane routing. Both fields are optional: when a list is empty, the
// classifier keeps its built-in default for that tier, so an absent
// `classifier:` block is byte-for-byte the old hardcoded behavior. Wired via
// classify.SetTierKeywords from cmd/hive.
type ClassifierConfig struct {
	// SimpleKeywords are title substrings that classify an issue as Tier
	// "Simple" (→ haiku). Empty keeps the built-in default set.
	SimpleKeywords []string `yaml:"simple_keywords,omitempty" json:"simple_keywords,omitempty"`
	// ComplexSignals are title substrings that classify an issue as Tier
	// "Complex" (→ opus). Empty keeps the built-in default set.
	ComplexSignals []string `yaml:"complex_signals,omitempty" json:"complex_signals,omitempty"`
}

// IntentConfig controls intent-verification reporting and merge-gate
// enforcement. The zero value is report-only with built-in path patterns, so an
// absent intent: block preserves existing merge eligibility behavior.
type IntentConfig struct {
	Enforce               bool     `yaml:"enforce,omitempty" json:"enforce,omitempty"`
	AlignmentModel        string   `yaml:"alignment_model,omitempty" json:"alignment_model,omitempty"`
	TestPathPatterns      []string `yaml:"test_path_patterns,omitempty" json:"test_path_patterns,omitempty"`
	DocsPathPatterns      []string `yaml:"docs_path_patterns,omitempty" json:"docs_path_patterns,omitempty"`
	GuardrailPathPatterns []string `yaml:"guardrail_path_patterns,omitempty" json:"guardrail_path_patterns,omitempty"`
	FeatureSignals        []string `yaml:"feature_signals,omitempty" json:"feature_signals,omitempty"`
}

// TrajectoryConfig governs the trajectory-review lane: a periodic,
// second-model check that reads each running agent's recent transcript and
// scores whether the sequence of actions is still working toward the agent's
// assigned intent (its last kick), pausing the agent when it diverges. This
// is defense against trajectory-level goal drift — individually-innocuous
// steps that assemble toward an unauthorized outcome — which action-level
// gating cannot see. On by default; the lane no-ops when no reviewer endpoint
// resolves, so "on" is safe even without inference set up.
//
// The reviewer needs any OpenAI-compatible /v1/chat/completions endpoint — a
// LiteLLM gateway, a vLLM server, or an llm-d front. It does NOT drive an
// interactive CLI: it makes one stateless request per agent per cycle and
// reads back a strict-JSON verdict, which the CLIs (stateful tmux sessions)
// cannot provide.
type TrajectoryConfig struct {
	// Enabled turns the review lane on. Pointer so an omitted key defaults to
	// enabled (applyDefaults sets it), while an explicit "false" disables it.
	Enabled *bool `yaml:"enabled"`
	// Endpoint is the reviewer's OpenAI-compatible base URL (LiteLLM, vLLM, or
	// llm-d — anything serving /v1/chat/completions). Empty → fall back to the
	// governor LiteLLM endpoint. Storing it here lets the reviewer target a
	// cheap local model independent of the agents' inference config.
	Endpoint string `yaml:"endpoint"`
	// APIKeyEnv / APIKeyFile resolve the reviewer endpoint's key (env var NAME
	// or file PATH; never the value). Empty → fall back to the LiteLLM key.
	// A bare vLLM server needs no key; a gateway usually does.
	APIKeyEnv  string `yaml:"api_key_env"`
	APIKeyFile string `yaml:"api_key_file"`
	// IntervalS is how often (seconds) the lane evaluates running agents.
	// It runs off the governor tick, so the effective floor is the governor
	// eval interval; a value below that reviews every tick. 0 → default.
	IntervalS int `yaml:"interval_s"`
	// Model is the reviewer model id. Empty → the governor LiteLLM
	// default_model. A cheap-but-capable model is appropriate; a tiny model
	// will emit weaker verdicts (the lane fails open, so it degrades toward
	// "catches less," not "false-pauses").
	Model string `yaml:"model"`
	// TranscriptLines caps how many trailing transcript lines are sent to the
	// reviewer. 0 → default. Bounds token cost and keeps the review focused
	// on recent behavior.
	TranscriptLines int `yaml:"transcript_lines"`
	// OnDivergence is the action taken when a trajectory is judged divergent:
	// "pause" (stop the agent and alert — default) or "alert" (notify only,
	// leave the agent running). Any other value is treated as "alert".
	OnDivergence string `yaml:"on_divergence"`
	// ExemptAgents are never reviewed (e.g. advisory-only agents that open no
	// PRs and touch no infrastructure).
	ExemptAgents []string `yaml:"exempt_agents"`
}

// ReplanConfig governs the Phase 3 stall-replan lane: a periodic check that
// finds approved plans whose sub-tasks have stopped progressing and re-kicks the
// architect to revise them, bounded by a per-plan replan cap. It runs off the
// governor tick on its own cadence (like the trajectory lane). On by default; it
// is a no-op when there are no approved plans, so "on" is always safe.
type ReplanConfig struct {
	// Enabled turns the stall-replan lane on. Pointer so an omitted key defaults
	// to enabled, while an explicit "false" disables it.
	Enabled *bool `yaml:"enabled"`
	// IntervalS is how often (seconds) the lane scans for stalled plans. It runs
	// off the governor tick, so the effective floor is the governor eval
	// interval. 0 → default (30m).
	IntervalS int `yaml:"interval_s"`
	// StallThresholdS is how long (seconds) a plan may go without any child
	// progressing before it is considered stalled. 0 → default (6h).
	StallThresholdS int `yaml:"stall_threshold_s"`
	// MaxReplans caps replans per plan before the lane stops and escalates to a
	// human. 0 → default (5).
	MaxReplans int `yaml:"max_replans"`
}

// IsEnabled reports whether the stall-replan lane is on. Default is ON: a nil
// Enabled (key omitted) counts as enabled, only an explicit false disables it.
func (r ReplanConfig) IsEnabled() bool {
	return r.Enabled == nil || *r.Enabled
}

// IsEnabled reports whether the trajectory-review lane is on. Default is ON:
// a nil Enabled (key omitted) counts as enabled, only an explicit false
// disables it. The lane itself no-ops when no reviewer endpoint resolves, so
// defaulting on is safe even for hives without inference configured.
func (t TrajectoryConfig) IsEnabled() bool {
	return t.Enabled == nil || *t.Enabled
}
