package client

import "context"

const costSourceEstimated = "estimated"

// CostSummary is the estimated-cost portion of GET /api/cost.
//
// The dashboard response also carries native gateway balances, model and
// session breakdowns, price-table metadata, and repository counts. The compact
// TUI needs none of those: it joins this per-agent estimate to the token rows
// and lets encoding/json discard the unrelated fields.
//
// TotalUSD is fully price-table-backed only when AllPriced reports true. The
// server lists models without an exact price in UnpricedModels, so a caller must
// not present a mixed summary's numeric total as an authoritative fleet cost.
type CostSummary struct {
	TotalUSD       float64          `json:"total_usd"`
	ByAgent        []CostAgentEntry `json:"by_agent"`
	UnpricedModels []string         `json:"unpriced_models"`
}

// AllPriced reports whether every model has an exact server-side price.
func (s CostSummary) AllPriced() bool { return len(s.UnpricedModels) == 0 }

// CostAgentEntry is one agent in the estimated-cost breakdown.
//
// USD is intentionally a float rather than a pointer because zero on the wire
// can be either a genuine $0.00 estimate or an unpriced-model placeholder.
// Source carries the distinction; callers must use Known rather than inferring
// it from USD.
type CostAgentEntry struct {
	Name        string  `json:"name"`
	USD         float64 `json:"usd"`
	Source      string  `json:"source"`
	Input       int64   `json:"input"`
	Output      int64   `json:"output"`
	CacheRead   int64   `json:"cache_read"`
	CacheCreate int64   `json:"cache_create"`
}

// Known reports whether USD is backed by an exact server-side price. An
// unpriced entry can carry USD 0 on the wire, but that zero is only a
// placeholder and must not be mistaken for a free model.
func (e CostAgentEntry) Known() bool { return e.Source == costSourceEstimated }

type costResponse struct {
	Estimated CostSummary `json:"estimated"`
}

// Costs returns the dashboard's estimated token-cost summary from GET
// /api/cost. The dashboard remains the sole pricing authority; this client
// decodes its estimate and does not calculate prices locally.
func (c *Client) Costs(ctx context.Context) (CostSummary, error) {
	var response costResponse
	if err := c.getJSON(ctx, "/api/cost", &response); err != nil {
		return CostSummary{}, err
	}
	return response.Estimated, nil
}
