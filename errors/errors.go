package errors

import (
	"errors"
	"fmt"
	"time"
)

var ErrNoFeeds = errors.New("没有捕获到 feeds 数据")
var ErrNoFeedDetail = errors.New("没有捕获到 feed 详情数据")

// ErrRateLimited is returned by the local pacing gate when an action cannot be
// performed right now: a per-class budget is exhausted, a risk-control cooldown
// is in effect, or another action is holding the single-flight slot.
//
// The gate returns this instead of blocking indefinitely: MCP clients have their
// own timeouts and a long silent wait is indistinguishable from a hang.
type ErrRateLimited struct {
	// Class is the action class that was refused ("read", "write", "publish").
	Class string
	// Reason is a short human-readable explanation of which limit was hit.
	Reason string
	// RetryAfter is how long the caller should wait before trying again.
	RetryAfter time.Duration
}

func (e *ErrRateLimited) Error() string {
	return fmt.Sprintf("rate limited: %s (class=%s), retry after %s", e.Reason, e.Class, e.RetryAfter.Round(time.Second))
}

// AsRateLimited reports whether err is (or wraps) an *ErrRateLimited.
func AsRateLimited(err error) (*ErrRateLimited, bool) {
	var rl *ErrRateLimited
	if errors.As(err, &rl) {
		return rl, true
	}
	return nil, false
}

// ErrRiskControl is returned when the site appears to have challenged the
// session: a captcha or security-verification interstitial, or a login modal on
// a run that had a cookie session.
//
// It exists so a flagged account is distinguishable from a slow network or
// changed markup. Everything else in this codebase reports both as a timeout,
// which is exactly the case where an agent would keep hammering.
//
// Solving the challenge is out of scope: headful runs leave the window open for
// a human, headless runs back off.
type ErrRiskControl struct {
	// Kind is which detector fired ("captcha_url", "captcha_dom",
	// "login_required", "fake").
	Kind string
	// URL is the page the challenge was seen on.
	URL string
	// Detail names the markers that matched and what the operator should do.
	Detail string
	// Screenshot is the path to the saved page image, empty when it failed.
	Screenshot string
	// Cooldown is how long the pacing gate was silenced. Zero means this was
	// the first signal and no cooldown was entered — the detectors are guesses
	// until a real challenge confirms them, so one signal only reports.
	Cooldown time.Duration
}

func (e *ErrRiskControl) Error() string {
	msg := fmt.Sprintf("risk control detected (kind=%s) at %s", e.Kind, e.URL)
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	if e.Screenshot != "" {
		msg += "; screenshot: " + e.Screenshot
	}
	return msg
}

// AsRiskControl reports whether err is (or wraps) an *ErrRiskControl.
func AsRiskControl(err error) (*ErrRiskControl, bool) {
	var rc *ErrRiskControl
	if errors.As(err, &rc) {
		return rc, true
	}
	return nil, false
}
