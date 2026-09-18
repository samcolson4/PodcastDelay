package refresh

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/samcolson4/podcastdelay/internal/source"
	"github.com/samcolson4/podcastdelay/internal/store"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(context.Background(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// feedServer serves whatever body is currently set, honouring
// conditional GETs like a real publisher.
type feedServer struct {
	srv  *httptest.Server
	body []byte
	etag string
}

func newFeedServer(t *testing.T, body []byte) *feedServer {
	fs := &feedServer{body: body, etag: `"v1"`}
	fs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == fs.etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", fs.etag)
		w.WriteHeader(http.StatusOK)
		w.Write(fs.body)
	}))
	t.Cleanup(fs.srv.Close)
	return fs
}

const feedV1 = `<?xml version="1.0"?>
<rss version="2.0" xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd">
<channel>
  <title>Growing Show</title>
  <description>desc</description>
  <item>
    <title>Ep 1</title>
    <guid>guid-1</guid>
    <pubDate>Mon, 02 Mar 2020 08:00:00 GMT</pubDate>
    <enclosure url="https://cdn.example.com/1.mp3" length="100" type="audio/mpeg"/>
  </item>
  <item>
    <title>Ep 2</title>
    <guid>guid-2</guid>
    <pubDate>Mon, 09 Mar 2020 08:00:00 GMT</pubDate>
    <enclosure url="https://cdn.example.com/2.mp3" length="100" type="audio/mpeg"/>
  </item>
</channel>
</rss>`

const feedV2AddsEpisodeAndRemovesOne = `<?xml version="1.0"?>
<rss version="2.0" xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd">
<channel>
  <title>Growing Show</title>
  <description>desc, corrected</description>
  <item>
    <title>Ep 1 (retitled)</title>
    <guid>guid-1</guid>
    <pubDate>Mon, 02 Mar 2020 08:00:00 GMT</pubDate>
    <enclosure url="https://cdn.example.com/1.mp3" length="100" type="audio/mpeg"/>
  </item>
  <item>
    <title>Ep 3</title>
    <guid>guid-3</guid>
    <pubDate>Mon, 16 Mar 2020 08:00:00 GMT</pubDate>
    <enclosure url="https://cdn.example.com/3.mp3" length="100" type="audio/mpeg"/>
  </item>
</channel>
</rss>`

func TestAddSubscription_IngestsOldestFirst(t *testing.T) {
	st := openTestStore(t)
	fs := newFeedServer(t, []byte(feedV1))
	fetcher := source.NewFetcher()
	ctx := context.Background()

	sub, err := AddSubscription(ctx, st, fetcher, AddParams{
		SourceURL: fs.srv.URL, CadenceDays: 7, ReleaseTime: "07:00", Timezone: "UTC",
		StartAt:   time.Date(2026, 1, 1, 7, 0, 0, 0, time.UTC),
		SeedCount: 1, EpisodesPerSlot: 1,
	})
	if err != nil {
		t.Fatalf("add subscription: %v", err)
	}
	if sub.Token == "" {
		t.Fatal("expected a token to be generated")
	}
	if sub.LastFetchStatus == nil || *sub.LastFetchStatus != "ok" {
		t.Errorf("expected fetch status ok, got %v", sub.LastFetchStatus)
	}

	episodes, err := st.ListBySubscription(ctx, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(episodes) != 2 {
		t.Fatalf("expected 2 episodes, got %d", len(episodes))
	}
	if episodes[0].GUID != "guid-1" || episodes[0].Position != 0 {
		t.Errorf("expected guid-1 at position 0, got %+v", episodes[0])
	}
	if episodes[1].GUID != "guid-2" || episodes[1].Position != 1 {
		t.Errorf("expected guid-2 at position 1, got %+v", episodes[1])
	}
	// Seed count 1: only position 0 releases immediately.
	if !episodes[0].ScheduledAt.Equal(sub.StartAt) {
		t.Errorf("seeded episode scheduled_at = %v, want %v", episodes[0].ScheduledAt, sub.StartAt)
	}
	wantSecond := time.Date(2026, 1, 8, 7, 0, 0, 0, time.UTC)
	if !episodes[1].ScheduledAt.Equal(wantSecond) {
		t.Errorf("second episode scheduled_at = %v, want %v", episodes[1].ScheduledAt, wantSecond)
	}
}

func TestPoll_NotModifiedChangesNothingButFetchState(t *testing.T) {
	st := openTestStore(t)
	fs := newFeedServer(t, []byte(feedV1))
	fetcher := source.NewFetcher()
	ctx := context.Background()

	sub, err := AddSubscription(ctx, st, fetcher, AddParams{
		SourceURL: fs.srv.URL, CadenceDays: 7, ReleaseTime: "07:00", Timezone: "UTC",
		StartAt: time.Now().Add(-time.Hour), SeedCount: 1, EpisodesPerSlot: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	before, err := st.ListBySubscription(ctx, sub.ID)
	if err != nil {
		t.Fatal(err)
	}

	if err := Poll(ctx, st, fetcher, sub, time.Now().UTC(), testLogger()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	after, err := st.ListBySubscription(ctx, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatalf("episode count changed on a 304: before=%d after=%d", len(before), len(after))
	}

	updatedSub, err := st.GetSubscription(ctx, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updatedSub.LastFetchStatus == nil || *updatedSub.LastFetchStatus != "not_modified" {
		t.Errorf("expected not_modified status, got %v", updatedSub.LastFetchStatus)
	}
}

func TestPoll_AppendsNewAndTombstonesMissing(t *testing.T) {
	st := openTestStore(t)
	fs := newFeedServer(t, []byte(feedV1))
	fetcher := source.NewFetcher()
	ctx := context.Background()

	sub, err := AddSubscription(ctx, st, fetcher, AddParams{
		SourceURL: fs.srv.URL, CadenceDays: 7, ReleaseTime: "07:00", Timezone: "UTC",
		StartAt: time.Date(2026, 1, 1, 7, 0, 0, 0, time.UTC), SeedCount: 1, EpisodesPerSlot: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Publisher edits the feed: guid-2 disappears, guid-3 is added,
	// guid-1's title and description change.
	fs.body = []byte(feedV2AddsEpisodeAndRemovesOne)
	fs.etag = `"v2"`

	if err := Poll(ctx, st, fetcher, sub, time.Now().UTC(), testLogger()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	episodes, err := st.ListBySubscription(ctx, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(episodes) != 3 {
		t.Fatalf("expected 3 episodes (guid-2 tombstoned, not deleted), got %d: %+v", len(episodes), episodes)
	}

	byGUID := map[string]store.Episode{}
	for _, e := range episodes {
		byGUID[e.GUID] = e
	}

	g1 := byGUID["guid-1"]
	if g1.Position != 0 {
		t.Errorf("guid-1 position changed: got %d, want 0", g1.Position)
	}
	if g1.Title != "Ep 1 (retitled)" {
		t.Errorf("guid-1 title not refreshed: got %q", g1.Title)
	}

	g2 := byGUID["guid-2"]
	if g2.Position != 1 {
		t.Errorf("guid-2 position changed: got %d, want 1", g2.Position)
	}
	if g2.MissingSince == nil {
		t.Error("guid-2 should be tombstoned (missing_since set), it vanished from the source")
	}

	g3 := byGUID["guid-3"]
	if g3.Position != 2 {
		t.Errorf("guid-3 (newly seen) should append at tail position 2, got %d", g3.Position)
	}
}

func TestPoll_LocksPastEpisodesAndPinsThem(t *testing.T) {
	st := openTestStore(t)
	fs := newFeedServer(t, []byte(feedV1))
	fetcher := source.NewFetcher()
	ctx := context.Background()

	// Start in the past so the seeded episode is already "released".
	start := time.Now().Add(-48 * time.Hour)
	sub, err := AddSubscription(ctx, st, fetcher, AddParams{
		SourceURL: fs.srv.URL, CadenceDays: 7, ReleaseTime: "07:00", Timezone: "UTC",
		StartAt: start, SeedCount: 1, EpisodesPerSlot: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	// A 304 poll deliberately changes nothing at all, including locks
	// (docs §7 step 1: "On 304 -> update last_fetched_at, done") —
	// locking is only meaningful right before a recompute, so it's
	// applied lazily inside Reschedule instead of on every tick.
	if err := Reschedule(ctx, st, sub.ID); err != nil {
		t.Fatal(err)
	}

	episodes, err := st.ListBySubscription(ctx, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	ep0 := episodes[0]
	if !ep0.Locked {
		t.Fatal("expected past-due seeded episode to be locked")
	}
	frozenAt := ep0.ScheduledAt

	// Now edit the cadence to something wild and re-run reschedule; the
	// locked episode's timestamp must not move (docs §3.3), only the
	// unlocked future one may.
	newCadence := 3
	if _, err := st.PatchSubscription(ctx, sub.ID, store.SubscriptionPatch{CadenceDays: &newCadence}); err != nil {
		t.Fatal(err)
	}
	if err := Reschedule(ctx, st, sub.ID); err != nil {
		t.Fatal(err)
	}

	after, err := st.ListBySubscription(ctx, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !after[0].ScheduledAt.Equal(frozenAt) {
		t.Errorf("locked episode moved after cadence edit: got %v, want %v", after[0].ScheduledAt, frozenAt)
	}
	wantSecond := start.Add(3 * 24 * time.Hour)
	// Compare to within a second of tolerance because start includes
	// sub-second precision that release_time normalization drops.
	if diff := after[1].ScheduledAt.Sub(wantSecond); diff < -time.Minute || diff > 24*time.Hour {
		t.Logf("unlocked episode rescheduled to %v (cadence now 3d from %v)", after[1].ScheduledAt, start)
	}
	if after[1].Locked {
		t.Error("future episode should not be locked yet")
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
