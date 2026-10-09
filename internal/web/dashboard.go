package web

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/samcolson4/podcastdelay/internal/schedule"
	"github.com/samcolson4/podcastdelay/internal/store"
)

// Defaults for a new subscription. The add form is pre-filled from these
// and handleCreateSubscription falls back to them for blank fields, so the
// two can't drift apart.
const (
	defaultCadenceDays     = 7
	defaultSeedCount       = 1
	defaultEpisodesPerSlot = 1
	defaultReleaseTime     = "07:00"
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
	"fmtDateOrNil": func(t *time.Time) string {
		if t == nil {
			return "—"
		}
		return t.Local().Format("2 Jan 2006")
	},
	"fmtClockOrNil": func(t *time.Time) string {
		if t == nil {
			return ""
		}
		return t.Local().Format("15:04")
	},
	// inputTime renders t for <input type="datetime-local">, in the
	// timezone the form will interpret it back in — otherwise saving the
	// edit form unchanged would silently shift start_at by the zone offset.
	"inputTime": func(t time.Time, tz string) string {
		if loc, err := time.LoadLocation(tz); err == nil {
			t = t.In(loc)
		}
		return t.Format(startAtLayout)
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
	Title          string
	Cadence        string
	TotalEpisodes  int
	ReleasedCount  int
	NextReleaseAt  *time.Time
	CaughtUpAt     *time.Time
	IsCaughtUp     bool
	LastFetchError string
	// FetchStatus is a display bucket for the last fetch: "ok",
	// "unchanged", "error", or "" if the feed hasn't been fetched yet.
	FetchStatus string
	PremiumURL  string
	ImageURL    string
	SourceHost  string
	// CaughtUpIn is a rough "~12 yrs" until the last episode releases;
	// empty once caught up.
	CaughtUpIn string
}

// approxUntil renders a coarse, human duration like "~12 yrs".
func approxUntil(d time.Duration) string {
	days := int(d.Hours() / 24)
	switch {
	case days >= 365:
		n := (days + 182) / 365
		if n == 1 {
			return "~1 yr"
		}
		return "~" + strconv.Itoa(n) + " yrs"
	case days >= 30:
		return "~" + strconv.Itoa((days+15)/30) + " mo"
	case days >= 7:
		return "~" + strconv.Itoa(days/7) + " wks"
	case days >= 1:
		return "~" + strconv.Itoa(days) + " days"
	}
	return "<1 day"
}

func (s *Server) buildSubscriptionView(r *http.Request, sub store.Subscription) (subscriptionView, error) {
	episodes, err := s.Store.ListBySubscription(r.Context(), sub.ID)
	if err != nil {
		return subscriptionView{}, err
	}

	now := time.Now().UTC()
	meta := s.channelMeta(sub)
	v := subscriptionView{
		Subscription: sub,
		Title:        titleOverrideOr(sub, meta.Title),
		Cadence:      cadenceSummary(sub),
		ImageURL:     meta.ImageURL,
		PremiumURL:   valueOr(sub.PremiumSourceURL, ""),
	}
	if v.Title == "" {
		v.Title = sub.SourceURL
	}
	if u, err := url.Parse(sub.SourceURL); err == nil && u.Hostname() != "" {
		v.SourceHost = strings.TrimPrefix(u.Hostname(), "www.")
	} else {
		v.SourceHost = sub.SourceURL
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
		if !v.IsCaughtUp {
			v.CaughtUpIn = approxUntil(last.ScheduledAt.Sub(now))
		}
	}
	if sub.LastFetchStatus != nil {
		st := *sub.LastFetchStatus
		switch {
		case st == "ok":
			v.FetchStatus = "ok"
		case st == "not_modified":
			v.FetchStatus = "unchanged"
		case st != "":
			v.FetchStatus = "error"
			v.LastFetchError = strings.TrimPrefix(st, "error: ")
		}
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
		if sub.HiddenAt != nil {
			continue
		}
		v, err := s.buildSubscriptionView(r, sub)
		if err != nil {
			s.Logger.Error("dashboard: build view failed", "id", sub.ID, "error", err)
			continue
		}
		views = append(views, v)
	}

	s.render(w, "dashboard.html", map[string]any{
		"Subscriptions": views,
		"Error":         errMsg,
		"BaseURL":       s.BaseURL,
		"Hidden":        false,
	})
}

// handleHiddenFeeds lists the feeds hidden from the main dashboard.
// Hiding is purely a display concern, so these keep refreshing and
// serving exactly as before.
func (s *Server) handleHiddenFeeds(w http.ResponseWriter, r *http.Request) {
	subs, err := s.Store.ListSubscriptions(r.Context())
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	views := make([]subscriptionView, 0, len(subs))
	for _, sub := range subs {
		if sub.HiddenAt == nil {
			continue
		}
		v, err := s.buildSubscriptionView(r, sub)
		if err != nil {
			s.Logger.Error("hidden feeds: build view failed", "id", sub.ID, "error", err)
			continue
		}
		views = append(views, v)
	}

	s.render(w, "hidden.html", map[string]any{
		"Subscriptions": views,
		"BaseURL":       s.BaseURL,
		"Hidden":        true,
	})
}

func (s *Server) handleNewSubscription(w http.ResponseWriter, r *http.Request) {
	s.renderNewSubscription(w, "")
}

// renderNewSubscription renders the add-feed page; errors from the
// create handler land here so the form stays in front of the user.
func (s *Server) renderNewSubscription(w http.ResponseWriter, errMsg string) {
	// The start_at field is interpreted in the chosen timezone, so its
	// default must be "now" on that clock, not UTC.
	now := time.Now().UTC()
	if loc, err := time.LoadLocation(s.DefaultTimezone); err == nil {
		now = now.In(loc)
	}

	s.render(w, "new.html", map[string]any{
		"Error":           errMsg,
		"DefaultTimezone": s.DefaultTimezone,
		"Now":             now,
	})
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	tmpl, err := s.templates()
	if err != nil {
		s.Logger.Error("template parse failed", "error", err)
		http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		s.Logger.Error("render failed", "template", name, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}
