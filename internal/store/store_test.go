package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/samcolson4/podcastdelay/internal/schedule"
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

func TestPremiumEpisodesAreSeparateFromFreeOnes(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	premiumURL := "https://example.com/premium.xml"
	sub, err := s.CreateSubscription(ctx, NewSubscription{
		Token: "tok-premium", SourceURL: "https://example.com/feed.xml",
		PremiumSourceURL: &premiumURL,
		CadenceDays:      7, ReleaseTime: "07:00", Timezone: "UTC",
		StartAt:   time.Date(2026, 1, 1, 7, 0, 0, 0, time.UTC),
		SeedCount: 1, EpisodesPerSlot: 1, ChannelJSON: "{}",
	})
	if err != nil {
		t.Fatal(err)
	}
	if sub.PremiumSourceURL == nil || *sub.PremiumSourceURL != premiumURL {
		t.Fatalf("premium source url not round-tripped: %v", sub.PremiumSourceURL)
	}

	free := time.Date(2026, 1, 1, 7, 0, 0, 0, time.UTC)
	bonus := time.Date(2026, 1, 2, 8, 0, 0, 0, time.UTC)
	// The same GUID in both feeds: an ad-free re-cut of one episode.
	if _, err := s.InsertEpisode(ctx, sub.ID, NewEpisode{
		GUID: "guid-1", Position: 0, ScheduledAt: free,
		Title: "Ep 1", EnclosureURL: "https://cdn.example.com/1.mp3",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertEpisode(ctx, sub.ID, NewEpisode{
		GUID: "guid-1", SourceRole: schedule.RolePremium, Position: 1, ScheduledAt: bonus,
		Title: "Bonus 1", EnclosureURL: "https://cdn.example.com/bonus-1.mp3",
	}); err != nil {
		t.Fatalf("the premium feed's guid-1 should be its own episode: %v", err)
	}

	gotFree, err := s.GetEpisodeByGUID(ctx, sub.ID, schedule.RolePrimary, "guid-1")
	if err != nil {
		t.Fatal(err)
	}
	gotPremium, err := s.GetEpisodeByGUID(ctx, sub.ID, schedule.RolePremium, "guid-1")
	if err != nil {
		t.Fatal(err)
	}
	if gotFree.Title != "Ep 1" || gotPremium.Title != "Bonus 1" {
		t.Errorf("role did not disambiguate the shared guid: %q / %q", gotFree.Title, gotPremium.Title)
	}

	// The rendered feed is ordered by release time, so a premium
	// episode interleaves with the free ones instead of sitting at the
	// top on account of its tail position.
	if _, err := s.InsertEpisode(ctx, sub.ID, NewEpisode{
		GUID: "guid-2", Position: 2, ScheduledAt: time.Date(2026, 1, 8, 7, 0, 0, 0, time.UTC),
		Title: "Ep 2", EnclosureURL: "https://cdn.example.com/2.mp3",
	}); err != nil {
		t.Fatal(err)
	}
	released, err := s.ListReleased(ctx, sub.ID, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), 0)
	if err != nil {
		t.Fatal(err)
	}
	gotOrder := make([]string, 0, len(released))
	for _, e := range released {
		gotOrder = append(gotOrder, e.Title)
	}
	want := []string{"Ep 2", "Bonus 1", "Ep 1"}
	if len(gotOrder) != len(want) {
		t.Fatalf("expected %v, got %v", want, gotOrder)
	}
	for i := range want {
		if gotOrder[i] != want[i] {
			t.Fatalf("feed order = %v, want %v", gotOrder, want)
		}
	}

	// Unmelding drops only the premium feed's episodes.
	if err := s.DeleteEpisodesByRole(ctx, sub.ID, schedule.RolePremium); err != nil {
		t.Fatal(err)
	}
	all, err := s.ListBySubscription(ctx, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("expected the 2 free episodes to remain, got %d", len(all))
	}
	for _, e := range all {
		if e.SourceRole != schedule.RolePrimary {
			t.Errorf("episode %q survived with role %q", e.Title, e.SourceRole)
		}
	}
}

func TestPatchSubscription_PremiumURLChangeDropsItsValidators(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	first := "https://example.com/premium-v1.xml"
	sub, err := s.CreateSubscription(ctx, NewSubscription{
		Token: "tok-validators", SourceURL: "https://example.com/feed.xml",
		PremiumSourceURL: &first,
		CadenceDays:      7, ReleaseTime: "07:00", Timezone: "UTC",
		StartAt: time.Now(), SeedCount: 1, EpisodesPerSlot: 1, ChannelJSON: "{}",
	})
	if err != nil {
		t.Fatal(err)
	}

	etag, lastModified := `"p1"`, "Mon, 02 Mar 2020 08:00:00 GMT"
	if err := s.UpdateFetchState(ctx, sub.ID, FetchState{
		ETag: &etag, LastModified: &lastModified,
		PremiumETag: &etag, PremiumLastModified: &lastModified,
		FetchedAt: time.Now(), Status: "ok",
	}); err != nil {
		t.Fatal(err)
	}

	// Re-patching the same URL leaves the validators alone.
	same := &first
	unchanged, err := s.PatchSubscription(ctx, sub.ID, SubscriptionPatch{PremiumSourceURL: &same})
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.PremiumETag == nil {
		t.Error("re-patching the same premium url dropped its etag")
	}

	second := "https://example.com/premium-v2.xml"
	moved := &second
	patched, err := s.PatchSubscription(ctx, sub.ID, SubscriptionPatch{PremiumSourceURL: &moved})
	if err != nil {
		t.Fatal(err)
	}
	if patched.PremiumSourceURL == nil || *patched.PremiumSourceURL != second {
		t.Fatalf("premium url not patched: %v", patched.PremiumSourceURL)
	}
	if patched.PremiumETag != nil || patched.PremiumLastModified != nil {
		t.Error("validators from the old premium url survived the change")
	}
	// The free feed's own validators are untouched by all of this.
	if patched.ETag == nil || *patched.ETag != etag {
		t.Errorf("free feed etag = %v, want %q", patched.ETag, etag)
	}

	var cleared *string
	unmelded, err := s.PatchSubscription(ctx, sub.ID, SubscriptionPatch{PremiumSourceURL: &cleared})
	if err != nil {
		t.Fatal(err)
	}
	if unmelded.PremiumSourceURL != nil {
		t.Errorf("premium url not cleared: %v", *unmelded.PremiumSourceURL)
	}
}
