package feed

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var updateGolden = os.Getenv("UPDATE_GOLDEN") == "1"

func sampleFeed() Feed {
	loc := time.UTC
	original := time.Date(2020, 3, 2, 8, 0, 0, 0, loc)
	epNum := 1
	season := 1
	explicit := false

	return Feed{
		SelfURL: "https://podcasts.example.com/f/tok123.xml",
		Title:   "Example Weekly Show (Delayed)",
		Channel: Channel{
			Title:       "Example Weekly Show (Delayed)",
			Description: "A test podcast feed used for PodcastDelay's golden tests.",
			Link:        "https://example.com/show",
			Language:    "en-us",
			Author:      "Example Media",
			ImageURL:    "https://example.com/art.jpg",
			Explicit:    false,
			ItunesType:  "episodic",
			Categories:  []string{"Technology"},
			Owner:       "Example Media",
			OwnerEmail:  "hello@example.com",
		},
		Items: []Item{
			{
				GUID:            "podcastdelay:tok123:ep-1",
				Title:           "Episode 1: The Beginning",
				Description:     "Where it all started.",
				Link:            "https://example.com/show/1",
				PubDate:         time.Date(2026, 1, 5, 7, 0, 0, 0, loc),
				OriginalPubDate: &original,
				EnclosureURL:    "https://chtbl.com/track/EXAMPLE/cdn.example.com/1.mp3",
				EnclosureType:   "audio/mpeg",
				EnclosureLength: 1000000,
				Duration:        "00:30:00",
				EpisodeNumber:   &epNum,
				Season:          &season,
				EpisodeType:     "full",
				Explicit:        &explicit,
			},
		},
		LastBuild:    time.Date(2026, 1, 5, 7, 0, 0, 0, loc),
		GeneratorTag: "PodcastDelay",
	}
}

func TestRender_Golden(t *testing.T) {
	got, err := RenderString(sampleFeed())
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	goldenPath := filepath.Join("..", "..", "testdata", "golden_feed.xml")
	if updateGolden {
		if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Skip("golden file updated")
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (run with UPDATE_GOLDEN=1 to create it): %v", err)
	}
	if got != string(want) {
		t.Errorf("rendered feed does not match golden file.\n--- got ---\n%s\n--- want ---\n%s", got, string(want))
	}
}

func TestRender_PubDateIsVirtualNotOriginal(t *testing.T) {
	f := sampleFeed()
	got, err := RenderString(f)
	if err != nil {
		t.Fatal(err)
	}

	item := f.Items[0]
	virtual := item.PubDate.Format(rfc1123Z)
	original := item.OriginalPubDate.Format(rfc1123Z)

	if !strings.Contains(got, "<pubDate>"+virtual+"</pubDate>") {
		t.Errorf("expected virtual pubDate %q in output:\n%s", virtual, got)
	}
	if strings.Contains(got, "<pubDate>"+original+"</pubDate>") {
		t.Errorf("original pubDate %q leaked into <pubDate>, should only appear in description", original)
	}
	if !strings.Contains(got, "Originally published 2 March 2020") {
		t.Errorf("expected original date note in description:\n%s", got)
	}
}

func TestRender_EnclosureURLPassthroughVerbatim(t *testing.T) {
	f := sampleFeed()
	got, err := RenderString(f)
	if err != nil {
		t.Fatal(err)
	}
	// The analytics prefix (chtbl.com/track/...) must survive untouched
	// so the publisher's download still gets counted (§1, §11).
	if !strings.Contains(got, `url="https://chtbl.com/track/EXAMPLE/cdn.example.com/1.mp3"`) {
		t.Errorf("enclosure URL not passed through verbatim:\n%s", got)
	}
}

func TestRender_GUIDPrefixed(t *testing.T) {
	f := sampleFeed()
	got, err := RenderString(f)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `<guid isPermaLink="false">podcastdelay:tok123:ep-1</guid>`) {
		t.Errorf("expected prefixed guid, got:\n%s", got)
	}
}

func TestRender_EscapesUntrustedText(t *testing.T) {
	f := sampleFeed()
	f.Items[0].Title = `Weird & <Title> "quoted"`
	got, err := RenderString(f)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "<Title>") {
		t.Errorf("title not escaped, unescaped angle bracket leaked into XML:\n%s", got)
	}
	if !strings.Contains(got, "Weird &amp; &lt;Title&gt;") {
		t.Errorf("expected escaped title, got:\n%s", got)
	}
}
