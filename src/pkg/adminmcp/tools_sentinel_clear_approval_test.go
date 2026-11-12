package adminmcp

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// sentinelClearGovernor is surfaceGovernor on a hive whose build reports
// sentinel.clear_counts_as_approval, at the given value.
func sentinelClearGovernor(on bool) map[string]any {
	g := surfaceGovernor()
	g["security"].(map[string]any)["sentinel"].(map[string]any)["clearCountsAsApproval"] = on
	return g
}

// Turning the setting on is a widening and the preview says what it widens:
// one human step instead of two, an approval that covers every reviewer
// question, and who still cannot approve this way.
func TestSentinelSettingsOpClearCountsAsApprovalOnIsWidening(t *testing.T) {
	provider := &surfaceProvider{governor: sentinelClearGovernor(false)}
	h := surfaceHandler(provider)
	p, id := capPreview(t, h, WriteOpSentinelSettings, map[string]any{"clear_counts_as_approval": true})
	if p.Request.Method != http.MethodPut || p.Request.Path != "/api/config/governor/security" {
		t.Fatalf("request = %s %s", p.Request.Method, p.Request.Path)
	}
	if got, want := bodyJSON(t, p.Request.Body), `{"sentinel":{"clearCountsAsApproval":true}}`; got != want {
		t.Fatalf("body = %s, want %s", got, want)
	}
	if !isWidening(p) || !strings.HasPrefix(p.WideningDisclosure, "Widening: fewer human steps before a merge.") {
		t.Fatalf("turning it on not disclosed as widening: %q", p.WideningDisclosure)
	}
	for _, want := range []string{
		"removes the sentinel-alert label",
		"recorded as approving that head",
		"every question the reviewer raised",
		"admits an outside contributor's head",
		"by a bot or by the pull request's own author still approves nothing",
		"new commits need a new decision",
		// What Sentinel flags is unchanged, and the preview still says so.
		"No widening: Sentinel flags every pull request it flags today.",
	} {
		if !strings.Contains(p.WideningDisclosure, want) {
			t.Errorf("disclosure lacks %q:\n%s", want, p.WideningDisclosure)
		}
	}
	if !strings.Contains(strings.Join(p.Effects, "\n"), "sentinel.clear_counts_as_approval") {
		t.Fatalf("effects do not show the change: %v", p.Effects)
	}
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); err != nil || len(provider.requests) != 1 {
		t.Fatalf("confirm err = %v, requests = %+v", err, provider.requests)
	}
}

// Turning it off narrows; the preview is not a widening and says approvals
// already recorded stand.
func TestSentinelSettingsOpClearCountsAsApprovalOffIsNotWidening(t *testing.T) {
	p, _ := capPreview(t, surfaceHandler(&surfaceProvider{governor: sentinelClearGovernor(true)}), WriteOpSentinelSettings, map[string]any{"clear_counts_as_approval": false})
	if isWidening(p) {
		t.Fatalf("turning it off disclosed as widening: %q", p.WideningDisclosure)
	}
	if !strings.Contains(p.WideningDisclosure, "only clears Sentinel again") || !strings.Contains(p.WideningDisclosure, "Approvals already recorded from earlier removals stand.") {
		t.Fatalf("disclosure = %q", p.WideningDisclosure)
	}
	if got, want := bodyJSON(t, p.Request.Body), `{"sentinel":{"clearCountsAsApproval":false}}`; got != want {
		t.Fatalf("body = %s, want %s", got, want)
	}
}

// A change that leaves the setting alone discloses exactly what it did
// before the setting existed, and does not send the field.
func TestSentinelSettingsOpClearCountsAsApprovalUntouchedLeavesDisclosure(t *testing.T) {
	args := map[string]any{"exempt_logins": map[string]any{"add": []any{"renovate[bot]"}}}
	without, _ := capPreview(t, surfaceHandler(&surfaceProvider{governor: surfaceGovernor()}), WriteOpSentinelSettings, args)
	with, _ := capPreview(t, surfaceHandler(&surfaceProvider{governor: sentinelClearGovernor(true)}), WriteOpSentinelSettings, args)
	if with.WideningDisclosure != without.WideningDisclosure {
		t.Fatalf("disclosure changed though the setting was not touched:\n%q\n%q", with.WideningDisclosure, without.WideningDisclosure)
	}
	if strings.Contains(bodyJSON(t, with.Request.Body), "clearCountsAsApproval") {
		t.Fatalf("an untouched setting was sent: %s", bodyJSON(t, with.Request.Body))
	}
}

func TestSentinelSettingsOpClearCountsAsApprovalRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		governor map[string]any
		args     map[string]any
		want     string
	}{
		"not a boolean":                 {sentinelClearGovernor(false), map[string]any{"clear_counts_as_approval": "yes"}, "must be true or false"},
		"already on":                    {sentinelClearGovernor(true), map[string]any{"clear_counts_as_approval": true}, "changes nothing"},
		"hive build without the field":  {surfaceGovernor(), map[string]any{"clear_counts_as_approval": true}, "does not report sentinel.clear_counts_as_approval"},
		"camelCase name is not offered": {sentinelClearGovernor(false), map[string]any{"clearCountsAsApproval": true}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			h := surfaceHandler(&surfaceProvider{governor: tc.governor})
			_, err := h.PreviewWrite(context.Background(), map[string]any{"operation": WriteOpSentinelSettings, "args": tc.args})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one naming %q", err, tc.want)
			}
		})
	}
}

// The setting is part of what the confirm re-reads: flipped by someone else
// after the preview, the confirm is refused.
func TestSentinelSettingsOpClearCountsAsApprovalIsFingerprinted(t *testing.T) {
	provider := &surfaceProvider{governor: sentinelClearGovernor(false)}
	h := surfaceHandler(provider)
	_, id := capPreview(t, h, WriteOpSentinelSettings, map[string]any{"exempt_logins": map[string]any{"add": []any{"renovate[bot]"}}})
	provider.governor["security"].(map[string]any)["sentinel"].(map[string]any)["clearCountsAsApproval"] = true
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); err == nil {
		t.Fatal("confirm accepted after the setting changed under the preview")
	}
	if len(provider.requests) != 0 {
		t.Fatalf("a stale confirm wrote %+v", provider.requests)
	}
}
