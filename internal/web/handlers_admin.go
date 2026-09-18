package web

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/samcolson4/podcastdelay/internal/refresh"
	"github.com/samcolson4/podcastdelay/internal/schedule"
	"github.com/samcolson4/podcastdelay/internal/store"
)

func pathInt64(r *http.Request, name string) (int64, error) {
	return strconv.ParseInt(r.PathValue(name), 10, 64)
}

func (s *Server) handleCreateSubscription(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	sourceURL := strings.TrimSpace(r.FormValue("source_url"))
	if sourceURL == "" {
		s.renderDashboardWithError(w, r, "source_url is required")
		return
	}

	cadenceDays, err := strconv.Atoi(defaultStr(r.FormValue("cadence_days"), "7"))
	if err != nil || cadenceDays < 1 {
		s.renderDashboardWithError(w, r, "cadence_days must be a positive integer")
		return
	}
	seedCount, err := strconv.Atoi(defaultStr(r.FormValue("seed_count"), "1"))
	if err != nil || seedCount < 0 {
		s.renderDashboardWithError(w, r, "seed_count must be a non-negative integer")
		return
	}
	episodesPerSlot, err := strconv.Atoi(defaultStr(r.FormValue("episodes_per_slot"), "1"))
	if err != nil || episodesPerSlot < 1 {
		s.renderDashboardWithError(w, r, "episodes_per_slot must be a positive integer")
		return
	}
	releaseTime := defaultStr(r.FormValue("release_time"), "07:00")
	timezone := defaultStr(r.FormValue("timezone"), s.DefaultTimezone)
	if _, err := time.LoadLocation(timezone); err != nil {
		s.renderDashboardWithError(w, r, "invalid timezone: "+err.Error())
		return
	}

	startAt := time.Now().UTC()
	if v := strings.TrimSpace(r.FormValue("start_at")); v != "" {
		parsed, err := time.Parse("2006-01-02T15:04", v)
		if err != nil {
			s.renderDashboardWithError(w, r, "invalid start_at, expected YYYY-MM-DDTHH:MM")
			return
		}
		loc, _ := time.LoadLocation(timezone)
		startAt = time.Date(parsed.Year(), parsed.Month(), parsed.Day(), parsed.Hour(), parsed.Minute(), 0, 0, loc)
	}

	var titleOverride *string
	if v := strings.TrimSpace(r.FormValue("title_override")); v != "" {
		titleOverride = &v
	}
	var maxFeedItems *int
	if v := strings.TrimSpace(r.FormValue("max_feed_items")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			s.renderDashboardWithError(w, r, "max_feed_items must be a positive integer")
			return
		}
		maxFeedItems = &n
	}

	_, err = refresh.AddSubscription(r.Context(), s.Store, s.Fetcher, refresh.AddParams{
		SourceURL:       sourceURL,
		TitleOverride:   titleOverride,
		CadenceDays:     cadenceDays,
		ReleaseTime:     releaseTime,
		Timezone:        timezone,
		StartAt:         startAt,
		SeedCount:       seedCount,
		EpisodesPerSlot: episodesPerSlot,
		MaxFeedItems:    maxFeedItems,
	})
	if err != nil {
		s.Logger.Error("admin: add subscription failed", "error", err)
		s.renderDashboardWithError(w, r, "could not add feed: "+err.Error())
		return
	}

	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) handlePatchSubscription(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	patch := store.SubscriptionPatch{}
	if v := r.FormValue("cadence_days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			http.Error(w, "invalid cadence_days", http.StatusBadRequest)
			return
		}
		patch.CadenceDays = &n
	}
	if v := r.FormValue("seed_count"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			http.Error(w, "invalid seed_count", http.StatusBadRequest)
			return
		}
		patch.SeedCount = &n
	}
	if v := r.FormValue("episodes_per_slot"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			http.Error(w, "invalid episodes_per_slot", http.StatusBadRequest)
			return
		}
		patch.EpisodesPerSlot = &n
	}
	if v := r.FormValue("release_time"); v != "" {
		patch.ReleaseTime = &v
	}
	if v := r.FormValue("timezone"); v != "" {
		if _, err := time.LoadLocation(v); err != nil {
			http.Error(w, "invalid timezone", http.StatusBadRequest)
			return
		}
		patch.Timezone = &v
	}
	if v := r.FormValue("title_override"); v != "" {
		patch.TitleOverride = &v
	}
	if v := r.FormValue("start_at"); v != "" {
		parsed, err := time.Parse("2006-01-02T15:04", v)
		if err != nil {
			http.Error(w, "invalid start_at", http.StatusBadRequest)
			return
		}
		patch.StartAt = &parsed
	}

	if _, err := s.Store.PatchSubscription(r.Context(), id, patch); err != nil {
		s.Logger.Error("admin: patch subscription failed", "id", id, "error", err)
		http.Error(w, "patch failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := refresh.Reschedule(r.Context(), s.Store, id); err != nil {
		s.Logger.Error("admin: reschedule after patch failed", "id", id, "error", err)
		http.Error(w, "reschedule failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	s.redirectOrOK(w, r, "/admin")
}

func (s *Server) handleDeleteSubscription(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := s.Store.DeleteSubscription(r.Context(), id); err != nil {
		s.Logger.Error("admin: delete subscription failed", "id", id, "error", err)
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	s.redirectOrOK(w, r, "/admin")
}

func (s *Server) handleForceRefresh(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	sub, err := s.Store.GetSubscription(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := refresh.Poll(r.Context(), s.Store, s.Fetcher, sub, time.Now().UTC(), s.Logger); err != nil {
		s.Logger.Error("admin: force refresh failed", "id", id, "error", err)
		http.Error(w, "refresh failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	s.redirectOrOK(w, r, "/admin")
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := s.Store.PauseSubscription(r.Context(), id, time.Now().UTC()); err != nil {
		http.Error(w, "pause failed", http.StatusInternalServerError)
		return
	}
	s.redirectOrOK(w, r, "/admin")
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := s.Store.ResumeSubscription(r.Context(), id, time.Now().UTC()); err != nil {
		http.Error(w, "resume failed", http.StatusInternalServerError)
		return
	}
	if err := refresh.Reschedule(r.Context(), s.Store, id); err != nil {
		s.Logger.Error("admin: reschedule after resume failed", "id", id, "error", err)
		http.Error(w, "reschedule failed", http.StatusInternalServerError)
		return
	}
	s.redirectOrOK(w, r, "/admin")
}

func (s *Server) handleExcludeEpisode(w http.ResponseWriter, r *http.Request) {
	s.setExcluded(w, r, true)
}

func (s *Server) handleIncludeEpisode(w http.ResponseWriter, r *http.Request) {
	s.setExcluded(w, r, false)
}

func (s *Server) setExcluded(w http.ResponseWriter, r *http.Request, excluded bool) {
	episodeID, err := pathInt64(r, "episode_id")
	if err != nil {
		http.Error(w, "bad episode id", http.StatusBadRequest)
		return
	}
	if err := s.Store.SetExcluded(r.Context(), episodeID, excluded); err != nil {
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}
	id := r.PathValue("id")
	http.Redirect(w, r, "/admin/subscriptions/"+id+"/schedule", http.StatusSeeOther)
}

func (s *Server) handleSchedulePreview(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	sub, err := s.Store.GetSubscription(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	episodes, err := s.Store.ListBySubscription(r.Context(), id)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	loc, err := time.LoadLocation(sub.Timezone)
	if err != nil {
		http.Error(w, "invalid timezone", http.StatusInternalServerError)
		return
	}
	cfg := schedule.Config{
		StartAt:         sub.StartAt,
		CadenceDays:     sub.CadenceDays,
		ReleaseTime:     sub.ReleaseTime,
		Location:        loc,
		SeedCount:       sub.SeedCount,
		EpisodesPerSlot: sub.EpisodesPerSlot,
		ShiftSeconds:    sub.ShiftSeconds,
	}

	type row struct {
		store.Episode
		Preview time.Time
	}
	rows := make([]row, 0, len(episodes))
	for _, e := range episodes {
		at := e.ScheduledAt
		if !e.Locked {
			if computed, err := cfg.ReleaseAt(e.Position); err == nil {
				at = computed
			}
		}
		rows = append(rows, row{Episode: e, Preview: at})
	}

	s.render(w, "schedule.html", map[string]any{
		"Subscription": sub,
		"Episodes":     rows,
	})
}

func (s *Server) redirectOrOK(w http.ResponseWriter, r *http.Request, location string) {
	if r.Header.Get("Accept") == "application/json" {
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, location, http.StatusSeeOther)
}

func defaultStr(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
