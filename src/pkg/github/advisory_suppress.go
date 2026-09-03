package github

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Advisory-digest repeat suppression (#5507).
//
// The digest write path already had a skip-if-unchanged guard (#4818) keyed on
// a sha256 of the FINAL comment body. That guard never fired in practice,
// because FormatDigestMarkdown stamps d.GeneratedAt into the body in two
// places — the "## 🐝 Advisory Digest — 2026-08-31 04:00 UTC" header and the
// "evaluated <RFC3339>" clause of the zero-finding line. Every cycle therefore
// produced a unique body hash, so every cycle wrote. On a spoke where all
// agents were login-blocked and no finding ever changed, that turned into ~250
// comments on ibm/alchemy-logging#686, the vast majority reading "0 findings".
//
// Two guards close it, both computed from the MATERIAL content of a digest —
// the findings, counts and prose — with every timestamp and formatting-churn
// artifact removed:
//
//  1. identical: the material fingerprint of the digest about to be posted
//     matches the material fingerprint of the digest already on the target.
//  2. zero_finding_cap: both the pending and the posted digest report zero
//     findings, and the posted one is younger than
//     zeroFindingDigestMinInterval. A zero-finding digest carries no news, so
//     one per target per 48h is plenty even when its wording drifts.
//
// Guard 2 applies ONLY when BOTH digests are zero-finding. A digest that goes
// 0 findings → 3 findings is material news and posts immediately, inside the
// window or not.
//
// State: there is deliberately no new persistent store. The comparison baseline
// is the digest comment ALREADY ON THE TARGET, which the post path fetches
// anyway (findDigestComment). That makes both guards inherently restart-safe:
// a governor that restarts re-reads the same comment and reaches the same
// decision. The observed failure was on login-blocked spokes, which restart
// often, so in-memory-only state would have forgotten and resumed spamming —
// exactly the bug. The pre-existing in-memory #4818 hash guard is retained on
// top as a cheap fast path; it is an optimization, not the correctness gate.

const (
	// zeroFindingDigestMinInterval is the minimum time between two
	// zero-finding advisory digests on the SAME target. A digest reporting no
	// findings says nothing that a reader needs twice a day, and these
	// comments land on THIRD-PARTY repositories where every subscriber is
	// notified. 48h keeps the freshness signal alive (the hub's staleness gate
	// alarms well after this) while collapsing ~12 zero-finding posts a day
	// into one.
	zeroFindingDigestMinInterval = 48 * time.Hour

	// suppressReasonIdentical marks a digest suppressed because its material
	// content is byte-identical to the digest already on the target.
	suppressReasonIdentical = "identical"
	// suppressReasonZeroFindingCap marks a digest suppressed because it is a
	// zero-finding digest posted inside zeroFindingDigestMinInterval of the
	// previous zero-finding digest on the same target.
	suppressReasonZeroFindingCap = "zero_finding_cap"
)

// AuditActionAdvisoryDigestSuppressed is the audit action recorded when a
// digest post is suppressed. It is LOCAL-AUDIT-ONLY by design: posting a
// "suppressed" note to the issue would replace one kind of subscriber noise
// with another.
const AuditActionAdvisoryDigestSuppressed = "advisory_digest_suppressed"

// volatileDigestPatterns match the parts of a rendered digest that change on
// every render without the findings changing. They are stripped before
// fingerprinting so that "same findings, one minute later" hashes identically.
//
// Getting this exclusion right is the whole fix: if a single timestamp leaks
// into the fingerprint every digest is unique and the suppression silently
// does nothing — which is precisely how the #4818 guard failed.
var volatileDigestPatterns = []*regexp.Regexp{
	// Header stamp: "## 🐝 Advisory Digest — 2026-08-31 04:00 UTC".
	// Matched by shape, not by prefix, so a renamed heading still scrubs.
	regexp.MustCompile(`\d{4}-\d{2}-\d{2} \d{2}:\d{2}(:\d{2})?( [A-Za-z/_+\-0-9]{1,10})?`),
	// RFC3339 stamps: "evaluated 2026-08-31T04:00:11Z", finding timestamps,
	// and the analyzed-snapshot footer's date.
	regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+\-]\d{2}:\d{2})`),
	// Relative ages the renderer emits ("3h ago", "2 days ago"): these tick
	// upward every cycle while the underlying finding is unchanged.
	regexp.MustCompile(`\b\d+\s*(s|m|h|d|second|minute|hour|day|week|month)s?\s+ago\b`),
	// Commit-pin footer: the snapshot SHA advances with every upstream push,
	// which is repo churn rather than a change in what the agents found.
	regexp.MustCompile(`\b[0-9a-f]{7,40}\b`),
}

// materialDigestFingerprint reduces a rendered digest body to a stable hash of
// its MATERIAL content: the findings, their counts and their prose, with all
// timestamps, relative ages, commit pins and whitespace churn removed. Two
// digests rendered a day apart from an unchanged finding set fingerprint
// identically; a digest whose finding set changed does not.
func materialDigestFingerprint(body string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(materialDigestContent(body))))
}

// materialDigestContent is the normalized text materialDigestFingerprint
// hashes. Split out so tests can assert on the normalization directly rather
// than only on hash equality — an assertion over opaque hashes cannot show
// WHICH part of the exclusion is wrong.
func materialDigestContent(body string) string {
	s := body
	for _, re := range volatileDigestPatterns {
		s = re.ReplaceAllString(s, "")
	}
	// Collapse whitespace last: stripping a stamp mid-line leaves ragged runs
	// of spaces that would otherwise re-introduce spurious differences.
	lines := strings.Split(s, "\n")
	kept := make([]string, 0, len(lines))
	for _, ln := range lines {
		ln = strings.Join(strings.Fields(ln), " ")
		if ln == "" {
			continue
		}
		kept = append(kept, ln)
	}
	return strings.Join(kept, "\n")
}

// zeroFindingLine matches the rendered "**Findings:** 0" summary that
// FormatDigestMarkdown emits for a digest with no open findings. The count is
// captured so a digest with any findings at all is never treated as
// zero-finding.
var zeroFindingLine = regexp.MustCompile(`(?m)^\*\*Findings:\*\*\s*(\d+)`)

// isZeroFindingDigest reports whether a rendered digest body announces zero
// findings. A body with no recognizable findings line returns false — the
// conservative answer, since the zero-finding cap must never suppress a digest
// whose content it could not read.
func isZeroFindingDigest(body string) bool {
	m := zeroFindingLine.FindStringSubmatch(body)
	if m == nil {
		return false
	}
	return m[1] == "0"
}

// ensureStringMap returns m, allocating it first when nil. The advisory maps
// are lazily created across several branches of PostAdvisoryDigest; this keeps
// each of them a one-liner under the same mutex.
func ensureStringMap(m map[string]string) map[string]string {
	if m == nil {
		return make(map[string]string)
	}
	return m
}

// digestSuppression is the decision for one post attempt.
type digestSuppression struct {
	// suppress reports whether the forge write must be skipped.
	suppress bool
	// reason is suppressReasonIdentical or suppressReasonZeroFindingCap, set
	// only when suppress is true.
	reason string
}

// evaluateDigestSuppression decides whether posting pending to a target that
// already carries posted (last updated at postedAt) should be suppressed.
//
// hasPosted is false when the target carries no digest comment yet — the first
// digest on a target ALWAYS posts, so a fresh advisory issue is never left
// empty by the guards.
func evaluateDigestSuppression(pending, posted string, hasPosted bool, postedAt, now time.Time) digestSuppression {
	if !hasPosted {
		return digestSuppression{}
	}
	if materialDigestFingerprint(pending) == materialDigestFingerprint(posted) {
		return digestSuppression{suppress: true, reason: suppressReasonIdentical}
	}
	// Zero-finding cap. Requires BOTH sides to be zero-finding: a transition
	// out of (or into) a non-empty finding set is material news and must post
	// even inside the window.
	if isZeroFindingDigest(pending) && isZeroFindingDigest(posted) &&
		!postedAt.IsZero() && now.Sub(postedAt) < zeroFindingDigestMinInterval {
		return digestSuppression{suppress: true, reason: suppressReasonZeroFindingCap}
	}
	return digestSuppression{}
}
