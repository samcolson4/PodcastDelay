package refresh

import (
	"context"
	"errors"
	"time"

	"github.com/samcolson4/podcastdelay/internal/schedule"
	"github.com/samcolson4/podcastdelay/internal/store"
)

// ErrNothingToRelease means the subscription has no upcoming episode
// left (everything is released, excluded, or gone from the source).
var ErrNothingToRelease = errors.New("refresh: no upcoming episode to release")

// ReleaseNext releases the earliest upcoming episode at now and locks
// it so later reschedules can't pull it back.
//
// With shiftFollowing false the rest of the schedule is left alone: the
// following episode keeps the date it already had.
//
// With shiftFollowing true the remaining episodes move so the manual
// release counts as the slot it replaced: in fixed mode the following
// episode lands one cadence after today at the release time, and in
// original mode every remaining episode keeps its original gap from the
// one just released. Seeded episodes that are still upcoming don't move
// (they release at start_at by definition).
func ReleaseNext(ctx context.Context, st *store.Store, subscriptionID int64, now time.Time, shiftFollowing bool) error {
	err := st.WithTx(ctx, func(q *store.Queries) error {
		sub, err := q.GetSubscription(ctx, subscriptionID)
		if err != nil {
			return err
		}
		loc, err := time.LoadLocation(sub.Timezone)
		if err != nil {
			return err
		}
		all, err := q.ListBySubscription(ctx, subscriptionID)
		if err != nil {
			return err
		}

		var next, following *store.Episode
		for i := range all {
			e := &all[i]
			if e.Excluded || e.Locked || e.MissingSince != nil || !e.ScheduledAt.After(now) {
				continue
			}
			if next == nil {
				next = e
				continue
			}
			following = e
			break
		}
		if next == nil {
			return ErrNothingToRelease
		}

		if shiftFollowing && following != nil {
			var delta time.Duration
			if sub.CadenceMode == schedule.ModeOriginal {
				delta = now.Sub(next.ScheduledAt)
			} else {
				hm, err := time.Parse("15:04", sub.ReleaseTime)
				if err != nil {
					return err
				}
				day := now.In(loc).AddDate(0, 0, sub.CadenceDays)
				target := time.Date(day.Year(), day.Month(), day.Day(), hm.Hour(), hm.Minute(), 0, 0, loc)
				delta = target.Sub(following.ScheduledAt)
			}
			if err := q.AddShiftSeconds(ctx, subscriptionID, int(delta.Seconds())); err != nil {
				return err
			}
		}
		return q.SetSchedule(ctx, next.ID, now, true)
	})
	if err != nil {
		return err
	}
	return Reschedule(ctx, st, subscriptionID)
}
