package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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

	s, err := New(st, source.NewFetcher(), "http://localhost:8080", "admin", "secret", "UTC", slog.New(slog.NewTextHandler(io.Discard, nil)))
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
		"start_at":          {time.Now().Add(-time.Hour).Format("2006-01-02T15:04")},
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
