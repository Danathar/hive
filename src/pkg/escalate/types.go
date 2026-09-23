package escalate

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Severity classifies how urgently a person is needed.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityDecision Severity = "decision"
	SeverityPage     Severity = "page"
)

// Valid reports whether s is one of the catalogued severities.
func (s Severity) Valid() bool {
	return severityRank(s) > 0
}

type Event struct {
	Severity Severity  `json:"severity"`
	Title    string    `json:"title,omitempty"`
	Body     string    `json:"body,omitempty"`
	Link     string    `json:"link,omitempty"`
	RunKey   string    `json:"run_key,omitempty"`
	Stage    string    `json:"stage,omitempty"`
	Artifact string    `json:"artifact,omitempty"`
	Gen      uint64    `json:"gen,omitempty"`
	Attempts int       `json:"attempts,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	At       time.Time `json:"at,omitempty"`
}

type Sink interface {
	Name() string
	Deliver(ctx context.Context, ev Event) error
}

func ParseSeverity(s string) (Severity, bool) {
	sv := Severity(strings.ToLower(strings.TrimSpace(s)))
	if !sv.Valid() {
		return "", false
	}
	return sv, true
}

func SeverityAtLeast(got, min Severity) bool {
	return severityRank(got) >= severityRank(min)
}

func severityRank(s Severity) int {
	switch s {
	case SeverityInfo:
		return 1
	case SeverityDecision:
		return 2
	case SeverityPage:
		return 3
	default:
		return 0
	}
}

func (e Event) valid() bool {
	return severityRank(e.Severity) > 0 && (strings.TrimSpace(e.Title) != "" || strings.TrimSpace(e.RunKey) != "")
}

func (e Event) String() string {
	if strings.TrimSpace(e.RunKey) == "" {
		return strings.TrimSpace(e.Title)
	}
	return fmt.Sprintf("%s escalation for run %s stage %s gen %d after %d attempts: %s",
		e.Severity, e.RunKey, e.Stage, e.Gen, e.Attempts, e.Reason)
}
