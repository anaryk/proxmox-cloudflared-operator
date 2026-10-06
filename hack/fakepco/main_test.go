package main

import (
	"testing"
	"time"
)

func TestTheControlsListenOnLoopbackOnly(t *testing.T) {
	for addr, ok := range map[string]bool{
		"127.0.0.1:7071": true,
		"[::1]:7071":     true,
		"0.0.0.0:7071":   false,
		"[::]:7071":      false,
		"10.0.0.5:7071":  false,
		"localhost:7071": false,
		"127.0.0.1":      false,
	} {
		if err := loopback(addr); (err == nil) != ok {
			t.Errorf("loopback(%q) = %v", addr, err)
		}
	}
}

func TestNowStartsTheClockAtItsTime(t *testing.T) {
	clock, err := clockFrom("", false)
	if err != nil || clock != nil {
		t.Fatalf("without -now: a clock of its own (%v), want the real one", err)
	}
	if _, err := clockFrom("yesterday", false); err == nil {
		t.Fatal("a time that is not RFC 3339 is taken")
	}
	start := time.Date(2026, 10, 6, 9, 30, 0, 0, time.UTC)
	clock, err = clockFrom("2026-10-06T09:30:00Z", false)
	if err != nil {
		t.Fatal(err)
	}
	if got := clock(); got.Before(start) || got.Sub(start) > time.Minute {
		t.Fatalf("the clock is at %s, not about %s", got, start)
	}
}

func TestStillKeepsTheClockAtTheTimeOfNow(t *testing.T) {
	if _, err := clockFrom("", true); err == nil {
		t.Fatal("-still without -now is taken")
	}
	clock, err := clockFrom("2026-10-06T09:30:00Z", true)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 6, 9, 30, 0, 0, time.UTC)
	first := clock()
	time.Sleep(10 * time.Millisecond)
	if !first.Equal(start) || !clock().Equal(start) {
		t.Fatalf("the clock moved: %s, then %s, from %s", first, clock(), start)
	}
}

func TestAScenarioIsLoadedByItsNameOrItsPath(t *testing.T) {
	e, err := load("empty", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := load("./nothing-here", nil); err == nil {
		t.Fatal("a directory that is not there is loaded")
	}
}
