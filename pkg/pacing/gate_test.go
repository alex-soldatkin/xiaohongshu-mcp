package pacing

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	myerrors "github.com/xpzouying/xiaohongshu-mcp/errors"
)

// fakeClock is the injectable clock: sleeping moves time forward instead of
// actually waiting, so the whole suite runs in milliseconds.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func (c *fakeClock) Sleep(_ context.Context, d time.Duration) error {
	c.Advance(d)
	return nil
}

// testGate builds a gate on a fake clock with no gap unless asked otherwise.
func testGate(t *testing.T, cfg Config, gap time.Duration) (*Gate, *fakeClock) {
	t.Helper()

	clock := newFakeClock()
	g := New(cfg,
		WithClock(clock.Now, clock.Sleep),
		WithGapSampler(func() time.Duration { return gap }),
	)
	return g, clock
}

func mustAcquire(t *testing.T, g *Gate, class Class) {
	t.Helper()

	release, err := g.Acquire(context.Background(), class)
	if err != nil {
		t.Fatalf("Acquire(%s): unexpected error: %v", class, err)
	}
	release()
}

func acquireErr(t *testing.T, g *Gate, class Class) *myerrors.ErrRateLimited {
	t.Helper()

	release, err := g.Acquire(context.Background(), class)
	if err == nil {
		release()
		t.Fatalf("Acquire(%s): expected rate limit, got success", class)
	}
	rl, ok := myerrors.AsRateLimited(err)
	if !ok {
		t.Fatalf("Acquire(%s): expected *ErrRateLimited, got %T: %v", class, err, err)
	}
	return rl
}

func TestGate_ReadBudgetSlidesOverAnHour(t *testing.T) {
	cfg := Config{MaxReadsPerHour: 3, MaxWait: time.Second}
	g, clock := testGate(t, cfg, 0)

	for i := 0; i < 3; i++ {
		mustAcquire(t, g, ClassRead)
		clock.Advance(time.Minute)
	}

	// Fourth read inside the hour is refused, and the retry points at the
	// moment the oldest of the three ages out.
	rl := acquireErr(t, g, ClassRead)
	want := time.Hour - 3*time.Minute
	if rl.RetryAfter != want {
		t.Fatalf("RetryAfter = %s, want %s", rl.RetryAfter, want)
	}

	// Once it does age out, exactly one slot frees up.
	clock.Advance(want)
	mustAcquire(t, g, ClassRead)
	acquireErr(t, g, ClassRead)
}

func TestGate_WriteHourAndDayBudgets(t *testing.T) {
	cfg := Config{MaxWritesPerHour: 2, MaxWritesPerDay: 3, MaxWait: time.Second}
	g, clock := testGate(t, cfg, 0)

	mustAcquire(t, g, ClassWrite)
	mustAcquire(t, g, ClassWrite)

	rl := acquireErr(t, g, ClassWrite)
	if rl.RetryAfter != time.Hour {
		t.Fatalf("hourly RetryAfter = %s, want 1h", rl.RetryAfter)
	}

	// Next hour: the hourly budget is clear but the daily one binds after one
	// more write.
	clock.Advance(time.Hour)
	mustAcquire(t, g, ClassWrite)

	rl = acquireErr(t, g, ClassWrite)
	if rl.RetryAfter <= time.Hour {
		t.Fatalf("daily RetryAfter = %s, want more than 1h", rl.RetryAfter)
	}

	clock.Advance(24 * time.Hour)
	mustAcquire(t, g, ClassWrite)
}

func TestGate_PublishBudgetIsSeparateFromWrites(t *testing.T) {
	cfg := Config{MaxWritesPerDay: 10, MaxPublishPerDay: 1, MaxWait: time.Second}
	g, _ := testGate(t, cfg, 0)

	mustAcquire(t, g, ClassPublish)
	acquireErr(t, g, ClassPublish)

	// Publishing does not consume the write budget.
	mustAcquire(t, g, ClassWrite)
}

func TestGate_ZeroDisablesALimit(t *testing.T) {
	cfg := Config{MaxWritesPerHour: 0, MaxWritesPerDay: 0, MaxWait: time.Second}
	g, _ := testGate(t, cfg, 0)

	for i := 0; i < 50; i++ {
		mustAcquire(t, g, ClassWrite)
	}
}

func TestGate_GapIsWaitedOutAndCreditedAgainstIdleTime(t *testing.T) {
	cfg := Config{MaxWait: time.Second}
	g, clock := testGate(t, cfg, 5*time.Second)

	start := clock.Now()
	mustAcquire(t, g, ClassRead)
	if got := clock.Now().Sub(start); got != 5*time.Second {
		t.Fatalf("first acquire slept %s, want 5s", got)
	}

	// Idle time counts towards the next gap: after 6s of idling there is
	// nothing left to wait for.
	clock.Advance(6 * time.Second)
	mark := clock.Now()
	mustAcquire(t, g, ClassRead)
	if got := clock.Now().Sub(mark); got != 0 {
		t.Fatalf("second acquire slept %s, want 0", got)
	}

	// After only 2s idle, the remaining 3s are slept.
	clock.Advance(2 * time.Second)
	mark = clock.Now()
	mustAcquire(t, g, ClassRead)
	if got := clock.Now().Sub(mark); got != 3*time.Second {
		t.Fatalf("third acquire slept %s, want 3s", got)
	}
}

func TestGate_CooldownBlocksEveryClass(t *testing.T) {
	cfg := Config{MaxWait: time.Second, RiskCooldown: 30 * time.Minute}
	g, clock := testGate(t, cfg, 0)

	g.Cooldown(0) // falls back to the configured risk cooldown

	for _, class := range []Class{ClassRead, ClassWrite, ClassPublish} {
		rl := acquireErr(t, g, class)
		if rl.RetryAfter != 30*time.Minute {
			t.Fatalf("%s RetryAfter = %s, want 30m", class, rl.RetryAfter)
		}
	}

	clock.Advance(30 * time.Minute)
	mustAcquire(t, g, ClassRead)
}

func TestGate_CooldownNeverShortensAnExistingOne(t *testing.T) {
	g, _ := testGate(t, Config{MaxWait: time.Second}, 0)

	g.Cooldown(time.Hour)
	g.Cooldown(time.Minute)

	rl := acquireErr(t, g, ClassRead)
	if rl.RetryAfter != time.Hour {
		t.Fatalf("RetryAfter = %s, want 1h", rl.RetryAfter)
	}
}

func TestGate_CountersSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pacing_state.json")
	cfg := Config{MaxWritesPerDay: 2, MaxWait: time.Second, StatePath: path}

	clock := newFakeClock()
	first := New(cfg, WithClock(clock.Now, clock.Sleep), WithGapSampler(func() time.Duration { return 0 }))
	mustAcquire(t, first, ClassWrite)
	mustAcquire(t, first, ClassWrite)
	first.Cooldown(10 * time.Minute)

	// Simulated restart: same file, same wall clock, brand new process state.
	second := New(cfg, WithClock(clock.Now, clock.Sleep), WithGapSampler(func() time.Duration { return 0 }))

	if got := second.Count(ClassWrite, dayWindow); got != 2 {
		t.Fatalf("restored write count = %d, want 2", got)
	}
	if rl := acquireErr(t, second, ClassWrite); rl.RetryAfter <= 0 {
		t.Fatalf("expected a positive RetryAfter after restart, got %s", rl.RetryAfter)
	}
	if until := second.CooldownUntil(); !until.Equal(clock.Now().Add(10 * time.Minute)) {
		t.Fatalf("restored cooldown = %s, want %s", until, clock.Now().Add(10*time.Minute))
	}

	// Events older than a day must not be resurrected by the restore.
	clock.Advance(25 * time.Hour)
	third := New(cfg, WithClock(clock.Now, clock.Sleep), WithGapSampler(func() time.Duration { return 0 }))
	if got := third.Count(ClassWrite, dayWindow); got != 0 {
		t.Fatalf("stale write count = %d, want 0", got)
	}
	mustAcquire(t, third, ClassWrite)
}

func TestGate_AcquireIsSerialised(t *testing.T) {
	// Real clock here: the point is the semaphore, not the arithmetic.
	g := New(Config{MaxWait: 5 * time.Second}, WithGapSampler(func() time.Duration { return 0 }))

	var inFlight, maxInFlight, total int32
	var wg sync.WaitGroup

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			release, err := g.Acquire(context.Background(), ClassRead)
			if err != nil {
				t.Errorf("Acquire: %v", err)
				return
			}
			defer release()

			n := atomic.AddInt32(&inFlight, 1)
			for {
				old := atomic.LoadInt32(&maxInFlight)
				if n <= old || atomic.CompareAndSwapInt32(&maxInFlight, old, n) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			atomic.AddInt32(&total, 1)
			atomic.AddInt32(&inFlight, -1)
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&maxInFlight); got != 1 {
		t.Fatalf("max concurrent actions = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&total); got != 20 {
		t.Fatalf("completed actions = %d, want 20", got)
	}
}

func TestGate_BusySlotFailsFastInsteadOfHanging(t *testing.T) {
	g := New(Config{MaxWait: 20 * time.Millisecond}, WithGapSampler(func() time.Duration { return 0 }))

	release, err := g.Acquire(context.Background(), ClassRead)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	defer release()

	start := time.Now()
	rl := acquireErr(t, g, ClassRead)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("second Acquire waited %s, expected to fail fast", elapsed)
	}
	if rl.RetryAfter != 20*time.Millisecond {
		t.Fatalf("RetryAfter = %s, want 20ms", rl.RetryAfter)
	}
}

func TestGate_AcquireRespectsContext(t *testing.T) {
	g := New(Config{}, WithGapSampler(func() time.Duration { return 0 }))

	release, err := g.Acquire(context.Background(), ClassRead)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := g.Acquire(ctx, ClassRead); err != context.Canceled {
		t.Fatalf("Acquire on a cancelled context: got %v, want context.Canceled", err)
	}
}

func TestGate_ReleaseIsCalledOnRefusalSoTheSlotStaysUsable(t *testing.T) {
	cfg := Config{MaxReadsPerHour: 1, MaxWait: 50 * time.Millisecond}
	g, _ := testGate(t, cfg, 0)

	mustAcquire(t, g, ClassRead)
	acquireErr(t, g, ClassRead) // over budget: must still hand the slot back

	// A class with budget left proves the slot was released.
	mustAcquire(t, g, ClassWrite)
}

func TestGate_UnknownClassIsUnbudgeted(t *testing.T) {
	g, _ := testGate(t, Config{MaxWait: time.Second}, 0)
	mustAcquire(t, g, Class("something-new"))
}
