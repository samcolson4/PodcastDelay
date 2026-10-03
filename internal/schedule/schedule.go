// Package schedule computes episode release times. It is pure and
// performs no I/O so that it can be tested exhaustively with fixed
// clocks and table-driven cases.
package schedule

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Cadence modes.
const (
	// ModeFixed releases one episode (or EpisodesPerSlot) every CadenceDays.
	ModeFixed = "fixed"
	// ModeOriginal replays the show's original release gaps: each episode
	// releases the same amount of time after the last seeded episode as it
	// was originally published after that episode.
	ModeOriginal = "original"
)

// Source roles. A subscription always has a primary (the show's regular,
// free feed) and may have a premium one melded into it (the publisher's
// bonus/subscriber feed). Only primary episodes are paced by the cadence:
// premium episodes hang off the free episode they were published
// alongside, so the pair stays together however fast the free feed is
// being replayed.
const (
	// RolePrimary marks episodes from the regular feed (default when empty).
	RolePrimary = "primary"
	// RolePremium marks episodes from the melded premium feed.
	RolePremium = "premium"
)

// NormalizeRole maps an unknown or empty role onto RolePrimary, so a
// value read from storage can never send an episode down an unintended
// scheduling path.
func NormalizeRole(role string) string {
	if role == RolePremium {
		return RolePremium
	}
	return RolePrimary
}

// Config describes everything needed to compute the release time of any
// episode in a subscription's queue.
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
	// Mode is ModeFixed (default when empty) or ModeOriginal.
	Mode string
	// PubDates holds the primary episodes' original publish dates in
	// rank order (nil where unknown) — rank being an episode's index
	// among the primary ones, which is what the cadence counts. Only
	// used by ModeOriginal. Recompute fills this in from the rows it is
	// given; set it yourself only when calling ReleaseAt directly.
	PubDates []*time.Time
}

// ReleaseAt returns the scheduled release time for the primary episode
// at the given zero-based rank (its index among the subscription's
// primary episodes, which equals its ingest position for a show with no
// premium feed melded in). Premium episodes are not paced by the
// cadence and are scheduled by Recompute instead.
func (c Config) ReleaseAt(rank int) (time.Time, error) {
	if rank < 0 {
		return time.Time{}, fmt.Errorf("schedule: negative rank %d", rank)
	}
	if c.EpisodesPerSlot < 1 {
		return time.Time{}, fmt.Errorf("schedule: episodes_per_slot must be >= 1, got %d", c.EpisodesPerSlot)
	}
	if c.Location == nil {
		return time.Time{}, fmt.Errorf("schedule: location is required")
	}

	if rank < c.SeedCount {
		// Seeded episodes all release at StartAt, staggered by a
		// minute each so podcast apps don't have to break a
		// pubDate tie arbitrarily.
		return c.StartAt.Add(time.Duration(rank) * time.Minute), nil
	}

	if c.Mode == ModeOriginal {
		return c.originalReleaseAt(rank), nil
	}

	hour, minute, err := parseReleaseTime(c.ReleaseTime)
	if err != nil {
		return time.Time{}, err
	}

	stepIndex := rank - c.SeedCount
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

// originalReleaseAt mirrors the source's own gaps. Episode p releases at
// StartAt + (pub(p) - pub(ref)), where ref is the last seeded episode (or
// the first episode when nothing is seeded). Two guards keep the feed sane:
// an episode with no known date releases with the one before it, and no
// episode ever releases before the one ahead of it in the queue (a
// backfilled old episode lands at the tail rather than jumping the line).
func (c Config) originalReleaseAt(rank int) time.Time {
	ref := c.SeedCount - 1
	if ref < 0 {
		ref = 0
	}
	var refDate time.Time
	if ref < len(c.PubDates) && c.PubDates[ref] != nil {
		refDate = *c.PubDates[ref]
	}

	// prev is the release time of the episode just before the first
	// non-seeded one; the loop floors each release at prev+1m.
	prev := c.StartAt.Add(time.Duration(c.SeedCount-1) * time.Minute)
	var at time.Time
	for i := c.SeedCount; i <= rank; i++ {
		at = prev.Add(time.Minute)
		if !refDate.IsZero() && i < len(c.PubDates) && c.PubDates[i] != nil {
			if candidate := c.StartAt.Add(c.PubDates[i].Sub(refDate)).Add(time.Duration(c.ShiftSeconds) * time.Second); candidate.After(at) {
				at = candidate
			}
		}
		prev = at
	}
	return at
}

// NormalizeReleaseTime validates a wall-clock release time and returns it
// in canonical "HH:MM" form. Seconds are accepted and dropped, because
// <input type="time"> sends "HH:MM:SS" in some browsers.
func NormalizeReleaseTime(s string) (string, error) {
	hour, minute, err := parseReleaseTime(s)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%02d:%02d", hour, minute), nil
}

func parseReleaseTime(s string) (hour, minute int, err error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{"15:04", "15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Hour(), t.Minute(), nil
		}
	}
	return 0, 0, fmt.Errorf("schedule: invalid release_time %q (want \"HH:MM\")", s)
}

// Episode is the minimal view of an episode row that scheduling needs.
// Rows must be given to ApplyLocks and Recompute in ingest-position
// order, since that is what fixes each primary episode's rank and each
// premium episode's place in its own queue.
type Episode struct {
	Position int
	// Role is RolePrimary (the default when empty) or RolePremium.
	Role string
	// PubDate is the episode's original publish date, nil when the
	// source didn't give one. It is what premium episodes are anchored
	// by, and what ModeOriginal paces the primary feed with.
	PubDate     *time.Time
	ScheduledAt time.Time
	Locked      bool
	// Excluded rows are passed through untouched — neither locked nor
	// rescheduled — so excluding an episode leaves a gap in the
	// schedule instead of moving everything after it (docs §10).
	Excluded bool
}

// ApplyLocks returns a copy of episodes with Locked set to true on any
// row whose ScheduledAt is now in the past. Already-locked and excluded
// rows are left untouched. This must run before Recompute, since a
// locked ScheduledAt is never rewritten again.
func ApplyLocks(now time.Time, episodes []Episode) []Episode {
	out := make([]Episode, len(episodes))
	for i, e := range episodes {
		if !e.Locked && !e.Excluded && !e.ScheduledAt.After(now) {
			e.Locked = true
		}
		out[i] = e
	}
	return out
}

// Recompute returns a copy of episodes with ScheduledAt refreshed from
// cfg for every row that is neither locked nor excluded. Locked rows are
// returned unchanged, which is what pins an already-released episode's
// date in place across cadence or start-date edits.
//
// Primary episodes are paced by the cadence, counted by their rank among
// the primary rows. Premium episodes are then anchored to them: each one
// releases as long after its free counterpart as it was originally
// published after it, so an hour's gap upstream stays an hour here and a
// day stays a day, whatever cadence the free feed is being replayed at.
//
// cfg.PubDates is derived from the rows rather than trusted, so there is
// no way for a caller to pass dates that disagree with the episodes.
func Recompute(cfg Config, episodes []Episode) ([]Episode, error) {
	out := make([]Episode, len(episodes))
	copy(out, episodes)

	cfg.PubDates = primaryPubDates(episodes)

	// Pass 1: the primary feed on its cadence, collecting what each
	// released (or locked) episode became, for the premium feed to hang
	// off. Excluded and locked rows keep their stored time but still
	// count, so a premium episode's anchor doesn't move when its free
	// counterpart is excluded.
	anchors := make([]anchor, 0, len(out))
	rank := 0
	for i, e := range out {
		if NormalizeRole(e.Role) != RolePrimary {
			continue
		}
		at := e.ScheduledAt
		if !e.Locked && !e.Excluded {
			computed, err := cfg.ReleaseAt(rank)
			if err != nil {
				return nil, fmt.Errorf("schedule: position %d: %w", e.Position, err)
			}
			at = computed
			out[i].ScheduledAt = at
		}
		anchors = append(anchors, anchor{PubDate: e.PubDate, ReleaseAt: at})
		rank++
	}

	// Pass 2: the premium feed, anchored to pass 1's results. prev keeps
	// the premium queue in its own order, so a bonus episode backfilled
	// years late lands behind the one before it rather than jumping the
	// queue (the same guard ModeOriginal applies to the free feed).
	dated := anchorsByDate(anchors)
	var prev time.Time
	for i, e := range out {
		if NormalizeRole(e.Role) != RolePremium || e.Excluded {
			continue
		}
		at := cfg.premiumReleaseAt(dated, e.PubDate, prev)
		if e.Locked {
			at = e.ScheduledAt
		} else {
			out[i].ScheduledAt = at
		}
		prev = at
	}

	return out, nil
}

// anchor is a free-feed episode a premium one can hang off: the date the
// publisher gave it, and the release time this feed gave it.
type anchor struct {
	PubDate   *time.Time
	ReleaseAt time.Time
}

// premiumReleaseAt places one premium episode: as far after its anchor's
// release as it was published after the anchor itself, never earlier
// than the premium episode before it. anchors must be anchorsByDate's
// output.
func (c Config) premiumReleaseAt(anchors []anchor, pubDate *time.Time, prev time.Time) time.Time {
	if pubDate != nil {
		if a, ok := anchorFor(anchors, *pubDate); ok {
			offset := pubDate.Sub(*a.PubDate)
			if offset < 0 {
				// Older than every free episode: release it with the
				// first one rather than before the feed starts.
				offset = 0
			}
			at := a.ReleaseAt.Add(offset)
			if !prev.IsZero() && !at.After(prev) {
				at = prev.Add(time.Minute)
			}
			return at
		}
	}

	// Undated, or nothing dated to anchor to: ride behind the episode in
	// front of it in the queue.
	if !prev.IsZero() {
		return prev.Add(time.Minute)
	}
	if len(anchors) > 0 {
		return anchors[len(anchors)-1].ReleaseAt.Add(time.Minute)
	}
	return c.StartAt
}

// anchorsByDate is the dated anchors ordered by publish date, which is
// how a premium episode is matched to one. Ingest order won't do: a
// backfilled free episode sits at the tail of the queue however old it
// claims to be (§3.2).
func anchorsByDate(anchors []anchor) []anchor {
	out := make([]anchor, 0, len(anchors))
	for _, a := range anchors {
		if a.PubDate != nil {
			out = append(out, a)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].PubDate.Before(*out[j].PubDate) })
	return out
}

// anchorFor picks the free episode a premium episode published at
// pubDate belongs with: the last one published at or before it, falling
// back to the earliest free episode when the premium one predates the
// whole show.
func anchorFor(anchors []anchor, pubDate time.Time) (anchor, bool) {
	if len(anchors) == 0 {
		return anchor{}, false
	}
	i := sort.Search(len(anchors), func(i int) bool { return anchors[i].PubDate.After(pubDate) })
	if i == 0 {
		return anchors[0], true
	}
	return anchors[i-1], true
}

// primaryPubDates is cfg.PubDates as Recompute needs it: the primary
// episodes' publish dates in rank order.
func primaryPubDates(episodes []Episode) []*time.Time {
	out := make([]*time.Time, 0, len(episodes))
	for _, e := range episodes {
		if NormalizeRole(e.Role) == RolePrimary {
			out = append(out, e.PubDate)
		}
	}
	return out
}
