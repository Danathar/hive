package client

import "context"

// Event is one entry from the dashboard's poll-shaped activity feed,
// GET /api/audit.
//
// WHY /api/audit, NOT /api/events. Despite this task's original wording,
// dashboard/openapi.json and dashboard.Server.handleSSE agree that
// GET /api/events is a long-lived text/event-stream of status snapshots. It
// cannot be fetched with getJSON and does not return event rows. The web
// dashboard's actual poll-shaped feed is GET /api/audit
// (static/index.html:fetchAuditLog), which returns newest-first entries and is
// the endpoint modeled here. Streaming /api/events remains T13a's concern.
//
// The first five fields mirror the published /api/audit schema. UserName is
// also part of the live response: dashboard.AuditEntry and handleAuditLog add
// it when an opaque OIDC user key has a display name. The OpenAPI schema has
// not yet caught up with that optional field, but dropping it would expose an
// opaque identity in the TUI where the web dashboard shows a person.
type Event struct {
	Timestamp string `json:"ts"`
	User      string `json:"user"`
	Action    string `json:"action"`
	Detail    string `json:"detail,omitempty"`
	Agent     string `json:"agent,omitempty"`
	UserName  string `json:"user_name,omitempty"`
}

// Events returns up to 200 recent activity entries, newest first, from
// GET /api/audit.
//
// The endpoint requires read-write or owner access. Insufficient access is
// returned as the same typed APIError as every other non-2xx client response.
func (c *Client) Events(ctx context.Context) ([]Event, error) {
	var response struct {
		Entries []Event `json:"entries"`
	}
	if err := c.getJSON(ctx, "/api/audit", &response); err != nil {
		return nil, err
	}
	return response.Entries, nil
}
