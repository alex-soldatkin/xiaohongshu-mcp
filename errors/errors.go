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
