package schedule

import (
	"testing"
	"time"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load location %q: %v", name, err)
	}
	return loc
}

func TestReleaseAt_Seeded(t *testing.T) {
	loc := mustLoc(t, "Europe/London")
	start := time.Date(2026, 1, 5, 7, 0, 0, 0, loc)
	cfg := Config{
		StartAt:         start,
		CadenceDays:     7,
		ReleaseTime:     "07:00",
		Location:        loc,
		SeedCount:       3,
		EpisodesPerSlot: 1,
	}

	for pos := 0; pos < 3; pos++ {
		got, err := cfg.ReleaseAt(pos)
		if err != nil {
			t.Fatalf("position %d: %v", pos, err)
		}
		want := start.Add(time.Duration(pos) * time.Minute)
		if !got.Equal(want) {
			t.Errorf("position %d: got %v, want %v", pos, got, want)
		}
	}
}

func TestReleaseAt_Cadence(t *testing.T) {
	loc := mustLoc(t, "Europe/London")
	start := time.Date(2026, 1, 5, 7, 0, 0, 0, loc)
	cfg := Config{
		StartAt:         start,
		CadenceDays:     7,
		ReleaseTime:     "07:00",
		Location:        loc,
		SeedCount:       1,
		EpisodesPerSlot: 1,
	}

	cases := []struct {
		position int
		want     time.Time
	}{
		{0, start},
		{1, time.Date(2026, 1, 12, 7, 0, 0, 0, loc)},
		{2, time.Date(2026, 1, 19, 7, 0, 0, 0, loc)},
		{3, time.Date(2026, 1, 26, 7, 0, 0, 0, loc)},
	}
	for _, c := range cases {
		got, err := cfg.ReleaseAt(c.position)
		if err != nil {
			t.Fatalf("position %d: %v", c.position, err)
		}
		if !got.Equal(c.want) {
			t.Errorf("position %d: got %v, want %v", c.position, got, c.want)
		}
	}
}

func TestReleaseAt_EpisodesPerSlotStagger(t *testing.T) {
	loc := mustLoc(t, "UTC")
	start := time.Date(2026, 1, 5, 7, 0, 0, 0, loc)
	cfg := Config{
		StartAt:         start,
		CadenceDays:     7,
		ReleaseTime:     "07:00",
		Location:        loc,
		SeedCount:       1,
		EpisodesPerSlot: 3,
	}

	// Positions 1,2,3 all land in the first cadence slot (2026-01-12)
	// and must be staggered by a minute each so ties don't reorder.
	slot1 := time.Date(2026, 1, 12, 7, 0, 0, 0, loc)
	cases := []struct {
		position int
		want     time.Time
	}{
		{0, start},
		{1, slot1},
		{2, slot1.Add(time.Minute)},
		{3, slot1.Add(2 * time.Minute)},
		{4, time.Date(2026, 1, 19, 7, 0, 0, 0, loc)},
	}
	for _, c := range cases {
		got, err := cfg.ReleaseAt(c.position)
		if err != nil {
			t.Fatalf("position %d: %v", c.position, err)
		}
		if !got.Equal(c.want) {
			t.Errorf("position %d: got %v, want %v", c.position, got, c.want)
		}
	}
}

func TestReleaseAt_ShiftSeconds(t *testing.T) {
	loc := mustLoc(t, "UTC")
	start := time.Date(2026, 1, 5, 7, 0, 0, 0, loc)
	cfg := Config{
		StartAt:         start,
		CadenceDays:     7,
		ReleaseTime:     "07:00",
		Location:        loc,
		SeedCount:       1,
		EpisodesPerSlot: 1,
		ShiftSeconds:    3 * 24 * 3600, // paused for 3 days
	}

	got, err := cfg.ReleaseAt(1)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 1, 15, 7, 0, 0, 0, loc) // +7d, then +3d shift
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}

	// Seeded episodes are never shifted; only cadence releases are.
	seeded, err := cfg.ReleaseAt(0)
	if err != nil {
		t.Fatal(err)
	}
	if !seeded.Equal(start) {
		t.Errorf("seeded episode shifted: got %v, want %v", seeded, start)
	}
}

// TestReleaseAt_DSTStable verifies that the wall-clock release hour
// survives a spring-forward DST boundary, i.e. we must use AddDate on
// the civil date rather than adding a fixed 7*24h duration.
func TestReleaseAt_DSTStable(t *testing.T) {
	loc := mustLoc(t, "Europe/London")
	// UK clocks spring forward on 2026-03-29.
	start := time.Date(2026, 3, 23, 7, 0, 0, 0, loc)
	cfg := Config{
		StartAt:         start,
		CadenceDays:     7,
		ReleaseTime:     "07:00",
		Location:        loc,
		SeedCount:       1,
		EpisodesPerSlot: 1,
	}

	got, err := cfg.ReleaseAt(1)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 3, 30, 7, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if got.Hour() != 7 {
		t.Errorf("wall-clock hour drifted across DST: got hour %d", got.Hour())
	}

	// A naive fixed 7*24h addition would land an hour off across the
	// spring-forward boundary; confirm AddDate actually differs from it
	// here, so this test would fail if someone "simplified" the
	// implementation back to a duration add.
	naive := start.Add(7 * 24 * time.Hour)
	if naive.Equal(got) {
		t.Fatal("naive duration-add matched AddDate result; DST boundary not exercised by this fixture")
	}
}

func TestApplyLocks(t *testing.T) {
	now := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	episodes := []Episode{
		{Position: 0, ScheduledAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Locked: false},
		{Position: 1, ScheduledAt: time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC), Locked: false}, // exactly now -> locked
		{Position: 2, ScheduledAt: time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC), Locked: false}, // future
		{Position: 3, ScheduledAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), Locked: true},   // already locked
	}

	got := ApplyLocks(now, episodes)

	want := []bool{true, true, false, true}
	for i, e := range got {
		if e.Locked != want[i] {
			t.Errorf("episode %d: locked = %v, want %v", i, e.Locked, want[i])
		}
	}
}

func TestRecompute_SkipsLocked(t *testing.T) {
	loc := mustLoc(t, "UTC")
	start := time.Date(2026, 1, 5, 7, 0, 0, 0, loc)
	cfg := Config{
		StartAt:         start,
		CadenceDays:     7,
		ReleaseTime:     "07:00",
		Location:        loc,
		SeedCount:       1,
		EpisodesPerSlot: 1,
	}

	frozen := time.Date(1999, 1, 1, 0, 0, 0, 0, loc)
	episodes := []Episode{
		{Position: 0, ScheduledAt: frozen, Locked: true}, // must not move even though formula disagrees
		{Position: 1, ScheduledAt: time.Time{}, Locked: false},
	}

	got, err := Recompute(cfg, episodes)
	if err != nil {
		t.Fatal(err)
	}
	if !got[0].ScheduledAt.Equal(frozen) {
		t.Errorf("locked episode moved: got %v, want %v", got[0].ScheduledAt, frozen)
	}
	wantUnlocked := time.Date(2026, 1, 12, 7, 0, 0, 0, loc)
	if !got[1].ScheduledAt.Equal(wantUnlocked) {
		t.Errorf("unlocked episode not recomputed: got %v, want %v", got[1].ScheduledAt, wantUnlocked)
	}
}

func TestReleaseAt_InvalidEpisodesPerSlot(t *testing.T) {
	cfg := Config{Location: time.UTC, EpisodesPerSlot: 0}
	if _, err := cfg.ReleaseAt(5); err == nil {
		t.Error("expected error for episodes_per_slot=0, got nil")
	}
}

func TestReleaseAt_NegativePosition(t *testing.T) {
	cfg := Config{Location: time.UTC, EpisodesPerSlot: 1}
	if _, err := cfg.ReleaseAt(-1); err == nil {
		t.Error("expected error for negative position, got nil")
	}
}

func TestReleaseAt_OriginalCadence(t *testing.T) {
	day := func(d int) *time.Time { x := time.Date(2020, 1, d, 12, 0, 0, 0, time.UTC); return &x }
	start := time.Date(2026, 10, 1, 7, 0, 0, 0, time.UTC)
	// gaps: ep0->ep1 = 3d, ep1->ep2 = 10d, ep3 backdated, ep4 undated
	pubs := []*time.Time{day(1), day(4), day(14), day(2), nil}
	cfg := Config{StartAt: start, Location: time.UTC, SeedCount: 2, EpisodesPerSlot: 1, Mode: ModeOriginal, PubDates: pubs}

	cases := []struct {
		pos  int
		want time.Time
	}{
		{0, start},
		{1, start.Add(time.Minute)},
		{2, start.AddDate(0, 0, 10)},                      // 10d after ep1
		{3, start.AddDate(0, 0, 10).Add(time.Minute)},     // backdated: queues behind ep2
		{4, start.AddDate(0, 0, 10).Add(2 * time.Minute)}, // undated: rides behind ep3
	}
	for _, c := range cases {
		got, err := cfg.ReleaseAt(c.pos)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Equal(c.want) {
			t.Errorf("pos %d: got %v want %v", c.pos, got, c.want)
		}
	}
}

func TestNormalizeReleaseTime(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "07:00", want: "07:00"},
		{in: " 7:05 ", want: "07:05"},
		{in: "07:00:00", want: "07:00"}, // some browsers' <input type="time">
		{in: "23:59", want: "23:59"},
		{in: "24:00", wantErr: true},
		{in: "7am", wantErr: true},
		{in: "", wantErr: true},
	} {
		got, err := NormalizeReleaseTime(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("NormalizeReleaseTime(%q): expected an error, got %q", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("NormalizeReleaseTime(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("NormalizeReleaseTime(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
