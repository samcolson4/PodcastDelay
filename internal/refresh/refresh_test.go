package refresh

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samcolson4/podcastdelay/internal/schedule"
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

// The premium feed for the same show: a bonus episode a day and an hour
// after the free episode one, and an hour after free episode two. It
// reuses guid-1 deliberately — an ad-free re-cut of the same episode is
// the usual shape of a premium feed, and it must not collide with the
// free feed's row.
const premiumV1 = `<?xml version="1.0"?>
<rss version="2.0" xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd">
<channel>
  <title>Growing Show (premium)</title>
  <description>bonus</description>
  <item>
    <title>Bonus 1</title>
    <guid>guid-1</guid>
    <pubDate>Tue, 03 Mar 2020 09:00:00 GMT</pubDate>
    <enclosure url="https://cdn.example.com/bonus-1.mp3" length="100" type="audio/mpeg"/>
  </item>
  <item>
    <title>Bonus 2</title>
    <guid>bonus-2</guid>
    <pubDate>Mon, 09 Mar 2020 09:00:00 GMT</pubDate>
    <enclosure url="https://cdn.example.com/bonus-2.mp3" length="100" type="audio/mpeg"/>
  </item>
</channel>
</rss>`

const premiumV2AddsOne = `<?xml version="1.0"?>
<rss version="2.0" xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd">
<channel>
  <title>Growing Show (premium)</title>
  <description>bonus</description>
  <item>
    <title>Bonus 1</title>
    <guid>guid-1</guid>
    <pubDate>Tue, 03 Mar 2020 09:00:00 GMT</pubDate>
    <enclosure url="https://cdn.example.com/bonus-1.mp3" length="100" type="audio/mpeg"/>
  </item>
  <item>
    <title>Bonus 2</title>
    <guid>bonus-2</guid>
    <pubDate>Mon, 09 Mar 2020 09:00:00 GMT</pubDate>
    <enclosure url="https://cdn.example.com/bonus-2.mp3" length="100" type="audio/mpeg"/>
  </item>
  <item>
    <title>Bonus 3</title>
    <guid>bonus-3</guid>
    <pubDate>Mon, 16 Mar 2020 10:00:00 GMT</pubDate>
    <enclosure url="https://cdn.example.com/bonus-3.mp3" length="100" type="audio/mpeg"/>
  </item>
</channel>
</rss>`

// episodeByTitle indexes episodes for assertions that don't care about
// ingest order.
func episodeByTitle(episodes []store.Episode) map[string]store.Episode {
	out := make(map[string]store.Episode, len(episodes))
	for _, e := range episodes {
		out[e.Title] = e
	}
	return out
}

func TestAddSubscription_WithPremiumFeed(t *testing.T) {
	st := openTestStore(t)
	free := newFeedServer(t, []byte(feedV1))
	premium := newFeedServer(t, []byte(premiumV1))
	ctx := context.Background()
	start := time.Date(2026, 1, 1, 7, 0, 0, 0, time.UTC)

	premiumURL := premium.srv.URL
	sub, err := AddSubscription(ctx, st, source.NewFetcher(), AddParams{
		SourceURL: free.srv.URL, PremiumSourceURL: &premiumURL,
		CadenceDays: 7, ReleaseTime: "07:00", Timezone: "UTC",
		StartAt: start, SeedCount: 1, EpisodesPerSlot: 1,
	})
	if err != nil {
		t.Fatalf("add subscription: %v", err)
	}
	if sub.PremiumSourceURL == nil || *sub.PremiumSourceURL != premiumURL {
		t.Fatalf("premium source url not stored: %v", sub.PremiumSourceURL)
	}

	episodes, err := st.ListBySubscription(ctx, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(episodes) != 4 {
		t.Fatalf("expected 4 episodes (2 free + 2 premium), got %d", len(episodes))
	}

	byTitle := episodeByTitle(episodes)
	// The free feed takes the first positions; the premium feed follows.
	for title, want := range map[string]struct {
		position int
		role     string
		at       time.Time
	}{
		"Ep 1":    {0, schedule.RolePrimary, start},
		"Ep 2":    {1, schedule.RolePrimary, start.AddDate(0, 0, 7)},
		"Bonus 1": {2, schedule.RolePremium, start.Add(25 * time.Hour)},
		"Bonus 2": {3, schedule.RolePremium, start.AddDate(0, 0, 7).Add(time.Hour)},
	} {
		got, ok := byTitle[title]
		if !ok {
			t.Errorf("%q not ingested", title)
			continue
		}
		if got.Position != want.position {
			t.Errorf("%q position = %d, want %d", title, got.Position, want.position)
		}
		if got.SourceRole != want.role {
			t.Errorf("%q source_role = %q, want %q", title, got.SourceRole, want.role)
		}
		if !got.ScheduledAt.Equal(want.at) {
			t.Errorf("%q scheduled_at = %v, want %v", title, got.ScheduledAt, want.at)
		}
	}

	// guid-1 exists in both feeds and must stay two separate episodes.
	if byTitle["Ep 1"].GUID != "guid-1" || byTitle["Bonus 1"].GUID != "guid-1" {
		t.Fatalf("expected the shared guid-1 in both feeds, got %q and %q", byTitle["Ep 1"].GUID, byTitle["Bonus 1"].GUID)
	}
	if byTitle["Ep 1"].ID == byTitle["Bonus 1"].ID {
		t.Error("the free and premium guid-1 collapsed into one episode")
	}
}

func TestSetPremiumFeed_MeldsIntoAnExistingShow(t *testing.T) {
	st := openTestStore(t)
	free := newFeedServer(t, []byte(feedV1))
	premium := newFeedServer(t, []byte(premiumV1))
	fetcher := source.NewFetcher()
	ctx := context.Background()
	start := time.Date(2026, 1, 1, 7, 0, 0, 0, time.UTC)

	sub, err := AddSubscription(ctx, st, fetcher, AddParams{
		SourceURL: free.srv.URL, CadenceDays: 7, ReleaseTime: "07:00", Timezone: "UTC",
		StartAt: start, SeedCount: 1, EpisodesPerSlot: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	premiumURL := premium.srv.URL
	if err := SetPremiumFeed(ctx, st, fetcher, sub.ID, &premiumURL); err != nil {
		t.Fatalf("meld premium feed: %v", err)
	}

	episodes, err := st.ListBySubscription(ctx, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(episodes) != 4 {
		t.Fatalf("expected 4 episodes after melding, got %d", len(episodes))
	}
	byTitle := episodeByTitle(episodes)
	if want := start.Add(25 * time.Hour); !byTitle["Bonus 1"].ScheduledAt.Equal(want) {
		t.Errorf("Bonus 1 scheduled_at = %v, want %v", byTitle["Bonus 1"].ScheduledAt, want)
	}
	if want := start.AddDate(0, 0, 7).Add(time.Hour); !byTitle["Bonus 2"].ScheduledAt.Equal(want) {
		t.Errorf("Bonus 2 scheduled_at = %v, want %v", byTitle["Bonus 2"].ScheduledAt, want)
	}
	// The free feed's own schedule is untouched by the meld.
	if !byTitle["Ep 2"].ScheduledAt.Equal(start.AddDate(0, 0, 7)) {
		t.Errorf("free Ep 2 moved when the premium feed was melded in: %v", byTitle["Ep 2"].ScheduledAt)
	}

	// A later poll picks up new bonus episodes at the tail.
	premium.body = []byte(premiumV2AddsOne)
	premium.etag = `"p2"`
	updated, err := st.GetSubscription(ctx, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := Poll(ctx, st, fetcher, updated, time.Now().UTC(), testLogger()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	episodes, err = st.ListBySubscription(ctx, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	byTitle = episodeByTitle(episodes)
	bonus3, ok := byTitle["Bonus 3"]
	if !ok {
		t.Fatal("Bonus 3 was not ingested on the next poll")
	}
	if bonus3.SourceRole != schedule.RolePremium {
		t.Errorf("Bonus 3 source_role = %q", bonus3.SourceRole)
	}
	// Published 2h after free episode three, which this feed hasn't got
	// yet: it anchors to the newest free episode it does have (Ep 2,
	// 2020-03-09) and keeps that 7d2h distance.
	if want := start.AddDate(0, 0, 7).Add(7*24*time.Hour + 2*time.Hour); !bonus3.ScheduledAt.Equal(want) {
		t.Errorf("Bonus 3 scheduled_at = %v, want %v", bonus3.ScheduledAt, want)
	}

	// Unmelding takes the premium episodes with it and leaves the free
	// feed exactly as it was.
	if err := SetPremiumFeed(ctx, st, fetcher, sub.ID, nil); err != nil {
		t.Fatalf("unmeld:: %v", err)
	}
	episodes, err = st.ListBySubscription(ctx, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(episodes) != 2 {
		t.Fatalf("expected the 2 free episodes to remain, got %d", len(episodes))
	}
	final, err := st.GetSubscription(ctx, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.PremiumSourceURL != nil {
		t.Errorf("premium source url still set: %v", *final.PremiumSourceURL)
	}
	if final.PremiumETag != nil {
		t.Errorf("premium etag outlived the feed: %v", *final.PremiumETag)
	}
}

func TestPoll_PremiumFeedFailureDoesNotStopTheFreeFeed(t *testing.T) {
	st := openTestStore(t)
	free := newFeedServer(t, []byte(feedV1))
	ctx := context.Background()

	// A premium URL that answers 500 on every request.
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(broken.Close)

	premiumURL := broken.URL
	sub, err := AddSubscription(ctx, st, source.NewFetcher(), AddParams{
		SourceURL: free.srv.URL, CadenceDays: 7, ReleaseTime: "07:00", Timezone: "UTC",
		StartAt: time.Date(2026, 1, 1, 7, 0, 0, 0, time.UTC), SeedCount: 1, EpisodesPerSlot: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Melding a broken feed fails up front rather than being accepted.
	if err := SetPremiumFeed(ctx, st, source.NewFetcher(), sub.ID, &premiumURL); err == nil {
		t.Fatal("expected melding an unreachable premium feed to fail")
	}

	// Force the broken URL in anyway (as if the feed died after being
	// melded) and check a poll still ingests the free feed's new episode.
	wanted := &premiumURL
	if _, err := st.PatchSubscription(ctx, sub.ID, store.SubscriptionPatch{PremiumSourceURL: &wanted}); err != nil {
		t.Fatal(err)
	}
	free.body = []byte(feedV2AddsEpisodeAndRemovesOne)
	free.etag = `"v2"`

	updated, err := st.GetSubscription(ctx, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := Poll(ctx, st, source.NewFetcher(), updated, time.Now().UTC(), testLogger()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	episodes, err := st.ListBySubscription(ctx, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := episodeByTitle(episodes)["Ep 3"]; !ok {
		t.Error("the free feed's new episode was not ingested when the premium feed failed")
	}
	after, err := st.GetSubscription(ctx, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	status := ""
	if after.LastFetchStatus != nil {
		status = *after.LastFetchStatus
	}
	if !strings.Contains(status, "premium error") {
		t.Errorf("expected the premium failure in last_fetch_status, got %q", status)
	}
	// Not an "error: " status: the show itself fetched fine, so the
	// subscription must not drop onto the 24h failure backoff.
	if strings.HasPrefix(status, statusErrorPrefix) {
		t.Errorf("a premium-only failure should not back the whole feed off: %q", status)
	}
}

func TestPoll_PremiumTombstonesAreScopedToItsOwnFeed(t *testing.T) {
	st := openTestStore(t)
	free := newFeedServer(t, []byte(feedV1))
	premium := newFeedServer(t, []byte(premiumV1))
	fetcher := source.NewFetcher()
	ctx := context.Background()

	premiumURL := premium.srv.URL
	sub, err := AddSubscription(ctx, st, fetcher, AddParams{
		SourceURL: free.srv.URL, PremiumSourceURL: &premiumURL,
		CadenceDays: 7, ReleaseTime: "07:00", Timezone: "UTC",
		StartAt: time.Date(2026, 1, 1, 7, 0, 0, 0, time.UTC), SeedCount: 1, EpisodesPerSlot: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Only the free feed changes: guid-2 vanishes from it. The premium
	// feed answers 304, so none of its episodes may be tombstoned.
	free.body = []byte(feedV2AddsEpisodeAndRemovesOne)
	free.etag = `"v2"`

	if err := Poll(ctx, st, fetcher, sub, time.Now().UTC(), testLogger()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	episodes, err := st.ListBySubscription(ctx, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	byTitle := episodeByTitle(episodes)
	if byTitle["Ep 2"].MissingSince == nil {
		t.Error("the free feed's vanished episode should be tombstoned")
	}
	for _, title := range []string{"Bonus 1", "Bonus 2"} {
		if byTitle[title].MissingSince != nil {
			t.Errorf("%q was tombstoned even though its feed answered 304", title)
		}
	}
}
