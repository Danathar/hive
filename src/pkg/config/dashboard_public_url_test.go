package config

import (
	"strings"
	"testing"
)

func TestValidateDashboardPublicURL(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr string
	}{
		{name: "empty is unset", in: "", want: ""},
		{name: "whitespace is unset", in: "   ", want: ""},
		{name: "https origin", in: "https://hive.example.com", want: "https://hive.example.com"},
		{name: "http origin with port", in: "http://hive.internal:8080", want: "http://hive.internal:8080"},
		{name: "trailing slash trimmed", in: "https://hive.example.com/", want: "https://hive.example.com"},
		{name: "surrounding whitespace trimmed", in: "  https://hive.example.com/ ", want: "https://hive.example.com"},
		{name: "no scheme", in: "hive.example.com", wantErr: "absolute http:// or https://"},
		{name: "wrong scheme", in: "ftp://hive.example.com", wantErr: "absolute http:// or https://"},
		{name: "missing host", in: "https:///linear/callback", wantErr: "missing host"},
		{name: "path rejected", in: "https://hive.example.com/dashboard", wantErr: "origin only"},
		{name: "query rejected", in: "https://hive.example.com/?x=1", wantErr: "origin only"},
		{name: "fragment rejected", in: "https://hive.example.com/#top", wantErr: "origin only"},
		{name: "credentials rejected", in: "https://u:p@hive.example.com", wantErr: "credentials"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateDashboardPublicURL(tc.in)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("ValidateDashboardPublicURL(%q) = %q, want error containing %q", tc.in, got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
				}
				if !strings.Contains(err.Error(), "dashboard.public_url") {
					t.Fatalf("error = %q, should name the field", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// Load rejects a malformed dashboard.public_url with a clear error and
// normalizes a valid one (trailing slash dropped) so callers never re-trim.
func TestLoad_DashboardPublicURL(t *testing.T) {
	base := minimalValidYAML("acme", "ghp_tok")

	path := writeTempConfig(t, base+"\ndashboard:\n  public_url: https://hive.example.com/\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() with valid public_url: %v", err)
	}
	if cfg.Dashboard.PublicURL != "https://hive.example.com" {
		t.Fatalf("Dashboard.PublicURL = %q, want trailing slash trimmed", cfg.Dashboard.PublicURL)
	}

	path = writeTempConfig(t, base+"\ndashboard:\n  public_url: hive.example.com/linear/callback\n")
	_, err = Load(path)
	if err == nil {
		t.Fatal("Load() expected error for public_url without scheme, got nil")
	}
	if !strings.Contains(err.Error(), "dashboard.public_url") {
		t.Fatalf("error = %q, should name dashboard.public_url", err.Error())
	}
}
