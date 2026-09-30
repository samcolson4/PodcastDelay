package refresh

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samcolson4/podcastdelay/internal/source"
	"github.com/samcolson4/podcastdelay/internal/store"
)

const threeEpisodeFeed = `<?xml version="1.0"?>
<rss version="2.0" xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd">
<channel>
  <title>Show</title>
  <description>desc</description>
  <item><title>Ep 1</title><guid>g1</guid><pubDate>Mon, 02 Mar 2020 08:00:00 GMT</pubDate>
    <enclosure url="https://cdn.example.com/1.mp3" length="100" type="audio/mpeg"/></item>
  <item><title>Ep 2</title><guid>g2</guid><pubDate>Mon, 09 Mar 2020 08:00:00 GMT</pubDate>
    <enclosure url="https://cdn.example.com/2.mp3" length="100" type="audio/mpeg"/></item>
  <item><title>Ep 3</title><guid>g3</guid><pubDate>Mon, 16 Mar 2020 08:00:00 GMT</pubDate>
    <enclosure url="https://cdn.example.com/3.mp3" length="100" type="audio/mpeg"/></item>
</channel>
</rss>`

func releaseNextSetup(t *testing.T) (*store.Store, store.Subscription, []store.Episode) {
	t.Helper()
	st := openTestStore(t)
	fs := newFeedServer(t, []byte(threeEpisodeFeed))
	ctx := context.Background()
	// Seed 1 released two days ago; ep 2 is due in ~5 days, ep 3 ~12 days.
	sub, err := AddSubscription(ctx, st, source.NewFetcher(), AddParams{
		SourceURL: fs.srv.URL, CadenceDays: 7, ReleaseTime: "07:00", Timezone: "UTC",
		StartAt: time.Now().UTC().Add(-48 * time.Hour), SeedCount: 1, EpisodesPerSlot: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	eps, err := st.ListBySubscription(ctx, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	return st, sub, eps
}

func TestReleaseNext_KeepScheduleLeavesFollowingEpisode(t *testing.T) {
	st, sub, before := releaseNextSetup(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := ReleaseNext(ctx, st, sub.ID, now, false); err != nil {
		t.Fatal(err)
	}
	after, _ := st.ListBySubscription(ctx, sub.ID)
	if !after[1].Locked || after[1].ScheduledAt.After(now) {
		t.Errorf("ep 2 should be released and locked, got %+v", after[1])
	}
	if !after[2].ScheduledAt.Equal(before[2].ScheduledAt) {
		t.Errorf("following episode moved: was %v, now %v", before[2].ScheduledAt, after[2].ScheduledAt)
	}
}

func TestReleaseNext_UpdateScheduleKeepsCadenceFromNow(t *testing.T) {
	st, sub, _ := releaseNextSetup(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := ReleaseNext(ctx, st, sub.ID, now, true); err != nil {
		t.Fatal(err)
	}
	after, _ := st.ListBySubscription(ctx, sub.ID)
	if !after[1].Locked {
		t.Fatalf("ep 2 should be locked, got %+v", after[1])
	}
	d := now.AddDate(0, 0, 7)
	want := time.Date(d.Year(), d.Month(), d.Day(), 7, 0, 0, 0, time.UTC)
	if !after[2].ScheduledAt.Equal(want) {
		t.Errorf("following episode = %v, want %v (one cadence after today at 07:00)", after[2].ScheduledAt, want)
	}
}

func TestReleaseNext_NothingLeft(t *testing.T) {
	st, sub, _ := releaseNextSetup(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for i := 0; i < 2; i++ {
		if err := ReleaseNext(ctx, st, sub.ID, now, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := ReleaseNext(ctx, st, sub.ID, now, false); !errors.Is(err, ErrNothingToRelease) {
		t.Errorf("expected ErrNothingToRelease, got %v", err)
	}
}
