// Package source fetches and parses a real podcast RSS/Atom feed. It
// never re-hosts audio: enclosure URLs are kept verbatim, and every
// fetch is polite (conditional GET, an identifying User-Agent).
package source

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/mmcdole/gofeed"
)

const userAgent = "PodcastDelay/1.0 (+https://github.com/samcolson4/podcastdelay)"

// Item is a normalized view of one feed entry, independent of whether
// the source was RSS or Atom.
type Item struct {
	GUID            string
	Title           string
	Description     string
	Link            string
	PubDate         *time.Time
	EnclosureURL    string
	EnclosureType   string
	EnclosureLength *int64
	Duration        string
	EpisodeNumber   *int
	Season          *int
	EpisodeType     string // full | trailer | bonus
	Explicit        *bool
	ImageURL        string
}

// Channel is the show-level metadata cached in subscriptions.channel_json.
type Channel struct {
	Title       string
	Description string
	Link        string
	Language    string
	Author      string
	ImageURL    string
	Explicit    bool
	ItunesType  string
	Categories  []string
	Owner       string
	OwnerEmail  string
}

// FetchResult is what a poll produces. NotModified is true on a 304,
// in which case Channel and Items are zero and the caller should
// change nothing but last_fetched_at.
type FetchResult struct {
	NotModified          bool
	ETag                 string
	LastModified         string
	PermanentRedirectURL string // non-empty when the source answered with a 301/308; caller should persist it
	Channel              Channel
	Items                []Item
}

// Fetcher performs conditional GETs and parses the result.
type Fetcher struct {
	Client *http.Client
}

func NewFetcher() *Fetcher {
	return &Fetcher{Client: &http.Client{Timeout: 30 * time.Second}}
}

// Fetch retrieves sourceURL, sending If-None-Match/If-Modified-Since
// when etag/lastModified are non-empty, and parses the body on a 200.
func (f *Fetcher) Fetch(ctx context.Context, sourceURL, etag, lastModified string) (FetchResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return FetchResult{}, fmt.Errorf("source: build request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/rss+xml, application/atom+xml, application/xml, text/xml")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	if lastModified != "" {
		req.Header.Set("If-Modified-Since", lastModified)
	}

	var permanentRedirectURL string
	client := *f.Client
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		// req.Response is the response that triggered this redirect;
		// a 301/308 is worth persisting so future polls skip the hop
		// (feed migrations are routine — see docs §10).
		if req.Response != nil && (req.Response.StatusCode == http.StatusMovedPermanently || req.Response.StatusCode == http.StatusPermanentRedirect) {
			permanentRedirectURL = req.URL.String()
		}
		if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 redirects")
		}
		return nil
	}

	resp, err := client.Do(req)
	if err != nil {
		return FetchResult{}, fmt.Errorf("source: fetch %s: %w", sourceURL, err)
	}
	defer resp.Body.Close()

	result := FetchResult{
		ETag:                 resp.Header.Get("ETag"),
		LastModified:         resp.Header.Get("Last-Modified"),
		PermanentRedirectURL: permanentRedirectURL,
	}

	switch resp.StatusCode {
	case http.StatusNotModified:
		result.NotModified = true
		return result, nil
	case http.StatusOK:
		// fall through to parse
	default:
		return FetchResult{}, fmt.Errorf("source: fetch %s: unexpected status %s", sourceURL, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return FetchResult{}, fmt.Errorf("source: read body: %w", err)
	}

	parsed, err := Parse(body)
	if err != nil {
		return FetchResult{}, fmt.Errorf("source: parse %s: %w", sourceURL, err)
	}
	result.Channel = parsed.Channel
	result.Items = parsed.Items
	return result, nil
}

type parsed struct {
	Channel Channel
	Items   []Item
}

// Parse turns raw feed bytes into normalized Channel/Items. It uses
// gofeed rather than hand-rolled XML because real-world feeds are
// malformed in inventive ways.
func Parse(body []byte) (parsed, error) {
	fp := gofeed.NewParser()
	feed, err := fp.ParseString(string(body))
	if err != nil {
		return parsed{}, fmt.Errorf("gofeed parse: %w", err)
	}

	ch := Channel{
		Title:       feed.Title,
		Description: feed.Description,
		Link:        feed.Link,
		Language:    feed.Language,
	}
	if feed.Image != nil {
		ch.ImageURL = feed.Image.URL
	}
	if feed.ITunesExt != nil {
		ch.Author = feed.ITunesExt.Author
		ch.Explicit = feed.ITunesExt.Explicit == "true" || feed.ITunesExt.Explicit == "yes"
		ch.ItunesType = feed.ITunesExt.Type
		if feed.ITunesExt.Owner != nil {
			ch.Owner = feed.ITunesExt.Owner.Name
			ch.OwnerEmail = feed.ITunesExt.Owner.Email
		}
		if feed.ITunesExt.Image != "" && ch.ImageURL == "" {
			ch.ImageURL = feed.ITunesExt.Image
		}
	}
	for _, c := range feed.Categories {
		ch.Categories = append(ch.Categories, c)
	}

	items := make([]Item, 0, len(feed.Items))
	seenGUIDs := make(map[string]bool, len(feed.Items))
	for _, fi := range feed.Items {
		guid := fi.GUID
		if guid == "" {
			guid = fi.Link
		}
		if guid == "" {
			// No GUID and no link: fall back to title+pubdate so we
			// still have something stable-ish rather than dropping it.
			guid = fi.Title
		}
		if seenGUIDs[guid] {
			// Duplicate GUID in source: first wins, rest logged by caller.
			continue
		}

		if fi.Enclosures == nil || len(fi.Enclosures) == 0 {
			// Text-only post, no audio: skip, caller may log it.
			continue
		}
		enc := fi.Enclosures[0]

		it := Item{
			GUID:          guid,
			Title:         fi.Title,
			Description:   descriptionOf(fi),
			Link:          fi.Link,
			PubDate:       fi.PublishedParsed,
			EnclosureURL:  enc.URL,
			EnclosureType: enc.Type,
		}
		if enc.Length != "" {
			var l int64
			if _, err := fmt.Sscanf(enc.Length, "%d", &l); err == nil {
				it.EnclosureLength = &l
			}
		}
		if fi.ITunesExt != nil {
			it.Duration = fi.ITunesExt.Duration
			it.EpisodeType = fi.ITunesExt.EpisodeType
			if fi.ITunesExt.Explicit == "true" || fi.ITunesExt.Explicit == "yes" {
				v := true
				it.Explicit = &v
			} else if fi.ITunesExt.Explicit == "false" || fi.ITunesExt.Explicit == "no" {
				v := false
				it.Explicit = &v
			}
			if fi.ITunesExt.Episode != "" {
				var n int
				if _, err := fmt.Sscanf(fi.ITunesExt.Episode, "%d", &n); err == nil {
					it.EpisodeNumber = &n
				}
			}
			if fi.ITunesExt.Season != "" {
				var n int
				if _, err := fmt.Sscanf(fi.ITunesExt.Season, "%d", &n); err == nil {
					it.Season = &n
				}
			}
			if fi.ITunesExt.Image != "" {
				it.ImageURL = fi.ITunesExt.Image
			}
		}
		if it.ImageURL == "" && fi.Image != nil {
			it.ImageURL = fi.Image.URL
		}

		items = append(items, it)
		seenGUIDs[guid] = true
	}

	return parsed{Channel: ch, Items: items}, nil
}

func descriptionOf(fi *gofeed.Item) string {
	if fi.Description != "" {
		return fi.Description
	}
	if fi.Content != "" {
		return fi.Content
	}
	return ""
}
