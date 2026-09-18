package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// NewEpisode is what the source parser hands the store on first sight
// of a GUID; Position and ScheduledAt are assigned by the ingest logic
// in internal/refresh before this is inserted.
type NewEpisode struct {
	GUID            string
	Position        int
	ScheduledAt     time.Time
	OriginalPubDate *time.Time
	Title           string
	Description     string
	Link            string
	EnclosureURL    string
	EnclosureType   string
	EnclosureLength *int64
	Duration        string
	EpisodeNumber   *int
	Season          *int
	EpisodeType     string
	Explicit        *bool
	ImageURL        string
}

const episodeColumns = `
	id, subscription_id, guid, position, scheduled_at, locked, excluded,
	original_pub_date, title, description, link, enclosure_url,
	enclosure_type, enclosure_length, duration, episode_number, season,
	episode_type, explicit, image_url, first_seen_at, missing_since`

func scanEpisode(row interface {
	Scan(dest ...any) error
}) (Episode, error) {
	var e Episode
	var originalPubDate, missingSince sql.NullString
	var description, link, enclosureType, duration, episodeType, imageURL sql.NullString
	var enclosureLength sql.NullInt64
	var episodeNumber, season sql.NullInt64
	var explicit sql.NullInt64
	var locked, excluded int
	var scheduledAt, firstSeenAt string

	err := row.Scan(
		&e.ID, &e.SubscriptionID, &e.GUID, &e.Position, &scheduledAt, &locked, &excluded,
		&originalPubDate, &e.Title, &description, &link, &e.EnclosureURL,
		&enclosureType, &enclosureLength, &duration, &episodeNumber, &season,
		&episodeType, &explicit, &imageURL, &firstSeenAt, &missingSince,
	)
	if err != nil {
		return Episode{}, err
	}

	e.Locked = locked != 0
	e.Excluded = excluded != 0
	e.Description = description.String
	e.Link = link.String
	e.EnclosureType = enclosureType.String
	e.Duration = duration.String
	e.EpisodeType = episodeType.String
	e.ImageURL = imageURL.String
	e.EnclosureLength = int64Ptr(enclosureLength)
	e.EpisodeNumber = intPtrFromNullInt(episodeNumber)
	e.Season = intPtrFromNullInt(season)
	e.Explicit = boolPtrFromNullInt(explicit)

	if e.ScheduledAt, err = fromDBTime(scheduledAt); err != nil {
		return Episode{}, fmt.Errorf("store: parse scheduled_at: %w", err)
	}
	if e.FirstSeenAt, err = fromDBTime(firstSeenAt); err != nil {
		return Episode{}, fmt.Errorf("store: parse first_seen_at: %w", err)
	}
	if e.OriginalPubDate, err = fromDBTimePtr(originalPubDate); err != nil {
		return Episode{}, fmt.Errorf("store: parse original_pub_date: %w", err)
	}
	if e.MissingSince, err = fromDBTimePtr(missingSince); err != nil {
		return Episode{}, fmt.Errorf("store: parse missing_since: %w", err)
	}
	return e, nil
}

// MaxPosition returns the highest assigned position for a subscription,
// or -1 if it has no episodes yet. New GUIDs are appended after this.
func (q *Queries) MaxPosition(ctx context.Context, subscriptionID int64) (int, error) {
	var max sql.NullInt64
	err := q.db.QueryRowContext(ctx, `SELECT MAX(position) FROM episodes WHERE subscription_id = ?`, subscriptionID).Scan(&max)
	if err != nil {
		return 0, fmt.Errorf("store: max position: %w", err)
	}
	if !max.Valid {
		return -1, nil
	}
	return int(max.Int64), nil
}

func (q *Queries) InsertEpisode(ctx context.Context, subscriptionID int64, n NewEpisode) (Episode, error) {
	now := time.Now().UTC()
	_, err := q.db.ExecContext(ctx, `
		INSERT INTO episodes (
			subscription_id, guid, position, scheduled_at, locked, excluded,
			original_pub_date, title, description, link, enclosure_url,
			enclosure_type, enclosure_length, duration, episode_number, season,
			episode_type, explicit, image_url, first_seen_at
		) VALUES (?, ?, ?, ?, 0, 0, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		subscriptionID, n.GUID, n.Position, toDBTime(n.ScheduledAt),
		toDBTimePtr(n.OriginalPubDate), n.Title, n.Description, n.Link, n.EnclosureURL,
		n.EnclosureType, nullInt64(n.EnclosureLength), n.Duration, nullIntFromIntPtr(n.EpisodeNumber), nullIntFromIntPtr(n.Season),
		n.EpisodeType, nullBoolFromPtr(n.Explicit), n.ImageURL, toDBTime(now),
	)
	if err != nil {
		return Episode{}, fmt.Errorf("store: insert episode %s: %w", n.GUID, err)
	}
	return q.GetEpisodeByGUID(ctx, subscriptionID, n.GUID)
}

func (q *Queries) GetEpisodeByGUID(ctx context.Context, subscriptionID int64, guid string) (Episode, error) {
	row := q.db.QueryRowContext(ctx, `SELECT `+episodeColumns+` FROM episodes WHERE subscription_id = ? AND guid = ?`, subscriptionID, guid)
	e, err := scanEpisode(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Episode{}, ErrNotFound
	}
	if err != nil {
		return Episode{}, fmt.Errorf("store: get episode %s: %w", guid, err)
	}
	return e, nil
}

// ListBySubscription returns every episode (including excluded and
// tombstoned) ordered by ingest position, for admin views and
// rescheduling.
func (q *Queries) ListBySubscription(ctx context.Context, subscriptionID int64) ([]Episode, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT `+episodeColumns+` FROM episodes WHERE subscription_id = ? ORDER BY position`, subscriptionID)
	if err != nil {
		return nil, fmt.Errorf("store: list episodes: %w", err)
	}
	defer rows.Close()

	var out []Episode
	for rows.Next() {
		e, err := scanEpisode(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan episode: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListReleased returns episodes visible in the public feed: not
// excluded, and either already locked or whose scheduled_at has passed.
// Ordered newest-first (by position, which matches ingest/chronological
// order within the source) for typical RSS reader conventions, capped
// at limit when limit > 0.
func (q *Queries) ListReleased(ctx context.Context, subscriptionID int64, now time.Time, limit int) ([]Episode, error) {
	query := `SELECT ` + episodeColumns + ` FROM episodes
		WHERE subscription_id = ? AND excluded = 0 AND (locked = 1 OR scheduled_at <= ?)
		ORDER BY position DESC`
	args := []any{subscriptionID, toDBTime(now)}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := q.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list released episodes: %w", err)
	}
	defer rows.Close()

	var out []Episode
	for rows.Next() {
		e, err := scanEpisode(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan episode: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// UpdateEpisodeMetadata refreshes the mutable, non-scheduling fields
// upstream is allowed to correct (title, description, ...) without
// touching position or schedule.
func (q *Queries) UpdateEpisodeMetadata(ctx context.Context, id int64, n NewEpisode) error {
	_, err := q.db.ExecContext(ctx, `
		UPDATE episodes SET
			original_pub_date = ?, title = ?, description = ?, link = ?, enclosure_url = ?,
			enclosure_type = ?, enclosure_length = ?, duration = ?, episode_number = ?, season = ?,
			episode_type = ?, explicit = ?, image_url = ?, missing_since = NULL
		WHERE id = ?`,
		toDBTimePtr(n.OriginalPubDate), n.Title, n.Description, n.Link, n.EnclosureURL,
		n.EnclosureType, nullInt64(n.EnclosureLength), n.Duration, nullIntFromIntPtr(n.EpisodeNumber), nullIntFromIntPtr(n.Season),
		n.EpisodeType, nullBoolFromPtr(n.Explicit), n.ImageURL, id,
	)
	if err != nil {
		return fmt.Errorf("store: update episode metadata %d: %w", id, err)
	}
	return nil
}

// SetSchedule writes back a recomputed scheduled_at/locked pair; used
// by the refresh loop after internal/schedule computes new values.
func (q *Queries) SetSchedule(ctx context.Context, id int64, scheduledAt time.Time, locked bool) error {
	_, err := q.db.ExecContext(ctx, `UPDATE episodes SET scheduled_at = ?, locked = ? WHERE id = ?`,
		toDBTime(scheduledAt), boolToInt(locked), id)
	if err != nil {
		return fmt.Errorf("store: set schedule %d: %w", id, err)
	}
	return nil
}

// SetMissingSince tombstones a GUID that disappeared from the source
// feed. Passing nil clears it (the GUID reappeared).
func (q *Queries) SetMissingSince(ctx context.Context, id int64, since *time.Time) error {
	_, err := q.db.ExecContext(ctx, `UPDATE episodes SET missing_since = ? WHERE id = ?`, toDBTimePtr(since), id)
	if err != nil {
		return fmt.Errorf("store: set missing_since %d: %w", id, err)
	}
	return nil
}

// SetExcluded toggles whether an episode is rendered in the feed. Its
// position and hence its slot in the schedule never change, so
// excluding it leaves a gap rather than shifting later episodes earlier.
func (q *Queries) SetExcluded(ctx context.Context, id int64, excluded bool) error {
	_, err := q.db.ExecContext(ctx, `UPDATE episodes SET excluded = ? WHERE id = ?`, boolToInt(excluded), id)
	if err != nil {
		return fmt.Errorf("store: set excluded %d: %w", id, err)
	}
	return nil
}
