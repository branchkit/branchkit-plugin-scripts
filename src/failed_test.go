package main

import (
	"testing"
	"time"
)

func dispatchScript(name, header string) *Script {
	return &Script{
		name:     name,
		skin:     &luaSkin,
		host:     &Host{},
		grants:   parsePermissions(header, "--"),
		handlers: map[string]func() error{},
		events:   map[string][]func(payload any) error{},
		timers:   map[string]*time.Timer{},
	}
}

// Scoped to the LAST dispatch. A refusal clears it on entry, so a script
// cannot be told about a failure that happened three calls ago.
func TestFailedIsClearedByTheNextDispatch(t *testing.T) {
	s := dispatchScript("volume", "-- permissions: dispatch:system\n")
	s.dispatchErr = "dispatch(system.set_volume): stale"

	// A refused dispatch is a HARD error, and it still resets the reader:
	// the refusal is raised in-language, so `failed()` must not go on
	// reporting the previous call.
	_, err := s.bridgeDispatch("windows.focus_left", nil)
	if err == nil {
		t.Fatal("a dispatch outside the granted prefix must be refused")
	}
	if s.bridgeFailed() != nil {
		t.Errorf("failed() must answer about the last dispatch, got %#v", s.bridgeFailed())
	}
}

// The settings row's error and the script's question are different values.
// lastErr is written by load failures, refusals and every other bridged
// call; answering `failed()` from it would hand a script somebody else's
// problem.
func TestFailedDoesNotReadTheRowError(t *testing.T) {
	s := dispatchScript("volume", "-- permissions: dispatch\n")
	s.lastErr = "on(\"x.y\"): outside the host manifest's consumed events"

	if s.bridgeFailed() != nil {
		t.Errorf("failed() must not report a non-dispatch error, got %#v", s.bridgeFailed())
	}
}
