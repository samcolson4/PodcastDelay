package source

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", name))
	if err != nil {
		t.Fatalf("read testdata %s: %v", name, err)
	}
	return b
}

func TestParse_SampleFeed(t *testing.T) {
	body := readTestdata(t, "sample_feed.xml")
	got, err := Parse(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if got.Channel.Title != "Example Weekly Show" {
		t.Errorf("channel title = %q", got.Channel.Title)
	}
	if got.Channel.Author != "Example Media" {
		t.Errorf("channel author = %q", got.Channel.Author)
	}
	if got.Channel.ItunesType != "episodic" {
		t.Errorf("channel itunes:type = %q", got.Channel.ItunesType)
	}

	// 5 items in the source: ep-1, ep-trailer, ep-2, a duplicate ep-2,
	// and a text-only post. Duplicate is dropped (first wins) and the
	// text-only post is dropped (no enclosure) -> 3 items survive.
	if len(got.Items) != 3 {
		t.Fatalf("expected 3 items, got %d: %+v", len(got.Items), got.Items)
	}

	byGUID := map[string]Item{}
	for _, it := range got.Items {
		byGUID[it.GUID] = it
	}

	ep1, ok := byGUID["ep-1"]
	if !ok {
		t.Fatal("missing ep-1")
	}
	if ep1.EnclosureURL != "https://chtbl.com/track/EXAMPLE/cdn.example.com/1.mp3" {
		t.Errorf("ep-1 enclosure url not preserved verbatim: %q", ep1.EnclosureURL)
	}
	if ep1.EpisodeNumber == nil || *ep1.EpisodeNumber != 1 {
		t.Errorf("ep-1 episode number = %v", ep1.EpisodeNumber)
	}
	if ep1.Explicit == nil || *ep1.Explicit != false {
		t.Errorf("ep-1 explicit = %v", ep1.Explicit)
	}
	if ep1.PubDate == nil {
		t.Fatal("ep-1 pubdate not parsed")
	}

	dupe, ok := byGUID["ep-2"]
	if !ok {
		t.Fatal("missing ep-2")
	}
	if dupe.EnclosureLength == nil || *dupe.EnclosureLength != 1100000 {
		t.Errorf("ep-2 enclosure length = %v, want the FIRST occurrence's 1100000", dupe.EnclosureLength)
	}

	trailer, ok := byGUID["ep-trailer"]
	if !ok {
		t.Fatal("missing ep-trailer")
	}
	if trailer.EpisodeType != "trailer" {
		t.Errorf("trailer episode type = %q", trailer.EpisodeType)
	}

	if _, ok := byGUID["ep-notes"]; ok {
		t.Error("text-only item with no enclosure should have been skipped")
	}
}

func TestFetcher_ConditionalGet(t *testing.T) {
	body := readTestdata(t, "sample_feed.xml")
	callCount := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if r.Header.Get("User-Agent") == "" {
			t.Error("expected a User-Agent header")
		}
		if r.Header.Get("If-None-Match") == `"abc"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"abc"`)
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	}))
	defer srv.Close()

	f := NewFetcher()

	res, err := f.Fetch(t.Context(), srv.URL, "", "")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if res.NotModified {
		t.Fatal("expected first fetch to not be 304")
	}
	if len(res.Items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(res.Items))
	}
	if res.ETag != `"abc"` {
		t.Errorf("etag = %q", res.ETag)
	}

	res2, err := f.Fetch(t.Context(), srv.URL, res.ETag, "")
	if err != nil {
		t.Fatalf("conditional fetch: %v", err)
	}
	if !res2.NotModified {
		t.Fatal("expected second fetch with matching ETag to be 304")
	}
	if callCount != 2 {
		t.Errorf("expected 2 requests, got %d", callCount)
	}
}
