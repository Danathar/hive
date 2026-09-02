package hivectl

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func tempStore(t *testing.T) *SessionStore {
	t.Helper()
	return NewSessionStore(filepath.Join(t.TempDir(), "hive", "sessions.json"))
}

func testSession(cookie string) Session {
	return Session{
		Cookie:     cookie,
		Username:   "octocat",
		ObtainedAt: time.Now(),
		ExpiresAt:  time.Now().Add(time.Hour),
	}
}

// TestSessionStoreSaveThenLoad is the round trip every other consumer depends
// on: what login saves is what the next command finds, verbatim — a cookie
// header that came back reformatted would authenticate as nobody.
func TestSessionStoreSaveThenLoad(t *testing.T) {
	store := tempStore(t)
	const server = "http://127.0.0.1:3001"

	if err := store.Save(server, testSession("hive_session=abc123")); err != nil {
		t.Fatalf("Save() = %v", err)
	}
	sess, err := store.Load(server)
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if sess == nil || sess.Cookie != "hive_session=abc123" {
		t.Fatalf("Load() = %+v, want cookie %q", sess, "hive_session=abc123")
	}
	if sess.Username != "octocat" {
		t.Errorf("Username = %q, want octocat", sess.Username)
	}
}

// TestSessionStoreFileIsNotWorldReadable is the acceptance criterion asserted
// as a test, per the issue: the cache holds a credential equivalent to a
// logged-in browser session, so any group/other bit on it hands a dashboard
// login to every local user.
func TestSessionStoreFileIsNotWorldReadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes are not meaningful on windows")
	}
	store := tempStore(t)
	if err := store.Save("http://127.0.0.1:3001", testSession("hive_session=secret")); err != nil {
		t.Fatalf("Save() = %v", err)
	}

	info, err := os.Stat(store.Path())
	if err != nil {
		t.Fatalf("Stat(%s) = %v", store.Path(), err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("session cache mode = %o, want no group/other bits", perm)
	}

	dirInfo, err := os.Stat(filepath.Dir(store.Path()))
	if err != nil {
		t.Fatalf("Stat(dir) = %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("session cache dir mode = %o, want no group/other bits", perm)
	}
}

// TestSessionStoreLoadAbsent pins the no-cache-yet shape: (nil, nil), not an
// error — a machine that has simply never run `hivectl login` is not a failure
// condition for any command.
func TestSessionStoreLoadAbsent(t *testing.T) {
	sess, err := tempStore(t).Load("http://127.0.0.1:3001")
	if err != nil {
		t.Fatalf("Load() = %v, want nil error for an absent cache", err)
	}
	if sess != nil {
		t.Fatalf("Load() = %+v, want nil", sess)
	}
}

// TestSessionStoreExpired pins the expired contract: the session comes BACK,
// alongside ErrSessionExpired. Callers need both — the error is what lets the
// operator be told "run hivectl login" instead of a bare 401, and the session
// is what logout still presents to the server for clearing.
func TestSessionStoreExpired(t *testing.T) {
	store := tempStore(t)
	const server = "http://127.0.0.1:3001"
	sess := testSession("hive_session=old")
	sess.ExpiresAt = time.Now().Add(-time.Minute)
	if err := store.Save(server, sess); err != nil {
		t.Fatalf("Save() = %v", err)
	}

	got, err := store.Load(server)
	if !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("Load() error = %v, want ErrSessionExpired", err)
	}
	if got == nil || got.Cookie != "hive_session=old" {
		t.Fatalf("Load() = %+v, want the expired session returned alongside the error", got)
	}
}

// TestSessionStoreDelete covers logout's half of the lifecycle, including the
// idempotent second delete.
func TestSessionStoreDelete(t *testing.T) {
	store := tempStore(t)
	const server = "http://127.0.0.1:3001"
	if err := store.Save(server, testSession("hive_session=abc")); err != nil {
		t.Fatalf("Save() = %v", err)
	}

	removed, err := store.Delete(server)
	if err != nil || !removed {
		t.Fatalf("Delete() = (%v, %v), want (true, nil)", removed, err)
	}
	if sess, err := store.Load(server); err != nil || sess != nil {
		t.Fatalf("Load() after delete = (%+v, %v), want (nil, nil)", sess, err)
	}
	removed, err = store.Delete(server)
	if err != nil || removed {
		t.Fatalf("second Delete() = (%v, %v), want (false, nil)", removed, err)
	}
}

// TestSessionStoreCorruptCacheIsAnError pins that a mangled cache file names
// itself and the remedy instead of masquerading as "not logged in" — silently
// treating it as absent would send an operator with a real session back
// through a login they should not need, with no explanation.
func TestSessionStoreCorruptCacheIsAnError(t *testing.T) {
	store := tempStore(t)
	if err := os.MkdirAll(filepath.Dir(store.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.Path(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load("http://127.0.0.1:3001"); err == nil {
		t.Fatal("Load() = nil error for a corrupt cache, want an error naming the file")
	}
}

// TestSessionKey pins the normalization that makes one login serve both
// spellings of the same loopback dashboard: hivectl's --server default is
// 127.0.0.1, the TUI's HIVE_DASHBOARD_URL default is localhost, and a session
// obtained under one must be found under the other.
func TestSessionKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b string
		same bool
	}{
		{"loopback spellings collapse", "http://127.0.0.1:3001", "http://localhost:3001", true},
		{"ipv6 loopback collapses too", "http://[::1]:3001", "http://localhost:3001", true},
		{"case and trailing slash fold", "HTTP://Hive.Example.com/", "http://hive.example.com", true},
		{"default port drops", "https://hive.example.com:443", "https://hive.example.com", true},
		{"different hosts stay distinct", "http://spoke-a:3001", "http://spoke-b:3001", false},
		{"different ports stay distinct", "http://localhost:3001", "http://localhost:3002", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ka, kb := SessionKey(tc.a), SessionKey(tc.b)
			if (ka == kb) != tc.same {
				t.Errorf("SessionKey(%q) = %q, SessionKey(%q) = %q, want same=%v", tc.a, ka, tc.b, kb, tc.same)
			}
		})
	}
}
