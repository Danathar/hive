package client

import "context"

// hiveIDResponse is the intentionally narrow wire shape returned by
// GET /api/hive-id. Hive identity is independent from the much larger status
// payload, so callers should not need to fetch or model unrelated fields.
type hiveIDResponse struct {
	ID string `json:"id"`
}

// HiveID returns the hive's configured display identity.
//
// An empty identity is valid and is returned unchanged. The TUI owns how that
// state is rendered; the client only preserves the dashboard response.
func (c *Client) HiveID(ctx context.Context) (string, error) {
	var response hiveIDResponse
	if err := c.getJSON(ctx, "/api/hive-id", &response); err != nil {
		return "", err
	}
	return response.ID, nil
}
