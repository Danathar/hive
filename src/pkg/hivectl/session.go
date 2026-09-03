package hivectl

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CookieEnv names the environment variable carrying a per-user session as a
// Cookie header value (e.g. "hive_session=..."), for hives that do not accept
// the shared dashboard token: hub-hosted ones, and spokes with an
// authorized_users allowlist, where the shared token is deliberately disabled
// because it grants unscoped owner with no per-user identity.
//
// The name is shared with pkg/tui/client's session lane (#5645/#5649): one
// exported variable serves the TUI and every non-interactive subcommand. It is
// defined here as well rather than imported because the dependency would point
// the wrong way — pkg/hivectl is the plain API client and must not pull in the
// TUI and its terminal stack just to spell an environment variable.
//
// An explicitly exported value ALWAYS wins over a cached session from
// `hivectl login`: an operator who set the variable did so deliberately, and a
// cache that could override it would make the exported credential silently
// inert.
const CookieEnv = "HIVE_DASHBOARD_COOKIE"

const (
	// sessionFileMode is asserted by tests, not just applied: the cache holds a
	// credential equivalent to a logged-in browser session, so a world-readable
	// file would hand every local user a dashboard login. 0600 — owner only.
	sessionFileMode = os.FileMode(0o600)

	// sessionDirMode keeps the containing directory owner-only too. The file
	// mode alone is not enough on systems where the config dir is fresh: a
	// 0755 directory leaks the file's existence and invites future files to
	// default open.
	sessionDirMode = os.FileMode(0o700)

	// sessionFallbackTTL mirrors the dashboard's sessionCookieMaxAge
	// (pkg/dashboard/session.go): hive_session cookies live 30 days. Used only
	// when the Set-Cookie that minted the session carried no explicit expiry,
	// so a cache entry never lives longer than the server-side session it
	// stands for.
	sessionFallbackTTL = 30 * 24 * time.Hour
)

// ErrSessionExpired reports that a cached session's stored expiry has passed.
//
// Load still returns the session alongside this error. That is deliberate: an
// expired cookie is harmless to PRESENT (the server re-validates every request
// and ignores invalid cookies on the token lane), and callers like logout want
// to hand it to the server for clearing regardless. What the error buys is the
// chance to tell the operator to run `hivectl login` instead of showing them a
// bare 401.
var ErrSessionExpired = errors.New("cached hive session has expired")

// Session is one cached per-user dashboard session.
//
// Cookie is a complete Cookie HEADER value ("hive_session=...", several joined
// with "; "), not a bare id — the same convention as HIVE_DASHBOARD_COOKIE, so
// the two sources are interchangeable everywhere they are consumed.
type Session struct {
	Cookie     string    `json:"cookie"`
	Username   string    `json:"username,omitempty"`
	ObtainedAt time.Time `json:"obtained_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// sessionFile is the on-disk shape: sessions keyed by normalized dashboard
// URL, so one operator can hold sessions for several hives at once.
type sessionFile struct {
	Sessions map[string]Session `json:"sessions"`
}

// SessionStore reads and writes the credential cache `hivectl login` fills.
type SessionStore struct {
	path string
}

// NewSessionStore builds a store over an explicit file path. Tests use this to
// stay off the developer's real cache; production callers use
// DefaultSessionStore.
func NewSessionStore(path string) *SessionStore {
	return &SessionStore{path: path}
}

// DefaultSessionStore places the cache in the standard per-user config
// location: $XDG_CONFIG_HOME/hive/sessions.json, falling back to the
// platform's os.UserConfigDir.
//
// XDG_CONFIG_HOME is honoured EXPLICITLY, before os.UserConfigDir, because Go
// ignores it on darwin — and a variable that redirects the cache on Linux but
// silently not on a Mac would make the location impossible to reason about
// (and every test that redirects it flaky by platform).
func DefaultSessionStore() (*SessionStore, error) {
	dir := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
	if dir == "" {
		d, err := os.UserConfigDir()
		if err != nil {
			return nil, fmt.Errorf("locate user config dir for the hive session cache: %w", err)
		}
		dir = d
	}
	return NewSessionStore(filepath.Join(dir, "hive", "sessions.json")), nil
}

// Path reports where the cache lives, for messages that tell the operator what
// was written — never for printing its contents.
func (s *SessionStore) Path() string { return s.path }

// SessionKey normalizes a dashboard URL into the cache key.
//
// Normalization exists because the same hive is named inconsistently across
// the tool: `hivectl --server` defaults to http://127.0.0.1:3001 while the
// TUI's HIVE_DASHBOARD_URL defaults to http://localhost:3001. Those are the
// same loopback dashboard, and a login performed under one spelling must be
// found under the other — so loopback hosts collapse to "localhost", case is
// folded, default ports and trailing slashes are dropped.
func SessionKey(server string) string {
	raw := strings.TrimSpace(server)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return strings.ToLower(strings.TrimRight(raw, "/"))
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	if host == "127.0.0.1" || host == "::1" {
		host = "localhost"
	}
	if strings.Contains(host, ":") {
		// A non-loopback IPv6 literal needs its brackets back.
		host = "[" + host + "]"
	}
	if port := u.Port(); port != "" && !isDefaultPort(scheme, port) {
		host += ":" + port
	}
	return scheme + "://" + host + strings.TrimRight(u.Path, "/")
}

func isDefaultPort(scheme, port string) bool {
	return (scheme == "http" && port == "80") || (scheme == "https" && port == "443")
}

// Load returns the cached session for server, or (nil, nil) when there is
// none. An entry past its expiry is returned WITH ErrSessionExpired — see the
// error's doc for why the session still comes back.
//
// A cache file that cannot be parsed is an error, not an empty result: the
// operator has a real session in a file this code corrupted or something else
// mangled, and pretending it is absent would send them back through a login
// they should not need. Callers on paths that must keep working regardless
// (every ordinary subcommand) degrade explicitly; login/logout surface it.
func (s *SessionStore) Load(server string) (*Session, error) {
	file, err := s.read()
	if err != nil {
		return nil, err
	}
	sess, ok := file.Sessions[SessionKey(server)]
	if !ok || sess.Cookie == "" {
		return nil, nil
	}
	if !sess.ExpiresAt.IsZero() && time.Now().After(sess.ExpiresAt) {
		return &sess, fmt.Errorf("%w (expired %s)", ErrSessionExpired, sess.ExpiresAt.Format(time.RFC3339))
	}
	return &sess, nil
}

// Save writes or replaces the session for server, creating the cache with
// owner-only permissions. The write goes through a same-directory temp file
// and a rename so a crash mid-write can never leave a half-written cache, and
// so the file is 0600 from its first byte — there is no window where the
// credential exists on disk with looser permissions.
func (s *SessionStore) Save(server string, sess Session) error {
	file, err := s.read()
	if err != nil {
		// A cache we cannot parse cannot be merged with. Refuse rather than
		// silently discarding whatever sessions it held for other hives.
		return err
	}
	if file.Sessions == nil {
		file.Sessions = map[string]Session{}
	}
	file.Sessions[SessionKey(server)] = sess
	return s.write(file)
}

// Delete removes the session for server, reporting whether one was present.
func (s *SessionStore) Delete(server string) (bool, error) {
	file, err := s.read()
	if err != nil {
		return false, err
	}
	key := SessionKey(server)
	if _, ok := file.Sessions[key]; !ok {
		return false, nil
	}
	delete(file.Sessions, key)
	return true, s.write(file)
}

func (s *SessionStore) read() (*sessionFile, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return &sessionFile{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read hive session cache %s: %w", s.path, err)
	}
	var file sessionFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("hive session cache %s is not valid JSON (delete it and run 'hivectl login' again): %w", s.path, err)
	}
	return &file, nil
}

func (s *SessionStore) write(file *sessionFile) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, sessionDirMode); err != nil {
		return fmt.Errorf("create hive config dir %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("encode hive session cache: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "sessions-*.json.tmp")
	if err != nil {
		return fmt.Errorf("write hive session cache: %w", err)
	}
	tmpName := tmp.Name()
	// CreateTemp opens 0600 already; Chmod pins it against a permissive umask
	// implementation ever changing that.
	if err := tmp.Chmod(sessionFileMode); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("restrict hive session cache permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("write hive session cache: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("write hive session cache: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("write hive session cache: %w", err)
	}
	return nil
}
