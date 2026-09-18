// Package config loads PodcastDelay's runtime configuration entirely
// from environment variables (docs/IMPLEMENTATION.md §9.2), so the
// container image needs no rebuild and no baked-in config file.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// PollFloor is the minimum allowed poll interval, enforced regardless
// of what PODCASTDELAY_POLL_INTERVAL is set to — we will not hammer a
// publisher's feed no matter how it's configured.
const PollFloor = 1 * time.Hour

// TickInterval is how often the background refresh loop wakes up to
// check which subscriptions are due (docs §7): every 30 minutes.
const TickInterval = 30 * time.Minute

type Config struct {
	DataDir         string
	Addr            string
	BaseURL         string
	AdminUser       string
	AdminPassword   string
	DefaultTimezone string
	PollInterval    time.Duration
	LogLevel        string
}

// Load reads configuration from the environment, applying defaults and
// validating required fields. AdminPassword may come from
// PODCASTDELAY_ADMIN_PASSWORD directly or PODCASTDELAY_ADMIN_PASSWORD_FILE
// (read from disk), the small self-hosting convenience described in §9.3.
func Load() (Config, error) {
	c := Config{
		DataDir:         getenv("PODCASTDELAY_DATA_DIR", "/data"),
		Addr:            getenv("PODCASTDELAY_ADDR", ":8080"),
		BaseURL:         strings.TrimRight(os.Getenv("PODCASTDELAY_BASE_URL"), "/"),
		AdminUser:       os.Getenv("PODCASTDELAY_ADMIN_USER"),
		DefaultTimezone: getenv("PODCASTDELAY_DEFAULT_TIMEZONE", "UTC"),
		LogLevel:        getenv("PODCASTDELAY_LOG_LEVEL", "info"),
	}

	pw, err := readSecret("PODCASTDELAY_ADMIN_PASSWORD")
	if err != nil {
		return Config{}, err
	}
	c.AdminPassword = pw

	pollStr := getenv("PODCASTDELAY_POLL_INTERVAL", "6h")
	poll, err := time.ParseDuration(pollStr)
	if err != nil {
		return Config{}, fmt.Errorf("config: invalid PODCASTDELAY_POLL_INTERVAL %q: %w", pollStr, err)
	}
	if poll < PollFloor {
		poll = PollFloor
	}
	c.PollInterval = poll

	if c.BaseURL == "" {
		return Config{}, errors.New("config: PODCASTDELAY_BASE_URL is required")
	}
	if c.AdminUser == "" {
		return Config{}, errors.New("config: PODCASTDELAY_ADMIN_USER is required")
	}
	// Refuse to start without a real admin password: this ends up
	// reachable from the open internet (§9.4), so there is no safe
	// default credential to ship.
	if c.AdminPassword == "" {
		return Config{}, errors.New("config: PODCASTDELAY_ADMIN_PASSWORD (or _FILE) is required")
	}

	if _, err := time.LoadLocation(c.DefaultTimezone); err != nil {
		return Config{}, fmt.Errorf("config: invalid PODCASTDELAY_DEFAULT_TIMEZONE %q: %w", c.DefaultTimezone, err)
	}

	return c, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// readSecret reads KEY, or KEY_FILE's file contents if KEY is unset.
func readSecret(key string) (string, error) {
	if v := os.Getenv(key); v != "" {
		return v, nil
	}
	pathKey := key + "_FILE"
	path := os.Getenv(pathKey)
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("config: read %s (%s): %w", pathKey, path, err)
	}
	return strings.TrimSpace(string(b)), nil
}

// ParseBool is a tiny helper for optional boolean env vars elsewhere.
func ParseBool(s string, def bool) bool {
	if s == "" {
		return def
	}
	b, err := strconv.ParseBool(s)
	if err != nil {
		return def
	}
	return b
}
