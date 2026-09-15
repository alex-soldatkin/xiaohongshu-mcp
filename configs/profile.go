package configs

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/xpzouying/xiaohongshu-mcp/cookies"
)

// ProfileDir resolves the Chrome user-data directory holding the persistent
// profile (issue #6).
//
// Priority: XHS_PROFILE_DIR, else a "profile" sibling of the session file. The
// sibling convention is the same one pacing.DefaultStatePath uses, so in Docker
// the profile lands in the already-mounted /app/data next to cookies.json
// rather than somewhere the volume does not cover.
//
// Note the legacy /tmp/cookies.json fallback in cookies.GetCookiesFilePath
// makes this /tmp/profile for old installs, which the OS may clean. The
// resolved path is logged at startup so that is visible rather than mysterious.
func ProfileDir() string {
	if dir := strings.TrimSpace(os.Getenv("XHS_PROFILE_DIR")); dir != "" {
		return dir
	}
	return filepath.Join(filepath.Dir(cookies.GetCookiesFilePath()), "profile")
}

// BrowserLifecycle bounds how long one browser process is kept alive.
//
// A zero value disables the corresponding policy: IdleTimeout 0 keeps the
// browser up until shutdown, MaxPages 0 never recycles on page count, MaxAge 0
// never recycles on age.
type BrowserLifecycle struct {
	IdleTimeout time.Duration
	MaxPages    int
	MaxAge      time.Duration
}

// DefaultBrowserLifecycle returns the defaults agreed in issue #6: close after
// ten idle minutes, recycle after 200 pages or a day of uptime. The recycle
// limits exist to bound Chrome's memory growth in a long-lived process.
func DefaultBrowserLifecycle() BrowserLifecycle {
	return BrowserLifecycle{
		IdleTimeout: 10 * time.Minute,
		MaxPages:    200,
		MaxAge:      24 * time.Hour,
	}
}

// BrowserLifecycleFromEnv applies XHS_BROWSER_IDLE, XHS_BROWSER_MAX_PAGES and
// XHS_BROWSER_MAX_AGE on top of the defaults. Zero is meaningful (it disables a
// policy), so parsing distinguishes "unset" from "set to 0".
func BrowserLifecycleFromEnv() BrowserLifecycle {
	lc := DefaultBrowserLifecycle()

	if d, ok := envDuration("XHS_BROWSER_IDLE"); ok {
		lc.IdleTimeout = d
	}
	if v, ok := envInt("XHS_BROWSER_MAX_PAGES"); ok {
		lc.MaxPages = v
	}
	if d, ok := envDuration("XHS_BROWSER_MAX_AGE"); ok {
		lc.MaxAge = d
	}
	return lc
}

// envInt reads a non-negative integer. The second return distinguishes "unset"
// from "explicitly zero", which matters because zero disables a policy.
func envInt(key string) (int, bool) {
	raw := os.Getenv(key)
	if raw == "" {
		return 0, false
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 0 {
		logrus.Warnf("invalid %s=%q, ignored", key, raw)
		return 0, false
	}
	return v, true
}

// envDuration accepts a Go duration ("10m") or a bare number of seconds.
func envDuration(key string) (time.Duration, bool) {
	raw := os.Getenv(key)
	if raw == "" {
		return 0, false
	}
	if d, err := time.ParseDuration(raw); err == nil && d >= 0 {
		return d, true
	}
	if secs, err := strconv.Atoi(raw); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second, true
	}
	logrus.Warnf("invalid %s=%q, ignored", key, raw)
	return 0, false
}
