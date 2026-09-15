package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/launcher/flags"
	"github.com/go-rod/rod/lib/proto"
	"github.com/sirupsen/logrus"
	"github.com/xpzouying/headless_browser"
	"github.com/xpzouying/xiaohongshu-mcp/configs"
	"github.com/xpzouying/xiaohongshu-mcp/cookies"
)

// Bounds on the CDP calls the manager makes on its own behalf. All of them can
// hang against a wedged browser, and none of them may wedge the manager.
const (
	healthPingTimeout = 3 * time.Second
	exportTimeout     = 3 * time.Second
	closeTimeout      = 10 * time.Second
	killTimeout       = 5 * time.Second
	leaseWaitTick     = 100 * time.Millisecond
)

// ErrManagerClosed is returned by Lease after Shutdown.
var ErrManagerClosed = errors.New("browser manager is shut down")

// ManagerConfig configures a Manager. Nothing here is read from the
// environment; the entry point resolves all of it through configs.
type ManagerConfig struct {
	// Headless runs the browser without a window.
	Headless bool
	// Options are the per-launch browser options: fingerprint seed, proxy,
	// timezone. The profile dir and the cookie jar are added by the manager.
	Options []Option
	// ProfileDir is the persistent Chrome profile. Empty means rod's temp
	// profile with no persistence, which is what tests want.
	ProfileDir string
	// Session is the cookie backup: the seed source at launch and the export
	// target on every lease release. nil disables both.
	Session cookies.Cookier
	// Lifecycle bounds how long one browser process lives.
	Lifecycle configs.BrowserLifecycle
}

// Manager owns a single long-lived browser and hands out pages.
//
// The point of issue #6 is that a browser whose profile is wiped between every
// action is incoherent: the account presents a stable device identity with no
// localStorage, no IndexedDB and no cache, over and over. Keeping one browser
// alive with one profile fixes both halves, and makes the second call cheap.
//
// Everything below runs under mu. The lock is held across CDP calls, which is
// why every one of them is bounded.
type Manager struct {
	cfg  ManagerConfig
	seed int // fingerprint seed, derived once from Options for the marker

	mu         sync.Mutex
	b          *headless_browser.Browser
	launcher   *launcher.Launcher // captured at launch; the only way to kill a browser with a dead connection
	gen        uint64             // bumped on every launch and teardown; leases carry the gen they belong to
	launchedAt time.Time
	pages      int // pages opened since this browser launched
	leases     int // pages currently handed out
	idle       *time.Timer
	closed     bool

	now func() time.Time // injectable for tests
}

// Lease is a page checked out of the manager. Release is idempotent.
type Lease struct {
	Page *rod.Page

	m    *Manager
	gen  uint64
	once sync.Once
}

// NewManager builds a manager. It performs no I/O and launches nothing: the
// browser starts on the first Lease, so a server with no traffic runs no
// browser.
func NewManager(cfg ManagerConfig) *Manager {
	return &Manager{
		cfg:  cfg,
		seed: newConfig(cfg.Headless, cfg.Options...).fingerprintSeed,
		now:  time.Now,
	}
}

// Lease returns a page, launching or relaunching the browser if needed.
func (m *Manager) Lease(ctx context.Context) (*Lease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return nil, ErrManagerClosed
	}
	m.stopIdleLocked()

	// A recycle that came due while the browser sat idle is taken now, when
	// nothing is in flight. One that comes due mid-session is deferred to the
	// Release that brings the lease count back to zero.
	if m.b != nil && m.leases == 0 && m.recycleDueLocked() {
		logrus.Infof("browser: recycling after %d pages / %s uptime", m.pages, m.now().Sub(m.launchedAt).Truncate(time.Second))
		m.teardownLocked(true)
	}

	if m.b != nil && !m.healthyLocked() {
		logrus.Warnf("browser: health ping failed, discarding the browser")
		m.killLocked()
	}

	if m.b == nil {
		if err := m.launchLocked(); err != nil {
			return nil, err
		}
	}

	page, err := m.newPageLocked()
	if err != nil {
		// A browser that cannot open a page is of no use to the next caller.
		m.killLocked()
		return nil, err
	}

	m.pages++
	m.leases++
	return &Lease{Page: page, m: m, gen: m.gen}, nil
}

// Release returns the page. Safe to call more than once.
func (l *Lease) Release() {
	l.once.Do(func() { l.m.release(l) })
}

func (m *Manager) release(l *Lease) {
	closePage(l.Page)

	m.mu.Lock()
	defer m.mu.Unlock()

	// The browser this page belonged to is already gone (killed, recycled or
	// shut down). Its lease count went with it; touching the current one would
	// corrupt the accounting.
	if l.gen != m.gen || m.b == nil {
		return
	}
	m.leases--

	// Export on every release, not only after write-class actions: reads
	// rotate acw_tc and websectiga too, and a crash after a read-only session
	// would otherwise leave the backup stale.
	m.exportLocked()

	if m.leases == 0 && m.recycleDueLocked() {
		logrus.Infof("browser: recycling after %d pages / %s uptime", m.pages, m.now().Sub(m.launchedAt).Truncate(time.Second))
		m.teardownLocked(false) // just exported
		return
	}
	m.armIdleLocked()
}

// Shutdown closes the browser, waiting for in-flight leases until ctx expires
// and force-closing after that. The manager refuses further leases afterwards.
func (m *Manager) Shutdown(ctx context.Context) {
	m.mu.Lock()
	m.closed = true
	m.stopIdleLocked()
	m.mu.Unlock()

	m.waitLeases(ctx)
	defer m.mu.Unlock()

	if m.b == nil {
		return
	}
	if m.leases > 0 {
		logrus.Warnf("browser: %d lease(s) still in flight at shutdown, closing anyway", m.leases)
	}
	m.teardownLocked(true)
}

// Reset closes the browser and deletes the profile, for "reset login".
//
// It exports nothing: the cookies being discarded are the point. It does not
// mark the manager closed either, so the next call launches a fresh browser
// into a fresh profile.
//
// Exclusivity comes from the lease count rather than from the pacing gate:
// Gate.Acquire records a budget event and can refuse with ErrRateLimited, and a
// reset that can be rate-limited is not a reset.
func (m *Manager) Reset(ctx context.Context) error {
	m.waitLeases(ctx)
	defer m.mu.Unlock()

	if m.b != nil {
		m.teardownLocked(false)
	}
	if m.cfg.ProfileDir == "" {
		return nil
	}

	logrus.Infof("browser: removing profile %s", m.cfg.ProfileDir)
	if err := os.RemoveAll(m.cfg.ProfileDir); err != nil {
		return fmt.Errorf("remove profile dir: %w", err)
	}
	return nil
}

// ExportCookies writes the live cookie jar back to the session file.
func (m *Manager) ExportCookies() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.exportLocked()
}

// Running reports whether a browser process is currently up. For tests and
// diagnostics only.
func (m *Manager) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.b != nil
}

// waitLeases blocks until no page is checked out or ctx expires, and returns
// with mu held either way.
func (m *Manager) waitLeases(ctx context.Context) {
	for {
		m.mu.Lock()
		if m.leases == 0 || m.b == nil {
			return
		}
		m.mu.Unlock()

		select {
		case <-ctx.Done():
			m.mu.Lock()
			return
		case <-time.After(leaseWaitTick):
		}
	}
}

// ---------------------------------------------------------------------------
// launch
// ---------------------------------------------------------------------------

func (m *Manager) launchLocked() error {
	if dir := m.cfg.ProfileDir; dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create profile dir %s: %w", dir, err)
		}
		if err := clearStaleSingleton(dir); err != nil {
			return err
		}
	}

	raw, savedAt, doSeed := m.seedDecisionLocked()

	b, err := m.startLocked(raw, doSeed)
	if err != nil {
		// A launch can fail on debris the first pass did not catch: a lock
		// written between the check and the launch, a stale
		// DevToolsActivePort. Clear and try once more before giving up — the
		// server stays up either way.
		logrus.Warnf("browser: launch failed (%v), retrying once after clearing the profile lock", err)
		if m.cfg.ProfileDir != "" {
			if clearErr := clearStaleSingleton(m.cfg.ProfileDir); clearErr != nil {
				return fmt.Errorf("%w (and %v)", err, clearErr)
			}
		}
		if b, err = m.startLocked(raw, doSeed); err != nil {
			return err
		}
	}

	m.b = b
	m.launchedAt = m.now()
	m.pages = 0

	if doSeed && m.cfg.ProfileDir != "" {
		// Record what we seeded from, so the next launch recognises the steady
		// state. A zero savedAt means a v1 or timestamp-less file; stamping it
		// with now() still gives the next launch something to compare against.
		if savedAt.IsZero() {
			savedAt = m.now()
		}
		if err := writeSeedMarker(m.cfg.ProfileDir, seedMarker{SeededFrom: savedAt, Seed: m.seed}); err != nil {
			logrus.Warnf("browser: cannot write the seed marker, the next launch will reseed: %v", err)
		}
	}
	return nil
}

// startLocked launches one browser, capturing the launcher on the way through.
func (m *Manager) startLocked(cookiesJSON string, doSeed bool) (*headless_browser.Browser, error) {
	var captured *launcher.Launcher

	opts := make([]Option, 0, len(m.cfg.Options)+3)
	opts = append(opts, m.cfg.Options...)
	opts = append(opts, WithLauncherHook(func(l *launcher.Launcher) { captured = l }))
	if m.cfg.ProfileDir != "" {
		opts = append(opts, WithUserDataDir(m.cfg.ProfileDir))
	}
	if doSeed {
		opts = append(opts, WithCookiesJSON(cookiesJSON))
	}

	b, err := launch(newConfig(m.cfg.Headless, opts...))
	if err != nil {
		m.launcher = captured // may still be needed to clean up a half-started process
		m.killLocked()
		return nil, err
	}
	m.launcher = captured
	return b, nil
}

// seedDecisionLocked applies the seeding state machine from issue #6.
//
// The profile is canonical and cookies.json is a backup. Replaying the backup
// on every launch is the stale-cookie bug: it overwrites whatever the server
// rotated since the file was written.
func (m *Manager) seedDecisionLocked() (raw string, savedAt time.Time, doSeed bool) {
	if m.cfg.Session == nil {
		return "", time.Time{}, false
	}

	data, err := m.cfg.Session.LoadCookies()
	if err != nil {
		// A missing file is the normal first run, not an error.
		logrus.Debugf("browser: no cookie backup to seed from: %v", err)
	}
	raw = string(data)
	hasCookies := hasCookieContent(raw)
	savedAt = m.cfg.Session.LoadSavedAt()

	// Without a profile dir there is nothing to remember, so the file is the
	// only source of session state and always applies.
	if m.cfg.ProfileDir == "" {
		return raw, savedAt, hasCookies
	}

	marker, hasMarker := readSeedMarker(m.cfg.ProfileDir)
	doSeed, reason := seedPolicy(marker, hasMarker, savedAt, hasCookies)
	logrus.Infof("browser: cookie seeding %s (%s)", map[bool]string{true: "ON", false: "off"}[doSeed], reason)

	if hasMarker && marker.Seed != 0 && m.seed != 0 && marker.Seed != m.seed {
		logrus.Warnf("browser: profile %s was minted under fingerprint seed %d but this process uses %d; "+
			"localStorage and window geometry belong to the old fingerprint. Delete the profile to start clean.",
			m.cfg.ProfileDir, marker.Seed, m.seed)
	}
	return raw, savedAt, doSeed
}

// hasCookieContent tells a jar with cookies in it from an empty or absent one.
// SaveCookies writes "[]" when there is nothing to save, and seeding from that
// is not the same as not seeding.
func hasCookieContent(raw string) bool {
	s := strings.TrimSpace(raw)
	return s != "" && s != "[]" && s != "null"
}

func (m *Manager) newPageLocked() (page *rod.Page, err error) {
	defer func() {
		if r := recover(); r != nil {
			page, err = nil, fmt.Errorf("open page failed: %v", r)
		}
	}()
	return m.b.NewPage(), nil
}

// ---------------------------------------------------------------------------
// health, export, teardown
// ---------------------------------------------------------------------------

// healthyLocked pings the browser. A browser can be dead (process gone) or
// wedged (process alive, CDP unresponsive); the timeout catches both.
func (m *Manager) healthyLocked() bool {
	done := make(chan bool, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- false
			}
		}()
		_, err := proto.BrowserGetVersion{}.Call(m.b.Rod())
		done <- err == nil
	}()

	select {
	case ok := <-done:
		return ok
	case <-time.After(healthPingTimeout):
		return false
	}
}

// exportLocked writes the live cookie jar back to the session file and updates
// the seed marker, keeping the backup current.
func (m *Manager) exportLocked() error {
	if m.b == nil || m.cfg.Session == nil {
		return nil
	}

	cks, err := getCookies(m.b)
	if err != nil {
		logrus.Warnf("browser: cannot read cookies for export: %v", err)
		return err
	}
	if len(cks) == 0 {
		// Never overwrite a good backup with an empty jar; a browser that
		// reports no cookies is more likely broken than logged out.
		return nil
	}

	data, err := json.Marshal(cks)
	if err != nil {
		return err
	}
	if err := m.cfg.Session.SaveCookies(data); err != nil {
		logrus.Warnf("browser: cannot save the cookie backup: %v", err)
		return err
	}

	if m.cfg.ProfileDir == "" {
		return nil
	}
	// Read saved_at back rather than reusing now(): the file stores RFC3339
	// with second precision, and the marker has to compare Equal to it.
	//
	// Order matters. Dying between the write and the marker leaves the file
	// newer than the marker, which costs one harmless reseed; the reverse
	// would mark a file as synced that never was.
	if err := writeSeedMarker(m.cfg.ProfileDir, seedMarker{
		SeededFrom: m.cfg.Session.LoadSavedAt(),
		Seed:       m.seed,
	}); err != nil {
		logrus.Warnf("browser: cannot update the seed marker: %v", err)
	}
	return nil
}

// teardownLocked closes the browser cleanly, optionally exporting first.
func (m *Manager) teardownLocked(export bool) {
	if m.b == nil {
		return
	}
	if export {
		m.exportLocked()
	}

	b, l := m.b, m.launcher
	m.b, m.launcher = nil, nil
	m.gen++
	m.pages, m.leases = 0, 0

	// Close() waits for the process to exit, which is what flushes the profile
	// to disk — the whole point of the persistent profile. It goes through
	// MustClose, so a connection that died in the meantime panics; fall back to
	// killing the process in that case.
	bounded(closeTimeout, "browser close", func() {
		defer func() {
			if r := recover(); r != nil {
				logrus.Warnf("browser: clean close failed (%v), killing the process", r)
				killLauncher(l)
			}
		}()
		b.Close()
	})
}

// killLocked drops a browser that cannot be closed cleanly.
//
// It must never call the library's Close(): that goes through
// rod.Browser.MustClose, which panics once the CDP connection is gone — which
// is precisely the situation this handles.
func (m *Manager) killLocked() {
	l := m.launcher
	m.b, m.launcher = nil, nil
	m.gen++
	m.pages, m.leases = 0, 0

	bounded(killTimeout, "browser kill", func() { killLauncher(l) })
}

// killLauncher kills the process and runs rod's cleanup with the user-data-dir
// flag removed, so the profile survives (os.RemoveAll("") is a no-op).
func killLauncher(l *launcher.Launcher) {
	if l == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			logrus.Warnf("browser: kill failed: %v", r)
		}
	}()
	l.Kill()
	l.Delete(flags.UserDataDir)
	l.Cleanup()
}

// bounded runs fn and gives up waiting after d. A zombie Chrome must not wedge
// the manager; the goroutine is left to finish on its own.
func bounded(d time.Duration, what string, fn func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		logrus.Warnf("browser: %s did not finish within %s, moving on", what, d)
	}
}

// getCookies reads the browser cookie jar under a timeout.
func getCookies(b *headless_browser.Browser) (cks []*proto.NetworkCookie, err error) {
	defer func() {
		if r := recover(); r != nil {
			cks, err = nil, fmt.Errorf("get cookies failed: %v", r)
		}
	}()
	return b.Rod().Timeout(exportTimeout).GetCookies()
}

func closePage(page *rod.Page) {
	if page == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			logrus.Debugf("browser: closing a page failed: %v", r)
		}
	}()
	_ = page.Close()
}

// ---------------------------------------------------------------------------
// recycle and idle
// ---------------------------------------------------------------------------

func (m *Manager) recycleDueLocked() bool {
	lc := m.cfg.Lifecycle
	if lc.MaxPages > 0 && m.pages >= lc.MaxPages {
		return true
	}
	return lc.MaxAge > 0 && m.now().Sub(m.launchedAt) >= lc.MaxAge
}

func (m *Manager) armIdleLocked() {
	d := m.cfg.Lifecycle.IdleTimeout
	if d <= 0 {
		return
	}
	if m.idle == nil {
		m.idle = time.AfterFunc(d, m.onIdle)
		return
	}
	m.idle.Reset(d)
}

func (m *Manager) stopIdleLocked() {
	if m.idle != nil {
		m.idle.Stop()
	}
}

// onIdle closes a browser nobody has used for IdleTimeout. A Lease racing with
// it simply blocks on mu and relaunches.
func (m *Manager) onIdle() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed || m.b == nil || m.leases > 0 {
		return
	}
	logrus.Infof("browser: idle for %s, closing", m.cfg.Lifecycle.IdleTimeout)
	m.teardownLocked(true)
}
