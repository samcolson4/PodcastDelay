package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// clearEnv makes a test independent of whatever the developer happens to
// have exported; an empty value reads as unset.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"PODCASTDELAY_DATA_DIR", "PODCASTDELAY_ADDR", "PODCASTDELAY_BASE_URL",
		"PODCASTDELAY_ADMIN_USER", "PODCASTDELAY_ADMIN_PASSWORD",
		"PODCASTDELAY_ADMIN_PASSWORD_FILE", "PODCASTDELAY_DEFAULT_TIMEZONE",
		"PODCASTDELAY_POLL_INTERVAL", "PODCASTDELAY_LOG_LEVEL",
	} {
		t.Setenv(key, "")
	}
}

func TestLoad_Defaults(t *testing.T) {
	clearEnv(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load with an empty environment should still parse: %v", err)
	}
	if c.DataDir != "/data" || c.Addr != ":8080" {
		t.Errorf("unexpected defaults: DataDir=%q Addr=%q", c.DataDir, c.Addr)
	}
	if c.DefaultTimezone != "UTC" || c.LogLevel != "info" {
		t.Errorf("unexpected defaults: timezone=%q log level=%q", c.DefaultTimezone, c.LogLevel)
	}
	if c.PollInterval != 6*time.Hour {
		t.Errorf("PollInterval = %v, want 6h", c.PollInterval)
	}
	if got, want := c.DBPath(), filepath.Join("/data", dbFileName); got != want {
		t.Errorf("DBPath = %q, want %q", got, want)
	}
}

func TestLoad_PollIntervalFloor(t *testing.T) {
	clearEnv(t)
	t.Setenv("PODCASTDELAY_POLL_INTERVAL", "5m")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.PollInterval != PollFloor {
		t.Errorf("PollInterval = %v, want the %v floor", c.PollInterval, PollFloor)
	}
}

func TestLoad_RejectsUnparseableValues(t *testing.T) {
	t.Run("poll interval", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("PODCASTDELAY_POLL_INTERVAL", "weekly")
		if _, err := Load(); err == nil {
			t.Error("expected an error for a non-duration poll interval")
		}
	})
	t.Run("timezone", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("PODCASTDELAY_DEFAULT_TIMEZONE", "Mars/Olympus")
		if _, err := Load(); err == nil {
			t.Error("expected an error for an unknown timezone")
		}
	})
}

func TestLoad_TrimsBaseURLSlash(t *testing.T) {
	clearEnv(t)
	t.Setenv("PODCASTDELAY_BASE_URL", "https://podcasts.example.com/")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseURL != "https://podcasts.example.com" {
		t.Errorf("BaseURL = %q, want no trailing slash", c.BaseURL)
	}
}

func TestLoad_AdminPasswordFromFile(t *testing.T) {
	clearEnv(t)
	path := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(path, []byte("  s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PODCASTDELAY_ADMIN_PASSWORD_FILE", path)

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.AdminPassword != "s3cret" {
		t.Errorf("AdminPassword = %q, want the trimmed file contents", c.AdminPassword)
	}

	t.Setenv("PODCASTDELAY_ADMIN_PASSWORD_FILE", filepath.Join(t.TempDir(), "missing"))
	if _, err := Load(); err == nil {
		t.Error("expected an error when the password file is unreadable")
	}
}

// The serve command needs a public URL and admin credentials; the one-shot
// CLI commands share the same loader and must not be forced to set them.
func TestRequireServe(t *testing.T) {
	full := Config{BaseURL: "https://example.com", AdminUser: "sam", AdminPassword: "pw"}
	if err := full.RequireServe(); err != nil {
		t.Errorf("complete config rejected: %v", err)
	}

	for name, c := range map[string]Config{
		"no base url": {AdminUser: "sam", AdminPassword: "pw"},
		"no user":     {BaseURL: "https://example.com", AdminPassword: "pw"},
		"no password": {BaseURL: "https://example.com", AdminUser: "sam"},
	} {
		if err := c.RequireServe(); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
