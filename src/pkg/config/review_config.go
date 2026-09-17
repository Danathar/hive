// Review-pipeline configuration: escalation, reviewer parallelism, and the
// auto-merge gate (required checks, self-authored ACMM gating).
package config

import (
	"strings"
)

func repoListSet(repos []string) map[string]bool {
	if len(repos) == 0 {
		return nil
	}
	set := make(map[string]bool, len(repos))
	for _, repo := range repos {
		repo = strings.TrimSpace(repo)
		if repo != "" {
			set[strings.ToLower(repo)] = true
		}
	}
	if len(set) == 0 {
		return nil
	}
	return set
}
