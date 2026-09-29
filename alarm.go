package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/daaku/serr"
)

// Defaults applied when an alarm leaves a field empty.
const (
	defaultLabel   = "Alarm"
	defaultSnooze  = 9 * time.Minute
	defaultTimeout = 3 * time.Minute
	defaultSound   = "alarm.ogg"
)

// dayOrder is the canonical mon..sun order alarms are stored in. The first
// three letters are the stored form.
var dayOrder = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

var dayNames = map[string]time.Weekday{
	"sun": time.Sunday, "sunday": time.Sunday,
	"mon": time.Monday, "monday": time.Monday,
	"tue": time.Tuesday, "tuesday": time.Tuesday,
	"wed": time.Wednesday, "wednesday": time.Wednesday,
	"thu": time.Thursday, "thursday": time.Thursday,
	"fri": time.Friday, "friday": time.Friday,
	"sat": time.Saturday, "saturday": time.Saturday,
}

// Alarm is one scheduled ring. Every field except At is optional, and the
// zero value of an optional field means the default applies.
type Alarm struct {
	At      time.Time `json:"at"`
	Label   string    `json:"label,omitempty"`
	Repeat  []string  `json:"repeat,omitempty"`
	Silent  bool      `json:"silent,omitempty"`
	Snooze  string    `json:"snooze,omitempty"`
	Timeout string    `json:"timeout,omitempty"`
	Sound   string    `json:"sound,omitempty"`
}

// Label returns the dialog text, defaulting to "Alarm".
func (a *Alarm) label() string {
	if strings.TrimSpace(a.Label) == "" {
		return defaultLabel
	}
	return a.Label
}

// snoozeDuration returns how long a snooze lasts, defaulting to 9m.
func (a *Alarm) snoozeDuration() (time.Duration, error) {
	return parseDuration(a.Snooze, defaultSnooze)
}

// timeoutDuration returns how long the dialog waits before dismissing,
// defaulting to 3m. A value of "0" disables the timeout.
func (a *Alarm) timeoutDuration() (time.Duration, error) {
	if strings.TrimSpace(a.Timeout) == "0" {
		return 0, nil
	}
	return parseDuration(a.Timeout, defaultTimeout)
}

func parseDuration(s string, def time.Duration) (time.Duration, error) {
	if strings.TrimSpace(s) == "" {
		return def, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, serr.Errorf("invalid duration %q: %w", s, err)
	}
	if d < 0 {
		return 0, serr.Errorf("duration must not be negative: %q", s)
	}
	return d, nil
}

// parseRepeat turns "tue", "mon,wed,fri" or "all" into canonical day names.
func parseRepeat(s string) ([]string, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(day string) {
		if !seen[day] {
			seen[day] = true
			out = append(out, day)
		}
	}
	for _, part := range strings.Split(s, ",") {
		part = strings.ToLower(strings.TrimSpace(part))
		if part == "" {
			continue
		}
		if part == "all" || part == "everyday" || part == "daily" {
			for _, day := range dayOrder {
				add(day)
			}
			continue
		}
		if _, ok := dayNames[part]; !ok {
			return nil, serr.Errorf("invalid repeat day %q", part)
		}
		add(canonicalDay(part))
	}
	sortDays(out)
	return out, nil
}

// canonicalDay maps an accepted day name to its stored three letter form.
func canonicalDay(name string) string {
	want := dayNames[name]
	for _, day := range dayOrder {
		if dayNames[day] == want {
			return day
		}
	}
	return name
}

func sortDays(days []string) {
	sort.SliceStable(days, func(i, j int) bool {
		return dayIndex(days[i]) < dayIndex(days[j])
	})
}

func dayIndex(day string) int {
	for i, d := range dayOrder {
		if d == day {
			return i
		}
	}
	return len(dayOrder)
}

func containsDay(days []string, w time.Weekday) bool {
	for _, day := range days {
		if dayNames[day] == w {
			return true
		}
	}
	return false
}

// nextOccurrence returns the first time strictly after `after` whose clock
// time matches a.At and whose weekday is in a.Repeat. Callers only use it for
// repeating alarms.
func nextOccurrence(a *Alarm, after time.Time) time.Time {
	if len(a.Repeat) == 0 {
		return a.At
	}
	hour, min, sec := a.At.Clock()
	loc := after.Location()
	for i := 0; i < 8; i++ {
		cand := time.Date(after.Year(), after.Month(), after.Day()+i, hour, min, sec, 0, loc)
		if !cand.After(after) {
			continue
		}
		if containsDay(a.Repeat, cand.Weekday()) {
			return cand
		}
	}
	return a.At
}

// parseClock parses the clock forms accepted by --at: "11am", "3:30pm",
// "15:04" and the second-precision variants.
func parseClock(s string) (hour, min, sec int, err error) {
	t := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(s), " ", ""))
	for _, layout := range []string{"3:04PM", "3:04:05PM", "3PM", "15:04", "15:04:05"} {
		if tm, perr := time.Parse(layout, t); perr == nil {
			return tm.Hour(), tm.Minute(), tm.Second(), nil
		}
	}
	return 0, 0, 0, serr.Errorf("invalid time %q", s)
}

// buildAlarm turns CLI flags into an alarm, resolving --at to the next time
// that clock reading actually occurs.
func buildAlarm(now time.Time, in, at, label, repeat string, silent bool, snooze, timeout, sound string) (*Alarm, error) {
	if in == "" && at == "" {
		return nil, serr.Errorf("one of --in or --at is required")
	}
	if in != "" && at != "" {
		return nil, serr.Errorf("--in and --at are mutually exclusive")
	}
	days, err := parseRepeat(repeat)
	if err != nil {
		return nil, err
	}
	a := &Alarm{
		Label:   label,
		Repeat:  days,
		Silent:  silent,
		Snooze:  snooze,
		Timeout: timeout,
		Sound:   sound,
	}
	if in != "" {
		d, err := time.ParseDuration(strings.TrimSpace(in))
		if err != nil {
			return nil, serr.Errorf("invalid --in %q: %w", in, err)
		}
		if d <= 0 {
			return nil, serr.Errorf("--in must be positive")
		}
		a.At = now.Add(d)
	} else {
		hour, min, sec, err := parseClock(at)
		if err != nil {
			return nil, err
		}
		if len(days) > 0 {
			probe := &Alarm{
				At:     time.Date(now.Year(), now.Month(), now.Day(), hour, min, sec, 0, now.Location()),
				Repeat: days,
			}
			a.At = nextOccurrence(probe, now)
		} else {
			cand := time.Date(now.Year(), now.Month(), now.Day(), hour, min, sec, 0, now.Location())
			if !cand.After(now) {
				cand = cand.AddDate(0, 0, 1)
			}
			a.At = cand
		}
	}
	if _, err := a.snoozeDuration(); err != nil {
		return nil, err
	}
	if _, err := a.timeoutDuration(); err != nil {
		return nil, err
	}
	return a, nil
}

// State is the whole on-disk state: the list of alarms.
type State struct {
	Alarms []*Alarm `json:"alarms"`
}

// store reads and writes the state file under the snoozer config directory.
type store struct {
	dir string
}

// newStore resolves the config directory from XDG_CONFIG_HOME (with the
// usual ~/.config fallback) and returns a store rooted at snoozer/.
func newStore() (*store, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return nil, serr.Wrap(err)
	}
	return &store{dir: filepath.Join(base, "snoozer")}, nil
}

func (s *store) alarmsPath() string { return filepath.Join(s.dir, "alarms.json") }
func (s *store) lockPath() string   { return filepath.Join(s.dir, "alarms.lock") }
func (s *store) pidPath() string    { return filepath.Join(s.dir, "daemon.pid") }
func (s *store) logPath() string    { return filepath.Join(s.dir, "daemon.log") }
func (s *store) soundPath(name string) string {
	return filepath.Join(s.dir, name)
}

// ensureSound writes the embedded default sound into the config directory if
// it is not already there, so the daemon has something to play.
func (s *store) ensureSound() error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return serr.Wrap(err)
	}
	path := s.soundPath(defaultSound)
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := os.WriteFile(path, alarmOgg, 0o644); err != nil {
		return serr.Wrap(err)
	}
	return nil
}

// resolveSound turns a stored sound name into a path. Bare names are looked
// up in the config directory first, then left as-is for the working dir.
func (s *store) resolveSound(name string) string {
	if strings.TrimSpace(name) == "" {
		name = defaultSound
	}
	if filepath.IsAbs(name) {
		return name
	}
	if p := s.soundPath(name); fileExists(p) {
		return p
	}
	return name
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// load reads the state file. A missing file is an empty state.
func (s *store) load() (*State, error) {
	b, err := os.ReadFile(s.alarmsPath())
	if errors.Is(err, os.ErrNotExist) {
		return &State{}, nil
	}
	if err != nil {
		return nil, serr.Wrap(err)
	}
	st := &State{}
	if len(strings.TrimSpace(string(b))) == 0 {
		return st, nil
	}
	if err := json.Unmarshal(b, st); err != nil {
		return nil, serr.Errorf("parse %s: %w", s.alarmsPath(), err)
	}
	return st, nil
}

// save writes the state to a fresh temp file and renames it into place, so a
// reader never sees a half written file.
func (s *store) save(st *State) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return serr.Wrap(err)
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return serr.Wrap(err)
	}
	b = append(b, '\n')
	f, err := os.CreateTemp(s.dir, ".alarms-*.json")
	if err != nil {
		return serr.Wrap(err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(b); err != nil {
		f.Close()
		return serr.Wrap(err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return serr.Wrap(err)
	}
	if err := f.Close(); err != nil {
		return serr.Wrap(err)
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		return serr.Wrap(err)
	}
	if err := os.Rename(tmp, s.alarmsPath()); err != nil {
		return serr.Wrap(err)
	}
	return nil
}

// lock takes the exclusive advisory lock that serializes state changes
// between the CLI and the daemon. The returned function releases it.
func (s *store) lock() (func(), error) {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return nil, serr.Wrap(err)
	}
	f, err := os.OpenFile(s.lockPath(), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, serr.Wrap(err)
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		f.Close()
		return nil, serr.Wrap(err)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// update runs a read-modify-write of the state under the lock.
func (s *store) update(fn func(*State) error) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	st, err := s.load()
	if err != nil {
		return err
	}
	if err := fn(st); err != nil {
		return err
	}
	return s.save(st)
}

// add appends an alarm, keeping the file ordered by ring time.
func (s *store) add(a *Alarm) error {
	return s.update(func(st *State) error {
		st.Alarms = append(st.Alarms, a)
		sort.SliceStable(st.Alarms, func(i, j int) bool {
			return st.Alarms[i].At.Before(st.Alarms[j].At)
		})
		return nil
	})
}

// takeDue removes and returns every alarm due at or before now. Repeating
// alarms are advanced to their next occurrence instead of removed.
func (s *store) takeDue(now time.Time) ([]*Alarm, error) {
	var due []*Alarm
	err := s.update(func(st *State) error {
		var kept []*Alarm
		for _, a := range st.Alarms {
			if a.At.After(now) {
				kept = append(kept, a)
				continue
			}
			due = append(due, a)
			if len(a.Repeat) > 0 {
				next := *a
				next.At = nextOccurrence(a, now)
				kept = append(kept, &next)
			}
		}
		st.Alarms = kept
		return nil
	})
	if err != nil {
		return nil, err
	}
	return due, nil
}

// nextWake returns the earliest ring time, or the zero time when there are
// no alarms.
func (s *store) nextWake() (time.Time, error) {
	st, err := s.load()
	if err != nil {
		return time.Time{}, err
	}
	var next time.Time
	for _, a := range st.Alarms {
		if a.At.IsZero() {
			continue
		}
		if next.IsZero() || a.At.Before(next) {
			next = a.At
		}
	}
	return next, nil
}
