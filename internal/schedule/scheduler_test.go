package schedule

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func waitForCount(t *testing.T, counter *atomic.Int64, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if counter.Load() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("callback count = %d, want at least %d", counter.Load(), want)
}

type warnCounter struct {
	mu    sync.Mutex
	count int
}

func (h *warnCounter) Enabled(context.Context, slog.Level) bool { return true }

func (h *warnCounter) Handle(_ context.Context, record slog.Record) error {
	if record.Level == slog.LevelWarn && record.Message == "scheduled task still running; skipping" {
		h.mu.Lock()
		h.count++
		h.mu.Unlock()
	}
	return nil
}

func (h *warnCounter) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *warnCounter) WithGroup(string) slog.Handler { return h }

func (h *warnCounter) value() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.count
}

func waitForSkips(t *testing.T, counter *warnCounter) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if counter.value() > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("skip warnings = %d, want at least 1", counter.value())
}

func TestValidate(t *testing.T) {
	s := New(testLogger())
	defer s.Stop(context.Background())

	for _, expr := range []string{"", "   ", "not a cron", "* * * *", "@every nope"} {
		if err := s.Validate(expr); err == nil {
			t.Fatalf("Validate(%q) = nil, want error", expr)
		}
	}
	for _, expr := range []string{"* * * * *", "*/5 * * * *", "0 3 * * 1", "@every 30m"} {
		if err := s.Validate(expr); err != nil {
			t.Fatalf("Validate(%q) = %v, want nil", expr, err)
		}
	}
}

func TestValidateRejectsTimezonePrefixes(t *testing.T) {
	s := New(testLogger())
	defer s.Stop(context.Background())

	// robfig/cron v3.0.1 panics on a prefix without a following space, and
	// parseable prefixes would override the host-local timezone contract.
	for _, expr := range []string{
		"TZ=UTC",
		"CRON_TZ=UTC",
		"TZ=UTC 0 3 * * *",
		"CRON_TZ=Asia/Shanghai 0 3 * * *",
	} {
		if err := s.Validate(expr); err == nil {
			t.Fatalf("Validate(%q) = nil, want error", expr)
		}
	}
}

func TestRegisterRejectsEmptyName(t *testing.T) {
	s := New(testLogger())
	defer s.Stop(context.Background())

	for _, name := range []string{"", "   "} {
		err := s.Register(name, "@every 10ms", func() error { return nil })
		if err == nil {
			t.Fatalf("Register(%q) = nil, want error", name)
		}
	}
	if got := len(s.jobs); got != 0 {
		t.Fatalf("len(jobs) = %d, want 0", got)
	}
}

func TestRegisterRejectsInvalidExpression(t *testing.T) {
	s := New(testLogger())
	defer s.Stop(context.Background())

	var count atomic.Int64
	err := s.Register("tick", "not a cron", func() error {
		count.Add(1)
		return nil
	})
	if err == nil {
		t.Fatal("Register with invalid expression = nil, want error")
	}
	if _, exists := s.jobs["tick"]; exists {
		t.Fatal("failed Register stored a job")
	}
	if got := count.Load(); got != 0 {
		t.Fatalf("callback count = %d, want 0", got)
	}
}

func TestCallbackRuns(t *testing.T) {
	s := New(testLogger())
	defer s.Stop(context.Background())

	var count atomic.Int64
	if err := s.Register("tick", "@every 10ms", func() error {
		count.Add(1)
		return nil
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	waitForCount(t, &count, 1)
}

func TestOverlapIsSkipped(t *testing.T) {
	skips := &warnCounter{}
	s := New(slog.New(skips))
	defer s.Stop(context.Background())

	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })

	started := make(chan struct{}, 1)
	var active atomic.Int64
	var peak atomic.Int64
	var starts atomic.Int64
	if err := s.Register("block", "@every 10ms", func() error {
		starts.Add(1)
		current := active.Add(1)
		defer active.Add(-1)
		for {
			observed := peak.Load()
			if current <= observed || peak.CompareAndSwap(observed, current) {
				break
			}
		}
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return nil
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("callback did not start")
	}
	// robfig/cron rounds @every below one second up to one second, so wait
	// until a later trigger is actually observed being skipped.
	waitForSkips(t, skips)
	if got := peak.Load(); got != 1 {
		t.Fatalf("peak concurrency = %d, want 1", got)
	}
	if got := starts.Load(); got != 1 {
		t.Fatalf("callback starts while blocked = %d, want 1", got)
	}
}

func TestUnregisterStopsFurtherRuns(t *testing.T) {
	s := New(testLogger())
	defer s.Stop(context.Background())

	var count atomic.Int64
	if err := s.Register("tick", "@every 10ms", func() error {
		count.Add(1)
		return nil
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	waitForCount(t, &count, 1)

	if !s.Unregister("tick") {
		t.Fatal("Unregister = false, want true")
	}
	if got := len(s.jobs); got != 0 {
		t.Fatalf("len(jobs) = %d, want 0", got)
	}
	time.Sleep(100 * time.Millisecond)
	first := count.Load()
	time.Sleep(1500 * time.Millisecond)
	if got := count.Load(); got != first {
		t.Fatalf("callback count changed after unregister: %d -> %d", first, got)
	}
	if s.Unregister("tick") {
		t.Fatal("second Unregister = true, want false")
	}
}

func TestStopStopsFurtherRuns(t *testing.T) {
	s := New(testLogger())

	var count atomic.Int64
	if err := s.Register("tick", "@every 10ms", func() error {
		count.Add(1)
		return nil
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	waitForCount(t, &count, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s.Stop(ctx)

	first := count.Load()
	time.Sleep(1500 * time.Millisecond)
	if got := count.Load(); got != first {
		t.Fatalf("callback count changed after stop: %d -> %d", first, got)
	}
}

func TestReregisterKeepsOneEntry(t *testing.T) {
	s := New(testLogger())
	defer s.Stop(context.Background())

	var count atomic.Int64
	callback := func() error {
		count.Add(1)
		return nil
	}
	if err := s.Register("tick", "0 0 1 1 *", callback); err != nil {
		t.Fatalf("Register: %v", err)
	}
	first := s.jobs["tick"]
	if first == nil {
		t.Fatal("Register did not store the job")
	}
	if err := s.Register("tick", "@every 10ms", callback); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if got := len(s.jobs); got != 1 {
		t.Fatalf("len(jobs) = %d, want 1", got)
	}
	if s.jobs["tick"] != first {
		t.Fatal("re-register replaced the job state")
	}
	waitForCount(t, &count, 1)
}
