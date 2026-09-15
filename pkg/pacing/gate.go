// Package pacing throttles account activity.
//
// It provides a single-flight gate: at most one browser action runs at a time,
// consecutive actions are separated by a lognormal pause, and each action class
// is capped by sliding-window budgets that survive a restart. Volume and cadence
// are what get accounts restricted, so this sits in front of every service
// method rather than being opt-in per call site.
package pacing

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	myerrors "github.com/xpzouying/xiaohongshu-mcp/errors"
)

// Class is the kind of account activity an action represents. Budgets are
// tracked per class.
type Class string

const (
	// ClassRead covers browsing: feeds, search, detail pages, profiles,
	// notification listing, login-status checks.
	ClassRead Class = "read"
	// ClassWrite covers anything that mutates someone else's view of the
	// account: likes, favourites, comments, replies, follows.
	ClassWrite Class = "write"
	// ClassPublish covers creating a note. The scarcest budget by far.
	ClassPublish Class = "publish"
)

// Gate serialises and paces actions. The zero value is not usable; use New.
type Gate struct {
	cfg Config

	// sem is the single-flight slot. Capacity one, ctx-aware via select.
	sem chan struct{}

	mu            sync.Mutex
	events        map[Class][]time.Time
	lastActionEnd time.Time
	cooldownUntil time.Time

	// now and sleep are injectable so the window arithmetic is testable
	// without real time passing.
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
	// sampleGap returns the pause to insert before the next action.
	sampleGap func() time.Duration
}

// Option customises a Gate, mainly for tests.
type Option func(*Gate)

// WithClock replaces the time source and the sleep primitive.
func WithClock(now func() time.Time, sleep func(ctx context.Context, d time.Duration) error) Option {
	return func(g *Gate) {
		if now != nil {
			g.now = now
		}
		if sleep != nil {
			g.sleep = sleep
		}
	}
}

// WithGapSampler replaces the between-actions distribution.
func WithGapSampler(f func() time.Duration) Option {
	return func(g *Gate) {
		if f != nil {
			g.sampleGap = f
		}
	}
}

// New builds a Gate and restores any persisted counters.
func New(cfg Config, opts ...Option) *Gate {
	g := &Gate{
		cfg:       cfg,
		sem:       make(chan struct{}, 1),
		events:    map[Class][]time.Time{},
		now:       time.Now,
		sleep:     sleepCtx,
		sampleGap: gapSampler(cfg.MinGap, cfg.MaxGap),
	}
	for _, opt := range opts {
		opt(g)
	}

	st := loadState(cfg.StatePath)
	for class, ms := range st.Events {
		g.events[class] = fromMillis(ms)
	}
	if st.CooldownUntil > 0 {
		g.cooldownUntil = time.UnixMilli(st.CooldownUntil)
	}
	g.prune(g.now())

	return g
}

// Acquire takes the single-flight slot, waits out the inter-action gap and then
// checks the budgets for class. On success it returns a release function that
// must be called when the action finishes.
//
// It returns *errors.ErrRateLimited when a budget is exhausted, when a
// risk-control cooldown is active, or when the slot stays busy for longer than
// Config.MaxWait. It never blocks silently for minutes.
func (g *Gate) Acquire(ctx context.Context, class Class) (func(), error) {
	// Fail fast on an active cooldown: no point queueing for the slot.
	if err := g.checkCooldown(class); err != nil {
		return nil, err
	}

	if err := g.takeSlot(ctx, class); err != nil {
		return nil, err
	}
	release := func() {
		g.mu.Lock()
		g.lastActionEnd = g.now()
		g.mu.Unlock()
		<-g.sem
	}

	// A cooldown may have been tripped while we were queueing.
	if err := g.checkCooldown(class); err != nil {
		release()
		return nil, err
	}

	if err := g.waitGap(ctx); err != nil {
		release()
		return nil, err
	}

	if err := g.reserve(class); err != nil {
		release()
		return nil, err
	}

	return release, nil
}

// Cooldown silences the gate for d (or the configured risk cooldown when d is
// not positive). Issue #11 calls this on risk-control detection.
func (g *Gate) Cooldown(d time.Duration) {
	if d <= 0 {
		d = g.cfg.RiskCooldown
	}
	if d <= 0 {
		return
	}

	g.mu.Lock()
	until := g.now().Add(d)
	if until.After(g.cooldownUntil) {
		g.cooldownUntil = until
	}
	st := g.snapshotLocked()
	g.mu.Unlock()

	logrus.Warnf("pacing: cooldown until %s", until.Format(time.RFC3339))
	g.persist(st)
}

// CooldownUntil reports the end of the current cooldown, zero when none.
func (g *Gate) CooldownUntil() time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.cooldownUntil
}

// Count returns how many actions of class fall inside window, as of now.
func (g *Gate) Count(class Class, window time.Duration) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(inWindow(g.events[class], g.now(), window))
}

func (g *Gate) takeSlot(ctx context.Context, class Class) error {
	var wait <-chan time.Time
	if g.cfg.MaxWait > 0 {
		t := time.NewTimer(g.cfg.MaxWait)
		defer t.Stop()
		wait = t.C
	}

	select {
	case g.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-wait:
		return &myerrors.ErrRateLimited{
			Class:      string(class),
			Reason:     "another action is already running",
			RetryAfter: g.cfg.MaxWait,
		}
	}
}

func (g *Gate) checkCooldown(class Class) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := g.now()
	if g.cooldownUntil.IsZero() || !now.Before(g.cooldownUntil) {
		return nil
	}
	return &myerrors.ErrRateLimited{
		Class:      string(class),
		Reason:     "risk-control cooldown in effect",
		RetryAfter: g.cooldownUntil.Sub(now),
	}
}

// waitGap sleeps the remainder of the sampled inter-action pause.
func (g *Gate) waitGap(ctx context.Context) error {
	gap := g.sampleGap()
	if gap <= 0 {
		return nil
	}

	g.mu.Lock()
	elapsed := time.Duration(0)
	if !g.lastActionEnd.IsZero() {
		elapsed = g.now().Sub(g.lastActionEnd)
	}
	g.mu.Unlock()

	remaining := gap - elapsed
	if remaining <= 0 {
		return nil
	}
	return g.sleep(ctx, remaining)
}

// reserve records one action of class, or reports which budget blocks it.
func (g *Gate) reserve(class Class) error {
	g.mu.Lock()

	now := g.now()
	if err := g.budgetErrorLocked(class, now); err != nil {
		g.mu.Unlock()
		return err
	}

	g.events[class] = append(g.events[class], now)
	g.prune(now)
	st := g.snapshotLocked()
	g.mu.Unlock()

	g.persist(st)
	return nil
}

// budgetErrorLocked returns the binding limit for class, if any. When several
// windows are over budget the one with the longest wait wins.
func (g *Gate) budgetErrorLocked(class Class, now time.Time) *myerrors.ErrRateLimited {
	type limit struct {
		max    int
		window time.Duration
		label  string
	}

	var limits []limit
	switch class {
	case ClassRead:
		limits = []limit{{g.cfg.MaxReadsPerHour, hourWindow, "reads"}}
	case ClassWrite:
		limits = []limit{
			{g.cfg.MaxWritesPerHour, hourWindow, "writes"},
			{g.cfg.MaxWritesPerDay, dayWindow, "writes"},
		}
	case ClassPublish:
		limits = []limit{{g.cfg.MaxPublishPerDay, dayWindow, "publishes"}}
	default:
		limits = nil
	}

	var worst *myerrors.ErrRateLimited
	for _, l := range limits {
		if l.max <= 0 { // zero disables this limit
			continue
		}
		events := inWindow(g.events[class], now, l.window)
		if len(events) < l.max {
			continue
		}
		// The budget frees up when the (len-max)-th oldest event ages out.
		retry := events[len(events)-l.max].Add(l.window).Sub(now)
		if retry < time.Second {
			retry = time.Second
		}
		if worst == nil || retry > worst.RetryAfter {
			worst = &myerrors.ErrRateLimited{
				Class:      string(class),
				Reason:     budgetReason(l.label, l.max, l.window),
				RetryAfter: retry,
			}
		}
	}
	return worst
}

func budgetReason(label string, max int, window time.Duration) string {
	unit := "hour"
	if window >= dayWindow {
		unit = "day"
	}
	return fmt.Sprintf("%s budget exhausted (%d/%s)", label, max, unit)
}

// prune drops events that can no longer affect any window. Caller holds mu.
func (g *Gate) prune(now time.Time) {
	cutoff := now.Add(-dayWindow)
	for class, events := range g.events {
		kept := events[:0:0]
		for _, e := range events {
			if e.After(cutoff) {
				kept = append(kept, e)
			}
		}
		if len(kept) == 0 {
			delete(g.events, class)
			continue
		}
		g.events[class] = kept
	}
}

// snapshotLocked copies the in-memory state for persistence. Caller holds mu.
func (g *Gate) snapshotLocked() persistedState {
	st := persistedState{Version: stateVersion, Events: map[Class][]int64{}}
	for class, events := range g.events {
		st.Events[class] = toMillis(events)
	}
	if !g.cooldownUntil.IsZero() {
		st.CooldownUntil = g.cooldownUntil.UnixMilli()
	}
	return st
}

// persist writes the counters out. A failure here is logged, not fatal: losing
// the file is bad for the budget but must not fail the user's action.
func (g *Gate) persist(st persistedState) {
	if err := saveState(g.cfg.StatePath, st); err != nil {
		logrus.Warnf("pacing: cannot persist counters to %s: %v", g.cfg.StatePath, err)
	}
}

// inWindow returns the suffix of events (ascending) that falls within window.
func inWindow(events []time.Time, now time.Time, window time.Duration) []time.Time {
	cutoff := now.Add(-window)
	for i, e := range events {
		if e.After(cutoff) {
			return events[i:]
		}
	}
	return nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
