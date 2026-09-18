// Package schedule computes episode release times. It is pure and
// performs no I/O so that it can be tested exhaustively with fixed
// clocks and table-driven cases.
package schedule

import (
	"fmt"
	"time"
)

// Config describes everything needed to compute the release time of any
// episode position in a subscription's queue.
type Config struct {
	// StartAt is the instant the first (seeded) episode releases.
	StartAt time.Time
	// CadenceDays is the number of days between releases once past the
	// seed window.
	CadenceDays int
	// ReleaseTime is the wall-clock time of day ("HH:MM") that
	// non-seeded episodes release at, in Location.
	ReleaseTime string
	// Location is the IANA timezone release times are computed in.
	Location *time.Location
	// SeedCount is the number of episodes released immediately at
	// StartAt (staggered by a minute each) rather than on cadence.
	SeedCount int
	// EpisodesPerSlot is how many episodes release at each cadence
	// slot (>1 to catch up faster).
	EpisodesPerSlot int
	// ShiftSeconds is accumulated pause time, added to every
	// non-seeded release.
	ShiftSeconds int
}

// ReleaseAt returns the scheduled release time for the episode at the
// given zero-based ingest position.
func (c Config) ReleaseAt(position int) (time.Time, error) {
	if position < 0 {
		return time.Time{}, fmt.Errorf("schedule: negative position %d", position)
	}
	if c.EpisodesPerSlot < 1 {
		return time.Time{}, fmt.Errorf("schedule: episodes_per_slot must be >= 1, got %d", c.EpisodesPerSlot)
	}
	if c.Location == nil {
		return time.Time{}, fmt.Errorf("schedule: location is required")
	}

	if position < c.SeedCount {
		// Seeded episodes all release at StartAt, staggered by a
		// minute each so podcast apps don't have to break a
		// pubDate tie arbitrarily.
		return c.StartAt.Add(time.Duration(position) * time.Minute), nil
	}

	hour, minute, err := parseReleaseTime(c.ReleaseTime)
	if err != nil {
		return time.Time{}, err
	}

	stepIndex := position - c.SeedCount
	slot := stepIndex/c.EpisodesPerSlot + 1 // 1-based count of cadence periods after start
	offsetInSlot := stepIndex % c.EpisodesPerSlot

	// AddDate (not a fixed duration) so the wall-clock hour survives a
	// DST boundary.
	local := c.StartAt.In(c.Location)
	slotDate := local.AddDate(0, 0, slot*c.CadenceDays)
	releaseAt := time.Date(
		slotDate.Year(), slotDate.Month(), slotDate.Day(),
		hour, minute, 0, 0,
		c.Location,
	)
	releaseAt = releaseAt.Add(time.Duration(offsetInSlot) * time.Minute)
	releaseAt = releaseAt.Add(time.Duration(c.ShiftSeconds) * time.Second)

	return releaseAt, nil
}

func parseReleaseTime(s string) (hour, minute int, err error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, 0, fmt.Errorf("schedule: invalid release_time %q: %w", s, err)
	}
	return t.Hour(), t.Minute(), nil
}

// Episode is the minimal view of an episode row that scheduling needs.
type Episode struct {
	Position    int
	ScheduledAt time.Time
	Locked      bool
}

// ApplyLocks returns a copy of episodes with Locked set to true on any
// row whose ScheduledAt is now in the past. Already-locked rows are
// left untouched. This must run before Recompute, since a locked
// ScheduledAt is never rewritten again.
func ApplyLocks(now time.Time, episodes []Episode) []Episode {
	out := make([]Episode, len(episodes))
	for i, e := range episodes {
		if !e.Locked && !e.ScheduledAt.After(now) {
			e.Locked = true
		}
		out[i] = e
	}
	return out
}

// Recompute returns a copy of episodes with ScheduledAt refreshed from
// cfg for every row that is not locked. Locked rows are returned
// unchanged, which is what pins an already-released episode's date in
// place across cadence or start-date edits.
func Recompute(cfg Config, episodes []Episode) ([]Episode, error) {
	out := make([]Episode, len(episodes))
	for i, e := range episodes {
		if !e.Locked {
			at, err := cfg.ReleaseAt(e.Position)
			if err != nil {
				return nil, fmt.Errorf("schedule: position %d: %w", e.Position, err)
			}
			e.ScheduledAt = at
		}
		out[i] = e
	}
	return out, nil
}

// Preview returns the release times for count positions starting at
// fromPosition, useful for showing the next N releases without
// touching the database.
func (c Config) Preview(fromPosition, count int) ([]time.Time, error) {
	out := make([]time.Time, 0, count)
	for i := 0; i < count; i++ {
		at, err := c.ReleaseAt(fromPosition + i)
		if err != nil {
			return nil, err
		}
		out = append(out, at)
	}
	return out, nil
}
