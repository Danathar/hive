package main

import "testing"

func TestShouldHoldAgentPR(t *testing.T) {
	tests := []struct {
		name  string
		agent string
		level int
		want  bool
	}{
		{name: "outreach remains held at full autonomy", agent: "outreach", level: 6, want: true},
		{name: "outreach identity is normalized", agent: " Outreach ", level: 6, want: true},
		{name: "other agents remain autonomous at level 6", agent: "scanner", level: 6, want: false},
		{name: "all agents are held at level 3", agent: "scanner", level: 3, want: true},
		{name: "all agents are held at level 5", agent: "quality", level: 5, want: true},
		{name: "manual level does not add a hold", agent: "scanner", level: 2, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldHoldAgentPR(tt.agent, tt.level); got != tt.want {
				t.Fatalf("shouldHoldAgentPR(%q, %d) = %v, want %v", tt.agent, tt.level, got, tt.want)
			}
		})
	}
}
