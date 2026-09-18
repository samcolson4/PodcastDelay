package web

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/http"
	"strings"
	"time"

	"github.com/samcolson4/podcastdelay/internal/feed"
	"github.com/samcolson4/podcastdelay/internal/store"
)

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.Ping(r.Context()); err != nil {
		http.Error(w, "unhealthy", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

// feedETag derives an ETag from (updated_at, highest released
// position) as docs §3.8 specifies, so podcast apps polling
// aggressively get cheap 304s.
func feedETag(sub store.Subscription, highestPosition int) string {
	h := fnv.New64a()
	fmt.Fprintf(h, "%d:%d", sub.UpdatedAt.UnixNano(), highestPosition)
	return fmt.Sprintf(`"%x"`, h.Sum64())
}

func (s *Server) handleFeed(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSuffix(r.PathValue("token"), ".xml")

	sub, err := s.Store.GetSubscriptionByToken(r.Context(), token)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	now := time.Now().UTC()
	limit := 0
	if sub.MaxFeedItems != nil {
		limit = *sub.MaxFeedItems
	}
	episodes, err := s.Store.ListReleased(r.Context(), sub.ID, now, limit)
	if err != nil {
		s.Logger.Error("feed: list released episodes failed", "subscription_id", sub.ID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	highestPosition := -1
	var lastBuild time.Time
	for _, e := range episodes {
		if e.Position > highestPosition {
			highestPosition = e.Position
		}
		if e.ScheduledAt.After(lastBuild) {
			lastBuild = e.ScheduledAt
		}
	}
	if lastBuild.IsZero() {
		lastBuild = sub.UpdatedAt
	}

	etag := feedETag(sub, highestPosition)
	if match := r.Header.Get("If-None-Match"); match != "" && match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	var meta store.ChannelMeta
	if err := json.Unmarshal([]byte(sub.ChannelJSON), &meta); err != nil {
		s.Logger.Error("feed: invalid channel_json", "subscription_id", sub.ID, "error", err)
	}

	title := meta.Title + " (Delayed)"
	if sub.TitleOverride != nil && *sub.TitleOverride != "" {
		title = *sub.TitleOverride
	}

	items := make([]feed.Item, 0, len(episodes))
	for _, e := range episodes {
		items = append(items, feed.Item{
			GUID:            fmt.Sprintf("podcastdelay:%s:%s", sub.Token, e.GUID),
			Title:           e.Title,
			Description:     e.Description,
			Link:            e.Link,
			PubDate:         e.ScheduledAt,
			OriginalPubDate: e.OriginalPubDate,
			EnclosureURL:    e.EnclosureURL,
			EnclosureType:   e.EnclosureType,
			EnclosureLength: derefInt64(e.EnclosureLength),
			Duration:        e.Duration,
			EpisodeNumber:   e.EpisodeNumber,
			Season:          e.Season,
			EpisodeType:     e.EpisodeType,
			Explicit:        e.Explicit,
			ImageURL:        e.ImageURL,
		})
	}

	f := feed.Feed{
		SelfURL: s.BaseURL + "/f/" + sub.Token + ".xml",
		Title:   title,
		Channel: feed.Channel{
			Title:       title,
			Description: meta.Description,
			Link:        meta.Link,
			Language:    meta.Language,
			Author:      meta.Author,
			ImageURL:    meta.ImageURL,
			Explicit:    meta.Explicit,
			ItunesType:  meta.ItunesType,
			Categories:  meta.Categories,
			Owner:       meta.Owner,
			OwnerEmail:  meta.OwnerEmail,
		},
		Items:        items,
		LastBuild:    lastBuild,
		GeneratorTag: "PodcastDelay",
	}

	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	w.Header().Set("ETag", etag)
	if err := feed.Render(w, f); err != nil {
		s.Logger.Error("feed: render failed", "subscription_id", sub.ID, "error", err)
	}
}

func derefInt64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}
