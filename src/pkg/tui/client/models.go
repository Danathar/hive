package client

import (
	"context"
	"fmt"
	"net/url"
)

// ModelOption is one model a backend offers, identified by the id the
// inference gateway advertises ("openai/gpt-5", "claude_opus_4.8").
//
// It is a STRING, not a struct, because that is what the endpoint sends.
// dashboard/openapi.json declares models as `items: {"type": "object"}` with no
// properties, but handleInferenceModels (pkg/dashboard/api.go:6795) assigns a
// []string — from fetchInferenceModelsForBackendDetailed, inferenceStaticModelAliases
// or intersectEntitled, all of which are []string — and the web dashboard reads
// it as one (`m.split('/').pop()`, static/index.html:7764). A struct with json
// tags "matching the spec" would fail to decode the real response, so the spec
// is the thing that is wrong here. Filed against #5077, the tracker for exactly
// this drift; the T4/T6 precedent is to transcribe from the handler and cite it.
type ModelOption string

// ModelSetResult is the response from POST /api/model/{agent}/{model}.
type ModelSetResult struct {
	Status string `json:"status"`
	Agent  string `json:"agent"`
	Model  string `json:"model"`
}

// ModelList is the whole response from GET /api/inference/models/{backend}.
//
// The list-level flags are not decoration: a caller that keeps only Models is
// unable to tell a discovered catalogue from a hardcoded guess, which is why
// this returns the envelope rather than the bare slice the task text proposed.
type ModelList struct {
	// Backend echoes the backend that was queried.
	Backend string `json:"backend"`

	// Models is what the backend offers. It is a FLOOR rather than a census
	// whenever Fallback or Partial is set — see Authoritative.
	Models []ModelOption `json:"models"`

	// Fallback reports that endpoint discovery found nothing and the server
	// substituted its static alias list (inferenceStaticModelAliases). These
	// ids are common guesses, unverified against the configured endpoint or
	// key, and the web dashboard labels them "(common alias, unverified)".
	// Presenting them as discovered would offer models the gateway may not
	// serve at all.
	Fallback bool `json:"fallback"`

	// Partial reports that some of the backend's endpoints answered and others
	// did not. Every id present really was discovered, but a model's ABSENCE
	// proves nothing — it may be served only by the endpoint that failed. This
	// is the #4438 lesson: auto-heal must sit out a partial sample rather than
	// switch an agent off a model that looked missing.
	Partial bool `json:"partial"`

	// Entitled reports that the server narrowed Models to the set the
	// configured key may actually use. Some LiteLLM gateways advertise the full
	// catalogue on /v1/models but scope a key's team to a subset that 403s at
	// inference time, so the unnarrowed list would offer unusable models.
	//
	// Absent from most responses: the handler sets it only for a litellm
	// backend whose entitled set the proxy has already learned.
	Entitled bool `json:"entitled,omitempty"`

	// EntitledSource names how the entitlement was learned ("key-info", or a
	// "team not allowed" 403). Set only alongside Entitled.
	//
	// Neither this nor Entitled appears in dashboard/openapi.json's response
	// schema, though the handler returns both — another facet of the drift
	// tracked in #5077.
	EntitledSource string `json:"entitledSource,omitempty"`
}

// Authoritative reports whether Models can be read as the complete set the
// backend serves, rather than a lower bound.
//
// Only a full discovery qualifies. A static fallback was never checked against
// the gateway, and a partial sample is missing whatever the unreachable
// endpoints serve — in both cases a model's absence from Models is not evidence
// that the backend lacks it, so nothing may retire a selection on that basis
// (#4438). This mirrors the guard the web dashboard already applies before
// reconciling agents' configured models.
func (l ModelList) Authoritative() bool { return !l.Fallback && !l.Partial }

// Models lists the models available for one inference backend, from
// GET /api/inference/models/{backend}.
//
// The task text proposed Models(ctx) returning a bare []ModelOption. Both parts
// are corrected here against the endpoint as built: {backend} is a required
// path parameter, so the backend must be passed; and the response's
// Fallback/Partial/Entitled flags qualify the list so materially that dropping
// them would hand a caller a set of model ids with no way to know whether they
// were discovered, guessed, or only partially enumerated.
//
// A backend with no configured endpoint answers 404 (watsonx or litellm before
// a gateway is configured, for instance). That is an ordinary state, not a
// broken hive — callers should treat it as "nothing to offer here" rather than
// surface it as an error, exactly as the dashboard does.
func (c *Client) Models(ctx context.Context, backend string) (ModelList, error) {
	if backend == "" {
		// "/api/inference/models/" matches no route, so the round trip would
		// return a 404 about the routing table instead of the actual mistake.
		return ModelList{}, fmt.Errorf("GET /api/inference/models: backend is required")
	}
	var list ModelList
	// PathEscape mirrors the dashboard's own encodeURIComponent on this call:
	// the backend lands in a path segment, and a separator in it would retarget
	// the request at a different route.
	path := "/api/inference/models/" + url.PathEscape(backend)
	if err := c.getJSON(ctx, path, &list); err != nil {
		return ModelList{}, err
	}
	return list, nil
}

// SetAgentModel persists a model override for one agent and restarts its
// session so the selection takes effect immediately.
//
// Model ids are passed through unchanged. In particular, this client does not
// canonicalize aliases or verify catalogue membership: the agent's effective
// backend owns that validation and returns its authoritative reason in an
// APIError when the model is unavailable.
func (c *Client) SetAgentModel(ctx context.Context, agent, model string) (ModelSetResult, error) {
	const prefix = "/api/model/"
	if agent == "" {
		return ModelSetResult{}, fmt.Errorf("POST %s: agent is required", prefix)
	}
	if model == "" {
		return ModelSetResult{}, fmt.Errorf("POST %s: model is required", prefix)
	}

	var result ModelSetResult
	path := prefix + url.PathEscape(agent) + "/" + url.PathEscape(model)
	if err := c.postJSON(ctx, path, nil, &result); err != nil {
		return ModelSetResult{}, err
	}
	return result, nil
}
