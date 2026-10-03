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

// ptrTime is the shape episodes carry publish dates in.
func ptrTime(t time.Time) *time.Time { return &t }

func TestRecompute_PremiumRidesWithItsFreeEpisode(t *testing.T) {
	loc := mustLoc(t, "UTC")
	start := time.Date(2026, 1, 1, 7, 0, 0, 0, loc)
	cfg := Config{
		StartAt:         start,
		CadenceDays:     7,
		ReleaseTime:     "07:00",
		Location:        loc,
		SeedCount:       1,
		EpisodesPerSlot: 1,
	}

	// The show published weekly on Mondays at 08:00; its premium feed
	// put out a bonus episode a day and an hour after the first, and an
	// hour after the second.
	free1 := time.Date(2020, 3, 2, 8, 0, 0, 0, loc)
	free2 := time.Date(2020, 3, 9, 8, 0, 0, 0, loc)
	free3 := time.Date(2020, 3, 16, 8, 0, 0, 0, loc)
	episodes := []Episode{
		{Position: 0, Role: RolePrimary, PubDate: ptrTime(free1)},
		{Position: 1, Role: RolePrimary, PubDate: ptrTime(free2)},
		{Position: 2, Role: RolePrimary, PubDate: ptrTime(free3)},
		{Position: 3, Role: RolePremium, PubDate: ptrTime(free1.Add(25 * time.Hour))},
		{Position: 4, Role: RolePremium, PubDate: ptrTime(free2.Add(time.Hour))},
	}

	got, err := Recompute(cfg, episodes)
	if err != nil {
		t.Fatal(err)
	}

	// The free feed is replayed weekly from start; each premium episode
	// keeps its real distance from the free episode it belongs with.
	want := []time.Time{
		start,
		time.Date(2026, 1, 8, 7, 0, 0, 0, loc),
		time.Date(2026, 1, 15, 7, 0, 0, 0, loc),
		start.Add(25 * time.Hour),
		time.Date(2026, 1, 8, 8, 0, 0, 0, loc),
	}
	for i, w := range want {
		if !got[i].ScheduledAt.Equal(w) {
			t.Errorf("position %d: got %v, want %v", i, got[i].ScheduledAt, w)
		}
	}
}

func TestRecompute_PremiumAnchorsToLockedFreeEpisode(t *testing.T) {
	loc := mustLoc(t, "UTC")
	start := time.Date(2026, 1, 1, 7, 0, 0, 0, loc)
	cfg := Config{StartAt: start, CadenceDays: 7, ReleaseTime: "07:00", Location: loc, SeedCount: 1, EpisodesPerSlot: 1}

	free := time.Date(2020, 3, 2, 8, 0, 0, 0, loc)
	// The free episode already went out at a time the formula no longer
	// agrees with; its premium companion must follow the real release.
	releasedAt := time.Date(2025, 12, 25, 6, 30, 0, 0, loc)
	episodes := []Episode{
		{Position: 0, Role: RolePrimary, PubDate: ptrTime(free), ScheduledAt: releasedAt, Locked: true},
		{Position: 1, Role: RolePremium, PubDate: ptrTime(free.Add(2 * time.Hour))},
	}

	got, err := Recompute(cfg, episodes)
	if err != nil {
		t.Fatal(err)
	}
	if want := releasedAt.Add(2 * time.Hour); !got[1].ScheduledAt.Equal(want) {
		t.Errorf("premium episode: got %v, want %v", got[1].ScheduledAt, want)
	}
}

func TestRecompute_PremiumEdgeCases(t *testing.T) {
	loc := mustLoc(t, "UTC")
	start := time.Date(2026, 1, 1, 7, 0, 0, 0, loc)
	cfg := Config{StartAt: start, CadenceDays: 7, ReleaseTime: "07:00", Location: loc, SeedCount: 1, EpisodesPerSlot: 1}

	free1 := time.Date(2020, 3, 2, 8, 0, 0, 0, loc)
	free2 := time.Date(2020, 3, 9, 8, 0, 0, 0, loc)
	episodes := []Episode{
		{Position: 0, Role: RolePrimary, PubDate: ptrTime(free1)},
		{Position: 1, Role: RolePrimary, PubDate: ptrTime(free2)},
		// Older than the whole free feed: comes out with episode one
		// rather than before the feed starts.
		{Position: 2, Role: RolePremium, PubDate: ptrTime(free1.AddDate(-1, 0, 0))},
		// Backfilled behind the one before it: queues after it instead
		// of jumping the line.
		{Position: 3, Role: RolePremium, PubDate: ptrTime(free1.Add(time.Hour))},
		// No date at all: rides behind the previous premium episode.
		{Position: 4, Role: RolePremium},
	}

	got, err := Recompute(cfg, episodes)
	if err != nil {
		t.Fatal(err)
	}
	want := []time.Time{
		start,
		time.Date(2026, 1, 8, 7, 0, 0, 0, loc),
		start,
		start.Add(time.Hour),
		start.Add(time.Hour + time.Minute),
	}
	for i, w := range want {
		if !got[i].ScheduledAt.Equal(w) {
			t.Errorf("position %d: got %v, want %v", i, got[i].ScheduledAt, w)
		}
	}
}

func TestRecompute_PremiumInOriginalCadence(t *testing.T) {
	loc := mustLoc(t, "UTC")
	start := time.Date(2026, 1, 1, 7, 0, 0, 0, loc)
	cfg := Config{StartAt: start, Location: loc, SeedCount: 1, EpisodesPerSlot: 1, Mode: ModeOriginal}

	free1 := time.Date(2020, 3, 2, 8, 0, 0, 0, loc)
	free2 := free1.AddDate(0, 0, 7)
	episodes := []Episode{
		{Position: 0, Role: RolePrimary, PubDate: ptrTime(free1)},
		{Position: 1, Role: RolePrimary, PubDate: ptrTime(free2)},
		{Position: 2, Role: RolePremium, PubDate: ptrTime(free2.Add(90 * time.Minute))},
	}

	got, err := Recompute(cfg, episodes)
	if err != nil {
		t.Fatal(err)
	}
	// Original cadence replays the free feed's own 7-day gap, and the
	// premium episode keeps its 90 minutes on top of it.
	wantFree2 := start.AddDate(0, 0, 7)
	if !got[1].ScheduledAt.Equal(wantFree2) {
		t.Errorf("free episode 2: got %v, want %v", got[1].ScheduledAt, wantFree2)
	}
	if want := wantFree2.Add(90 * time.Minute); !got[2].ScheduledAt.Equal(want) {
		t.Errorf("premium episode: got %v, want %v", got[2].ScheduledAt, want)
	}
}

func TestRecompute_PremiumEpisodesDoNotConsumeCadenceSlots(t *testing.T) {
	loc := mustLoc(t, "UTC")
	start := time.Date(2026, 1, 1, 7, 0, 0, 0, loc)
	cfg := Config{StartAt: start, CadenceDays: 7, ReleaseTime: "07:00", Location: loc, SeedCount: 1, EpisodesPerSlot: 1}

	day := func(d int) *time.Time { x := time.Date(2020, 3, d, 8, 0, 0, 0, loc); return &x }
	// A premium episode sits between the free ones in ingest position
	// (it was melded in later, so it has a tail position) — the free
	// feed's pace must be counted over free episodes only.
	episodes := []Episode{
		{Position: 0, Role: RolePrimary, PubDate: day(2)},
		{Position: 1, Role: RolePremium, PubDate: day(3)},
		{Position: 2, Role: RolePrimary, PubDate: day(9)},
		{Position: 3, Role: RolePrimary, PubDate: day(16)},
	}

	got, err := Recompute(cfg, episodes)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 1, 8, 7, 0, 0, 0, loc); !got[2].ScheduledAt.Equal(want) {
		t.Errorf("second free episode: got %v, want %v", got[2].ScheduledAt, want)
	}
	if want := time.Date(2026, 1, 15, 7, 0, 0, 0, loc); !got[3].ScheduledAt.Equal(want) {
		t.Errorf("third free episode: got %v, want %v", got[3].ScheduledAt, want)
	}
}

func TestRecompute_LeavesExcludedUntouched(t *testing.T) {
	loc := mustLoc(t, "UTC")
	start := time.Date(2026, 1, 1, 7, 0, 0, 0, loc)
	cfg := Config{StartAt: start, CadenceDays: 7, ReleaseTime: "07:00", Location: loc, SeedCount: 1, EpisodesPerSlot: 1}

	frozen := time.Date(1999, 1, 1, 0, 0, 0, 0, loc)
	episodes := []Episode{
		{Position: 0, Role: RolePrimary},
		{Position: 1, Role: RolePrimary, ScheduledAt: frozen, Excluded: true},
		{Position: 2, Role: RolePrimary},
	}

	got, err := Recompute(cfg, episodes)
	if err != nil {
		t.Fatal(err)
	}
	if !got[1].ScheduledAt.Equal(frozen) {
		t.Errorf("excluded episode moved: got %v", got[1].ScheduledAt)
	}
	// Excluding leaves a gap rather than pulling the next episode
	// forward: position 2 still releases in the second cadence slot.
	if want := time.Date(2026, 1, 15, 7, 0, 0, 0, loc); !got[2].ScheduledAt.Equal(want) {
		t.Errorf("episode after the excluded one: got %v, want %v", got[2].ScheduledAt, want)
	}
}

func TestApplyLocks_SkipsExcluded(t *testing.T) {
	now := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	got := ApplyLocks(now, []Episode{
		{Position: 0, ScheduledAt: now.AddDate(0, 0, -1), Excluded: true},
	})
	if got[0].Locked {
		t.Error("an excluded episode should not be locked")
	}
}

func TestNormalizeRole(t *testing.T) {
	for in, want := range map[string]string{
		"primary":  RolePrimary,
		"premium":  RolePremium,
		"":         RolePrimary,
		"nonsense": RolePrimary,
	} {
		if got := NormalizeRole(in); got != want {
			t.Errorf("NormalizeRole(%q) = %q, want %q", in, got, want)
		}
	}
}
