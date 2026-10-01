package main

import (
	"testing"
	"time"
)

func TestNextSleep(t *testing.T) {
	now := referenceNow
	cases := []struct {
		name string
		next time.Time
		want time.Duration
	}{
		{"no alarms", time.Time{}, idleSleep},
		{"overdue", now.Add(-time.Hour), 0},
		{"imminent", now.Add(5 * time.Second), 5 * time.Second},
		{"at the cap", now.Add(sleepSlice), sleepSlice},
		{"past the cap", now.Add(6 * time.Hour), sleepSlice},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := nextSleep(c.next, now); got != c.want {
				t.Errorf("nextSleep(%s) = %s, want %s", c.next, got, c.want)
			}
		})
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
