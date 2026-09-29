package main

import (
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

// referenceNow is a Wednesday, useful for weekday math.
var referenceNow = time.Date(2024, 1, 3, 10, 0, 0, 0, time.UTC)

func TestParseRepeat(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"tue", []string{"tue"}},
		{"monday", []string{"mon"}},
		{"wed,mon", []string{"mon", "wed"}},
		{"all", []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}},
		{"FRI , sat", []string{"fri", "sat"}},
		{"mon,mon", []string{"mon"}},
	}
	for _, c := range cases {
		got, err := parseRepeat(c.in)
		if err != nil {
			t.Fatalf("parseRepeat(%q): %v", c.in, err)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseRepeat(%q) = %v, want %v", c.in, got, c.want)
		}
	}
	if _, err := parseRepeat("noday"); err == nil {
		t.Error("parseRepeat(noday) succeeded, want error")
	}
}

func TestParseClock(t *testing.T) {
	cases := []struct {
		in      string
		h, m, s int
	}{
		{"11am", 11, 0, 0},
		{"3:30pm", 15, 30, 0},
		{"9:40pm", 21, 40, 0},
		{"15:04", 15, 4, 0},
		{"7:05:09 PM", 19, 5, 9},
		{"12am", 0, 0, 0},
		{"12pm", 12, 0, 0},
	}
	for _, c := range cases {
		h, m, s, err := parseClock(c.in)
		if err != nil {
			t.Fatalf("parseClock(%q): %v", c.in, err)
		}
		if h != c.h || m != c.m || s != c.s {
			t.Errorf("parseClock(%q) = %02d:%02d:%02d, want %02d:%02d:%02d",
				c.in, h, m, s, c.h, c.m, c.s)
		}
	}
	if _, _, _, err := parseClock("noonish"); err == nil {
		t.Error("parseClock(noonish) succeeded, want error")
	}
}

func TestNextOccurrence(t *testing.T) {
	// Wednesday 10:00. The next Monday at 09:00 is Jan 8.
	a := &Alarm{At: time.Date(2024, 1, 3, 9, 0, 0, 0, time.UTC), Repeat: []string{"mon"}}
	got := nextOccurrence(a, referenceNow)
	want := time.Date(2024, 1, 8, 9, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("nextOccurrence = %s, want %s", got, want)
	}

	// With every day it is the same day, later.
	a = &Alarm{At: time.Date(2024, 1, 3, 15, 30, 0, 0, time.UTC), Repeat: dayOrder}
	got = nextOccurrence(a, referenceNow)
	want = time.Date(2024, 1, 3, 15, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("nextOccurrence all = %s, want %s", got, want)
	}

	// Strictly after: the same instant rolls forward a day.
	a = &Alarm{At: time.Date(2024, 1, 3, 10, 0, 0, 0, time.UTC), Repeat: dayOrder}
	got = nextOccurrence(a, referenceNow)
	want = time.Date(2024, 1, 4, 10, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("nextOccurrence same instant = %s, want %s", got, want)
	}
}

func TestBuildAlarm(t *testing.T) {
	cases := []struct {
		name   string
		in, at string
		repeat string
		want   time.Time
	}{
		{"in", "5m", "", "", referenceNow.Add(5 * time.Minute)},
		{"at today", "", "11am", "", time.Date(2024, 1, 3, 11, 0, 0, 0, time.UTC)},
		{"at tomorrow", "", "9am", "", time.Date(2024, 1, 4, 9, 0, 0, 0, time.UTC)},
		{"repeat all", "", "11am", "all", time.Date(2024, 1, 3, 11, 0, 0, 0, time.UTC)},
		{"repeat mon", "", "11am", "mon", time.Date(2024, 1, 8, 11, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, err := buildAlarm(referenceNow, c.in, c.at, "", c.repeat, false, "", "", "")
			if err != nil {
				t.Fatal(err)
			}
			if !a.At.Equal(c.want) {
				t.Errorf("At = %s, want %s", a.At, c.want)
			}
		})
	}
}

func TestBuildAlarmErrors(t *testing.T) {
	if _, err := buildAlarm(referenceNow, "", "", "", "", false, "", "", ""); err == nil {
		t.Error("no time succeeded, want error")
	}
	if _, err := buildAlarm(referenceNow, "5m", "11am", "", "", false, "", "", ""); err == nil {
		t.Error("both --in and --at succeeded, want error")
	}
	if _, err := buildAlarm(referenceNow, "", "11am", "", "", false, "nope", "", ""); err == nil {
		t.Error("bad snooze succeeded, want error")
	}
	if _, err := buildAlarm(referenceNow, "0s", "", "", "", false, "", "", ""); err == nil {
		t.Error("zero --in succeeded, want error")
	}
}

func TestAlarmDefaults(t *testing.T) {
	a := &Alarm{}
	if a.label() != defaultLabel {
		t.Errorf("label = %q, want %q", a.label(), defaultLabel)
	}
	if got, _ := a.snoozeDuration(); got != defaultSnooze {
		t.Errorf("snooze = %s, want %s", got, defaultSnooze)
	}
	if got, _ := a.timeoutDuration(); got != defaultTimeout {
		t.Errorf("timeout = %s, want %s", got, defaultTimeout)
	}
	b := &Alarm{Timeout: "0"}
	if got, _ := b.timeoutDuration(); got != 0 {
		t.Errorf("timeout 0 = %s, want 0", got)
	}
}

// newTestStore points the config directory at a fresh temp dir.
func newTestStore(t *testing.T) *store {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	s, err := newStore()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestStoreRoundTrip(t *testing.T) {
	s := newTestStore(t)
	a := &Alarm{
		At:      referenceNow,
		Label:   "walk",
		Repeat:  []string{"mon", "wed"},
		Silent:  true,
		Snooze:  "5m",
		Timeout: "2m",
		Sound:   "custom.ogg",
	}
	if err := s.add(a); err != nil {
		t.Fatal(err)
	}
	st, err := s.load()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Alarms) != 1 {
		t.Fatalf("got %d alarms, want 1", len(st.Alarms))
	}
	got := st.Alarms[0]
	if !got.At.Equal(a.At) || got.Label != a.Label || got.Silent != a.Silent ||
		!reflect.DeepEqual(got.Repeat, a.Repeat) || got.Snooze != a.Snooze ||
		got.Timeout != a.Timeout || got.Sound != a.Sound {
		t.Errorf("round trip mismatch: %+v", got)
	}
	// The atomic write must not leave temp files behind.
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if len(e.Name()) > 1 && e.Name()[0] == '.' {
			t.Errorf("leftover temp file %s", e.Name())
		}
	}
}

func TestStoreMissingFile(t *testing.T) {
	s := newTestStore(t)
	st, err := s.load()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Alarms) != 0 {
		t.Errorf("missing file gave %d alarms, want 0", len(st.Alarms))
	}
	if next, err := s.nextWake(); err != nil || !next.IsZero() {
		t.Errorf("nextWake = %s, %v; want zero", next, err)
	}
}

func TestStoreConcurrentUpdate(t *testing.T) {
	s := newTestStore(t)
	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- s.add(&Alarm{At: referenceNow.Add(time.Duration(i) * time.Minute)})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	st, err := s.load()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Alarms) != n {
		t.Errorf("got %d alarms, want %d", len(st.Alarms), n)
	}
}

func TestTakeDue(t *testing.T) {
	s := newTestStore(t)
	oneOff := &Alarm{At: referenceNow.Add(-time.Minute), Label: "one"}
	repeat := &Alarm{At: referenceNow.Add(-time.Minute), Label: "repeat", Repeat: []string{"wed"}}
	future := &Alarm{At: referenceNow.Add(time.Hour), Label: "future"}
	for _, a := range []*Alarm{oneOff, repeat, future} {
		if err := s.add(a); err != nil {
			t.Fatal(err)
		}
	}
	due, err := s.takeDue(referenceNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 2 {
		t.Fatalf("got %d due, want 2", len(due))
	}
	st, err := s.load()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Alarms) != 2 {
		t.Fatalf("after takeDue got %d alarms, want 2", len(st.Alarms))
	}
	for _, a := range st.Alarms {
		if a.Label == "one" {
			t.Error("one-off alarm was not removed")
		}
		if a.Label == "repeat" {
			want := time.Date(2024, 1, 10, 9, 59, 0, 0, time.UTC)
			if !a.At.Equal(want) {
				t.Errorf("repeat advanced to %s, want %s", a.At, want)
			}
		}
	}
}

func TestResolveSound(t *testing.T) {
	s := newTestStore(t)
	if err := s.ensureSound(); err != nil {
		t.Fatal(err)
	}
	if got := s.resolveSound(""); got != filepath.Join(s.dir, defaultSound) {
		t.Errorf("resolveSound(default) = %q", got)
	}
	if got := s.resolveSound("custom.ogg"); got != "custom.ogg" {
		t.Errorf("resolveSound(custom) = %q, want custom.ogg", got)
	}
	abs := filepath.Join(t.TempDir(), "x.ogg")
	if got := s.resolveSound(abs); got != abs {
		t.Errorf("resolveSound(abs) = %q, want %q", got, abs)
	}
}
