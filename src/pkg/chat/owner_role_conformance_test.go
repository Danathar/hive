package chat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type chatCommandRequestLog struct {
	paths          []string
	ownerPathPosts int
}

func TestReadRoleCommandsConformToOwnerOnlyTable(t *testing.T) {
	log := &chatCommandRequestLog{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		writeChatConformanceResponse(t, w, r)
	}))
	defer ts.Close()

	s := NewService(&recordingBackend{}, Config{
		DashboardURL: ts.URL,
		AllowedUsers: []string{"owner:owner", "reader:read"},
	}, discardLogger())
	s.client = ts.Client()
	s.SetAgentNames([]string{"scanner"})
	s.registerBuiltinCommands()

	commandArgs := map[string]string{
		"status":        "",
		"governor":      "",
		"help":          "",
		"kick":          "scanner check status",
		"pause":         "scanner",
		"resume":        "scanner",
		"standby":       "quality hivecommons/hive#1",
		"standby-clear": "alice claude opus",
		"runs":          "list",
		"persona":       "",
		"inception":     "",
	}

	s.mu.RLock()
	commands := make([]string, 0, len(s.commands))
	for cmd := range s.commands {
		commands = append(commands, cmd)
	}
	s.mu.RUnlock()

	for _, cmd := range commands {
		args, ok := commandArgs[cmd]
		if !ok {
			t.Fatalf("registered command %q is missing from conformance inputs", cmd)
		}
		log.reset()
		s.routeMessage(context.Background(), Message{ID: cmd, Text: "!" + strings.TrimSpace(cmd+" "+args), AuthorID: "reader"})
		var sent []string
		drainQueue(s, &sent)
		if log.ownerPathPosts != 0 {
			t.Fatalf("%s made %d owner-only POST(s): %#v", cmd, log.ownerPathPosts, log.paths)
		}
		if isOwnerOnlyCommand(cmd) {
			if len(sent) != 1 || !strings.Contains(sent[0], "owner role required") {
				t.Fatalf("%s reply = %#v, want owner role refusal", cmd, sent)
			}
			continue
		}
		if len(sent) == 0 {
			t.Fatalf("%s produced no reply", cmd)
		}
	}
}

func TestReadRoleAgentShorthandRefusesBeforeDashboard(t *testing.T) {
	log := &chatCommandRequestLog{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		writeChatConformanceResponse(t, w, r)
	}))
	defer ts.Close()

	s := NewService(&recordingBackend{}, Config{
		DashboardURL: ts.URL,
		AllowedUsers: []string{"owner:owner", "reader:read"},
	}, discardLogger())
	s.client = ts.Client()
	s.SetAgentNames([]string{"scanner"})
	s.registerBuiltinCommands()

	s.routeMessage(context.Background(), Message{ID: "agent", Text: "!scanner check status", AuthorID: "reader"})
	var sent []string
	drainQueue(s, &sent)
	if log.ownerPathPosts != 0 || len(log.paths) != 0 {
		t.Fatalf("agent shorthand made dashboard requests: %#v", log.paths)
	}
	if len(sent) != 1 || !strings.Contains(sent[0], "owner role required") {
		t.Fatalf("reply = %#v, want owner role refusal", sent)
	}
}

func TestOwnerRoleOwnerOnlyCommandsReachDashboard(t *testing.T) {
	log := &chatCommandRequestLog{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		writeChatConformanceResponse(t, w, r)
	}))
	defer ts.Close()

	s := NewService(&recordingBackend{}, Config{
		DashboardURL: ts.URL,
		AllowedUsers: []string{"owner:owner", "reader:read"},
	}, discardLogger())
	s.client = ts.Client()
	s.SetAgentNames([]string{"scanner"})
	s.registerBuiltinCommands()

	cases := []struct {
		name     string
		message  string
		wantPath string
	}{
		{name: "kick", message: "!kick scanner check status", wantPath: "/api/kick/scanner"},
		{name: "pause", message: "!pause scanner", wantPath: "/api/pause/scanner"},
		{name: "resume", message: "!resume scanner", wantPath: "/api/resume/scanner"},
		{name: "standby", message: "!standby quality hivecommons/hive#1", wantPath: "/api/contribute/standby/dispatch"},
		{name: "standby clear", message: "!standby-clear alice claude opus", wantPath: "/api/contribute/standby/clear"},
		{name: "agent shorthand", message: "!scanner check status", wantPath: "/api/kick/scanner"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log.reset()
			s.routeMessage(context.Background(), Message{ID: tc.name, Text: tc.message, AuthorID: "owner"})
			var sent []string
			drainQueue(s, &sent)
			if len(log.paths) != 1 || log.paths[0] != tc.wantPath {
				t.Fatalf("paths = %#v, want [%q]", log.paths, tc.wantPath)
			}
			if len(sent) != 1 || strings.Contains(sent[0], "owner role required") {
				t.Fatalf("reply = %#v, want success reply", sent)
			}
		})
	}
}

func (l *chatCommandRequestLog) reset() {
	l.paths = nil
	l.ownerPathPosts = 0
}

func (l *chatCommandRequestLog) record(r *http.Request) {
	l.paths = append(l.paths, r.URL.Path)
	if r.Method == http.MethodPost && isOwnerDashboardPath(r.URL.Path) {
		l.ownerPathPosts++
	}
}

func isOwnerDashboardPath(path string) bool {
	return strings.HasPrefix(path, "/api/kick/") ||
		strings.HasPrefix(path, "/api/pause/") ||
		strings.HasPrefix(path, "/api/resume/") ||
		path == "/api/contribute/standby/dispatch" ||
		path == "/api/contribute/standby/clear"
}

func writeChatConformanceResponse(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/api/status":
		if err := json.NewEncoder(w).Encode(statusSnapshot{}); err != nil {
			t.Fatalf("write status: %v", err)
		}
	case r.URL.Path == "/api/runs":
		if _, err := w.Write([]byte(`[]`)); err != nil {
			t.Fatalf("write runs: %v", err)
		}
	default:
		w.WriteHeader(http.StatusOK)
	}
}
