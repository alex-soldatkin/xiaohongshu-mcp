package pacing

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/xpzouying/xiaohongshu-mcp/cookies"
	"github.com/xpzouying/xiaohongshu-mcp/humanize"
)

// Config holds the pacing budgets. Every field is overridable from the
// environment; a zero value disables that particular limit.
type Config struct {
	// MinGap / MaxGap bound the lognormal pause inserted before every action.
	// MinGap == 0 disables the pause entirely.
	MinGap time.Duration
	MaxGap time.Duration

	MaxReadsPerHour  int
	MaxWritesPerHour int
	MaxWritesPerDay  int
	MaxPublishPerDay int

	// RiskCooldown is how long Cooldown() silences the gate by default.
	RiskCooldown time.Duration

	// MaxWait bounds how long Acquire waits for the single-flight slot before
	// giving up with ErrRateLimited. Queueing beyond a few seconds looks like a
	// hang to an MCP client, so we fail fast and hand back a retry_after.
	MaxWait time.Duration

	// StatePath is the file the counters are persisted to. Empty disables
	// persistence (used by tests).
	StatePath string
}

// window lengths used by the budget arithmetic.
const (
	hourWindow = time.Hour
	dayWindow  = 24 * time.Hour
)

// DefaultConfig returns the conservative defaults agreed in issue #5.
func DefaultConfig() Config {
	return Config{
		MinGap:           3 * time.Second,
		MaxGap:           8 * time.Second,
		MaxReadsPerHour:  120,
		MaxWritesPerHour: 20,
		MaxWritesPerDay:  100,
		MaxPublishPerDay: 5,
		RiskCooldown:     30 * time.Minute,
		MaxWait:          30 * time.Second,
		StatePath:        DefaultStatePath(),
	}
}

// DefaultStatePath puts the counter file next to the session file, so it lands
// inside whatever volume already holds cookies.json.
func DefaultStatePath() string {
	return filepath.Join(filepath.Dir(cookies.GetCookiesFilePath()), "pacing_state.json")
}

// ConfigFromEnv starts from DefaultConfig and applies the XHS_* overrides.
func ConfigFromEnv() Config {
	cfg := DefaultConfig()

	if ms, ok := envInt("XHS_MIN_GAP_MS"); ok {
		cfg.MinGap = time.Duration(ms) * time.Millisecond
		// Keep the shape of the default 3-8s window when the floor is moved.
		cfg.MaxGap = cfg.MinGap * 8 / 3
	}
	if v, ok := envInt("XHS_MAX_READS_PER_HOUR"); ok {
		cfg.MaxReadsPerHour = v
	}
	if v, ok := envInt("XHS_MAX_WRITES_PER_HOUR"); ok {
		cfg.MaxWritesPerHour = v
	}
	if v, ok := envInt("XHS_MAX_WRITES_PER_DAY"); ok {
		cfg.MaxWritesPerDay = v
	}
	if v, ok := envInt("XHS_MAX_PUBLISH_PER_DAY"); ok {
		cfg.MaxPublishPerDay = v
	}
	if d, ok := envDuration("XHS_RISK_COOLDOWN"); ok {
		cfg.RiskCooldown = d
	}
	if d, ok := envDuration("XHS_GATE_MAX_WAIT"); ok {
		cfg.MaxWait = d
	}
	if p := os.Getenv("XHS_PACING_STATE"); p != "" {
		cfg.StatePath = p
	}

	return cfg
}

// envInt reads a non-negative integer. Zero is meaningful (it disables a
// limit), so the second return distinguishes "unset" from "set to 0".
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

// envDuration accepts a Go duration ("30m") or a bare number of seconds.
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

// gapDistribution builds the "pause between two actions" distribution.
//
// The plan for issue #5 called for a BetweenActions entry in
// humanize/provider.go; that package is being rewritten under a separate issue,
// so the distribution lives here for now and only reuses humanize.LogNormal.
// Folding it back into the shared timing profile is deferred follow-up work.
func gapSampler(min, max time.Duration) func() time.Duration {
	if min <= 0 {
		// Zero disables the pause. The zero LogNormal is not "no delay", so
		// short-circuit rather than sampling it.
		return func() time.Duration { return 0 }
	}
	if max < min {
		max = min
	}
	// Median sits halfway up the window so the bulk of the mass lands inside
	// [min, max] rather than piling up on the clamp.
	median := float64(min+max) / 2 / float64(time.Second)
	dist := humanize.LogNormal{
		Mu:    math.Log(median),
		Sigma: 0.35,
		Min:   min,
		Max:   max,
	}
	return dist.Sample
}
