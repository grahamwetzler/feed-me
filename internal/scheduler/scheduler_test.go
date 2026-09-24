package scheduler

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"rss-er/internal/config"
)

func site(id string, interval time.Duration) *config.Site {
	s := &config.Site{ID: id}
	s.Schedule.Interval.D = interval
	return s
}

func TestLoopStartupAndOrder(t *testing.T) {
	t0 := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	clock := t0
	last := map[string]time.Time{
		"stale":  t0.Add(-2 * time.Hour),    // interval 1h: overdue, runs at startup
		"recent": t0.Add(-10 * time.Minute), // interval 30m: waits for its tick at +20m
		// "new" has never run: runs at startup
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var runs []string
	s := &Scheduler{
		Sites: []*config.Site{site("stale", time.Hour), site("recent", 30*time.Minute), site("new", time.Hour)},
		LastRun: func(_ context.Context, id string) (time.Time, error) {
			return last[id], nil
		},
		Run: func(_ context.Context, site *config.Site) {
			runs = append(runs, fmt.Sprintf("%s@%v", site.ID, clock.Sub(t0)))
			clock = clock.Add(5 * time.Minute) // each run takes 5 minutes
			if len(runs) == 7 {
				cancel()
			}
		},
		Now: func() time.Time { return clock },
		Sleep: func(ctx context.Context, d time.Duration) bool {
			clock = clock.Add(d)
			return ctx.Err() == nil
		},
	}
	if err := s.Loop(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"new@0s", "stale@5m0s", // overdue at startup, back to back
		"recent@20m0s", "recent@50m0s", // on its own 30m tick
		"new@1h0m0s", "stale@1h5m0s", // an interval after each started
		"recent@1h20m0s",
	}
	if !slices.Equal(runs, want) {
		t.Errorf("runs:\n got %v\nwant %v", runs, want)
	}
}

func TestLoopStopsWhileWaiting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	s := &Scheduler{
		Sites:   []*config.Site{site("a", time.Hour)},
		LastRun: func(context.Context, string) (time.Time, error) { return time.Now(), nil },
		Run:     func(context.Context, *config.Site) { t.Error("ran a site that wasn't due") },
	}
	go func() { done <- s.Loop(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Loop did not return after cancel")
	}
}

func TestLoopStoppedDuringStartupIsNotAnError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Scheduler{
		Sites: []*config.Site{site("a", time.Hour)},
		LastRun: func(ctx context.Context, _ string) (time.Time, error) {
			cancel() // a signal arrives while the store is read
			return time.Time{}, ctx.Err()
		},
		Run: func(context.Context, *config.Site) { t.Error("ran after cancel") },
	}
	if err := s.Loop(ctx); err != nil {
		t.Errorf("Loop = %v, want nil for a shutdown", err)
	}
}
