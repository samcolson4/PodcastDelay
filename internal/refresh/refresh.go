// Package refresh implements the only background job in the system:
// periodically re-fetching each subscription's source feeds, ingesting
// any new episodes at the tail, tombstoning any that vanished, and
// locking/recomputing scheduled_at (docs/IMPLEMENTATION.md §7).
//
// A subscription has one or two source feeds: the show's regular (free)
// feed, and optionally the publisher's premium feed melded into the same
// delayed output (docs §3.9). Both are polled in the same cycle, and
// both land in the same episode table distinguished by source_role.
//
// It also implements first-time ingest (AddSubscription), since adding
// a show is just "refresh a subscription that doesn't exist yet" plus
// row creation, and both the admin HTTP handler and the CLI need it.
package refresh

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/samcolson4/podcastdelay/internal/schedule"
	"github.com/samcolson4/podcastdelay/internal/source"
	"github.com/samcolson4/podcastdelay/internal/store"
)

// AddParams is the set of fields needed to create a new subscription
// and perform its first ingest.
type AddParams struct {
	SourceURL string
	// PremiumSourceURL is the publisher's premium/bonus feed, melded
	// into the same delayed feed. Nil for the usual single-feed show.
	PremiumSourceURL *string
	TitleOverride    *string
	CadenceDays      int
	CadenceMode      string
	ReleaseTime      string
	Timezone         string
	StartAt          time.Time
	SeedCount        int
	EpisodesPerSlot  int
	MaxFeedItems     *int
}

// Values recorded in subscriptions.last_fetch_status. The refresh loop
// keys its failure backoff off statusErrorPrefix, so the vocabulary lives
// here rather than being spelled out at each use.
const (
	statusOK          = "ok"
	statusNotModified = "not_modified"
	statusErrorPrefix = "error: "
)

func errorStatus(err error) string { return statusErrorPrefix + err.Error() }

// premiumErrorStatus annotates an otherwise-fine poll with a premium
// feed that failed. Deliberately not an errorStatus: the free feed is
// the show, and a broken bonus feed shouldn't push the whole
// subscription onto the 24h failure backoff.
func premiumErrorStatus(base string, err error) string {
	return fmt.Sprintf("%s (premium error: %s)", base, err)
}

func newToken() (string, error) {
	b := make([]byte, 16) // 128 bits, per docs §4
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("refresh: generate token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func marshalChannel(ch source.Channel) (string, error) {
	meta := store.ChannelMeta{
		Title:       ch.Title,
		Description: ch.Description,
		Link:        ch.Link,
		Language:    ch.Language,
		Author:      ch.Author,
		ImageURL:    ch.ImageURL,
		Explicit:    ch.Explicit,
		ItunesType:  ch.ItunesType,
		Categories:  ch.Categories,
		Owner:       ch.Owner,
		OwnerEmail:  ch.OwnerEmail,
	}
	b, err := json.Marshal(meta)
	if err != nil {
		return "", fmt.Errorf("refresh: marshal channel: %w", err)
	}
	return string(b), nil
}

// ScheduleConfigFor builds the release-time configuration for sub from
// its stored settings. Every caller that needs to compute release times —
// ingest, the refresh loop, the admin schedule preview — goes through
// here, so there is exactly one place where a new scheduling knob has to
// be wired up.
func ScheduleConfigFor(sub store.Subscription) (schedule.Config, error) {
	loc, err := time.LoadLocation(sub.Timezone)
	if err != nil {
		return schedule.Config{}, fmt.Errorf("refresh: subscription %d: invalid timezone %q: %w", sub.ID, sub.Timezone, err)
	}
	return schedule.Config{
		StartAt:         sub.StartAt,
		CadenceDays:     sub.CadenceDays,
		ReleaseTime:     sub.ReleaseTime,
		Location:        loc,
		SeedCount:       sub.SeedCount,
		EpisodesPerSlot: sub.EpisodesPerSlot,
		ShiftSeconds:    sub.ShiftSeconds,
		Mode:            sub.CadenceMode,
	}, nil
}

// ScheduleRows is the schedule engine's view of stored episodes. The
// slice must stay in ingest-position order, which is what every store
// query returns.
func ScheduleRows(episodes []store.Episode) []schedule.Episode {
	rows := make([]schedule.Episode, len(episodes))
	for i, e := range episodes {
		rows[i] = schedule.Episode{
			Position:    e.Position,
			Role:        e.SourceRole,
			PubDate:     e.OriginalPubDate,
			ScheduledAt: e.ScheduledAt,
			Locked:      e.Locked,
			Excluded:    e.Excluded,
		}
	}
	return rows
}

// PlanSchedule computes release times for a whole set of rows against a
// subscription's settings: the free feed on its cadence, the premium
// feed anchored to it. Locked and excluded rows come back untouched.
func PlanSchedule(sub store.Subscription, rows []schedule.Episode) ([]schedule.Episode, error) {
	cfg, err := ScheduleConfigFor(sub)
	if err != nil {
		return nil, err
	}
	return schedule.Recompute(cfg, rows)
}

// AddSubscription fetches the show's feed (or feeds) for the first time,
// creates the subscription row, and ingests every item found as freshly
// seen episodes ordered oldest-first (the ingest-order freeze, docs §3.2).
func AddSubscription(ctx context.Context, st *store.Store, fetcher *source.Fetcher, p AddParams) (store.Subscription, error) {
	token, err := newToken()
	if err != nil {
		return store.Subscription{}, err
	}

	result, err := fetcher.Fetch(ctx, p.SourceURL, "", "")
	if err != nil {
		return store.Subscription{}, fmt.Errorf("refresh: initial fetch: %w", err)
	}
	if result.NotModified {
		// Can't happen on a first fetch (no conditional headers sent),
		// but guard anyway rather than silently creating an empty show.
		return store.Subscription{}, fmt.Errorf("refresh: initial fetch returned 304 unexpectedly")
	}

	// A premium feed given up front is part of the request, so a failure
	// fails the add rather than quietly creating a half-melded show.
	var premium source.FetchResult
	if p.PremiumSourceURL != nil {
		premium, err = fetcher.Fetch(ctx, *p.PremiumSourceURL, "", "")
		if err != nil {
			return store.Subscription{}, fmt.Errorf("refresh: initial premium fetch: %w", err)
		}
	}

	sourceURL := p.SourceURL
	if result.PermanentRedirectURL != "" {
		sourceURL = result.PermanentRedirectURL
	}
	premiumSourceURL := p.PremiumSourceURL
	if premium.PermanentRedirectURL != "" {
		premiumSourceURL = &premium.PermanentRedirectURL
	}

	channelJSON, err := marshalChannel(result.Channel)
	if err != nil {
		return store.Subscription{}, err
	}

	// Fail before creating anything if the timezone is unusable.
	if _, err := time.LoadLocation(p.Timezone); err != nil {
		return store.Subscription{}, fmt.Errorf("refresh: invalid timezone %q: %w", p.Timezone, err)
	}

	var sub store.Subscription
	err = st.WithTx(ctx, func(q *store.Queries) error {
		var txErr error
		sub, txErr = q.CreateSubscription(ctx, store.NewSubscription{
			Token:            token,
			SourceURL:        sourceURL,
			PremiumSourceURL: premiumSourceURL,
			TitleOverride:    p.TitleOverride,
			CadenceDays:      p.CadenceDays,
			CadenceMode:      p.CadenceMode,
			ReleaseTime:      p.ReleaseTime,
			Timezone:         p.Timezone,
			StartAt:          p.StartAt,
			SeedCount:        p.SeedCount,
			EpisodesPerSlot:  p.EpisodesPerSlot,
			MaxFeedItems:     p.MaxFeedItems,
			ChannelJSON:      channelJSON,
		})
		if txErr != nil {
			return txErr
		}

		// Positions run through the free feed first and then the
		// premium one, so a show with no premium feed gets exactly the
		// positions it always did.
		items := sortOldestFirst(result.Items)
		premiumItems := sortOldestFirst(premium.Items)
		rows := make([]schedule.Episode, 0, len(items)+len(premiumItems))
		for i, it := range items {
			rows = append(rows, schedule.Episode{Position: i, Role: schedule.RolePrimary, PubDate: it.PubDate})
		}
		for i, it := range premiumItems {
			rows = append(rows, schedule.Episode{Position: len(items) + i, Role: schedule.RolePremium, PubDate: it.PubDate})
		}

		// Build the schedule from the stored row, not from AddParams, so
		// ingest schedules against the same normalized values every later
		// refresh will use.
		planned, txErr := PlanSchedule(sub, rows)
		if txErr != nil {
			return txErr
		}
		for i, it := range items {
			if _, err := q.InsertEpisode(ctx, sub.ID, toNewEpisode(it, schedule.RolePrimary, i, planned[i].ScheduledAt)); err != nil {
				return err
			}
		}
		for i, it := range premiumItems {
			position := len(items) + i
			if _, err := q.InsertEpisode(ctx, sub.ID, toNewEpisode(it, schedule.RolePremium, position, planned[position].ScheduledAt)); err != nil {
				return err
			}
		}

		return q.UpdateFetchState(ctx, sub.ID, store.FetchState{
			ETag:                nilIfEmpty(result.ETag),
			LastModified:        nilIfEmpty(result.LastModified),
			PremiumETag:         nilIfEmpty(premium.ETag),
			PremiumLastModified: nilIfEmpty(premium.LastModified),
			FetchedAt:           time.Now().UTC(),
			Status:              statusOK,
		})
	})
	if err != nil {
		return store.Subscription{}, err
	}
	return st.GetSubscription(ctx, sub.ID)
}

// SetPremiumFeed melds a premium feed into an existing subscription, or
// unmelds it when premiumURL is nil, and leaves the schedule up to date
// either way. A newly melded feed is ingested straight away rather than
// at the next poll, since the whole point of melding is that the bonus
// episodes turn up alongside the free ones; an unreachable feed fails
// here rather than being accepted and quietly erroring in the
// background.
//
// Replacing one premium URL with another keeps the episodes already
// ingested: anything the new feed still carries matches by GUID, and
// anything it doesn't gets tombstoned like any other vanished episode.
// Unmelding instead deletes them, because there is no longer a feed to
// tombstone them against.
func SetPremiumFeed(ctx context.Context, st *store.Store, fetcher *source.Fetcher, id int64, premiumURL *string) error {
	if premiumURL == nil {
		if err := st.WithTx(ctx, func(q *store.Queries) error {
			if _, err := q.PatchSubscription(ctx, id, store.SubscriptionPatch{PremiumSourceURL: &premiumURL}); err != nil {
				return err
			}
			return q.DeleteEpisodesByRole(ctx, id, schedule.RolePremium)
		}); err != nil {
			return err
		}
		return Reschedule(ctx, st, id)
	}

	result, err := fetcher.Fetch(ctx, *premiumURL, "", "")
	if err != nil {
		return fmt.Errorf("refresh: fetch premium feed: %w", err)
	}
	if result.PermanentRedirectURL != "" {
		premiumURL = &result.PermanentRedirectURL
	}

	now := time.Now().UTC()
	// The validators from this fetch are deliberately not stored: the
	// next ordinary poll fetches the new feed unconditionally once and
	// records them then, which keeps this from having to touch the free
	// feed's fetch state at all.
	return st.WithTx(ctx, func(q *store.Queries) error {
		sub, err := q.PatchSubscription(ctx, id, store.SubscriptionPatch{PremiumSourceURL: &premiumURL})
		if err != nil {
			return err
		}
		if err := upsertEpisodes(ctx, q, sub, schedule.RolePremium, result.Items, now); err != nil {
			return err
		}
		return relockAndReschedule(ctx, q, sub, now)
	})
}

// sortOldestFirst orders items by PubDate ascending, falling back to
// feed order (their existing slice position) and then leaving no-date
// items last among ties — see docs §10 "Item with no pubDate".
func sortOldestFirst(items []source.Item) []source.Item {
	out := make([]source.Item, len(items))
	copy(out, items)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].PubDate, out[j].PubDate
		if a == nil && b == nil {
			return false
		}
		if a == nil {
			return false // no-date items sort after dated ones
		}
		if b == nil {
			return true
		}
		return a.Before(*b)
	})
	return out
}

func toNewEpisode(it source.Item, role string, position int, scheduledAt time.Time) store.NewEpisode {
	return store.NewEpisode{
		GUID:            it.GUID,
		SourceRole:      role,
		Position:        position,
		ScheduledAt:     scheduledAt,
		OriginalPubDate: it.PubDate,
		Title:           it.Title,
		Description:     it.Description,
		Link:            it.Link,
		EnclosureURL:    it.EnclosureURL,
		EnclosureType:   it.EnclosureType,
		EnclosureLength: it.EnclosureLength,
		Duration:        it.Duration,
		EpisodeNumber:   it.EpisodeNumber,
		Season:          it.Season,
		EpisodeType:     it.EpisodeType,
		Explicit:        it.Explicit,
		ImageURL:        it.ImageURL,
	}
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Poll performs one refresh cycle for a single subscription, exactly
// as described in docs §7:
//  1. Conditional GET of each of the show's feeds; a 304 everywhere only
//     bumps last_fetched_at.
//  2. On parse failure, record the failure and change nothing else —
//     a broken upstream fetch must never corrupt an existing schedule.
//     A broken *premium* feed is noted in the status but doesn't stop
//     the free feed being ingested.
//  3. Refresh channel_json.
//  4. Upsert episodes per feed: new GUIDs appended at the tail; existing
//     rows get metadata updates but never a new position; GUIDs that
//     vanished from that feed are tombstoned.
//  5. Lock any episode whose scheduled_at has passed.
//  6. Recompute scheduled_at for unlocked, non-excluded episodes.
//
// Everything happens in one transaction.
func Poll(ctx context.Context, st *store.Store, fetcher *source.Fetcher, sub store.Subscription, now time.Time, logger *slog.Logger) error {
	// Validators are per-URL, so a feed we didn't successfully re-fetch
	// keeps the ones it already had.
	state := store.FetchState{
		ETag:                sub.ETag,
		LastModified:        sub.LastModified,
		PremiumETag:         sub.PremiumETag,
		PremiumLastModified: sub.PremiumLastModified,
		FetchedAt:           now,
	}

	result, err := fetcher.Fetch(ctx, sub.SourceURL, deref(sub.ETag), deref(sub.LastModified))
	if err != nil {
		logger.Warn("refresh: fetch failed", "subscription_id", sub.ID, "error", err)
		state.Status = errorStatus(err)
		return st.UpdateFetchState(ctx, sub.ID, state)
	}

	var premium source.FetchResult
	var premiumErr error
	if sub.PremiumSourceURL != nil {
		premium, premiumErr = fetcher.Fetch(ctx, *sub.PremiumSourceURL, deref(sub.PremiumETag), deref(sub.PremiumLastModified))
		if premiumErr != nil {
			logger.Warn("refresh: premium fetch failed", "subscription_id", sub.ID, "error", premiumErr)
		}
	}
	premiumChanged := premiumErr == nil && sub.PremiumSourceURL != nil && !premium.NotModified

	if _, err := time.LoadLocation(sub.Timezone); err != nil {
		logger.Error("refresh: invalid stored timezone", "subscription_id", sub.ID, "timezone", sub.Timezone, "error", err)
		state.Status = errorStatus(errors.New("invalid timezone"))
		return st.UpdateFetchState(ctx, sub.ID, state)
	}

	if !result.NotModified {
		state.ETag, state.LastModified = nilIfEmpty(result.ETag), nilIfEmpty(result.LastModified)
	}
	if premiumChanged {
		state.PremiumETag, state.PremiumLastModified = nilIfEmpty(premium.ETag), nilIfEmpty(premium.LastModified)
	}
	state.Status = statusOK
	if result.NotModified && !premiumChanged {
		state.Status = statusNotModified
	}
	if premiumErr != nil {
		state.Status = premiumErrorStatus(state.Status, premiumErr)
	}

	// Nothing changed upstream: deliberately touch nothing but the fetch
	// state, not even locks (docs §7 step 1). Locking is only meaningful
	// right before a recompute, so it happens there instead.
	if result.NotModified && !premiumChanged {
		return st.UpdateFetchState(ctx, sub.ID, state)
	}

	return st.WithTx(ctx, func(q *store.Queries) error {
		if !result.NotModified {
			if result.PermanentRedirectURL != "" && result.PermanentRedirectURL != sub.SourceURL {
				// Persist the new URL; PatchSubscription doesn't cover
				// source_url so this is a direct, narrow update.
				if err := q.UpdateSourceURL(ctx, sub.ID, schedule.RolePrimary, result.PermanentRedirectURL); err != nil {
					return err
				}
			}

			channelJSON, err := marshalChannel(result.Channel)
			if err != nil {
				return err
			}
			if err := q.UpdateChannelJSON(ctx, sub.ID, channelJSON); err != nil {
				return err
			}

			if err := upsertEpisodes(ctx, q, sub, schedule.RolePrimary, result.Items, now); err != nil {
				return err
			}
		}

		if premiumChanged {
			if premium.PermanentRedirectURL != "" && premium.PermanentRedirectURL != *sub.PremiumSourceURL {
				if err := q.UpdateSourceURL(ctx, sub.ID, schedule.RolePremium, premium.PermanentRedirectURL); err != nil {
					return err
				}
			}
			// The premium feed's channel metadata is deliberately
			// ignored: the show's identity comes from its free feed.
			if err := upsertEpisodes(ctx, q, sub, schedule.RolePremium, premium.Items, now); err != nil {
				return err
			}
		}

		if err := relockAndReschedule(ctx, q, sub, now); err != nil {
			return err
		}

		return q.UpdateFetchState(ctx, sub.ID, state)
	})
}

// upsertEpisodes reconciles one of a show's feeds against the episodes
// already ingested from it: newly seen GUIDs are appended at the tail
// (sorted oldest first among themselves — docs §3.2), existing rows get
// their metadata refreshed without ever touching their position, and any
// GUID that has disappeared from that feed is tombstoned.
//
// Everything is scoped to role: the two feeds are reconciled
// independently, so a premium feed that reuses the free feed's GUIDs
// (the usual shape of an ad-free re-cut) stays a separate set of
// episodes, and a 304 on one feed never tombstones the other's.
func upsertEpisodes(ctx context.Context, q *store.Queries, sub store.Subscription, role string, items []source.Item, now time.Time) error {
	all, err := q.ListBySubscription(ctx, sub.ID)
	if err != nil {
		return err
	}
	var existing []store.Episode
	existingByGUID := make(map[string]store.Episode, len(all))
	for _, e := range all {
		if schedule.NormalizeRole(e.SourceRole) != schedule.NormalizeRole(role) {
			continue
		}
		existing = append(existing, e)
		existingByGUID[e.GUID] = e
	}

	seenInSource := make(map[string]bool, len(items))
	var newItems []source.Item
	for _, it := range items {
		seenInSource[it.GUID] = true
		if existing, ok := existingByGUID[it.GUID]; ok {
			if err := q.UpdateEpisodeMetadata(ctx, existing.ID, toNewEpisode(it, role, existing.Position, existing.ScheduledAt)); err != nil {
				return err
			}
			continue
		}
		newItems = append(newItems, it)
	}

	// New GUIDs are appended at the tail, sorted oldest-first among
	// only themselves — never reshuffled against episodes already ingested.
	newItems = sortOldestFirst(newItems)
	if len(newItems) > 0 {
		nextPosition, err := q.MaxPosition(ctx, sub.ID)
		if err != nil {
			return err
		}
		nextPosition++

		// Schedule the new rows alongside everything already stored:
		// a premium episode's release time depends on the free episode
		// it belongs with, which may be any row in the subscription.
		rows := ScheduleRows(all)
		for i, it := range newItems {
			rows = append(rows, schedule.Episode{Position: nextPosition + i, Role: role, PubDate: it.PubDate})
		}
		planned, err := PlanSchedule(sub, rows)
		if err != nil {
			return err
		}
		for i, it := range newItems {
			at := planned[len(all)+i].ScheduledAt
			if _, err := q.InsertEpisode(ctx, sub.ID, toNewEpisode(it, role, nextPosition+i, at)); err != nil {
				return err
			}
		}
	}

	// Tombstone anything that vanished; clear the tombstone on anything
	// that reappeared (UpdateEpisodeMetadata above already clears it
	// for rows still present, so this only sets it for absentees).
	for _, e := range existing {
		if !seenInSource[e.GUID] && e.MissingSince == nil {
			if err := q.SetMissingSince(ctx, e.ID, &now); err != nil {
				return err
			}
		}
	}

	return nil
}

// relockAndReschedule applies the lock-then-recompute pair from
// internal/schedule to every episode in the subscription. Excluded and
// already-locked rows come back untouched.
func relockAndReschedule(ctx context.Context, q *store.Queries, sub store.Subscription, now time.Time) error {
	all, err := q.ListBySubscription(ctx, sub.ID)
	if err != nil {
		return err
	}

	locked := schedule.ApplyLocks(now, ScheduleRows(all))
	recomputed, err := PlanSchedule(sub, locked)
	if err != nil {
		return err
	}

	for i, r := range recomputed {
		orig := all[i]
		if r.Locked == orig.Locked && r.ScheduledAt.Equal(orig.ScheduledAt) {
			continue
		}
		if err := q.SetSchedule(ctx, orig.ID, r.ScheduledAt, r.Locked); err != nil {
			return err
		}
	}
	return nil
}
