// Command snoozer is an alarm clock for Linux. With no arguments it runs the
// daemon in the foreground; with --in or --at it adds an alarm and wakes the
// daemon so it reschedules. The same binary is both the CLI and the daemon.
package main

import (
	_ "embed"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/daaku/serr"
)

// alarmOgg is the bundled default sound, installed into the config directory
// on first use so the daemon can play it.
//
//go:embed alarm.ogg
var alarmOgg []byte

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "snoozer:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) == 1 {
		return runDaemon()
	}
	fs := flag.NewFlagSet("snoozer", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: snoozer [--in=DUR | --at=TIME] [options]")
		fs.PrintDefaults()
	}
	in := fs.String("in", "", "ring after a duration, e.g. 5m")
	at := fs.String("at", "", "ring at a clock time, e.g. 11am, 3:30pm")
	label := fs.String("label", "", "text shown in the alarm dialog")
	repeat := fs.String("repeat", "", "days to repeat (mon, tue, ... or all)")
	silent := fs.Bool("silent", false, "do not play a sound")
	snooze := fs.String("snooze", "", "snooze duration (default 9m)")
	timeout := fs.String("timeout", "", "dialog timeout (default 3m, 0 disables)")
	sound := fs.String("sound", "", "sound file (default alarm.ogg)")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return serr.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	s, err := newStore()
	if err != nil {
		return err
	}
	if err := s.ensureSound(); err != nil {
		return err
	}
	a, err := buildAlarm(time.Now(), *in, *at, *label, *repeat, *silent, *snooze, *timeout, *sound)
	if err != nil {
		return err
	}
	if err := s.add(a); err != nil {
		return err
	}
	notifyDaemon(s)
	fmt.Println(a.Summary())
	return nil
}

// notifyDaemon sends SIGUSR1 so a running daemon reloads the state file. If
// there is nobody to signal the alarm is saved but will never ring, which is
// worth saying out loud.
func notifyDaemon(s *store) {
	const hint = "start it with: systemctl --user enable --now snoozer"
	b, err := os.ReadFile(s.pidPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "snoozer: daemon is not running, the alarm will not ring; %s\n", hint)
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "snoozer: bad daemon pid file %s; %s\n", s.pidPath(), hint)
		return
	}
	if err := syscall.Kill(pid, syscall.SIGUSR1); err != nil {
		fmt.Fprintf(os.Stderr, "snoozer: daemon pid %d is gone, the alarm will not ring; %s\n", pid, hint)
	}
}
