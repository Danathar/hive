package client

import "context"

// TokenUsage is the response from GET /api/tokens.
//
// SOURCE OF TRUTH, AND A SPEC GAP. dashboard/openapi.json publishes the
// operation but describes its 200 response only as an untyped object. These
// fields are therefore transcribed from tokens.AggregateSummary and
// tokens.AgentModelBucket (src/pkg/tokens/collector.go), which are returned
// directly by dashboard.Server.handleTokens (src/pkg/dashboard/api.go). The
// missing OpenAPI schema is tracked by kubestellar/hive#5077.
//
// Cost is deliberately absent. The endpoint returns token counts, not money;
// cost is estimated separately by the dashboard's /api/cost endpoint. Adding a
// cost field here would silently invent a wire contract that does not exist.
//
// Status is present only for the handler's {"status":"no_collector"}
// response. When a collector exists, every other field mirrors
// tokens.AggregateSummary field-for-field.
type TokenUsage struct {
	Status           string                  `json:"status,omitempty"`
	TotalTokens      int64                   `json:"total_tokens"`
	TotalInput       int64                   `json:"total_input"`
	TotalOutput      int64                   `json:"total_output"`
	TotalCacheRead   int64                   `json:"total_cache_read"`
	TotalCacheCreate int64                   `json:"total_cache_create"`
	TotalMessages    int                     `json:"total_messages"`
	ByAgent          map[string]int64        `json:"by_agent"`
	ByModel          map[string]int64        `json:"by_model"`
	ByAgentDetail    map[string]*TokenBucket `json:"by_agent_detail"`
	ByModelDetail    map[string]*TokenBucket `json:"by_model_detail"`
	Sessions         []TokenSession          `json:"sessions"`
	SessionCount     int                     `json:"session_count"`
}

// TokenBucket is one per-agent or per-model breakdown in TokenUsage.
type TokenBucket struct {
	Input       int64 `json:"input"`
	Output      int64 `json:"output"`
	CacheRead   int64 `json:"cache_read"`
	CacheCreate int64 `json:"cache_create"`
	Messages    int   `json:"messages"`
	Sessions    int   `json:"sessions"`
}

// TokenSession mirrors tokens.SessionSummary, the entries in TokenUsage's
// sessions array.
type TokenSession struct {
	SessionID      string            `json:"session_id"`
	Agent          string            `json:"agent"`
	Model          string            `json:"model"`
	InputTokens    int64             `json:"input_tokens"`
	OutputTokens   int64             `json:"output_tokens"`
	CacheRead      int64             `json:"cache_read"`
	CacheCreate    int64             `json:"cache_create"`
	TotalTokens    int64             `json:"total_tokens"`
	Messages       int               `json:"messages"`
	FirstActive    int64             `json:"first_active,omitempty"`
	LastActive     int64             `json:"last_active,omitempty"`
	Backend        string            `json:"backend,omitempty"`
	Usage          []TokenUsageEvent `json:"usage,omitempty"`
	UsageCoalesced int               `json:"usage_coalesced,omitempty"`
}

// TokenUsageEvent is one timestamped token slice within a session.
type TokenUsageEvent struct {
	TimestampMs int64  `json:"ts_ms"`
	Model       string `json:"model,omitempty"`
	Coalesced   int    `json:"coalesced,omitempty"`
	Input       int64  `json:"input"`
	Output      int64  `json:"output"`
	CacheRead   int64  `json:"cache_read"`
	CacheCreate int64  `json:"cache_create"`
}

// Tokens returns the dashboard's token-usage summary from GET /api/tokens.
func (c *Client) Tokens(ctx context.Context) (TokenUsage, error) {
	var usage TokenUsage
	if err := c.getJSON(ctx, "/api/tokens", &usage); err != nil {
		return TokenUsage{}, err
	}
	return usage, nil
}
