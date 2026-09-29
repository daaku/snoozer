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
	"syscall"
	"time"

	"github.com/daaku/serr"
)

// runDaemon is the foreground loop: it sleeps until the next alarm, reloads
// on SIGUSR1, and rings due alarms. Only one daemon runs at a time. systemd
// (or whatever supervises it) owns backgrounding and restarts.
func runDaemon() error {
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
		timer := newTimer(next)
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

// newTimer sleeps until t, or for a long time when there is no next alarm.
func newTimer(t time.Time) *time.Timer {
	if t.IsZero() {
		return time.NewTimer(365 * 24 * time.Hour)
	}
	d := time.Until(t)
	if d < 0 {
		d = 0
	}
	return time.NewTimer(d)
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
// rings them one at a time.
func ringDue(ctx context.Context, s *store) error {
	due, err := s.takeDue(time.Now())
	if err != nil {
		return err
	}
	for _, a := range due {
		if err := ring(ctx, s, a); err != nil {
			log.Printf("alarm %q: %v", a.label(), err)
		}
	}
	return nil
}

// ring plays the sound, shows the zenity dialog, and on snooze schedules a
// one-off copy of the alarm.
func ring(ctx context.Context, s *store, a *Alarm) error {
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
	stopSound := startSound(ctx, s, a)
	defer stopSound()
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

// startSound launches mpv looping the alarm sound and returns a function that
// stops it. Silent alarms, missing files and missing audio devices are all
// non-errors: the dialog still appears.
func startSound(ctx context.Context, s *store, a *Alarm) func() {
	noop := func() {}
	if a.Silent {
		return noop
	}
	path := s.resolveSound(a.Sound)
	if !fileExists(path) {
		log.Printf("alarm %q: sound %s not found", a.label(), path)
		return noop
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
