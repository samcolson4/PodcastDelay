package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/samcolson4/podcastdelay/internal/schedule"
)

// startAtLayout is what <input type="datetime-local"> posts, and what the
// same field is rendered back as.
const startAtLayout = "2006-01-02T15:04"

// The admin UI and the form-encoded API share one set of field readers so
// that "cadence_days must be a positive integer" means the same thing on
// create as it does on edit. Each returns nil when the field is absent,
// which both handlers read as "leave it alone".

// optionalInt reads form field name as an integer of at least min,
// returning nil when the field is absent or blank.
func optionalInt(r *http.Request, name string, min int) (*int, error) {
	v := strings.TrimSpace(r.FormValue(name))
	if v == "" {
		return nil, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < min {
		return nil, fmt.Errorf("%s must be an integer >= %d", name, min)
	}
	return &n, nil
}

// intOrDefault is optionalInt with a fallback for the create form, where
// every scheduling field has a sensible default.
func intOrDefault(r *http.Request, name string, min, def int) (int, error) {
	n, err := optionalInt(r, name, min)
	if err != nil || n == nil {
		return def, err
	}
	return *n, nil
}

// optionalString reads a trimmed form field, returning nil when blank.
func optionalString(r *http.Request, name string) *string {
	v := strings.TrimSpace(r.FormValue(name))
	if v == "" {
		return nil
	}
	return &v
}

// formChecked reads an HTML checkbox: present (any value) means on.
// Checkboxes are the one field shape where absence is "off" rather than
// "leave it alone", because a browser omits an unticked box entirely.
func formChecked(r *http.Request, name string) bool {
	return strings.TrimSpace(r.FormValue(name)) != ""
}

// optionalFeedURL reads a feed URL, rejecting anything we couldn't fetch
// so a typo surfaces on the form rather than as a background poll error.
func optionalFeedURL(r *http.Request, name string) (*string, error) {
	v := optionalString(r, name)
	if v == nil {
		return nil, nil
	}
	u, err := url.Parse(*v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%s must be an http(s) URL", name)
	}
	return v, nil
}

// optionalTimezone validates an IANA timezone name.
func optionalTimezone(r *http.Request, name string) (*string, error) {
	tz := optionalString(r, name)
	if tz == nil {
		return nil, nil
	}
	if _, err := time.LoadLocation(*tz); err != nil {
		return nil, fmt.Errorf("invalid %s %q", name, *tz)
	}
	return tz, nil
}

// optionalReleaseTime validates and canonicalizes an "HH:MM" field, so an
// unusable value can never reach the schedule engine (where it would fail
// on every later recompute instead of here).
func optionalReleaseTime(r *http.Request, name string) (*string, error) {
	v := optionalString(r, name)
	if v == nil {
		return nil, nil
	}
	normalized, err := schedule.NormalizeReleaseTime(*v)
	if err != nil {
		return nil, fmt.Errorf("%s must be a wall-clock time like 07:00", name)
	}
	return &normalized, nil
}

// optionalCadenceMode validates the fixed/original choice.
func optionalCadenceMode(r *http.Request, name string) (*string, error) {
	v := optionalString(r, name)
	if v == nil {
		return nil, nil
	}
	if *v != schedule.ModeFixed && *v != schedule.ModeOriginal {
		return nil, fmt.Errorf("%s must be %s or %s", name, schedule.ModeFixed, schedule.ModeOriginal)
	}
	return v, nil
}

// optionalStartAt parses a datetime-local value as wall-clock time in loc,
// which is the timezone the subscription's releases are computed in.
func optionalStartAt(r *http.Request, name string, loc *time.Location) (*time.Time, error) {
	v := optionalString(r, name)
	if v == nil {
		return nil, nil
	}
	t, err := time.ParseInLocation(startAtLayout, *v, loc)
	if err != nil {
		return nil, fmt.Errorf("%s must look like 2026-01-06T07:00", name)
	}
	return &t, nil
}
