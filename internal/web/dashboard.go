package web

import (
	"encoding/json"
	"html/template"
	"net/http"
	"time"

	"github.com/samcolson4/podcastdelay/internal/store"
)

var templateFuncs = template.FuncMap{
	"fmtTime": func(t time.Time) string {
		if t.IsZero() {
			return "—"
		}
		return t.Local().Format("2 Jan 2006 15:04")
	},
	"fmtTimeOrNil": func(t *time.Time) string {
		if t == nil {
			return "—"
		}
		return t.Local().Format("2 Jan 2006 15:04")
	},
	"fmtInputTime": func(t time.Time) string {
		return t.Format("2006-01-02T15:04")
	},
}

// subscriptionView bundles a subscription with everything the
// dashboard renders about it: title, progress ("ep 12 of 240, caught
// up in 2029"), and next release.
type subscriptionView struct {
	store.Subscription
	Title          string
	TotalEpisodes  int
	ReleasedCount  int
	NextReleaseAt  *time.Time
	CaughtUpAt     *time.Time
	IsCaughtUp     bool
	LastFetchError string
}

func (s *Server) buildSubscriptionView(r *http.Request, sub store.Subscription) (subscriptionView, error) {
	episodes, err := s.Store.ListBySubscription(r.Context(), sub.ID)
	if err != nil {
		return subscriptionView{}, err
	}

	var meta store.ChannelMeta
	_ = json.Unmarshal([]byte(sub.ChannelJSON), &meta)
	title := meta.Title
	if sub.TitleOverride != nil && *sub.TitleOverride != "" {
		title = *sub.TitleOverride
	}
	if title == "" {
		title = sub.SourceURL
	}

	now := time.Now().UTC()
	v := subscriptionView{Subscription: sub, Title: title}

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
		v.LastFetchError = *sub.LastFetchStatus
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
