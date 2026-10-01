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
	"golang.org/x/sys/unix"
)

// lateWarn is how late an alarm has to be before the daemon says so.
const lateWarn = time.Minute

// sleeper waits until a wall clock deadline. Go's own timers run on the
// monotonic clock, which stops while the machine is suspended, so a timer set
// before a suspend comes back hours after its deadline. This waits on a
// CLOCK_BOOTTIME timerfd instead: boottime keeps counting through suspend, so
// an alarm that came due while the machine slept is already expired the
// instant it resumes. There is no wake-up polling, and it is not an alarm
// clock: only CLOCK_REALTIME_ALARM with CAP_WAKE_ALARM can wake a sleeping
// machine, which snoozer deliberately does not ask for.
type sleeper struct {
	f *os.File
	C chan time.Time
}

// newSleeper creates the one timerfd the daemon waits on.
func newSleeper() (*sleeper, error) {
	fd, err := unix.TimerfdCreate(unix.CLOCK_BOOTTIME, unix.TFD_CLOEXEC|unix.TFD_NONBLOCK)
	if err != nil {
		return nil, serr.Wrap(err)
	}
	s := &sleeper{
		f: os.NewFile(uintptr(fd), "snoozer-wake"),
		C: make(chan time.Time, 1),
	}
	go s.loop()
	return s, nil
}

// loop is the timerfd's only reader for the life of the daemon. Arming
// replaces the deadline rather than making another timer, so re-arming on
// every SIGUSR1 neither leaks a timer nor leaks a goroutine.
func (s *sleeper) loop() {
	buf := make([]byte, 8)
	for {
		if _, err := s.f.Read(buf); err != nil {
			if !errors.Is(err, os.ErrClosed) {
				log.Printf("wake timer: %v", err)
			}
			return // closed on shutdown, or the timerfd went away
		}
		select {
		case s.C <- time.Now():
		default:
			// A wake is already pending; the daemon re-arms from state.
		}
	}
}

// arm fires at t. An overdue deadline fires on the next read, which is the
// catch-up path after a suspend or a restart.
func (s *sleeper) arm(t time.Time) error {
	d := time.Until(t)
	if d <= 0 {
		// A zero interval means disarm, not "due now", so give it a nanosecond.
		d = time.Nanosecond
	}
	its := &unix.ItimerSpec{Value: unix.NsecToTimespec(d.Nanoseconds())}
	if err := unix.TimerfdSettime(s.fd(), 0, its, nil); err != nil {
		return serr.Wrap(err)
	}
	return nil
}

// disarm stops the timer. With no alarms pending, SIGUSR1 is the only thing
// that can change anything.
func (s *sleeper) disarm() error {
	if err := unix.TimerfdSettime(s.fd(), 0, &unix.ItimerSpec{}, nil); err != nil {
		return serr.Wrap(err)
	}
	return nil
}

func (s *sleeper) fd() int { return int(s.f.Fd()) }

func (s *sleeper) close() error { return serr.Wrap(s.f.Close()) }

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
	sl, err := newSleeper()
	if err != nil {
		return err
	}
	defer sl.close()

	for {
		next, err := s.nextWake()
		if err != nil {
			log.Printf("read state: %v", err)
			next = time.Now().Add(time.Minute)
		}
		if next.IsZero() {
			if err := sl.disarm(); err != nil {
				log.Printf("disarm: %v", err)
			}
		} else if err := sl.arm(next); err != nil {
			log.Printf("arm: %v", err)
		}
		select {
		case <-ctx.Done():
			log.Printf("daemon stopping")
			return nil
		case <-reload:
			log.Printf("state changed, rescheduling")
		case <-sl.C:
			if err := ringDue(ctx, s); err != nil {
				log.Printf("ring: %v", err)
			}
		}
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
