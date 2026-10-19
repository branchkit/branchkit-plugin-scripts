package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func effectHost() *Host {
	h := &Host{known: []string{effectDisplacedEvent}, effects: map[string]map[string]bool{}}
	h.SetDeclaredCollections(nil)
	return h
}

func effectScript(h *Host, name, header string) *Script {
	s := &Script{
		name:     name,
		skin:     &luaSkin,
		host:     h,
		grants:   parsePermissions(header, "--"),
		handlers: map[string]func() error{},
		events:   map[string][]func(payload any) error{},
		timers:   map[string]*time.Timer{},
	}
	if h.scripts == nil {
		h.scripts = map[string]*Script{}
	}
	h.scripts[name] = s
	return s
}

// Holding an effect changes system behavior — muted notifications, a screen
// that will not sleep — so it declares. Releasing does not: letting go must
// never be the call that gets refused.
func TestHoldNeedsTheGrantAndReleaseDoesNot(t *testing.T) {
	s := effectScript(effectHost(), "meeting", "-- permissions: hud\n")
	_, err := s.bridgeHold("suppress_notifications")
	if err == nil || !strings.Contains(err.Error(), "-- permissions: effect") {
		t.Fatalf("hold should be refused with the token to add, got: %v", err)
	}
	if !effectScript(effectHost(), "meeting", "-- permissions: effect:suppress_notifications\n").
		grants.allows("effect", "suppress_notifications") {
		t.Fatal("effect:<name> should allow that effect")
	}
	if effectScript(effectHost(), "meeting", "-- permissions: effect:suppress_notifications\n").
		grants.allows("effect", "disable_screen_dim") {
		t.Fatal("effect:<name> must not allow a different effect")
	}
}

// The platform's stack has one frame per PLUGIN, so two scripts wanting the
// same effect are one holder to it. The host refcounts underneath: assert on
// the empty→held transition, retract only when the last script lets go.
// Without it, the second script's assert is an idempotent no-op and the
// FIRST script's release pulls the frame out from under it.
func TestEffectHoldersAreRefcountedPerScript(t *testing.T) {
	h := effectHost()
	h.noteEffectHolder("suppress_notifications", "meeting")
	h.noteEffectHolder("suppress_notifications", "focus")

	if !h.holdsEffect("suppress_notifications", "meeting") ||
		!h.holdsEffect("suppress_notifications", "focus") {
		t.Fatal("both scripts should register as holders")
	}

	if last := h.dropEffectHolder("suppress_notifications", "meeting"); last {
		t.Fatal("releasing while a sibling still wants it must not retract the frame")
	}
	if !h.holdsEffect("suppress_notifications", "focus") {
		t.Fatal("one script's release erased another's claim")
	}

	if last := h.dropEffectHolder("suppress_notifications", "focus"); !last {
		t.Fatal("the last release should retract the frame")
	}
	if h.holdsEffect("suppress_notifications", "focus") {
		t.Fatal("holders should be cleared")
	}
}

// Releasing what you never took must not retract a frame a sibling relies on.
func TestReleasingWhatYouNeverHeldIsNotTheLastRelease(t *testing.T) {
	h := effectHost()
	h.noteEffectHolder("suppress_notifications", "meeting")
	if last := h.dropEffectHolder("suppress_notifications", "stranger"); last {
		t.Fatal("a non-holder's release must not read as the last one")
	}
	if !h.holdsEffect("suppress_notifications", "meeting") {
		t.Fatal("a non-holder's release erased the real holder")
	}
	if last := h.dropEffectHolder("disable_screen_dim", "meeting"); last {
		t.Fatal("releasing an effect nobody holds must not read as the last release")
	}
}

// Teardown gives back only what goes unwanted. The load-bearing case is a
// script that mutes notifications and is then edited away — but a sibling
// still wanting the effect must keep it.
func TestReleaseEffectsLeavesWhatASiblingStillWants(t *testing.T) {
	h := effectHost()
	h.noteEffectHolder("suppress_notifications", "meeting")
	h.noteEffectHolder("suppress_notifications", "focus")
	h.noteEffectHolder("disable_screen_dim", "meeting")

	// releaseEffects would RPC, so exercise the decision it makes rather
	// than the call: meeting's departure leaves suppress_notifications
	// wanted and disable_screen_dim unwanted.
	if last := h.dropEffectHolder("suppress_notifications", "meeting"); last {
		t.Fatal("focus still wants suppress_notifications")
	}
	if last := h.dropEffectHolder("disable_screen_dim", "meeting"); !last {
		t.Fatal("nobody else wants disable_screen_dim; it should be given back")
	}
}

// Displacement is broadcast — every subscribed plugin hears every one — so
// the host has to narrow it or "you lost the effect" reaches scripts that
// never held anything.
func TestDisplacementIsScopedToTheScriptThatLost(t *testing.T) {
	h := effectHost()
	h.noteEffectHolder("suppress_notifications", "meeting")

	// Another plugin's displacement is not ours to relay.
	other := mustJSONBytes(t, map[string]any{
		"displaced_owner": "voice", "effect": "suppress_notifications", "new_owner": "browser",
	})
	if scripts, effect := h.displacedScripts(other); len(scripts) != 0 || effect != "" {
		t.Fatalf("another plugin's displacement leaked through: (%v, %q)", scripts, effect)
	}
	if !h.holdsEffect("suppress_notifications", "meeting") {
		t.Fatal("another plugin's displacement must not touch our bookkeeping")
	}

	// The frame is gone for the whole plugin, so every script that wanted it
	// lost — not just the one that asked last.
	h.noteEffectHolder("suppress_notifications", "focus")
	ours := mustJSONBytes(t, map[string]any{
		"displaced_owner": pluginID, "effect": "suppress_notifications", "new_owner": "voice",
	})
	scripts, effect := h.displacedScripts(ours)
	if effect != "suppress_notifications" {
		t.Fatalf("effect should be identified, got %q", effect)
	}
	if len(scripts) != 2 || scripts[0] != "focus" || scripts[1] != "meeting" {
		t.Fatalf("every holder should be told, got %v", scripts)
	}
	// The frame is gone, so the bookkeeping must not claim otherwise.
	if h.holdsEffect("suppress_notifications", "meeting") ||
		h.holdsEffect("suppress_notifications", "focus") {
		t.Fatal("displacement should drop every claim")
	}
}

// Ours, but the script that held it is gone — nobody to tell, and the
// bookkeeping is already clean.
func TestDisplacementWithNoHolderOnFileIsNotRelayed(t *testing.T) {
	h := effectHost()
	ours := mustJSONBytes(t, map[string]any{
		"displaced_owner": pluginID, "effect": "disable_screen_dim", "new_owner": "voice",
	})
	scripts, effect := h.displacedScripts(ours)
	if len(scripts) != 0 {
		t.Fatalf("no script should be named, got %v", scripts)
	}
	if effect != "disable_screen_dim" {
		t.Fatalf("the effect should still be identified as ours, got %q", effect)
	}
}

func TestFanInDeliversDisplacementOnlyToTheLoser(t *testing.T) {
	h := effectHost()
	h.noteEffectHolder("suppress_notifications", "meeting")

	seen := map[string]int{}
	for _, name := range []string{"meeting", "bystander"} {
		s := effectScript(h, name, "-- permissions: events\n")
		n := name
		s.events[effectDisplacedEvent] = []func(payload any) error{
			func(any) error { seen[n]++; return nil },
		}
	}

	h.FanIn(effectDisplacedEvent, "_platform", mustJSONBytes(t, map[string]any{
		"displaced_owner": pluginID, "effect": "suppress_notifications", "new_owner": "voice",
	}))

	if seen["meeting"] != 1 {
		t.Errorf("the script that lost the frame should be told once, got %d", seen["meeting"])
	}
	if seen["bystander"] != 0 {
		t.Errorf("a script that never held it was told %d times", seen["bystander"])
	}
}

func TestFanInDropsAnotherPluginsDisplacement(t *testing.T) {
	h := effectHost()
	seen := 0
	s := effectScript(h, "meeting", "-- permissions: events\n")
	s.events[effectDisplacedEvent] = []func(payload any) error{
		func(any) error { seen++; return nil },
	}
	h.FanIn(effectDisplacedEvent, "_platform", mustJSONBytes(t, map[string]any{
		"displaced_owner": "voice", "effect": "suppress_notifications", "new_owner": "browser",
	}))
	if seen != 0 {
		t.Fatalf("another plugin's displacement reached a script %d times", seen)
	}
}

func mustJSONBytes(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
