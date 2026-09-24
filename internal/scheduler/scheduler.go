// Package scheduler runs each site every schedule.interval (§8.1).
package scheduler

import (
	"context"
	"time"

	"feed-me/internal/config"
)

// Scheduler runs one site at a time, since the store has a single writer.
// At startup a site whose last run is older than its interval runs at once;
// the others wait for their next tick, so restarts don't burst fetches.
type Scheduler struct {
	Sites   []*config.Site
	LastRun func(ctx context.Context, siteID string) (time.Time, error)
	Run     func(ctx context.Context, site *config.Site)

	// Now and Sleep are replaceable in tests. Sleep returns false if ctx ended first.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) bool
}

// Loop runs sites as they come due until ctx ends.
func (s *Scheduler) Loop(ctx context.Context) error {
	now, sleep := s.Now, s.Sleep
	if now == nil {
		now = time.Now
	}
	if sleep == nil {
		sleep = Sleep
	}
	if len(s.Sites) == 0 {
		<-ctx.Done()
		return nil
	}
	next := make([]time.Time, len(s.Sites))
	for i, site := range s.Sites {
		last, err := s.LastRun(ctx, site.ID)
		if ctx.Err() != nil {
			return nil // stopped, not failed, even if the read failed for it
		}
		if err != nil {
			return err
		}
		if !last.IsZero() {
			next[i] = last.Add(site.Schedule.Interval.D) // zero (never run) is due at once
		}
	}
	for {
		i := 0
		for j := range next {
			if next[j].Before(next[i]) {
				i = j // earliest first; ties go to config order
			}
		}
		if d := next[i].Sub(now()); d > 0 && !sleep(ctx, d) {
			return nil
		}
		if ctx.Err() != nil {
			return nil
		}
		start := now()
		s.Run(ctx, s.Sites[i])
		next[i] = start.Add(s.Sites[i].Schedule.Interval.D)
	}
}

// Sleep waits for d, returning false if ctx ends first.
func Sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
