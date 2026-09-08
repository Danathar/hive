package advisory

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// LinkedWork describes an explicit GitHub reference, not an inferred fix.
type LinkedWork struct {
	Owner     string    `json:"owner"`
	Repo      string    `json:"repo"`
	Number    int       `json:"number"`
	URL       string    `json:"url"`
	Kind      string    `json:"kind,omitempty"` // issue or pr; empty when unknown
	State     string    `json:"state"`          // OPEN, CLOSED, MERGED, or UNKNOWN
	CheckedAt time.Time `json:"checked_at,omitzero"`
}

type workRef struct {
	owner, repo string
	number      int
}

var (
	linkedWorkBareRefPattern = regexp.MustCompile(`(^|[^0-9A-Za-z_/#-])#([0-9]+)\b`)
	linkedWorkURLPattern     = regexp.MustCompile(`https://github\.com/([A-Za-z0-9][A-Za-z0-9_.-]*)/([A-Za-z0-9][A-Za-z0-9_.-]*)/(?:issues|pull)/([0-9]+)\b`)
)

// findingWorkRefs reuses the digest linkifier's reference syntax. Bare numbers
// require the digest's repo context; qualified references carry their own.
func findingWorkRefs(f Finding, owner, repo string) []workRef {
	var refs []workRef
	seen := make(map[workRef]bool)
	add := func(o, r string, n int) {
		ref := workRef{strings.ToLower(o), strings.ToLower(r), n}
		if o != "" && r != "" && n > 0 && !seen[ref] {
			refs = append(refs, workRef{o, r, n})
			seen[ref] = true
		}
	}
	if m := ghNumRefPattern.FindStringSubmatch(strings.TrimSpace(f.File)); m != nil {
		n, err := strconv.Atoi(m[1])
		o, r := owner, repo
		if hintedOwner, hintedRepo, ok := repoHintFromTitle(f.Title, n, owner); ok {
			o, r = hintedOwner, hintedRepo
		}
		if err == nil {
			add(o, r, n)
		}
	}
	for field, text := range []string{f.File, f.Title, f.Detail} {
		for _, m := range linkedWorkURLPattern.FindAllStringSubmatch(text, -1) {
			if n, err := strconv.Atoi(m[3]); err == nil {
				add(m[1], m[2], n)
			}
		}
		// Remove URLs before scanning shorthand: anchors such as #123 on a
		// URL are not additional issue references in the digest's repo.
		text = workURLPattern.ReplaceAllString(text, " ")
		for _, token := range inlineRefPattern.FindAllString(text, -1) {
			// Bead external refs may carry Hive's gh- source prefix.
			if field == 0 && strings.Contains(token, "/") {
				token = strings.TrimPrefix(token, "gh-")
			}
			if o, r, n, ok := splitInlineRef(token, owner); ok {
				add(o, r, n)
			}
		}
		for _, m := range linkedWorkBareRefPattern.FindAllStringSubmatch(text, -1) {
			if n, err := strconv.Atoi(m[2]); err == nil {
				add(owner, repo, n)
			}
		}
	}
	return refs
}

var workURLPattern = regexp.MustCompile(`https?://[^\s<>\x60]+`)

// ResolveLinkedWork annotates the already-ranked digest, without changing its
// counts, ranking, bead lifecycle, or evidence flags. Successful and failed
// lookups are cached for this build, with the same 250-reference bound used by
// the stale-reference retirement machinery. There are no frontend lookups.
// A nil resolver or exhausted budget retains the explicit link as UNKNOWN.
func ResolveLinkedWork(d *Digest, owner, repo string, resolve func(string, string, int) (LinkedWork, bool)) {
	if d == nil {
		return
	}
	cache := make(map[workRef]LinkedWork)
	budget := 250
	agents := make([]string, 0, len(d.ByAgent))
	for agent := range d.ByAgent {
		agents = append(agents, agent)
	}
	sort.Strings(agents)
	for _, agent := range agents {
		for i := range d.ByAgent[agent] {
			f := &d.ByAgent[agent][i]
			f.LinkedWork = nil
			for _, ref := range findingWorkRefs(*f, owner, repo) {
				key := workRef{strings.ToLower(ref.owner), strings.ToLower(ref.repo), ref.number}
				work, found := cache[key]
				if !found {
					work = LinkedWork{Owner: ref.owner, Repo: ref.repo, Number: ref.number,
						URL: issueURL(ref.owner, ref.repo, ref.number), State: "UNKNOWN"}
					if resolve != nil && budget > 0 {
						budget--
						if result, ok := resolve(ref.owner, ref.repo, ref.number); ok &&
							(result.Kind == "issue" || result.Kind == "pr") &&
							(result.State == "OPEN" || result.State == "CLOSED" || (result.Kind == "pr" && result.State == "MERGED")) {
							work.Kind, work.State, work.CheckedAt = result.Kind, result.State, result.CheckedAt
							if work.Kind == "pr" {
								work.URL = fmt.Sprintf("https://github.com/%s/%s/pull/%d", ref.owner, ref.repo, ref.number)
							}
						}
					}
					cache[key] = work
				}
				f.LinkedWork = append(f.LinkedWork, work)
			}
		}
	}
}
