// Command podcastdelay serves and manages delayed podcast feeds. Run
// with no arguments (or "serve") to start the HTTP server; "add" adds a
// feed from the command line without going through the admin UI;
// "healthcheck" is what the container's HEALTHCHECK invokes, since a
// distroless image has no shell or curl.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	// Embeds the IANA timezone database into the binary so
	// time.LoadLocation works on a scratch/distroless image with no
	// /usr/share/zoneinfo (docs §9.1).
	_ "time/tzdata"

	"github.com/samcolson4/podcastdelay/internal/config"
	"github.com/samcolson4/podcastdelay/internal/refresh"
	"github.com/samcolson4/podcastdelay/internal/source"
	"github.com/samcolson4/podcastdelay/internal/store"
	"github.com/samcolson4/podcastdelay/internal/web"
)

func main() {
	cmd := "serve"
	args := os.Args[1:]
	if len(args) > 0 && !isFlag(args[0]) {
		cmd = args[0]
		args = args[1:]
	}

	var err error
	switch cmd {
	case "serve":
		err = runServe(args)
	case "healthcheck":
		err = runHealthcheck(args)
	case "add":
		err = runAdd(args)
	default:
		err = fmt.Errorf("unknown command %q (expected serve, add, or healthcheck)", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "podcastdelay:", err)
		os.Exit(1)
	}
}

func isFlag(s string) bool {
	return len(s) > 0 && s[0] == '-'
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	fs.Parse(args)

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := newLogger(cfg.LogLevel)
	slog.SetDefault(logger)

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, filepath.Join(cfg.DataDir, "podcastdelay.db"))
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	fetcher := source.NewFetcher()

	srv, err := web.New(st, fetcher, cfg.BaseURL, cfg.AdminUser, cfg.AdminPassword, cfg.DefaultTimezone, logger)
	if err != nil {
		return fmt.Errorf("build web server: %w", err)
	}

	httpServer := &http.Server{
		Addr:         cfg.Addr,
		Handler:      srv.Routes(),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	refreshCtx, cancelRefresh := context.WithCancel(ctx)
	defer cancelRefresh()
	refreshDone := make(chan struct{})
	go func() {
		defer close(refreshDone)
		refresh.RunLoop(refreshCtx, st, fetcher, cfg.PollInterval, config.TickInterval, logger)
	}()

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", cfg.Addr, "base_url", cfg.BaseURL)
		serveErr <- httpServer.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutting down")
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve: %w", err)
		}
	}

	cancelRefresh()
	<-refreshDone

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return httpServer.Shutdown(shutdownCtx)
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

// runHealthcheck does a plain TCP+HTTP GET of /healthz using only the
// standard library, so it works with no shell in the final image (§9.3
// invokes this directly as the container HEALTHCHECK).
func runHealthcheck(args []string) error {
	fs := flag.NewFlagSet("healthcheck", flag.ExitOnError)
	fs.Parse(args)

	addr := os.Getenv("PODCASTDELAY_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	url := "http://localhost" + addr + "/healthz"
	if addr[0] != ':' {
		url = "http://" + addr + "/healthz"
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("healthcheck request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck returned status %d", resp.StatusCode)
	}
	return nil
}

// runAdd is the CLI fast path for adding a show without opening the
// admin UI: `podcastdelay add <url> --every 7d --start tomorrow --seed 2`.
func runAdd(args []string) error {
	fs := flag.NewFlagSet("add", flag.ExitOnError)
	every := fs.String("every", "7d", `cadence, e.g. "7d" for weekly`)
	start := fs.String("start", "now", `"now", "tomorrow", or RFC3339 (e.g. 2026-01-06T07:00:00Z)`)
	seed := fs.Int("seed", 1, "episodes released immediately on day one")
	perSlot := fs.Int("episodes-per-slot", 1, "episodes released per cadence slot")
	releaseTime := fs.String("release-time", "07:00", `wall-clock release time, "HH:MM"`)
	timezone := fs.String("timezone", "", "IANA timezone (defaults to PODCASTDELAY_DEFAULT_TIMEZONE or UTC)")
	title := fs.String("title", "", "title override for the delayed feed")
	maxItems := fs.Int("max-feed-items", 0, "cap the rendered feed window (0 = unlimited)")
	fs.Parse(args)

	if fs.NArg() < 1 {
		return errors.New("usage: podcastdelay add <source-url> [--every 7d] [--start tomorrow] [--seed 1]")
	}
	sourceURL := fs.Arg(0)

	cadenceDays, err := parseCadenceDays(*every)
	if err != nil {
		return err
	}

	tz := *timezone
	if tz == "" {
		tz = os.Getenv("PODCASTDELAY_DEFAULT_TIMEZONE")
	}
	if tz == "" {
		tz = "UTC"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return fmt.Errorf("invalid --timezone %q: %w", tz, err)
	}

	startAt, err := parseStart(*start, loc)
	if err != nil {
		return err
	}

	dataDir := os.Getenv("PODCASTDELAY_DATA_DIR")
	if dataDir == "" {
		dataDir = "/data"
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	st, err := store.Open(ctx, filepath.Join(dataDir, "podcastdelay.db"))
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	var titleOverride *string
	if *title != "" {
		titleOverride = title
	}
	var maxFeedItems *int
	if *maxItems > 0 {
		maxFeedItems = maxItems
	}

	sub, err := refresh.AddSubscription(ctx, st, source.NewFetcher(), refresh.AddParams{
		SourceURL:       sourceURL,
		TitleOverride:   titleOverride,
		CadenceDays:     cadenceDays,
		ReleaseTime:     *releaseTime,
		Timezone:        tz,
		StartAt:         startAt,
		SeedCount:       *seed,
		EpisodesPerSlot: *perSlot,
		MaxFeedItems:    maxFeedItems,
	})
	if err != nil {
		return fmt.Errorf("add subscription: %w", err)
	}

	baseURL := os.Getenv("PODCASTDELAY_BASE_URL")
	fmt.Printf("Added. Feed URL: %s/f/%s.xml\n", baseURL, sub.Token)
	return nil
}

func parseCadenceDays(every string) (int, error) {
	if len(every) == 0 {
		return 0, errors.New("--every must not be empty")
	}
	if every[len(every)-1] == 'd' {
		var n int
		if _, err := fmt.Sscanf(every, "%dd", &n); err == nil && n > 0 {
			return n, nil
		}
	}
	d, err := time.ParseDuration(every)
	if err != nil {
		return 0, fmt.Errorf("invalid --every %q (want e.g. \"7d\" or a Go duration): %w", every, err)
	}
	days := int(d.Hours() / 24)
	if days < 1 {
		days = 1
	}
	return days, nil
}

func parseStart(start string, loc *time.Location) (time.Time, error) {
	switch start {
	case "now":
		return time.Now().In(loc), nil
	case "tomorrow":
		now := time.Now().In(loc)
		return time.Date(now.Year(), now.Month(), now.Day()+1, now.Hour(), now.Minute(), 0, 0, loc), nil
	default:
		t, err := time.ParseInLocation(time.RFC3339, start, loc)
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid --start %q (want \"now\", \"tomorrow\", or RFC3339): %w", start, err)
		}
		return t, nil
	}
}
