package web

import (
	"fmt"
	"hash/fnv"
	"net/http"
	"strings"
	"time"

	"github.com/samcolson4/podcastdelay/internal/feed"
	"github.com/samcolson4/podcastdelay/internal/schedule"
	"github.com/samcolson4/podcastdelay/internal/store"
)

// viaSuffix marks the delayed copy in podcast apps so it is obvious which
// of the two feeds is which when both are subscribed.
const viaSuffix = " (via PodcastDelay)"

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.Ping(r.Context()); err != nil {
		http.Error(w, "unhealthy", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

// feedETag derives an ETag from (updated_at, what has released) as docs
// §3.8 specifies, so podcast apps polling aggressively get cheap 304s.
// "What has released" is the number of items and the newest release
// time rather than the highest position: a melded premium episode's
// position is at the tail whatever its date, so it can release behind a
// free episode with a higher position and would otherwise leave the
// ETag — and the app's view of the feed — unchanged.
func feedETag(sub store.Subscription, episodes []store.Episode) string {
	var newest time.Time
	for _, e := range episodes {
		if e.ScheduledAt.After(newest) {
			newest = e.ScheduledAt
		}
	}
	h := fnv.New64a()
	fmt.Fprintf(h, "%d:%d:%d", sub.UpdatedAt.UnixNano(), len(episodes), newest.UnixNano())
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

	var lastBuild time.Time
	for _, e := range episodes {
		if e.ScheduledAt.After(lastBuild) {
			lastBuild = e.ScheduledAt
		}
	}
	if lastBuild.IsZero() {
		lastBuild = sub.UpdatedAt
	}

	etag := feedETag(sub, episodes)
	if match := r.Header.Get("If-None-Match"); match != "" && match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	meta := s.channelMeta(sub)
	title := titleOverrideOr(sub, meta.Title+viaSuffix)

	items := make([]feed.Item, 0, len(episodes))
	for _, e := range episodes {
		items = append(items, feed.Item{
			GUID:            feedGUID(sub.Token, e),
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
		SelfURL:      s.BaseURL + "/f/" + sub.Token + ".xml",
		Title:        title,
		Channel:      feedChannel(title, meta),
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

// feedGUID namespaces an episode's GUID so that subscribing to the real
// feed as well doesn't deduplicate the two shows into one (docs §3.4).
// A premium episode carries its role too, because a premium feed is
// often an ad-free re-cut of the same episodes under the same GUIDs, and
// the pair has to stay two episodes in the app. The free feed's form is
// deliberately unchanged: an episode whose GUID moves looks new, and
// gets re-downloaded.
func feedGUID(token string, e store.Episode) string {
	if schedule.NormalizeRole(e.SourceRole) == schedule.RolePremium {
		return fmt.Sprintf("podcastdelay:%s:premium:%s", token, e.GUID)
	}
	return fmt.Sprintf("podcastdelay:%s:%s", token, e.GUID)
}

// feedChannel maps the cached show metadata onto the renderer's own
// channel type, with the delayed feed's title substituted for the
// publisher's.
func feedChannel(title string, meta store.ChannelMeta) feed.Channel {
	return feed.Channel{
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
	}
}

func derefInt64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}
