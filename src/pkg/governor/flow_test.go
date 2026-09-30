package governor

import (
	"testing"
	"time"
)

func TestSurgeDuration(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		mode    Mode
		history []ModeChange
		want    time.Duration
		known   bool
	}{
		{"not surge", ModeBusy, nil, 0, true},
		{"missing history", ModeSurge, nil, 0, false},
		{"current episode", ModeSurge, []ModeChange{{To: ModeSurge, Timestamp: now.Add(-10 * 24 * time.Hour)}, {To: ModeBusy, Timestamp: now.Add(-4 * 24 * time.Hour)}, {To: ModeSurge, Timestamp: now.Add(-72 * time.Hour)}}, 72 * time.Hour, true},
		{"mismatched history", ModeSurge, []ModeChange{{To: ModeBusy, Timestamp: now.Add(-72 * time.Hour)}}, 0, false},
		{"future", ModeSurge, []ModeChange{{To: ModeSurge, Timestamp: now.Add(time.Hour)}}, 0, false},
		{"zero timestamp", ModeSurge, []ModeChange{{To: ModeSurge}}, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := &Governor{state: State{Mode: tt.mode}, modeHistory: tt.history, now: func() time.Time { return now }}
			got, known := g.SurgeDuration()
			if got != tt.want || known != tt.known {
				t.Fatalf("got (%v,%v), want (%v,%v)", got, known, tt.want, tt.known)
			}
		})
	}
}
