// Package refresh implements the only background job in the system:
// periodically re-fetching each subscription's source feed, ingesting
// any new episodes at the tail, tombstoning any that vanished, and
// locking/recomputing scheduled_at (docs/IMPLEMENTATION.md §7).
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
	SourceURL       string
	TitleOverride   *string
	CadenceDays     int
	ReleaseTime     string
	Timezone        string
	StartAt         time.Time
	SeedCount       int
	EpisodesPerSlot int
	MaxFeedItems    *int
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

// AddSubscription fetches sourceURL for the first time, creates the
// subscription row, and ingests every item found as freshly seen
// episodes ordered oldest-first (the ingest-order freeze, docs §3.2).
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

	sourceURL := p.SourceURL
	if result.PermanentRedirectURL != "" {
		sourceURL = result.PermanentRedirectURL
	}

	channelJSON, err := marshalChannel(result.Channel)
	if err != nil {
		return store.Subscription{}, err
	}

	loc, err := time.LoadLocation(p.Timezone)
	if err != nil {
		return store.Subscription{}, fmt.Errorf("refresh: invalid timezone %q: %w", p.Timezone, err)
	}

	var sub store.Subscription
	err = st.WithTx(ctx, func(q *store.Queries) error {
		var txErr error
		sub, txErr = q.CreateSubscription(ctx, store.NewSubscription{
			Token:           token,
			SourceURL:       sourceURL,
			TitleOverride:   p.TitleOverride,
			CadenceDays:     p.CadenceDays,
			ReleaseTime:     p.ReleaseTime,
			Timezone:        p.Timezone,
			StartAt:         p.StartAt,
			SeedCount:       p.SeedCount,
			EpisodesPerSlot: p.EpisodesPerSlot,
			MaxFeedItems:    p.MaxFeedItems,
			ChannelJSON:     channelJSON,
		})
		if txErr != nil {
			return txErr
		}

		items := sortOldestFirst(result.Items)
		cfg := schedule.Config{
			StartAt:         p.StartAt,
			CadenceDays:     p.CadenceDays,
			ReleaseTime:     p.ReleaseTime,
			Location:        loc,
			SeedCount:       p.SeedCount,
			EpisodesPerSlot: p.EpisodesPerSlot,
			ShiftSeconds:    0,
		}
		for i, it := range items {
			at, err := cfg.ReleaseAt(i)
			if err != nil {
				return err
			}
			if _, err := q.InsertEpisode(ctx, sub.ID, toNewEpisode(it, i, at)); err != nil {
				return err
			}
		}

		return q.UpdateFetchState(ctx, sub.ID, nilIfEmpty(result.ETag), nilIfEmpty(result.LastModified), time.Now().UTC(), "ok")
	})
	if err != nil {
		return store.Subscription{}, err
	}
	return st.GetSubscription(ctx, sub.ID)
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

func toNewEpisode(it source.Item, position int, scheduledAt time.Time) store.NewEpisode {
	return store.NewEpisode{
		GUID:            it.GUID,
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

// Poll performs one refresh cycle for a single subscription, exactly
// as described in docs §7:
//  1. Conditional GET; a 304 only bumps last_fetched_at.
//  2. On parse failure, record the failure and change nothing else —
//     a broken upstream fetch must never corrupt an existing schedule.
//  3. Refresh channel_json.
//  4. Upsert episodes: new GUIDs appended at the tail; existing rows
//     get metadata updates but never a new position; vanished GUIDs
//     are tombstoned.
//  5. Lock any episode whose scheduled_at has passed.
//  6. Recompute scheduled_at for unlocked, non-excluded episodes.
//
// Everything happens in one transaction.
func Poll(ctx context.Context, st *store.Store, fetcher *source.Fetcher, sub store.Subscription, now time.Time, logger *slog.Logger) error {
	etag := ""
	if sub.ETag != nil {
		etag = *sub.ETag
	}
	lastModified := ""
	if sub.LastModified != nil {
		lastModified = *sub.LastModified
	}

	result, err := fetcher.Fetch(ctx, sub.SourceURL, etag, lastModified)
	if err != nil {
		logger.Warn("refresh: fetch failed", "subscription_id", sub.ID, "error", err)
		return st.UpdateFetchState(ctx, sub.ID, sub.ETag, sub.LastModified, now, "error: "+err.Error())
	}

	if result.NotModified {
		return st.UpdateFetchState(ctx, sub.ID, sub.ETag, sub.LastModified, now, "not_modified")
	}

	loc, err := time.LoadLocation(sub.Timezone)
	if err != nil {
		logger.Error("refresh: invalid stored timezone", "subscription_id", sub.ID, "timezone", sub.Timezone, "error", err)
		return st.UpdateFetchState(ctx, sub.ID, sub.ETag, sub.LastModified, now, "error: invalid timezone")
	}

	return st.WithTx(ctx, func(q *store.Queries) error {
		if result.PermanentRedirectURL != "" && result.PermanentRedirectURL != sub.SourceURL {
			// Persist the new URL; PatchSubscription doesn't cover
			// source_url so this is a direct, narrow update.
			if err := q.UpdateSourceURL(ctx, sub.ID, result.PermanentRedirectURL); err != nil {
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

		if err := upsertEpisodes(ctx, q, sub, result.Items, now, loc); err != nil {
			return err
		}

		if err := relockAndReschedule(ctx, q, sub, now, loc); err != nil {
			return err
		}

		return q.UpdateFetchState(ctx, sub.ID, nilIfEmpty(result.ETag), nilIfEmpty(result.LastModified), now, "ok")
	})
}

// upsertEpisodes appends newly seen GUIDs at the tail (sorted oldest
// first among themselves — docs §3.2), refreshes metadata on existing
// rows without ever touching their position, and tombstones any GUID
// that has disappeared from the source.
func upsertEpisodes(ctx context.Context, q *store.Queries, sub store.Subscription, items []source.Item, now time.Time, loc *time.Location) error {
	existing, err := q.ListBySubscription(ctx, sub.ID)
	if err != nil {
		return err
	}
	existingByGUID := make(map[string]store.Episode, len(existing))
	seenInSource := make(map[string]bool, len(items))
	for _, e := range existing {
		existingByGUID[e.GUID] = e
	}

	var newItems []source.Item
	for _, it := range items {
		seenInSource[it.GUID] = true
		if existing, ok := existingByGUID[it.GUID]; ok {
			if err := q.UpdateEpisodeMetadata(ctx, existing.ID, toNewEpisode(it, existing.Position, existing.ScheduledAt)); err != nil {
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

		cfg := schedule.Config{
			StartAt:         sub.StartAt,
			CadenceDays:     sub.CadenceDays,
			ReleaseTime:     sub.ReleaseTime,
			Location:        loc,
			SeedCount:       sub.SeedCount,
			EpisodesPerSlot: sub.EpisodesPerSlot,
			ShiftSeconds:    sub.ShiftSeconds,
		}
		for _, it := range newItems {
			at, err := cfg.ReleaseAt(nextPosition)
			if err != nil {
				return err
			}
			if _, err := q.InsertEpisode(ctx, sub.ID, toNewEpisode(it, nextPosition, at)); err != nil {
				return err
			}
			nextPosition++
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
// internal/schedule to every non-excluded episode in the subscription.
func relockAndReschedule(ctx context.Context, q *store.Queries, sub store.Subscription, now time.Time, loc *time.Location) error {
	all, err := q.ListBySubscription(ctx, sub.ID)
	if err != nil {
		return err
	}

	rows := make([]schedule.Episode, 0, len(all))
	index := make([]store.Episode, 0, len(all))
	for _, e := range all {
		if e.Excluded {
			continue // leaves a gap rather than being touched at all (docs §10)
		}
		rows = append(rows, schedule.Episode{Position: e.Position, ScheduledAt: e.ScheduledAt, Locked: e.Locked})
		index = append(index, e)
	}

	locked := schedule.ApplyLocks(now, rows)

	cfg := schedule.Config{
		StartAt:         sub.StartAt,
		CadenceDays:     sub.CadenceDays,
		ReleaseTime:     sub.ReleaseTime,
		Location:        loc,
		SeedCount:       sub.SeedCount,
		EpisodesPerSlot: sub.EpisodesPerSlot,
		ShiftSeconds:    sub.ShiftSeconds,
	}
	recomputed, err := schedule.Recompute(cfg, locked)
	if err != nil {
		return err
	}

	for i, r := range recomputed {
		orig := index[i]
		if r.Locked == orig.Locked && r.ScheduledAt.Equal(orig.ScheduledAt) {
			continue
		}
		if err := q.SetSchedule(ctx, orig.ID, r.ScheduledAt, r.Locked); err != nil {
			return err
		}
	}
	return nil
}
