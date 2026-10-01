package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/daaku/serr"
)

// Go timers run on the monotonic clock, which stops while the machine is
// suspended, so one long sleep overshoots the alarm by however long the
// machine slept. Sleeping in slices and re-reading the wall clock on each wake
// bounds that overshoot to one slice. idleSleep covers having no alarms at
// all, where nothing can be overdue and SIGUSR1 is the only wake-up.
const (
	sleepSlice = 30 * time.Second
	idleSleep  = 24 * time.Hour
	lateWarn   = time.Minute
)

// runDaemon is the foreground loop: it sleeps until the next alarm, reloads
// on SIGUSR1, and rings due alarms. Only one daemon runs at a time. systemd
// (or whatever supervises it) owns backgrounding and restarts.
func runDaemon() error {
	// Log to stderr without our own timestamp: journald stamps each line and
	// captures stderr, so a second timestamp is just noise.
	log.SetOutput(os.Stderr)
	log.SetFlags(0)
	log.SetPrefix("snoozer: ")
	s, err := newStore()
	if err != nil {
		return err
	}
	if err := s.ensureSound(); err != nil {
		return err
	}
	release, err := lockDaemon(s)
	if err != nil {
		return err
	}
	defer release()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	reload := make(chan os.Signal, 1)
	signal.Notify(reload, syscall.SIGUSR1)
	defer signal.Stop(reload)

	log.Printf("daemon started, pid %d", os.Getpid())
	for {
		next, err := s.nextWake()
		if err != nil {
			log.Printf("read state: %v", err)
			next = time.Now().Add(time.Minute)
		}
		timer := time.NewTimer(nextSleep(next, time.Now()))
		select {
		case <-ctx.Done():
			timer.Stop()
			log.Printf("daemon stopping")
			return nil
		case <-reload:
			timer.Stop()
			log.Printf("state changed, rescheduling")
		case <-timer.C:
			if err := ringDue(ctx, s); err != nil {
				log.Printf("ring: %v", err)
			}
		}
	}
}

// nextSleep returns how long to wait before the next check. The cap is what
// stops a suspended machine from oversleeping its alarms: see sleepSlice.
func nextSleep(next, now time.Time) time.Duration {
	if next.IsZero() {
		return idleSleep
	}
	d := next.Sub(now)
	switch {
	case d < 0:
		return 0
	case d > sleepSlice:
		return sleepSlice
	default:
		return d
	}
}

func (s *store) daemonLockPath() string { return filepath.Join(s.dir, "daemon.lock") }

// lockDaemon takes the singleton lock and records the pid for CLI signals.
func lockDaemon(s *store) (func(), error) {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return nil, serr.Wrap(err)
	}
	f, err := os.OpenFile(s.daemonLockPath(), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, serr.Wrap(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, serr.Errorf("another snoozer daemon is already running")
	}
	if err := os.WriteFile(s.pidPath(), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
		return nil, serr.Wrap(err)
	}
	return func() {
		os.Remove(s.pidPath())
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// ringDue claims every due alarm (advancing repeats, dropping one-offs) and
// rings them all at once. Catch-up is the usual reason for a batch: the
// machine slept, or the daemon was down, and several alarms came due. Each
// gets its own zenity dialog and they are all up together, so dismissing one
// never hides another behind a three minute timeout.
func ringDue(ctx context.Context, s *store) error {
	now := time.Now()
	due, err := s.takeDue(now)
	if err != nil {
		return err
	}
	if len(due) == 0 {
		return nil
	}
	log.Printf("ringing %d alarm(s) at once", len(due))
	if late := now.Sub(due[0].At); late > lateWarn {
		log.Printf("oldest alarm is late by %s", late.Round(time.Second))
	}
	// One sound for the batch: N overlapping loops is just noise.
	if noisy := firstNoisy(due); noisy != nil {
		stopSound := startSound(ctx, s, noisy)
		defer stopSound()
	}
	var wg sync.WaitGroup
	for _, a := range due {
		wg.Add(1)
		go func(a *Alarm) {
			defer wg.Done()
			if err := askAndSnooze(ctx, s, a); err != nil {
				log.Printf("alarm %q: %v", a.label(), err)
			}
		}(a)
	}
	wg.Wait()
	return nil
}

// firstNoisy picks the sound the batch plays: the earliest alarm that is not
// silent. All silent, so nil, so no sound at all.
func firstNoisy(due []*Alarm) *Alarm {
	for _, a := range due {
		if !a.Silent {
			return a
		}
	}
	return nil
}

// askAndSnooze shows one alarm's zenity dialog and, on snooze, schedules a
// one-off copy of it. Sound belongs to the batch, not to this, so simultaneous
// alarms share a single loop.
func askAndSnooze(ctx context.Context, s *store, a *Alarm) error {
	snooze, err := a.snoozeDuration()
	if err != nil {
		log.Printf("alarm %q: %v", a.label(), err)
		snooze = defaultSnooze
	}
	timeout, err := a.timeoutDuration()
	if err != nil {
		log.Printf("alarm %q: %v", a.label(), err)
		timeout = defaultTimeout
	}
	snoozed, err := askSnooze(ctx, a, timeout)
	if err != nil {
		return err
	}
	if !snoozed {
		return nil
	}
	next := &Alarm{
		At:      time.Now().Add(snooze),
		Label:   a.Label,
		Silent:  a.Silent,
		Snooze:  a.Snooze,
		Timeout: a.Timeout,
		Sound:   a.Sound,
	}
	log.Printf("alarm %q snoozed until %s", a.label(), next.At.Format(time.Kitchen))
	return s.add(next)
}

// startSound launches mpv looping one alarm sound and returns a function that
// stops it. A missing custom sound falls back to the bundled one rather than
// to silence, and a missing audio device is not an error: the dialogs stand on
// their own.
func startSound(ctx context.Context, s *store, a *Alarm) func() {
	noop := func() {}
	path := s.resolveSound(a.Sound)
	if !fileExists(path) {
		fallback := s.resolveSound(defaultSound)
		if fallback != path && fileExists(fallback) {
			log.Printf("alarm %q: sound %s not found, playing %s", a.label(), path, fallback)
			path = fallback
		} else {
			log.Printf("alarm %q: sound %s not found", a.label(), path)
			return noop
		}
	}
	cmd := exec.CommandContext(ctx, "mpv", "--no-video", "--really-quiet", "--loop=inf", path)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		log.Printf("alarm %q: mpv: %v", a.label(), err)
		return noop
	}
	done := make(chan struct{})
	go func() {
		// A missing or busy audio device makes mpv exit; that is not fatal.
		if err := cmd.Wait(); err != nil {
			log.Printf("alarm %q: mpv: %v", a.label(), err)
		}
		close(done)
	}()
	return func() {
		if cmd.Process != nil {
			cmd.Process.Signal(syscall.SIGTERM)
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			if cmd.Process != nil {
				cmd.Process.Kill()
			}
		}
	}
}

// askSnooze shows the zenity question. Exit code 0 means snooze; dismiss,
// timeout and any other exit code all mean stop.
func askSnooze(ctx context.Context, a *Alarm, timeout time.Duration) (bool, error) {
	args := []string{
		"--question",
		"--modal",
		"--ok-label=Snooze",
		"--cancel-label=Dismiss",
		"--title=Alarm",
		"--icon=alarm",
		"--text=" + a.label(),
	}
	if timeout > 0 {
		args = append(args, fmt.Sprintf("--timeout=%d", int(timeout.Seconds())))
	}
	err := exec.CommandContext(ctx, "zenity", args...).Run()
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return false, nil
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	return false, serr.Wrap(err)
}
