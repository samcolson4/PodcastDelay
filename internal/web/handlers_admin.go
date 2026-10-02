package web

import (
	"errors"
	"fmt"
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

	// One shape for every field error: re-render the dashboard with the
	// message above the form, so nothing typed in is lost.
	fail := func(err error) { s.renderDashboardWithError(w, r, err.Error()) }

	sourceURL := optionalString(r, "source_url")
	if sourceURL == nil {
		fail(errors.New("source_url is required"))
		return
	}

	cadenceDays, err := intOrDefault(r, "cadence_days", 1, defaultCadenceDays)
	if err != nil {
		fail(err)
		return
	}
	seedCount, err := intOrDefault(r, "seed_count", 0, defaultSeedCount)
	if err != nil {
		fail(err)
		return
	}
	episodesPerSlot, err := intOrDefault(r, "episodes_per_slot", 1, defaultEpisodesPerSlot)
	if err != nil {
		fail(err)
		return
	}
	maxFeedItems, err := optionalInt(r, "max_feed_items", 1)
	if err != nil {
		fail(err)
		return
	}

	cadenceMode, err := optionalCadenceMode(r, "cadence_mode")
	if err != nil {
		fail(err)
		return
	}
	releaseTime, err := optionalReleaseTime(r, "release_time")
	if err != nil {
		fail(err)
		return
	}
	timezone, err := optionalTimezone(r, "timezone")
	if err != nil {
		fail(err)
		return
	}

	tz := valueOr(timezone, s.DefaultTimezone)
	loc, err := time.LoadLocation(tz)
	if err != nil {
		fail(fmt.Errorf("invalid timezone %q", tz))
		return
	}
	startAt, err := optionalStartAt(r, "start_at", loc)
	if err != nil {
		fail(err)
		return
	}

	_, err = refresh.AddSubscription(r.Context(), s.Store, s.Fetcher, refresh.AddParams{
		SourceURL:       *sourceURL,
		TitleOverride:   optionalString(r, "title_override"),
		CadenceDays:     cadenceDays,
		CadenceMode:     valueOr(cadenceMode, schedule.ModeFixed),
		ReleaseTime:     valueOr(releaseTime, defaultReleaseTime),
		Timezone:        tz,
		StartAt:         valueOr(startAt, time.Now().In(loc)),
		SeedCount:       seedCount,
		EpisodesPerSlot: episodesPerSlot,
		MaxFeedItems:    maxFeedItems,
	})
	if err != nil {
		s.Logger.Error("admin: add subscription failed", "error", err)
		fail(fmt.Errorf("could not add feed: %w", err))
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
	current, err := s.Store.GetSubscription(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	// Every field is optional: absent means "leave it alone".
	var patch store.SubscriptionPatch
	if patch.CadenceDays, err = optionalInt(r, "cadence_days", 1); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if patch.SeedCount, err = optionalInt(r, "seed_count", 0); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if patch.EpisodesPerSlot, err = optionalInt(r, "episodes_per_slot", 1); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if patch.CadenceMode, err = optionalCadenceMode(r, "cadence_mode"); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if patch.ReleaseTime, err = optionalReleaseTime(r, "release_time"); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if patch.Timezone, err = optionalTimezone(r, "timezone"); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	patch.TitleOverride = optionalString(r, "title_override")

	// start_at is wall-clock in the subscription's timezone, which this
	// same request may be changing.
	loc, err := time.LoadLocation(valueOr(patch.Timezone, current.Timezone))
	if err != nil {
		loc = time.UTC
	}
	if patch.StartAt, err = optionalStartAt(r, "start_at", loc); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
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
	id, err := pathInt64(r, "id")
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	episodeID, err := pathInt64(r, "episode_id")
	if err != nil {
		http.Error(w, "bad episode id", http.StatusBadRequest)
		return
	}
	if err := s.Store.SetExcluded(r.Context(), episodeID, excluded); err != nil {
		s.Logger.Error("admin: set excluded failed", "episode_id", episodeID, "error", err)
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/admin/subscriptions/%d/schedule", id), http.StatusSeeOther)
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

	cfg, err := refresh.ScheduleConfigFor(sub, refresh.PubDatesByPosition(episodes))
	if err != nil {
		s.Logger.Error("admin: schedule preview failed", "id", id, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
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
		"Title":        s.subscriptionTitle(sub),
		"Cadence":      cadenceSummary(sub),
		"Episodes":     rows,
	})
}

func (s *Server) redirectOrOK(w http.ResponseWriter, r *http.Request, location string) {
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, location, http.StatusSeeOther)
}

// valueOr dereferences p, falling back to def when the field was absent.
func valueOr[T any](p *T, def T) T {
	if p == nil {
		return def
	}
	return *p
}
