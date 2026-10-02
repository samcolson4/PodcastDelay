// Package web is the entire HTTP surface: the public delayed feed, and
// a server-rendered admin UI behind HTTP basic auth. No JS build step,
// no SPA — docs/IMPLEMENTATION.md §5 calls for a few hundred lines of
// html/template, and that's what this is.
package web

import (
	"crypto/subtle"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"

	"github.com/samcolson4/podcastdelay/internal/source"
	"github.com/samcolson4/podcastdelay/internal/store"
)

//go:embed templates/*.html
var templatesFS embed.FS

// Options is everything the HTTP layer needs from the rest of the
// program. It is a struct rather than a parameter list because four of
// the fields are strings and transposing two of them (say, the admin
// user and password) would otherwise compile happily.
type Options struct {
	Store           *store.Store
	Fetcher         *source.Fetcher
	BaseURL         string // absolute, no trailing slash
	AdminUser       string
	AdminPassword   string
	DefaultTimezone string // default for new subscriptions
	Logger          *slog.Logger
}

// Server serves the public feed route and the admin UI. Options is
// embedded so handlers read configuration as s.BaseURL, s.Store, ...
type Server struct {
	Options

	tmpl *template.Template
}

func New(opts Options) (*Server, error) {
	if opts.Store == nil || opts.Fetcher == nil {
		return nil, errors.New("web: Store and Fetcher are required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.DefaultTimezone == "" {
		opts.DefaultTimezone = "UTC"
	}
	tmpl, err := template.New("").Funcs(templateFuncs).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("web: parse templates: %w", err)
	}
	return &Server{Options: opts, tmpl: tmpl}, nil
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /f/{token}", s.handleFeed)

	admin := http.NewServeMux()
	admin.HandleFunc("GET /admin", s.handleDashboard)
	admin.HandleFunc("POST /admin/subscriptions", s.handleCreateSubscription)
	admin.HandleFunc("PATCH /admin/subscriptions/{id}", s.handlePatchSubscription)
	admin.HandleFunc("POST /admin/subscriptions/{id}/edit", s.handlePatchSubscription) // form-friendly alias, no JS/method-override needed
	admin.HandleFunc("DELETE /admin/subscriptions/{id}", s.handleDeleteSubscription)
	admin.HandleFunc("POST /admin/subscriptions/{id}/delete", s.handleDeleteSubscription)
	admin.HandleFunc("POST /admin/subscriptions/{id}/refresh", s.handleForceRefresh)
	admin.HandleFunc("POST /admin/subscriptions/{id}/pause", s.handlePause)
	admin.HandleFunc("POST /admin/subscriptions/{id}/resume", s.handleResume)
	admin.HandleFunc("POST /admin/subscriptions/{id}/episodes/{episode_id}/exclude", s.handleExcludeEpisode)
	admin.HandleFunc("POST /admin/subscriptions/{id}/episodes/{episode_id}/include", s.handleIncludeEpisode)
	admin.HandleFunc("GET /admin/subscriptions/{id}/schedule", s.handleSchedulePreview)

	mux.Handle("/admin", s.basicAuth(admin))
	mux.Handle("/admin/", s.basicAuth(admin))

	return mux
}

func (s *Server) basicAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		userMatch := subtle.ConstantTimeCompare([]byte(user), []byte(s.AdminUser)) == 1
		passMatch := subtle.ConstantTimeCompare([]byte(pass), []byte(s.AdminPassword)) == 1
		if !ok || !userMatch || !passMatch {
			w.Header().Set("WWW-Authenticate", `Basic realm="podcastdelay admin"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
