package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/samcolson4/podcastdelay/internal/schedule"
	"github.com/samcolson4/podcastdelay/internal/source"
	"github.com/samcolson4/podcastdelay/internal/store"
)

const testFeed = `<?xml version="1.0"?>
<rss version="2.0" xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd">
<channel>
  <title>Widget Weekly</title>
  <description>Widgets, weekly.</description>
  <item>
    <title>Ep 1</title>
    <guid>guid-1</guid>
    <pubDate>Mon, 02 Mar 2020 08:00:00 GMT</pubDate>
    <enclosure url="https://chtbl.com/track/X/cdn.example.com/1.mp3" length="100" type="audio/mpeg"/>
  </item>
  <item>
    <title>Ep 2</title>
    <guid>guid-2</guid>
    <pubDate>Mon, 09 Mar 2020 08:00:00 GMT</pubDate>
    <enclosure url="https://chtbl.com/track/X/cdn.example.com/2.mp3" length="100" type="audio/mpeg"/>
  </item>
</channel>
</rss>`

type testHarness struct {
	server  *Server
	http    *httptest.Server
	store   *store.Store
	feedSrv *httptest.Server
}

func newTestHarness(t *testing.T) *testHarness {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	feedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		w.Write([]byte(testFeed))
	}))
	t.Cleanup(feedSrv.Close)

	s, err := New(Options{
		Store:           st,
		Fetcher:         source.NewFetcher(),
		BaseURL:         "http://localhost:8080",
		AdminUser:       "admin",
		AdminPassword:   "secret",
		DefaultTimezone: "UTC",
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	httpSrv := httptest.NewServer(s.Routes())
	t.Cleanup(httpSrv.Close)

	return &testHarness{server: s, http: httpSrv, store: st, feedSrv: feedSrv}
}

func (h *testHarness) adminRequest(t *testing.T, method, path string, form url.Values) *http.Response {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, h.http.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth("admin", "secret")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := h.http.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func (h *testHarness) addSubscription(t *testing.T) store.Subscription {
	t.Helper()
	form := url.Values{
		"source_url":        {h.feedSrv.URL},
		"cadence_days":      {"7"},
		"release_time":      {"07:00"},
		"timezone":          {"UTC"},
		"start_at":          {time.Now().UTC().Add(-time.Hour).Format(startAtLayout)},
		"seed_count":        {"1"},
		"episodes_per_slot": {"1"},
	}
	resp := h.adminRequest(t, http.MethodPost, "/admin/subscriptions", form)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("add subscription: expected 200 (redirect followed), got %d", resp.StatusCode)
	}
	subs, err := h.store.ListSubscriptions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 1 {
		t.Fatalf("expected 1 subscription, got %d", len(subs))
	}
	return subs[0]
}

func TestAdminRoutes_RequireAuth(t *testing.T) {
	h := newTestHarness(t)
	resp, err := http.Get(h.http.URL + "/admin")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 without auth, got %d", resp.StatusCode)
	}
}

func TestHealthz(t *testing.T) {
	h := newTestHarness(t)
	resp, err := http.Get(h.http.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

func TestFeedNotFound(t *testing.T) {
	h := newTestHarness(t)
	resp, err := http.Get(h.http.URL + "/f/nonexistent.xml")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestCreateSubscriptionAndServeFeed(t *testing.T) {
	h := newTestHarness(t)
	sub := h.addSubscription(t)

	feedResp, err := http.Get(h.http.URL + "/f/" + sub.Token + ".xml")
	if err != nil {
		t.Fatal(err)
	}
	defer feedResp.Body.Close()
	if feedResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from feed route, got %d", feedResp.StatusCode)
	}
	if ct := feedResp.Header.Get("Content-Type"); !strings.Contains(ct, "rss+xml") {
		t.Errorf("expected rss content type, got %q", ct)
	}
	body, err := io.ReadAll(feedResp.Body)
	if err != nil {
		t.Fatal(err)
	}
	bodyStr := string(body)

	if !strings.Contains(bodyStr, "podcastdelay:"+sub.Token+":guid-1") {
		t.Errorf("expected prefixed guid in output:\n%s", bodyStr)
	}
	if !strings.Contains(bodyStr, "chtbl.com/track/X") {
		t.Errorf("expected enclosure passthrough in output:\n%s", bodyStr)
	}
	// Only the seeded episode (position 0) should have released given
	// start_at an hour ago and cadence 7 days.
	if strings.Contains(bodyStr, "guid-2") {
		t.Errorf("unseeded, not-yet-due episode should not appear yet:\n%s", bodyStr)
	}

	etag := feedResp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("expected an ETag header")
	}
	req2, _ := http.NewRequest(http.MethodGet, h.http.URL+"/f/"+sub.Token+".xml", nil)
	req2.Header.Set("If-None-Match", etag)
	resp2, err := h.http.Client().Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotModified {
		t.Errorf("expected 304 on matching ETag, got %d", resp2.StatusCode)
	}
}

func TestPauseResumeDelete(t *testing.T) {
	h := newTestHarness(t)
	sub := h.addSubscription(t)
	id := strconv.FormatInt(sub.ID, 10)

	pauseResp := h.adminRequest(t, http.MethodPost, "/admin/subscriptions/"+id+"/pause", url.Values{})
	pauseResp.Body.Close()
	if pauseResp.StatusCode != http.StatusOK && pauseResp.StatusCode != http.StatusSeeOther {
		t.Errorf("pause: unexpected status %d", pauseResp.StatusCode)
	}
	paused, err := h.store.GetSubscription(context.Background(), sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if paused.PausedAt == nil {
		t.Fatal("expected paused_at to be set")
	}

	resumeResp := h.adminRequest(t, http.MethodPost, "/admin/subscriptions/"+id+"/resume", url.Values{})
	resumeResp.Body.Close()
	if resumeResp.StatusCode != http.StatusOK && resumeResp.StatusCode != http.StatusSeeOther {
		t.Errorf("resume: unexpected status %d", resumeResp.StatusCode)
	}
	resumed, err := h.store.GetSubscription(context.Background(), sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.PausedAt != nil {
		t.Error("expected paused_at cleared after resume")
	}

	delResp := h.adminRequest(t, http.MethodPost, "/admin/subscriptions/"+id+"/delete", url.Values{})
	delResp.Body.Close()
	remaining, err := h.store.ListSubscriptions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Errorf("expected subscription deleted, got %d remaining", len(remaining))
	}
}

func TestPatchSubscription_ReschedulesUnlockedOnly(t *testing.T) {
	h := newTestHarness(t)
	sub := h.addSubscription(t)
	id := strconv.FormatInt(sub.ID, 10)

	before, err := h.store.ListBySubscription(context.Background(), sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Seeded episode (position 0) is already released (start_at was an
	// hour ago); lock it explicitly via a reschedule pass first.
	// (handleCreateSubscription doesn't lock on its own — see
	// internal/refresh's Reschedule for why.)
	patchResp := h.adminRequest(t, http.MethodPost, "/admin/subscriptions/"+id+"/edit", url.Values{
		"cadence_days": {"3"},
	})
	patchResp.Body.Close()
	if patchResp.StatusCode != http.StatusOK && patchResp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(patchResp.Body)
		t.Fatalf("patch: unexpected status %d: %s", patchResp.StatusCode, body)
	}

	after, err := h.store.ListBySubscription(context.Background(), sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !after[0].Locked {
		t.Error("expected past-due episode to be locked by the patch's implicit reschedule")
	}
	if !after[0].ScheduledAt.Equal(before[0].ScheduledAt) {
		t.Errorf("locked episode's scheduled_at moved: got %v, want %v", after[0].ScheduledAt, before[0].ScheduledAt)
	}
	if after[1].ScheduledAt.Equal(before[1].ScheduledAt) {
		t.Error("expected unlocked future episode's scheduled_at to change after cadence edit")
	}
}

func TestCreateSubscription_RejectsBadFieldsWithoutCreating(t *testing.T) {
	h := newTestHarness(t)

	for _, tc := range []struct {
		name  string
		field string
		value string
	}{
		{name: "cadence", field: "cadence_days", value: "0"},
		{name: "release time", field: "release_time", value: "7am"},
		{name: "cadence mode", field: "cadence_mode", value: "weekly"},
		{name: "timezone", field: "timezone", value: "Mars/Olympus"},
		{name: "seed count", field: "seed_count", value: "-1"},
	} {
		form := url.Values{"source_url": {h.feedSrv.URL}, tc.field: {tc.value}}
		resp := h.adminRequest(t, http.MethodPost, "/admin/subscriptions", form)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(body), tc.field) {
			t.Errorf("%s: expected the form error to name %q, got:\n%s", tc.name, tc.field, body)
		}
	}

	subs, err := h.store.ListSubscriptions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 0 {
		t.Errorf("expected no subscriptions to be created, got %d", len(subs))
	}
}

func TestPatchSubscription_RejectsBadFields(t *testing.T) {
	h := newTestHarness(t)
	sub := h.addSubscription(t)
	id := strconv.FormatInt(sub.ID, 10)

	for _, form := range []url.Values{
		{"cadence_days": {"0"}},
		{"episodes_per_slot": {"nope"}},
		{"release_time": {"25:00"}},
		{"cadence_mode": {"weekly"}},
		{"timezone": {"Mars/Olympus"}},
		{"start_at": {"yesterday"}},
	} {
		resp := h.adminRequest(t, http.MethodPost, "/admin/subscriptions/"+id+"/edit", form)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("patch %v: expected 400, got %d", form, resp.StatusCode)
		}
	}

	unchanged, err := h.store.GetSubscription(context.Background(), sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.CadenceDays != sub.CadenceDays || unchanged.ReleaseTime != sub.ReleaseTime {
		t.Errorf("rejected patch still changed the subscription: %+v", unchanged)
	}
}

// Saving the edit form without touching start_at must not move it: the
// field is rendered in the subscription's timezone and parsed back in the
// same one, so a zone offset can't creep in on every save.
func TestSchedulePage_StartAtRoundTrips(t *testing.T) {
	h := newTestHarness(t)
	sub := h.addSubscription(t)
	id := strconv.FormatInt(sub.ID, 10)

	patch := h.adminRequest(t, http.MethodPost, "/admin/subscriptions/"+id+"/edit",
		url.Values{"timezone": {"Europe/London"}})
	patch.Body.Close()

	before, err := h.store.GetSubscription(context.Background(), sub.ID)
	if err != nil {
		t.Fatal(err)
	}

	page := h.adminRequest(t, http.MethodGet, "/admin/subscriptions/"+id+"/schedule", nil)
	body, err := io.ReadAll(page.Body)
	page.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`name="start_at"[^>]*value="([^"]+)"`).FindStringSubmatch(string(body))
	if m == nil {
		t.Fatalf("no start_at field on the schedule page:\n%s", body)
	}

	resave := h.adminRequest(t, http.MethodPost, "/admin/subscriptions/"+id+"/edit", url.Values{
		"timezone": {"Europe/London"},
		"start_at": {m[1]},
	})
	resave.Body.Close()

	after, err := h.store.GetSubscription(context.Background(), sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The form only carries minutes, so compare at that resolution.
	if !after.StartAt.Truncate(time.Minute).Equal(before.StartAt.Truncate(time.Minute)) {
		t.Errorf("start_at moved on an unchanged save: got %v, want %v (form value %q)",
			after.StartAt, before.StartAt, m[1])
	}
}

func TestStaticAssets_ServedWithoutAuth(t *testing.T) {
	h := newTestHarness(t)
	for _, p := range []string{"/static/pico.min.css", "/static/htmx.min.js", "/static/app.js", "/static/app.css"} {
		resp, err := http.Get(h.http.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: expected 200, got %d", p, resp.StatusCode)
		}
	}
	resp, err := http.Get(h.http.URL + "/static/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("/static/: expected 404 (no listing), got %d", resp.StatusCode)
	}
}

func TestReleaseNext(t *testing.T) {
	h := newTestHarness(t)
	sub := h.addSubscription(t) // seed 1: ep 1 released, ep 2 upcoming
	id := strconv.FormatInt(sub.ID, 10)

	resp := h.adminRequest(t, http.MethodPost, "/admin/subscriptions/"+id+"/release-next", url.Values{"mode": {"keep"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("release-next: expected 200 (redirect followed), got %d", resp.StatusCode)
	}
	eps, err := h.store.ListBySubscription(context.Background(), sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !eps[1].Locked || eps[1].ScheduledAt.After(time.Now()) {
		t.Errorf("episode 2 should be released and locked, got locked=%v at=%v", eps[1].Locked, eps[1].ScheduledAt)
	}

	// Nothing upcoming is left now.
	resp = h.adminRequest(t, http.MethodPost, "/admin/subscriptions/"+id+"/release-next", url.Values{"mode": {"keep"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("second release-next: expected 409, got %d", resp.StatusCode)
	}
}

// The same show's premium feed: a bonus episode half an hour after each
// free one.
const testPremiumFeed = `<?xml version="1.0"?>
<rss version="2.0" xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd">
<channel>
  <title>Widget Weekly Extra</title>
  <description>Widgets, bonus.</description>
  <item>
    <title>Bonus 1</title>
    <guid>bonus-1</guid>
    <pubDate>Mon, 02 Mar 2020 08:30:00 GMT</pubDate>
    <enclosure url="https://chtbl.com/track/X/cdn.example.com/bonus-1.mp3" length="100" type="audio/mpeg"/>
  </item>
  <item>
    <title>Bonus 2</title>
    <guid>bonus-2</guid>
    <pubDate>Mon, 09 Mar 2020 08:30:00 GMT</pubDate>
    <enclosure url="https://chtbl.com/track/X/cdn.example.com/bonus-2.mp3" length="100" type="audio/mpeg"/>
  </item>
</channel>
</rss>`

// premiumFeedServer serves the premium feed above.
func (h *testHarness) premiumFeedServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"p1"`)
		w.Write([]byte(testPremiumFeed))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (h *testHarness) episodeTitles(t *testing.T, subID int64) []string {
	t.Helper()
	episodes, err := h.store.ListBySubscription(context.Background(), subID)
	if err != nil {
		t.Fatal(err)
	}
	titles := make([]string, 0, len(episodes))
	for _, e := range episodes {
		titles = append(titles, e.Title)
	}
	return titles
}

func TestCreateSubscriptionWithPremiumFeed_MergesBothIntoOneFeed(t *testing.T) {
	h := newTestHarness(t)
	premium := h.premiumFeedServer(t)

	form := url.Values{
		"source_url":         {h.feedSrv.URL},
		"premium_source_url": {premium.URL},
		"cadence_days":       {"7"},
		"release_time":       {"07:00"},
		"timezone":           {"UTC"},
		"start_at":           {time.Now().UTC().Add(-time.Hour).Format(startAtLayout)},
		"seed_count":         {"1"},
		"episodes_per_slot":  {"1"},
	}
	resp := h.adminRequest(t, http.MethodPost, "/admin/subscriptions", form)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("add with premium feed: got %d:\n%s", resp.StatusCode, body)
	}

	subs, err := h.store.ListSubscriptions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 1 {
		t.Fatalf("expected 1 subscription, got %d", len(subs))
	}
	sub := subs[0]
	if sub.PremiumSourceURL == nil || *sub.PremiumSourceURL != premium.URL {
		t.Fatalf("premium url not stored: %v", sub.PremiumSourceURL)
	}
	if titles := h.episodeTitles(t, sub.ID); len(titles) != 4 {
		t.Fatalf("expected 2 free + 2 premium episodes, got %v", titles)
	}

	feedResp, err := http.Get(h.http.URL + "/f/" + sub.Token + ".xml")
	if err != nil {
		t.Fatal(err)
	}
	defer feedResp.Body.Close()
	feedBody, err := io.ReadAll(feedResp.Body)
	if err != nil {
		t.Fatal(err)
	}
	rendered := string(feedBody)

	// The free feed started an hour ago, so its first episode is out and
	// the bonus episode published half an hour after it is too. The
	// second pair is still in the future.
	for _, want := range []string{"podcastdelay:" + sub.Token + ":guid-1", "podcastdelay:" + sub.Token + ":premium:bonus-1"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("expected %q in the merged feed:\n%s", want, rendered)
		}
	}
	for _, unwanted := range []string{"guid-2", "bonus-2"} {
		if strings.Contains(rendered, unwanted) {
			t.Errorf("%q is not due yet but appeared:\n%s", unwanted, rendered)
		}
	}
	// Newest first: the bonus episode released after the free one, so it
	// leads the feed.
	if strings.Index(rendered, "Bonus 1") > strings.Index(rendered, "<title>Ep 1</title>") {
		t.Errorf("expected the later-released bonus episode first:\n%s", rendered)
	}
}

func TestFeedGUID_PremiumEpisodesStayDistinct(t *testing.T) {
	// A premium feed is often an ad-free re-cut of the same episodes
	// under the same GUIDs. Two episodes rendering one GUID would be
	// deduplicated into one by the app, so the role is part of it.
	free := store.Episode{GUID: "guid-1", SourceRole: schedule.RolePrimary}
	premium := store.Episode{GUID: "guid-1", SourceRole: schedule.RolePremium}

	// The free feed's form is the one docs §3.4 specifies and must not
	// drift: a GUID that changes looks like a new episode to the app.
	if got, want := feedGUID("tok", free), "podcastdelay:tok:guid-1"; got != want {
		t.Errorf("free episode guid = %q, want %q", got, want)
	}
	if feedGUID("tok", premium) == feedGUID("tok", free) {
		t.Errorf("premium episode shares the free one's guid: %q", feedGUID("tok", premium))
	}
	// An episode row written before source_role existed reads as free.
	if got := feedGUID("tok", store.Episode{GUID: "guid-1"}); got != feedGUID("tok", free) {
		t.Errorf("roleless episode guid = %q, want the free feed's form", got)
	}
}

func TestPatchSubscription_MeldsAndUnmeldsAPremiumFeed(t *testing.T) {
	h := newTestHarness(t)
	sub := h.addSubscription(t)
	premium := h.premiumFeedServer(t)
	id := strconv.FormatInt(sub.ID, 10)
	ctx := context.Background()

	before, err := h.store.ListBySubscription(ctx, sub.ID)
	if err != nil {
		t.Fatal(err)
	}

	resp := h.adminRequest(t, http.MethodPost, "/admin/subscriptions/"+id+"/edit",
		url.Values{"premium_source_url": {premium.URL}})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("meld: got %d:\n%s", resp.StatusCode, body)
	}

	melded, err := h.store.GetSubscription(ctx, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if melded.PremiumSourceURL == nil || *melded.PremiumSourceURL != premium.URL {
		t.Fatalf("premium url not stored: %v", melded.PremiumSourceURL)
	}
	after, err := h.store.ListBySubscription(ctx, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before)+2 {
		t.Fatalf("expected the 2 bonus episodes to be ingested, got %v", h.episodeTitles(t, sub.ID))
	}
	// The free feed's already-released episode keeps its date.
	if !after[0].ScheduledAt.Equal(before[0].ScheduledAt) {
		t.Errorf("melding moved an already-released episode: %v -> %v", before[0].ScheduledAt, after[0].ScheduledAt)
	}

	// Unmelding takes the bonus episodes with it.
	resp = h.adminRequest(t, http.MethodPost, "/admin/subscriptions/"+id+"/edit",
		url.Values{"premium_source_url": {premium.URL}, "remove_premium": {"1"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unmeld: got %d", resp.StatusCode)
	}
	unmelded, err := h.store.GetSubscription(ctx, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unmelded.PremiumSourceURL != nil {
		t.Errorf("premium url survived removal: %v", *unmelded.PremiumSourceURL)
	}
	if titles := h.episodeTitles(t, sub.ID); len(titles) != len(before) {
		t.Errorf("expected only the free feed's episodes to remain, got %v", titles)
	}
}

func TestPremiumFeedURL_MustLookFetchable(t *testing.T) {
	h := newTestHarness(t)

	resp := h.adminRequest(t, http.MethodPost, "/admin/subscriptions",
		url.Values{"source_url": {h.feedSrv.URL}, "premium_source_url": {"feeds.example.com/premium"}})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "premium_source_url") {
		t.Errorf("expected the form error to name premium_source_url, got:\n%s", body)
	}
	subs, err := h.store.ListSubscriptions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 0 {
		t.Fatalf("expected nothing created, got %d subscriptions", len(subs))
	}

	sub := h.addSubscription(t)
	resp = h.adminRequest(t, http.MethodPost, "/admin/subscriptions/"+strconv.FormatInt(sub.ID, 10)+"/edit",
		url.Values{"premium_source_url": {"not a url at all"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for an unusable premium url, got %d", resp.StatusCode)
	}
}

func TestFeedETag_ChangesWhenAPremiumEpisodeReleases(t *testing.T) {
	// A premium episode's position is at the tail of the sequence
	// whatever its date, so an ETag keyed on the highest released
	// position would not move when one comes out — and the app would
	// keep getting a 304 for a feed that has changed.
	sub := store.Subscription{UpdatedAt: time.Date(2026, 1, 1, 7, 0, 0, 0, time.UTC)}
	free := []store.Episode{
		{Position: 0, ScheduledAt: time.Date(2026, 1, 1, 7, 0, 0, 0, time.UTC)},
		{Position: 9, ScheduledAt: time.Date(2026, 1, 8, 7, 0, 0, 0, time.UTC)},
	}
	withPremium := append(append([]store.Episode{}, free...), store.Episode{
		Position: 3, SourceRole: schedule.RolePremium,
		ScheduledAt: time.Date(2026, 1, 8, 8, 0, 0, 0, time.UTC),
	})

	if feedETag(sub, free) == feedETag(sub, withPremium) {
		t.Error("the ETag did not change when a premium episode released")
	}
	if feedETag(sub, free) != feedETag(sub, free) {
		t.Error("the ETag is not stable for an unchanged feed")
	}
}
