package main

import (
	"testing"
	"time"
)

func TestSleeper(t *testing.T) {
	sl, err := newSleeper()
	if err != nil {
		t.Skipf("no CLOCK_BOOTTIME timerfd: %v", err)
	}
	defer sl.close()

	if err := sl.arm(time.Now().Add(80 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if !woke(sl, 5*time.Second) {
		t.Fatal("sleeper did not fire at its deadline")
	}

	// An overdue deadline fires immediately. This is the catch-up path after a
	// suspend: boottime counted the machine's sleep, so the deadline is past.
	if err := sl.arm(time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if !woke(sl, 2*time.Second) {
		t.Fatal("overdue deadline did not fire")
	}

	// With no alarms the timer is disarmed and stays quiet.
	if err := sl.disarm(); err != nil {
		t.Fatal(err)
	}
	if woke(sl, 200*time.Millisecond) {
		t.Fatal("disarmed sleeper woke on its own")
	}
}

// woke reports whether the sleeper woke within the limit.
func woke(sl *sleeper, limit time.Duration) bool {
	select {
	case <-sl.C:
		return true
	case <-time.After(limit):
		return false
	}
}

func TestFirstNoisy(t *testing.T) {
	silent := []*Alarm{{Silent: true}, {Silent: true}}
	if got := firstNoisy(silent); got != nil {
		t.Errorf("firstNoisy(all silent) = %+v, want nil", got)
	}
	loud := &Alarm{Label: "Steps"}
	got := firstNoisy([]*Alarm{{Silent: true}, loud, {Silent: true}})
	if got != loud {
		t.Errorf("firstNoisy = %+v, want the non-silent alarm", got)
	}
	if got := firstNoisy(nil); got != nil {
		t.Errorf("firstNoisy(nil) = %+v, want nil", got)
	}
}
