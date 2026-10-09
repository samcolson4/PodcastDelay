package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/samcolson4/podcastdelay/internal/schedule"
)

// ErrNotFound is returned when a lookup by id or token matches nothing.
var ErrNotFound = errors.New("store: not found")

// NewSubscription is the set of fields needed to create a subscription;
// timestamps, token and id are assigned by the store.
type NewSubscription struct {
	Token            string
	SourceURL        string
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
	ChannelJSON      string
}

func (q *Queries) CreateSubscription(ctx context.Context, n NewSubscription) (Subscription, error) {
	now := time.Now().UTC()
	res, err := q.db.ExecContext(ctx, `
		INSERT INTO subscriptions (
			token, source_url, premium_source_url, title_override, cadence_days, release_time,
			timezone, start_at, seed_count, episodes_per_slot, shift_seconds,
			max_feed_items, channel_json, created_at, updated_at, cadence_mode
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?, ?)`,
		n.Token, n.SourceURL, nullString(n.PremiumSourceURL), nullString(n.TitleOverride), n.CadenceDays, n.ReleaseTime,
		n.Timezone, toDBTime(n.StartAt), n.SeedCount, n.EpisodesPerSlot,
		nullIntFromIntPtr(n.MaxFeedItems), n.ChannelJSON, toDBTime(now), toDBTime(now),
		modeOrFixed(n.CadenceMode),
	)
	if err != nil {
		return Subscription{}, fmt.Errorf("store: create subscription: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Subscription{}, fmt.Errorf("store: create subscription: %w", err)
	}
	return q.GetSubscription(ctx, id)
}

const subscriptionColumns = `
	id, token, source_url, premium_source_url, title_override, cadence_days, release_time,
	timezone, start_at, seed_count, episodes_per_slot, shift_seconds,
	paused_at, hidden_at, max_feed_items, channel_json, etag, last_modified,
	premium_etag, premium_last_modified,
	last_fetched_at, last_fetch_status, created_at, updated_at, cadence_mode`

func scanSubscription(row rowScanner) (Subscription, error) {
	var s Subscription
	var titleOverride, etag, lastModified, lastFetchStatus sql.NullString
	var premiumSourceURL, premiumETag, premiumLastModified sql.NullString
	var pausedAt, hiddenAt, lastFetchedAt sql.NullString
	var maxFeedItems sql.NullInt64
	var startAt, createdAt, updatedAt string

	err := row.Scan(
		&s.ID, &s.Token, &s.SourceURL, &premiumSourceURL, &titleOverride, &s.CadenceDays, &s.ReleaseTime,
		&s.Timezone, &startAt, &s.SeedCount, &s.EpisodesPerSlot, &s.ShiftSeconds,
		&pausedAt, &hiddenAt, &maxFeedItems, &s.ChannelJSON, &etag, &lastModified,
		&premiumETag, &premiumLastModified,
		&lastFetchedAt, &lastFetchStatus, &createdAt, &updatedAt, &s.CadenceMode,
	)
	if err != nil {
		return Subscription{}, err
	}

	s.TitleOverride = stringPtr(titleOverride)
	s.PremiumSourceURL = stringPtr(premiumSourceURL)
	s.ETag = stringPtr(etag)
	s.LastModified = stringPtr(lastModified)
	s.PremiumETag = stringPtr(premiumETag)
	s.PremiumLastModified = stringPtr(premiumLastModified)
	s.LastFetchStatus = stringPtr(lastFetchStatus)
	s.MaxFeedItems = intPtrFromNullInt(maxFeedItems)

	if s.StartAt, err = fromDBTime(startAt); err != nil {
		return Subscription{}, fmt.Errorf("store: parse start_at: %w", err)
	}
	if s.CreatedAt, err = fromDBTime(createdAt); err != nil {
		return Subscription{}, fmt.Errorf("store: parse created_at: %w", err)
	}
	if s.UpdatedAt, err = fromDBTime(updatedAt); err != nil {
		return Subscription{}, fmt.Errorf("store: parse updated_at: %w", err)
	}
	if s.PausedAt, err = fromDBTimePtr(pausedAt); err != nil {
		return Subscription{}, fmt.Errorf("store: parse paused_at: %w", err)
	}
	if s.HiddenAt, err = fromDBTimePtr(hiddenAt); err != nil {
		return Subscription{}, fmt.Errorf("store: parse hidden_at: %w", err)
	}
	if s.LastFetchedAt, err = fromDBTimePtr(lastFetchedAt); err != nil {
		return Subscription{}, fmt.Errorf("store: parse last_fetched_at: %w", err)
	}
	return s, nil
}

func (q *Queries) GetSubscription(ctx context.Context, id int64) (Subscription, error) {
	row := q.db.QueryRowContext(ctx, `SELECT `+subscriptionColumns+` FROM subscriptions WHERE id = ?`, id)
	s, err := scanSubscription(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Subscription{}, ErrNotFound
	}
	if err != nil {
		return Subscription{}, fmt.Errorf("store: get subscription %d: %w", id, err)
	}
	return s, nil
}

func (q *Queries) GetSubscriptionByToken(ctx context.Context, token string) (Subscription, error) {
	row := q.db.QueryRowContext(ctx, `SELECT `+subscriptionColumns+` FROM subscriptions WHERE token = ?`, token)
	s, err := scanSubscription(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Subscription{}, ErrNotFound
	}
	if err != nil {
		return Subscription{}, fmt.Errorf("store: get subscription by token: %w", err)
	}
	return s, nil
}

func (q *Queries) ListSubscriptions(ctx context.Context) ([]Subscription, error) {
	subs, err := queryAll(ctx, q.db, scanSubscription, `SELECT `+subscriptionColumns+` FROM subscriptions ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list subscriptions: %w", err)
	}
	return subs, nil
}

// SubscriptionPatch carries only the fields PATCH /admin/subscriptions/{id}
// is allowed to change; nil means "leave alone".
type SubscriptionPatch struct {
	TitleOverride *string
	// PremiumSourceURL is set to non-nil to change the melded premium
	// feed, pointing at nil to unmeld it. Clearing it also clears the
	// premium feed's cached validators, so a later re-meld refetches
	// from scratch.
	PremiumSourceURL **string
	CadenceDays      *int
	CadenceMode      *string
	ReleaseTime      *string
	Timezone         *string
	StartAt          *time.Time
	SeedCount        *int
	EpisodesPerSlot  *int
	MaxFeedItems     **int // set to non-nil to change, pointing at nil to clear
}

func (q *Queries) PatchSubscription(ctx context.Context, id int64, p SubscriptionPatch) (Subscription, error) {
	current, err := q.GetSubscription(ctx, id)
	if err != nil {
		return Subscription{}, err
	}

	if p.TitleOverride != nil {
		current.TitleOverride = p.TitleOverride
	}
	if p.PremiumSourceURL != nil {
		// Conditional-GET validators belong to the URL that issued
		// them, so pointing at a different feed (or none) drops them.
		if next := *p.PremiumSourceURL; !sameString(next, current.PremiumSourceURL) {
			current.PremiumSourceURL = next
			current.PremiumETag, current.PremiumLastModified = nil, nil
		}
	}
	if p.CadenceDays != nil {
		current.CadenceDays = *p.CadenceDays
	}
	if p.CadenceMode != nil {
		current.CadenceMode = modeOrFixed(*p.CadenceMode)
	}
	if p.ReleaseTime != nil {
		current.ReleaseTime = *p.ReleaseTime
	}
	if p.Timezone != nil {
		current.Timezone = *p.Timezone
	}
	if p.StartAt != nil {
		current.StartAt = *p.StartAt
	}
	if p.SeedCount != nil {
		current.SeedCount = *p.SeedCount
	}
	if p.EpisodesPerSlot != nil {
		current.EpisodesPerSlot = *p.EpisodesPerSlot
	}
	if p.MaxFeedItems != nil {
		current.MaxFeedItems = *p.MaxFeedItems
	}

	now := time.Now().UTC()
	_, err = q.db.ExecContext(ctx, `
		UPDATE subscriptions SET
			title_override = ?, cadence_days = ?, release_time = ?, timezone = ?,
			start_at = ?, seed_count = ?, episodes_per_slot = ?, max_feed_items = ?,
			premium_source_url = ?, premium_etag = ?, premium_last_modified = ?,
			updated_at = ?, cadence_mode = ?
		WHERE id = ?`,
		nullString(current.TitleOverride), current.CadenceDays, current.ReleaseTime, current.Timezone,
		toDBTime(current.StartAt), current.SeedCount, current.EpisodesPerSlot, nullIntFromIntPtr(current.MaxFeedItems),
		nullString(current.PremiumSourceURL), nullString(current.PremiumETag), nullString(current.PremiumLastModified),
		toDBTime(now), current.CadenceMode, id,
	)
	if err != nil {
		return Subscription{}, fmt.Errorf("store: patch subscription %d: %w", id, err)
	}
	return q.GetSubscription(ctx, id)
}

func (q *Queries) DeleteSubscription(ctx context.Context, id int64) error {
	res, err := q.db.ExecContext(ctx, `DELETE FROM subscriptions WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete subscription %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: delete subscription %d: %w", id, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// PauseSubscription records the pause instant; ResumeSubscription adds
// the elapsed pause duration onto shift_seconds and clears it. Both are
// how a holiday pushes only the still-unlocked future releases out.
func (q *Queries) PauseSubscription(ctx context.Context, id int64, now time.Time) error {
	sub, err := q.GetSubscription(ctx, id)
	if err != nil {
		return err
	}
	if sub.PausedAt != nil {
		return nil // already paused; idempotent
	}
	_, err = q.db.ExecContext(ctx, `UPDATE subscriptions SET paused_at = ?, updated_at = ? WHERE id = ?`,
		toDBTime(now), toDBTime(now), id)
	if err != nil {
		return fmt.Errorf("store: pause subscription %d: %w", id, err)
	}
	return nil
}

func (q *Queries) ResumeSubscription(ctx context.Context, id int64, now time.Time) error {
	sub, err := q.GetSubscription(ctx, id)
	if err != nil {
		return err
	}
	if sub.PausedAt == nil {
		return nil // not paused; idempotent
	}
	elapsed := int(now.Sub(*sub.PausedAt).Seconds())
	if elapsed < 0 {
		elapsed = 0
	}
	_, err = q.db.ExecContext(ctx, `UPDATE subscriptions SET paused_at = NULL, shift_seconds = shift_seconds + ?, updated_at = ? WHERE id = ?`,
		elapsed, toDBTime(now), id)
	if err != nil {
		return fmt.Errorf("store: resume subscription %d: %w", id, err)
	}
	return nil
}

// HideSubscription tucks a subscription out of the main feeds list.
// Unlike pausing, it has no effect on the schedule or on serving the
// feed — it's purely a dashboard display concern.
func (q *Queries) HideSubscription(ctx context.Context, id int64, now time.Time) error {
	_, err := q.db.ExecContext(ctx, `UPDATE subscriptions SET hidden_at = ?, updated_at = ? WHERE id = ?`,
		toDBTime(now), toDBTime(now), id)
	if err != nil {
		return fmt.Errorf("store: hide subscription %d: %w", id, err)
	}
	return nil
}

func (q *Queries) UnhideSubscription(ctx context.Context, id int64, now time.Time) error {
	_, err := q.db.ExecContext(ctx, `UPDATE subscriptions SET hidden_at = NULL, updated_at = ? WHERE id = ?`,
		toDBTime(now), id)
	if err != nil {
		return fmt.Errorf("store: unhide subscription %d: %w", id, err)
	}
	return nil
}

// AddShiftSeconds moves every non-seeded release by delta seconds
// (negative pulls them earlier). Callers should Reschedule afterwards.
func (q *Queries) AddShiftSeconds(ctx context.Context, id int64, delta int) error {
	_, err := q.db.ExecContext(ctx, `UPDATE subscriptions SET shift_seconds = shift_seconds + ?, updated_at = ? WHERE id = ?`,
		delta, toDBTime(time.Now().UTC()), id)
	if err != nil {
		return fmt.Errorf("store: add shift %d: %w", id, err)
	}
	return nil
}

// FetchState is the outcome of one poll attempt: the conditional-GET
// validators each of the show's feeds handed back, and a single status
// line describing how the attempt went.
type FetchState struct {
	ETag                *string
	LastModified        *string
	PremiumETag         *string
	PremiumLastModified *string
	FetchedAt           time.Time
	Status              string
}

// UpdateFetchState records the outcome of a poll attempt.
func (q *Queries) UpdateFetchState(ctx context.Context, id int64, fs FetchState) error {
	_, err := q.db.ExecContext(ctx, `
		UPDATE subscriptions SET
			etag = ?, last_modified = ?, premium_etag = ?, premium_last_modified = ?,
			last_fetched_at = ?, last_fetch_status = ?, updated_at = ?
		WHERE id = ?`,
		nullString(fs.ETag), nullString(fs.LastModified), nullString(fs.PremiumETag), nullString(fs.PremiumLastModified),
		toDBTime(fs.FetchedAt), fs.Status, toDBTime(fs.FetchedAt), id,
	)
	if err != nil {
		return fmt.Errorf("store: update fetch state %d: %w", id, err)
	}
	return nil
}

// UpdateSourceURL persists a new URL for one of a show's feeds after a
// permanent (301/308) redirect — feed migrations are routine and
// shouldn't require manual intervention (docs §10).
func (q *Queries) UpdateSourceURL(ctx context.Context, id int64, role, sourceURL string) error {
	column := "source_url"
	if schedule.NormalizeRole(role) == schedule.RolePremium {
		column = "premium_source_url"
	}
	_, err := q.db.ExecContext(ctx, `UPDATE subscriptions SET `+column+` = ?, updated_at = ? WHERE id = ?`,
		sourceURL, toDBTime(time.Now().UTC()), id)
	if err != nil {
		return fmt.Errorf("store: update %s url %d: %w", role, id, err)
	}
	return nil
}

// UpdateChannelJSON refreshes the cached show-level metadata.
func (q *Queries) UpdateChannelJSON(ctx context.Context, id int64, channelJSON string) error {
	_, err := q.db.ExecContext(ctx, `UPDATE subscriptions SET channel_json = ?, updated_at = ? WHERE id = ?`,
		channelJSON, toDBTime(time.Now().UTC()), id)
	if err != nil {
		return fmt.Errorf("store: update channel json %d: %w", id, err)
	}
	return nil
}

// sameString compares two optional strings by value.
func sameString(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// modeOrFixed guards the cadence_mode column against unknown values:
// anything we don't recognise behaves as the default fixed cadence.
func modeOrFixed(m string) string {
	if m == schedule.ModeOriginal {
		return schedule.ModeOriginal
	}
	return schedule.ModeFixed
}
