package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/samcolson4/podcastdelay/internal/schedule"
)

// TestMigrate_UpgradesAnExistingDatabase exercises the path a real
// install takes: a database already carrying episodes, migrated forward.
// The premium-feed migration rebuilds the episodes table (SQLite can't
// widen a UNIQUE constraint in place), so "the rows survive it" is worth
// asserting rather than assuming — the SQLite file is the whole of the
// irreplaceable state (docs §9.6).
func TestMigrate_UpgradesAnExistingDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")

	// Build a database at an older schema version by applying only the
	// migrations that existed then.
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?_pragma=foreign_keys(1)", path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE schema_migrations (
		version TEXT PRIMARY KEY, applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"0001_init.sql", "0002_cadence_mode.sql"} {
		stmts, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, string(stmts)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations (version) VALUES (?)`, name); err != nil {
			t.Fatal(err)
		}
	}

	scheduledAt := time.Date(2026, 1, 8, 7, 0, 0, 0, time.UTC)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO subscriptions (token, source_url, cadence_days, release_time, timezone,
			start_at, seed_count, episodes_per_slot, shift_seconds, channel_json, created_at, updated_at)
		VALUES ('legacy', 'https://example.com/feed.xml', 7, '07:00', 'UTC', ?, 1, 1, 0, '{}', ?, ?)`,
		toDBTime(scheduledAt), toDBTime(scheduledAt), toDBTime(scheduledAt)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO episodes (subscription_id, guid, position, scheduled_at, locked, excluded,
			title, enclosure_url, first_seen_at)
		VALUES (1, 'guid-1', 0, ?, 1, 0, 'Ep 1', 'https://cdn.example.com/1.mp3', ?)`,
		toDBTime(scheduledAt), toDBTime(scheduledAt)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Opening the store now runs every outstanding migration.
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("migrate forward: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	sub, err := s.GetSubscriptionByToken(ctx, "legacy")
	if err != nil {
		t.Fatalf("subscription lost in migration: %v", err)
	}
	if sub.PremiumSourceURL != nil {
		t.Errorf("expected no premium feed on a migrated row, got %v", *sub.PremiumSourceURL)
	}

	episodes, err := s.ListBySubscription(ctx, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(episodes) != 1 {
		t.Fatalf("expected the existing episode to survive, got %d", len(episodes))
	}
	e := episodes[0]
	if e.GUID != "guid-1" || e.Position != 0 || !e.Locked || e.Title != "Ep 1" {
		t.Errorf("episode came through the rebuild changed: %+v", e)
	}
	if !e.ScheduledAt.Equal(scheduledAt) {
		t.Errorf("scheduled_at = %v, want %v — an already-released date must never move", e.ScheduledAt, scheduledAt)
	}
	if e.SourceRole != schedule.RolePrimary {
		t.Errorf("existing episodes should be the free feed's: got role %q", e.SourceRole)
	}

	// The rebuilt table is still usable: new rows, the widened GUID
	// constraint, and the feed index all survived.
	if _, err := s.InsertEpisode(ctx, sub.ID, NewEpisode{
		GUID: "guid-1", SourceRole: schedule.RolePremium, Position: 1,
		ScheduledAt: scheduledAt.Add(time.Hour), Title: "Bonus 1",
		EnclosureURL: "https://cdn.example.com/bonus-1.mp3",
	}); err != nil {
		t.Fatalf("insert into the rebuilt table: %v", err)
	}
	if _, err := s.InsertEpisode(ctx, sub.ID, NewEpisode{
		GUID: "guid-1", Position: 2, ScheduledAt: scheduledAt,
		Title: "duplicate", EnclosureURL: "https://cdn.example.com/1.mp3",
	}); err == nil {
		t.Error("expected the free feed's guid-1 to still be unique within its own feed")
	}
}
