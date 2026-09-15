//go:build integration

package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xpzouying/xiaohongshu-mcp/configs"
	"github.com/xpzouying/xiaohongshu-mcp/cookies"
)

// These tests drive real browsers. Run them with:
//
//	GOARCH=arm64 go test -tags integration -run TestManager -v ./browser/

func requireBrowser(t *testing.T) {
	t.Helper()
	if _, err := EnsureBrowser(); err != nil {
		t.Skipf("SKIP: bundled browser unavailable (set GOARCH=arm64 on an arm64 host): %v", err)
	}
}

// newTestManager builds a manager over a temp profile and session file.
func newTestManager(t *testing.T, profileDir string, lc configs.BrowserLifecycle) *Manager {
	t.Helper()
	requireBrowser(t)

	return NewManager(ManagerConfig{
		Headless:   true,
		Options:    []Option{WithFingerprintSeed(probeSeed)},
		ProfileDir: profileDir,
		Session:    cookies.NewLoadCookie(filepath.Join(t.TempDir(), "cookies.json")),
		Lifecycle:  lc,
	})
}

// managerTestServer serves a trivial page from a fixed loopback origin, so that
// localStorage written by one browser is visible to the next.
func managerTestServer(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		http.SetCookie(w, &http.Cookie{Name: "probe_session", Value: "v", Path: "/", MaxAge: 3600})
		_, _ = w.Write([]byte(probeHTML))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	return "http://" + ln.Addr().String() + "/"
}

func leaseAt(t *testing.T, m *Manager, url string) *Lease {
	t.Helper()

	lease, err := m.Lease(context.Background())
	require.NoError(t, err)
	page := lease.Page.Timeout(30 * time.Second)
	page.MustNavigate(url).MustWaitLoad()
	lease.Page = page.CancelTimeout()
	return lease
}

func managerGen(m *Manager) uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.gen
}

// TestManagerReusesBrowser is the core of issue #6: two consecutive actions must
// share one browser process, not launch one each.
func TestManagerReusesBrowser(t *testing.T) {
	url := managerTestServer(t)
	m := newTestManager(t, filepath.Join(t.TempDir(), "profile"), configs.BrowserLifecycle{})
	defer m.Shutdown(context.Background())

	first := leaseAt(t, m, url)
	gen := managerGen(m)
	first.Release()

	second := leaseAt(t, m, url)
	defer second.Release()

	assert.Equal(t, gen, managerGen(m), "the second lease relaunched the browser instead of reusing it")
	assert.True(t, m.Running())

	t.Run("Release 是幂等的", func(t *testing.T) {
		first.Release()
		assert.Equal(t, gen, managerGen(m))
	})
}

// TestManagerRecyclesOnPageCount pins the memory-growth mitigation: a browser
// that has served its page budget is replaced once nothing is in flight.
func TestManagerRecyclesOnPageCount(t *testing.T) {
	url := managerTestServer(t)
	m := newTestManager(t, filepath.Join(t.TempDir(), "profile"), configs.BrowserLifecycle{MaxPages: 1})
	defer m.Shutdown(context.Background())

	first := leaseAt(t, m, url)
	gen := managerGen(m)
	first.Release()

	assert.False(t, m.Running(), "a browser over its page budget must be closed on release")

	second := leaseAt(t, m, url)
	defer second.Release()
	assert.NotEqual(t, gen, managerGen(m), "expected a fresh browser after the recycle")
}

// TestManagerRelaunchesAfterCrash: a browser killed behind the manager's back
// must be detected by the health ping and replaced, not handed out dead.
func TestManagerRelaunchesAfterCrash(t *testing.T) {
	url := managerTestServer(t)
	m := newTestManager(t, filepath.Join(t.TempDir(), "profile"), configs.BrowserLifecycle{})
	defer m.Shutdown(context.Background())

	first := leaseAt(t, m, url)
	gen := managerGen(m)
	first.Release()

	// Kill the process the way a SIGKILL or an OOM would.
	m.mu.Lock()
	killLauncher(m.launcher)
	m.mu.Unlock()

	second := leaseAt(t, m, url)
	defer second.Release()
	assert.NotEqual(t, gen, managerGen(m), "the manager handed out a page from a dead browser")
}

// TestManagerIdleShutdown: an idle browser goes away and comes back on demand.
func TestManagerIdleShutdown(t *testing.T) {
	url := managerTestServer(t)
	m := newTestManager(t, filepath.Join(t.TempDir(), "profile"), configs.BrowserLifecycle{IdleTimeout: 500 * time.Millisecond})
	defer m.Shutdown(context.Background())

	leaseAt(t, m, url).Release()

	require.Eventually(t, func() bool { return !m.Running() }, 15*time.Second, 100*time.Millisecond,
		"the browser was still up well past its idle timeout")

	lease := leaseAt(t, m, url)
	defer lease.Release()
	assert.True(t, m.Running(), "the next lease must relaunch")
}

// TestManagerProfileSurvivesShutdown is the persistence claim at the manager
// level: a second manager over the same directory sees the first one's
// localStorage.
func TestManagerProfileSurvivesShutdown(t *testing.T) {
	url := managerTestServer(t)
	dir := filepath.Join(t.TempDir(), "profile")
	want := fmt.Sprintf("b1-%d", time.Now().UnixNano())

	first := newTestManager(t, dir, configs.BrowserLifecycle{})
	lease := leaseAt(t, first, url)
	lease.Page.MustEval(`(v) => localStorage.setItem('__mgr_b1', v)`, want)
	lease.Release()
	first.Shutdown(context.Background())

	second := newTestManager(t, dir, configs.BrowserLifecycle{})
	defer second.Shutdown(context.Background())

	lease2 := leaseAt(t, second, url)
	defer lease2.Release()
	assert.Equal(t, want, lease2.Page.MustEval(`() => localStorage.getItem('__mgr_b1') || ''`).Str())
}

// TestManagerExportsCookies: the session file is a backup that has to keep up
// with the live jar, otherwise every relaunch replays rotated cookies.
func TestManagerExportsCookies(t *testing.T) {
	url := managerTestServer(t)
	dir := filepath.Join(t.TempDir(), "profile")
	sessionPath := filepath.Join(t.TempDir(), "cookies.json")
	session := cookies.NewLoadCookie(sessionPath)

	requireBrowser(t)
	m := NewManager(ManagerConfig{
		Headless:   true,
		Options:    []Option{WithFingerprintSeed(probeSeed)},
		ProfileDir: dir,
		Session:    session,
		Lifecycle:  configs.BrowserLifecycle{},
	})
	defer m.Shutdown(context.Background())

	leaseAt(t, m, url).Release()

	savedAt := session.LoadSavedAt()
	require.False(t, savedAt.IsZero(), "releasing a lease must export the cookie jar")

	raw, err := session.LoadCookies()
	require.NoError(t, err)
	var jar []map[string]any
	require.NoError(t, json.Unmarshal(raw, &jar))
	assert.NotEmpty(t, jar, "exported an empty jar")

	// The marker must match the file exactly, or the next launch would decide
	// the file is newer and reseed from it forever.
	marker, ok := readSeedMarker(dir)
	require.True(t, ok, "export must record what the backup now holds")
	assert.True(t, marker.SeededFrom.Equal(savedAt), "marker %s != file %s", marker.SeededFrom, savedAt)

	seed, reason := seedPolicy(marker, true, session.LoadSavedAt(), true)
	assert.False(t, seed, "steady state must not reseed: %s", reason)

	t.Run("saved_at advances after another action", func(t *testing.T) {
		time.Sleep(1100 * time.Millisecond) // RFC3339 has second precision
		leaseAt(t, m, url).Release()
		assert.True(t, session.LoadSavedAt().After(savedAt), "saved_at did not advance")
	})
}

// TestManagerReset is "reset login": the profile goes, the browser goes, and the
// next call starts clean.
func TestManagerReset(t *testing.T) {
	url := managerTestServer(t)
	dir := filepath.Join(t.TempDir(), "profile")
	m := newTestManager(t, dir, configs.BrowserLifecycle{})
	defer m.Shutdown(context.Background())

	lease := leaseAt(t, m, url)
	lease.Page.MustEval(`() => localStorage.setItem('__mgr_b1', 'old')`)
	lease.Release()

	require.NoError(t, m.Reset(context.Background()))
	assert.False(t, m.Running())
	_, err := os.Stat(dir)
	assert.True(t, os.IsNotExist(err), "the profile dir survived Reset")

	lease2 := leaseAt(t, m, url)
	defer lease2.Release()
	assert.Equal(t, "", lease2.Page.MustEval(`() => localStorage.getItem('__mgr_b1') || ''`).Str(),
		"reset login left the old local state behind")
}

// TestManagerShutdownRefusesLeases: after shutdown the manager must not quietly
// launch another browser.
func TestManagerShutdownRefusesLeases(t *testing.T) {
	m := newTestManager(t, filepath.Join(t.TempDir(), "profile"), configs.BrowserLifecycle{})
	m.Shutdown(context.Background())

	_, err := m.Lease(context.Background())
	assert.ErrorIs(t, err, ErrManagerClosed)
}

// TestManagerRefusesLockedProfile: two servers on one profile corrupt it, so a
// lock held by a live process on this host is an error, not something to steal.
func TestManagerRefusesLockedProfile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "profile")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	host, err := os.Hostname()
	require.NoError(t, err)
	require.NoError(t, os.Symlink(fmt.Sprintf("%s-%d", host, os.Getpid()), filepath.Join(dir, "SingletonLock")))

	m := newTestManager(t, dir, configs.BrowserLifecycle{})
	_, err = m.Lease(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), fmt.Sprintf("pid %d", os.Getpid()))
}
