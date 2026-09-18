package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound is returned when a lookup by id or token matches nothing.
var ErrNotFound = errors.New("store: not found")

// NewSubscription is the set of fields needed to create a subscription;
// timestamps, token and id are assigned by the store.
type NewSubscription struct {
	Token           string
	SourceURL       string
	TitleOverride   *string
	CadenceDays     int
	ReleaseTime     string
	Timezone        string
	StartAt         time.Time
	SeedCount       int
	EpisodesPerSlot int
	MaxFeedItems    *int
	ChannelJSON     string
}

func (q *Queries) CreateSubscription(ctx context.Context, n NewSubscription) (Subscription, error) {
	now := time.Now().UTC()
	res, err := q.db.ExecContext(ctx, `
		INSERT INTO subscriptions (
			token, source_url, title_override, cadence_days, release_time,
			timezone, start_at, seed_count, episodes_per_slot, shift_seconds,
			max_feed_items, channel_json, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?)`,
		n.Token, n.SourceURL, nullString(n.TitleOverride), n.CadenceDays, n.ReleaseTime,
		n.Timezone, toDBTime(n.StartAt), n.SeedCount, n.EpisodesPerSlot,
		nullIntFromIntPtr(n.MaxFeedItems), n.ChannelJSON, toDBTime(now), toDBTime(now),
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
	id, token, source_url, title_override, cadence_days, release_time,
	timezone, start_at, seed_count, episodes_per_slot, shift_seconds,
	paused_at, max_feed_items, channel_json, etag, last_modified,
	last_fetched_at, last_fetch_status, created_at, updated_at`

func scanSubscription(row interface {
	Scan(dest ...any) error
}) (Subscription, error) {
	var s Subscription
	var titleOverride, etag, lastModified, lastFetchStatus sql.NullString
	var pausedAt, lastFetchedAt sql.NullString
	var maxFeedItems sql.NullInt64
	var startAt, createdAt, updatedAt string

	err := row.Scan(
		&s.ID, &s.Token, &s.SourceURL, &titleOverride, &s.CadenceDays, &s.ReleaseTime,
		&s.Timezone, &startAt, &s.SeedCount, &s.EpisodesPerSlot, &s.ShiftSeconds,
		&pausedAt, &maxFeedItems, &s.ChannelJSON, &etag, &lastModified,
		&lastFetchedAt, &lastFetchStatus, &createdAt, &updatedAt,
	)
	if err != nil {
		return Subscription{}, err
	}

	s.TitleOverride = stringPtr(titleOverride)
	s.ETag = stringPtr(etag)
	s.LastModified = stringPtr(lastModified)
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
	rows, err := q.db.QueryContext(ctx, `SELECT `+subscriptionColumns+` FROM subscriptions ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list subscriptions: %w", err)
	}
	defer rows.Close()

	var out []Subscription
	for rows.Next() {
		s, err := scanSubscription(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan subscription: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListDueForRefresh returns subscriptions whose last_fetched_at is
// older than the given cutoff (or has never been fetched).
func (q *Queries) ListDueForRefresh(ctx context.Context, cutoff time.Time) ([]Subscription, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT `+subscriptionColumns+` FROM subscriptions
		WHERE last_fetched_at IS NULL OR last_fetched_at < ?
		ORDER BY id`, toDBTime(cutoff))
	if err != nil {
		return nil, fmt.Errorf("store: list due subscriptions: %w", err)
	}
	defer rows.Close()

	var out []Subscription
	for rows.Next() {
		s, err := scanSubscription(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan subscription: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SubscriptionPatch carries only the fields PATCH /admin/subscriptions/{id}
// is allowed to change; nil means "leave alone".
type SubscriptionPatch struct {
	TitleOverride   *string
	CadenceDays     *int
	ReleaseTime     *string
	Timezone        *string
	StartAt         *time.Time
	SeedCount       *int
	EpisodesPerSlot *int
	MaxFeedItems    **int // set to non-nil to change, pointing at nil to clear
}

func (q *Queries) PatchSubscription(ctx context.Context, id int64, p SubscriptionPatch) (Subscription, error) {
	current, err := q.GetSubscription(ctx, id)
	if err != nil {
		return Subscription{}, err
	}

	if p.TitleOverride != nil {
		current.TitleOverride = p.TitleOverride
	}
	if p.CadenceDays != nil {
		current.CadenceDays = *p.CadenceDays
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
			updated_at = ?
		WHERE id = ?`,
		nullString(current.TitleOverride), current.CadenceDays, current.ReleaseTime, current.Timezone,
		toDBTime(current.StartAt), current.SeedCount, current.EpisodesPerSlot, nullIntFromIntPtr(current.MaxFeedItems),
		toDBTime(now), id,
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

// UpdateFetchState records the outcome of a poll attempt.
func (q *Queries) UpdateFetchState(ctx context.Context, id int64, etag, lastModified *string, fetchedAt time.Time, status string) error {
	_, err := q.db.ExecContext(ctx, `
		UPDATE subscriptions SET etag = ?, last_modified = ?, last_fetched_at = ?, last_fetch_status = ?, updated_at = ?
		WHERE id = ?`,
		nullString(etag), nullString(lastModified), toDBTime(fetchedAt), status, toDBTime(fetchedAt), id,
	)
	if err != nil {
		return fmt.Errorf("store: update fetch state %d: %w", id, err)
	}
	return nil
}

// UpdateSourceURL persists a new source URL after a permanent (301/308)
// redirect — feed migrations are routine and shouldn't require manual
// intervention (docs §10).
func (q *Queries) UpdateSourceURL(ctx context.Context, id int64, sourceURL string) error {
	_, err := q.db.ExecContext(ctx, `UPDATE subscriptions SET source_url = ?, updated_at = ? WHERE id = ?`,
		sourceURL, toDBTime(time.Now().UTC()), id)
	if err != nil {
		return fmt.Errorf("store: update source url %d: %w", id, err)
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
