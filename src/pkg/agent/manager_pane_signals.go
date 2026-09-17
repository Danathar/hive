package agent

// copilotModelFailureMarker is the Copilot CLI's equivalent of Claude Code's
// "API Error:" chrome. The copilot backend never emits "API Error:" at all, so
// gating solely on that string made this whole watchdog blind to every copilot
// agent: the CLI exhausted its own retries, printed the line below, dropped
// back to its idle prompt, and then sat there until the next cadence kick —
// which on an hourly cadence wastes most of an hour of a live session.
//
// Observed verbatim on the hosted console spoke's scanner agent:
//
//	Execution failed: Error: Failed to get response from the AI model;
//	retried 5 times (total retry wait time: 6.00 seconds) Last error:
//	Failed native model HTTP request: error decoding response body:
//	request or response body error: error reading a body from connection:
//	cannot decrypt peer's message
const copilotModelFailureMarker = "failed to get response from the ai model"

// copilotTransportErrorPatterns are the transport-layer failures the Copilot
// CLI reports underneath copilotModelFailureMarker. Each one means the request
// did not complete, so repeating it can succeed — the same admission test the
// Claude-side list above applies.
//
// The marker alone is deliberately NOT sufficient. Copilot wraps every model
// failure in that sentence, including ones a retry cannot fix, so a bare match
// would nudge an agent in a loop against an auth or quota wall. Requiring a
// transport cause keeps membership as narrow as the Claude-side list, and the
// call site still re-checks authorization and quota independently.
var copilotTransportErrorPatterns = []string{
	"failed native model http request",
	"error decoding response body",
	"error reading a body from connection",
	// TLS record failure seen when the connection is torn down mid-body;
	// transport-level, never a property of the request content.
	"cannot decrypt peer's message",
}
