package web

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"time"

	"github.com/samcolson4/podcastdelay/internal/schedule"
	"github.com/samcolson4/podcastdelay/internal/store"
)

const displayTimeLayout = "2 Jan 2006 15:04"

var templateFuncs = template.FuncMap{
	// fmtTime takes time.Time or *time.Time so the templates don't need
	// to care which one a field happens to be.
	"fmtTime": func(v any) string {
		var t time.Time
		switch x := v.(type) {
		case time.Time:
			t = x
		case *time.Time:
			if x != nil {
				t = *x
			}
		}
		if t.IsZero() {
			return "—"
		}
		return t.Local().Format(displayTimeLayout)
	},
	// inputTime renders t for <input type="datetime-local">, in the
	// timezone the form will interpret it back in — otherwise saving the
	// edit form unchanged would silently shift start_at by the zone offset.
	"inputTime": func(t time.Time, tz string) string {
		if loc, err := time.LoadLocation(tz); err == nil {
			t = t.In(loc)
		}
		return t.Format("2006-01-02T15:04")
	},
}

// channelMeta decodes a subscription's cached show metadata. A blob we
// can't read costs us the show's title, not the page.
func (s *Server) channelMeta(sub store.Subscription) store.ChannelMeta {
	var meta store.ChannelMeta
	if err := json.Unmarshal([]byte(sub.ChannelJSON), &meta); err != nil {
		s.Logger.Warn("invalid channel_json", "subscription_id", sub.ID, "error", err)
	}
	return meta
}

// titleOverrideOr applies the subscription's title override, if it has one.
func titleOverrideOr(sub store.Subscription, fallback string) string {
	if sub.TitleOverride != nil && *sub.TitleOverride != "" {
		return *sub.TitleOverride
	}
	return fallback
}

// subscriptionTitle is the admin-facing name of a show: the override, the
// title the publisher gave it, or (before the first successful fetch) the
// source URL.
func (s *Server) subscriptionTitle(sub store.Subscription) string {
	title := titleOverrideOr(sub, s.channelMeta(sub).Title)
	if title == "" {
		title = sub.SourceURL
	}
	return title
}

// cadenceSummary describes a subscription's release pace in one line, so
// both admin pages say the same thing about it.
func cadenceSummary(sub store.Subscription) string {
	if sub.CadenceMode == schedule.ModeOriginal {
		return "original cadence (the show's own release gaps)"
	}
	unit := "days"
	if sub.CadenceDays == 1 {
		unit = "day"
	}
	summary := fmt.Sprintf("every %d %s at %s (%s)", sub.CadenceDays, unit, sub.ReleaseTime, sub.Timezone)
	if sub.EpisodesPerSlot > 1 {
		summary += fmt.Sprintf(", %d episodes per slot", sub.EpisodesPerSlot)
	}
	return summary
}

// subscriptionView bundles a subscription with everything the
// dashboard renders about it: title, progress ("ep 12 of 240, caught
// up in 2029"), and next release.
type subscriptionView struct {
	store.Subscription
	Title         string
	Cadence       string
	TotalEpisodes int
	ReleasedCount int
	NextReleaseAt *time.Time
	CaughtUpAt    *time.Time
	IsCaughtUp    bool
	FetchStatus   string
}

func (s *Server) buildSubscriptionView(r *http.Request, sub store.Subscription) (subscriptionView, error) {
	episodes, err := s.Store.ListBySubscription(r.Context(), sub.ID)
	if err != nil {
		return subscriptionView{}, err
	}

	now := time.Now().UTC()
	v := subscriptionView{
		Subscription: sub,
		Title:        s.subscriptionTitle(sub),
		Cadence:      cadenceSummary(sub),
	}

	var nonExcluded []store.Episode
	for _, e := range episodes {
		if e.Excluded {
			continue
		}
		nonExcluded = append(nonExcluded, e)
	}
	v.TotalEpisodes = len(nonExcluded)

	for _, e := range nonExcluded {
		released := e.Locked || !e.ScheduledAt.After(now)
		if released {
			v.ReleasedCount++
		} else if v.NextReleaseAt == nil || e.ScheduledAt.Before(*v.NextReleaseAt) {
			at := e.ScheduledAt
			v.NextReleaseAt = &at
		}
	}
	if len(nonExcluded) > 0 {
		last := nonExcluded[len(nonExcluded)-1]
		v.CaughtUpAt = &last.ScheduledAt
		v.IsCaughtUp = !last.ScheduledAt.After(now)
	}
	if sub.LastFetchStatus != nil {
		v.FetchStatus = *sub.LastFetchStatus
	}

	return v, nil
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	s.renderDashboardWithError(w, r, "")
}

func (s *Server) renderDashboardWithError(w http.ResponseWriter, r *http.Request, errMsg string) {
	subs, err := s.Store.ListSubscriptions(r.Context())
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	views := make([]subscriptionView, 0, len(subs))
	for _, sub := range subs {
		v, err := s.buildSubscriptionView(r, sub)
		if err != nil {
			s.Logger.Error("dashboard: build view failed", "id", sub.ID, "error", err)
			continue
		}
		views = append(views, v)
	}

	s.render(w, "dashboard.html", map[string]any{
		"Subscriptions":   views,
		"Error":           errMsg,
		"BaseURL":         s.BaseURL,
		"DefaultTimezone": s.DefaultTimezone,
		"Now":             time.Now().UTC(),
	})
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		s.Logger.Error("render failed", "template", name, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}
