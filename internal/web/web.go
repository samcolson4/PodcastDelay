// Package web is the entire HTTP surface: the public delayed feed, and
// a server-rendered admin UI behind HTTP basic auth. No JS build step,
// no SPA — docs/IMPLEMENTATION.md §5 calls for a few hundred lines of
// html/template, plus vendored Pico CSS and htmx under static/.
package web

import (
	"crypto/subtle"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/samcolson4/podcastdelay/internal/source"
	"github.com/samcolson4/podcastdelay/internal/store"
)

//go:embed templates/*.html static
var assetsFS embed.FS

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

	// DevWebDir, when set, serves templates/ and static/ from this
	// directory on every request instead of the embedded copy, so UI
	// edits show up on a plain browser refresh (no rebuild).
	DevWebDir string

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
	tmpl, err := template.New("").Funcs(templateFuncs).ParseFS(assetsFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("web: parse templates: %w", err)
	}
	return &Server{Options: opts, tmpl: tmpl}, nil
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /f/{token}", s.handleFeed)
	mux.Handle("GET /static/", http.StripPrefix("/static/", s.staticHandler()))

	admin := http.NewServeMux()
	admin.HandleFunc("GET /admin", s.handleDashboard)
	admin.HandleFunc("GET /admin/hidden", s.handleHiddenFeeds)
	admin.HandleFunc("GET /admin/subscriptions/new", s.handleNewSubscription)
	admin.HandleFunc("POST /admin/subscriptions", s.handleCreateSubscription)
	admin.HandleFunc("PATCH /admin/subscriptions/{id}", s.handlePatchSubscription)
	admin.HandleFunc("POST /admin/subscriptions/{id}/edit", s.handlePatchSubscription) // form-friendly alias, no JS/method-override needed
	admin.HandleFunc("DELETE /admin/subscriptions/{id}", s.handleDeleteSubscription)
	admin.HandleFunc("POST /admin/subscriptions/{id}/delete", s.handleDeleteSubscription)
	admin.HandleFunc("POST /admin/subscriptions/{id}/refresh", s.handleForceRefresh)
	admin.HandleFunc("POST /admin/subscriptions/{id}/pause", s.handlePause)
	admin.HandleFunc("POST /admin/subscriptions/{id}/resume", s.handleResume)
	admin.HandleFunc("POST /admin/subscriptions/{id}/hide", s.handleHide)
	admin.HandleFunc("POST /admin/subscriptions/{id}/unhide", s.handleUnhide)
	admin.HandleFunc("POST /admin/subscriptions/{id}/release-next", s.handleReleaseNext)
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

// templates returns the parsed templates: the embedded set, or a fresh
// parse from DevWebDir so template edits apply without a rebuild.
func (s *Server) templates() (*template.Template, error) {
	if s.DevWebDir == "" {
		return s.tmpl, nil
	}
	return template.New("").Funcs(templateFuncs).ParseFS(os.DirFS(s.DevWebDir), "templates/*.html")
}

func (s *Server) staticHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "" || strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r) // no directory listings
			return
		}
		var root fs.FS = assetsFS
		if s.DevWebDir != "" {
			root = os.DirFS(s.DevWebDir)
			w.Header().Set("Cache-Control", "no-store")
		}
		sub, err := fs.Sub(root, "static")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		http.FileServerFS(sub).ServeHTTP(w, r)
	})
}
