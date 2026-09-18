package refresh

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/samcolson4/podcastdelay/internal/source"
	"github.com/samcolson4/podcastdelay/internal/store"
)

// failureBackoff is how long to wait before retrying a subscription
// whose last poll attempt errored, per docs §7 ("back off to 24h after
// repeated failures"). We approximate "repeated" with "most recent
// attempt failed" rather than adding a failure-count column: for a
// single-user system polling at most a handful of feeds, the
// simplification costs nothing and keeps the schema smaller.
const failureBackoff = 24 * time.Hour

// Reschedule recomputes scheduled_at for every unlocked, non-excluded
// episode of a subscription against its current settings. Call this
// after anything that can move future releases: a cadence/start/seed
// edit (PATCH) or a pause resuming (which bumps shift_seconds).
func Reschedule(ctx context.Context, st *store.Store, subscriptionID int64) error {
	return st.WithTx(ctx, func(q *store.Queries) error {
		sub, err := q.GetSubscription(ctx, subscriptionID)
		if err != nil {
			return err
		}
		loc, err := time.LoadLocation(sub.Timezone)
		if err != nil {
			return err
		}
		return relockAndReschedule(ctx, q, sub, time.Now().UTC(), loc)
	})
}

// RunLoop is the single background goroutine described in docs §7. It
// ticks every tickInterval, and on each tick polls any subscription
// whose last_fetched_at is stale relative to defaultPollInterval
// (widened to failureBackoff after the previous attempt errored). It
// blocks until ctx is canceled.
func RunLoop(ctx context.Context, st *store.Store, fetcher *source.Fetcher, defaultPollInterval, tickInterval time.Duration, logger *slog.Logger) {
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	tick(ctx, st, fetcher, defaultPollInterval, logger) // run once immediately on startup

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tick(ctx, st, fetcher, defaultPollInterval, logger)
		}
	}
}

func tick(ctx context.Context, st *store.Store, fetcher *source.Fetcher, defaultPollInterval time.Duration, logger *slog.Logger) {
	subs, err := st.ListSubscriptions(ctx)
	if err != nil {
		logger.Error("refresh: list subscriptions failed", "error", err)
		return
	}

	now := time.Now().UTC()
	for _, sub := range subs {
		interval := defaultPollInterval
		if sub.LastFetchStatus != nil && strings.HasPrefix(*sub.LastFetchStatus, "error:") {
			interval = failureBackoff
		}
		if sub.LastFetchedAt != nil && now.Sub(*sub.LastFetchedAt) < interval {
			continue
		}

		if err := Poll(ctx, st, fetcher, sub, now, logger); err != nil {
			logger.Error("refresh: poll failed", "subscription_id", sub.ID, "error", err)
		}
	}
}
