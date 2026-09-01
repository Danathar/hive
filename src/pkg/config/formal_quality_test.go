package config

import (
	"encoding/json"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestFormalQualityCapabilityRequiresOptInAndL5(t *testing.T) {
	tests := []struct {
		name   string
		formal bool
		level  int
		want   bool
	}{
		{name: "absent at L6", level: 6},
		{name: "opted in below floor", formal: true, level: 4},
		{name: "opted in at floor", formal: true, level: 5, want: true},
		{name: "opted in above floor", formal: true, level: 6, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := (QualityConfig{Formal: tt.formal}).FormalEnabled(tt.level)
			if got != tt.want {
				t.Fatalf("FormalEnabled(%d) = %v, want %v", tt.level, got, tt.want)
			}
		})
	}
}

func TestFormalQualityConfigSerialization(t *testing.T) {
	var cfg Config
	if err := yaml.Unmarshal([]byte("quality:\n  formal: true\n"), &cfg); err != nil {
		t.Fatalf("unmarshal YAML: %v", err)
	}
	if !cfg.Quality.Formal {
		t.Fatal("quality.formal did not parse from YAML")
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal JSON: %v", err)
	}
	var roundTrip struct {
		Quality QualityConfig `json:"quality"`
	}
	if err := json.Unmarshal(data, &roundTrip); err != nil {
		t.Fatalf("unmarshal JSON: %v", err)
	}
	if !roundTrip.Quality.Formal {
		t.Fatalf("quality.formal lost in JSON round trip: %s", data)
	}
}
