// Command podcastdelay serves and manages delayed podcast feeds. Run
// with no arguments (or "serve") to start the HTTP server; "add" adds a
// feed from the command line without going through the admin UI; "meld"
// adds (or removes) a premium feed on a show that already exists;
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
	"strconv"
	"strings"
	"syscall"
	"time"

	// Embeds the IANA timezone database into the binary so
	// time.LoadLocation works on a scratch/distroless image with no
	// /usr/share/zoneinfo (docs §9.1).
	_ "time/tzdata"

	"github.com/samcolson4/podcastdelay/internal/config"
	"github.com/samcolson4/podcastdelay/internal/refresh"
	"github.com/samcolson4/podcastdelay/internal/schedule"
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
	case "meld":
		err = runMeld(args)
	default:
		err = fmt.Errorf("unknown command %q (expected serve, add, meld, or healthcheck)", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "podcastdelay:", err)
		os.Exit(1)
	}
}

func isFlag(s string) bool {
	return strings.HasPrefix(s, "-")
}

// parseArgs parses flags wherever they appear and returns the positional
// arguments. Go's flag package stops parsing at the first non-flag
// argument, but `add <url> --every 7d` — the obvious way to write these
// commands, and the form the README documents — puts the URL first,
// which would silently leave every flag after it at its default.
func parseArgs(fs *flag.FlagSet, args []string) []string {
	var positional []string
	for {
		fs.Parse(args)
		if fs.NArg() == 0 {
			return positional
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	fs.Parse(args)

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.RequireServe(); err != nil {
		return err
	}

	logger := newLogger(cfg.LogLevel)
	slog.SetDefault(logger)

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.DBPath())
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	fetcher := source.NewFetcher()

	srv, err := web.New(web.Options{
		Store:           st,
		Fetcher:         fetcher,
		BaseURL:         cfg.BaseURL,
		AdminUser:       cfg.AdminUser,
		AdminPassword:   cfg.AdminPassword,
		DefaultTimezone: cfg.DefaultTimezone,
		Logger:          logger,
	})
	if err != nil {
		return fmt.Errorf("build web server: %w", err)
	}

	if cfg.DevWebDir != "" {
		srv.DevWebDir = cfg.DevWebDir
		logger.Warn("serving web assets from disk (dev mode)", "dir", cfg.DevWebDir)
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

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	addr := cfg.Addr
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
	premium := fs.String("premium", "", "the show's premium/bonus feed URL, melded into the same delayed feed")
	every := fs.String("every", "7d", `cadence, e.g. "7d" for weekly, or "original" to mirror the show's real release gaps`)
	start := fs.String("start", "now", `"now", "tomorrow", or RFC3339 (e.g. 2026-01-06T07:00:00Z)`)
	seed := fs.Int("seed", 1, "episodes you have already heard, released immediately on day one")
	perSlot := fs.Int("episodes-per-slot", 1, "episodes released per cadence slot")
	releaseTime := fs.String("release-time", "07:00", `wall-clock release time, "HH:MM"`)
	timezone := fs.String("timezone", "", "IANA timezone (defaults to PODCASTDELAY_DEFAULT_TIMEZONE or UTC)")
	title := fs.String("title", "", "title override for the delayed feed")
	maxItems := fs.Int("max-feed-items", 0, "cap the rendered feed window (0 = unlimited)")
	positional := parseArgs(fs, args)

	if len(positional) < 1 {
		return errors.New("usage: podcastdelay add <source-url> [--premium <url>] [--every 7d] [--start tomorrow] [--seed 1]")
	}
	sourceURL := positional[0]

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	cadenceMode, cadenceDays := schedule.ModeFixed, 7
	if *every == schedule.ModeOriginal {
		cadenceMode = schedule.ModeOriginal
	} else if cadenceDays, err = parseCadenceDays(*every); err != nil {
		return err
	}

	tz := *timezone
	if tz == "" {
		tz = cfg.DefaultTimezone
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return fmt.Errorf("invalid --timezone %q: %w", tz, err)
	}

	startAt, err := parseStart(*start, loc)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	st, err := store.Open(ctx, cfg.DBPath())
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
	var premiumURL *string
	if *premium != "" {
		premiumURL = premium
	}

	sub, err := refresh.AddSubscription(ctx, st, source.NewFetcher(), refresh.AddParams{
		SourceURL:        sourceURL,
		PremiumSourceURL: premiumURL,
		TitleOverride:    titleOverride,
		CadenceDays:      cadenceDays,
		CadenceMode:      cadenceMode,
		ReleaseTime:      *releaseTime,
		Timezone:         tz,
		StartAt:          startAt,
		SeedCount:        *seed,
		EpisodesPerSlot:  *perSlot,
		MaxFeedItems:     maxFeedItems,
	})
	if err != nil {
		return fmt.Errorf("add subscription: %w", err)
	}

	fmt.Printf("Added. Feed URL: %s/f/%s.xml\n", cfg.BaseURL, sub.Token)
	return nil
}

// runMeld melds a premium feed into a show that already exists, or
// removes one: `podcastdelay meld <id-or-token> <premium-url>`. The
// id or token is the one printed by `add` (and shown in the admin UI's
// feed URL).
func runMeld(args []string) error {
	fs := flag.NewFlagSet("meld", flag.ExitOnError)
	remove := fs.Bool("remove", false, "unmeld the premium feed and delete the episodes that came from it")
	positional := parseArgs(fs, args)

	if len(positional) < 1 || (len(positional) < 2 && !*remove) {
		return errors.New("usage: podcastdelay meld <id-or-token> <premium-url> | podcastdelay meld <id-or-token> --remove")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	st, err := store.Open(ctx, cfg.DBPath())
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	sub, err := findSubscription(ctx, st, positional[0])
	if err != nil {
		return err
	}

	var premiumURL *string
	if !*remove {
		url := positional[1]
		premiumURL = &url
	}
	if err := refresh.SetPremiumFeed(ctx, st, source.NewFetcher(), sub.ID, premiumURL); err != nil {
		return fmt.Errorf("meld premium feed: %w", err)
	}

	if *remove {
		fmt.Printf("Unmelded the premium feed from %s/f/%s.xml\n", cfg.BaseURL, sub.Token)
		return nil
	}
	fmt.Printf("Melded. Feed URL: %s/f/%s.xml\n", cfg.BaseURL, sub.Token)
	return nil
}

// findSubscription resolves the one argument a CLI user actually has to
// hand: either the numeric id or the feed token.
func findSubscription(ctx context.Context, st *store.Store, ref string) (store.Subscription, error) {
	if id, err := strconv.ParseInt(ref, 10, 64); err == nil {
		sub, err := st.GetSubscription(ctx, id)
		if err != nil {
			return store.Subscription{}, fmt.Errorf("no feed with id %d: %w", id, err)
		}
		return sub, nil
	}
	sub, err := st.GetSubscriptionByToken(ctx, ref)
	if err != nil {
		return store.Subscription{}, fmt.Errorf("no feed with id or token %q: %w", ref, err)
	}
	return sub, nil
}

func parseCadenceDays(every string) (int, error) {
	if every == "" {
		return 0, errors.New("--every must not be empty")
	}
	if days, ok := strings.CutSuffix(every, "d"); ok {
		n, err := strconv.Atoi(days)
		if err == nil && n > 0 {
			return n, nil
		}
		return 0, fmt.Errorf("invalid --every %q (want e.g. \"7d\")", every)
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
