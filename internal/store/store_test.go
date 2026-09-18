package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(context.Background(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSubscriptionCRUD(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	sub, err := s.CreateSubscription(ctx, NewSubscription{
		Token:           "tok123",
		SourceURL:       "https://example.com/feed.xml",
		CadenceDays:     7,
		ReleaseTime:     "07:00",
		Timezone:        "Europe/London",
		StartAt:         time.Date(2026, 1, 1, 7, 0, 0, 0, time.UTC),
		SeedCount:       1,
		EpisodesPerSlot: 1,
		ChannelJSON:     `{"title":"Test Show"}`,
	})
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	if sub.ID == 0 {
		t.Fatal("expected non-zero id")
	}

	got, err := s.GetSubscriptionByToken(ctx, "tok123")
	if err != nil {
		t.Fatalf("get by token: %v", err)
	}
	if got.SourceURL != sub.SourceURL {
		t.Errorf("source url mismatch: got %q", got.SourceURL)
	}

	newCadence := 14
	patched, err := s.PatchSubscription(ctx, sub.ID, SubscriptionPatch{CadenceDays: &newCadence})
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	if patched.CadenceDays != 14 {
		t.Errorf("cadence not patched: got %d", patched.CadenceDays)
	}

	now := time.Now()
	if err := s.PauseSubscription(ctx, sub.ID, now); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := s.ResumeSubscription(ctx, sub.ID, now.Add(10*time.Second)); err != nil {
		t.Fatalf("resume: %v", err)
	}
	resumed, err := s.GetSubscription(ctx, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.ShiftSeconds < 9 || resumed.ShiftSeconds > 11 {
		t.Errorf("expected ~10s shift, got %d", resumed.ShiftSeconds)
	}
	if resumed.PausedAt != nil {
		t.Errorf("expected paused_at cleared, got %v", resumed.PausedAt)
	}

	if err := s.DeleteSubscription(ctx, sub.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetSubscription(ctx, sub.ID); err != ErrNotFound {
		t.Errorf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestEpisodeIngestAndFeedQuery(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	sub, err := s.CreateSubscription(ctx, NewSubscription{
		Token: "tok", SourceURL: "https://example.com/feed.xml",
		CadenceDays: 7, ReleaseTime: "07:00", Timezone: "UTC",
		StartAt:   time.Date(2026, 1, 1, 7, 0, 0, 0, time.UTC),
		SeedCount: 1, EpisodesPerSlot: 1, ChannelJSON: "{}",
	})
	if err != nil {
		t.Fatal(err)
	}

	past := time.Date(2026, 1, 1, 7, 0, 0, 0, time.UTC)
	future := time.Date(2099, 1, 1, 7, 0, 0, 0, time.UTC)

	e1, err := s.InsertEpisode(ctx, sub.ID, NewEpisode{
		GUID: "guid-1", Position: 0, ScheduledAt: past,
		Title: "Ep 1", EnclosureURL: "https://cdn.example.com/1.mp3",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertEpisode(ctx, sub.ID, NewEpisode{
		GUID: "guid-2", Position: 1, ScheduledAt: future,
		Title: "Ep 2", EnclosureURL: "https://cdn.example.com/2.mp3",
	}); err != nil {
		t.Fatal(err)
	}

	max, err := s.MaxPosition(ctx, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if max != 1 {
		t.Errorf("expected max position 1, got %d", max)
	}

	released, err := s.ListReleased(ctx, sub.ID, time.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(released) != 1 || released[0].GUID != "guid-1" {
		t.Fatalf("expected only guid-1 released, got %+v", released)
	}

	// Lock episode 1, then confirm it stays even if we "reschedule" its
	// row to the far future directly via SetSchedule with locked=true —
	// the point being callers, not the query, are responsible for never
	// recomputing locked rows; ListReleased must still honour locked=1
	// regardless of scheduled_at.
	if err := s.SetSchedule(ctx, e1.ID, past, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSchedule(ctx, e1.ID, future, true); err != nil {
		t.Fatal(err)
	}
	released, err = s.ListReleased(ctx, sub.ID, time.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(released) != 1 || released[0].GUID != "guid-1" {
		t.Fatalf("expected locked guid-1 to remain released despite future scheduled_at, got %+v", released)
	}

	// Excluding removes it from the feed without touching position.
	if err := s.SetExcluded(ctx, e1.ID, true); err != nil {
		t.Fatal(err)
	}
	released, err = s.ListReleased(ctx, sub.ID, time.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(released) != 0 {
		t.Fatalf("expected excluded episode hidden, got %+v", released)
	}

	all, err := s.ListBySubscription(ctx, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].Position != 0 || all[1].Position != 1 {
		t.Fatalf("expected 2 episodes in position order, got %+v", all)
	}
}

func TestWithTxRollsBackOnError(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	err := s.WithTx(ctx, func(q *Queries) error {
		_, err := q.CreateSubscription(ctx, NewSubscription{
			Token: "will-rollback", SourceURL: "https://example.com/x.xml",
			CadenceDays: 7, ReleaseTime: "07:00", Timezone: "UTC",
			StartAt: time.Now(), SeedCount: 1, EpisodesPerSlot: 1, ChannelJSON: "{}",
		})
		if err != nil {
			return err
		}
		return context.DeadlineExceeded // force rollback
	})
	if err == nil {
		t.Fatal("expected error from WithTx")
	}

	if _, err := s.GetSubscriptionByToken(ctx, "will-rollback"); err != ErrNotFound {
		t.Errorf("expected rollback to discard the insert, got err=%v", err)
	}
}
