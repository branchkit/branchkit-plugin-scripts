package main

import (
	"strings"
	"testing"
	"time"
)

// emitScript is a Script wired to a host with no plugin connection: enough to
// exercise every gate and name check bridgeEmit applies BEFORE it would reach
// the wire. The refusals are the contract worth pinning — what the platform
// does with a well-formed emit is the platform's own tested surface.
func emitScript(name, header string) *Script {
	return &Script{
		name:     name,
		skin:     &luaSkin,
		host:     &Host{known: []string{"scripts.*.*"}},
		grants:   parsePermissions(header, "--"),
		handlers: map[string]func() error{},
		events:   map[string][]func(payload any) error{},
		timers:   map[string]*time.Timer{},
	}
}

// Deny-by-default is what makes the header worth anything: a pasted script
// does not get to opt out of declaring by omitting the line.
func TestEmitRefusedWithoutTheGrant(t *testing.T) {
	s := emitScript("headphones", "-- permissions: hud\n")
	err := s.bridgeEmit("charged", nil)
	if err == nil {
		t.Fatal("emit without the grant must be refused")
	}
	// The refusal has to teach the exact line to paste, in this skin's
	// comment syntax — the ceremony only collapses if the fix is visible.
	if !strings.Contains(err.Error(), "-- permissions: emit") {
		t.Fatalf("refusal should name the token to add, got: %v", err)
	}
}

func TestEmitAcceptsBareAndQualifiedGrants(t *testing.T) {
	// A qualified grant reaches its own event and nothing else.
	s := emitScript("headphones", "-- permissions: emit:charged\n")
	if !s.grants.allows("emit", "charged") {
		t.Fatal("emit:charged should allow the charged event")
	}
	if s.grants.allows("emit", "drained") {
		t.Fatal("emit:charged must not allow a different event")
	}
	// The bare grant covers the namespace.
	bare := emitScript("headphones", "-- permissions: emit\n")
	if !bare.grants.allows("emit", "drained") {
		t.Fatal("bare emit should allow any event name")
	}
}

// The event name occupies a segment of a type the host is answerable for, and
// it is script-supplied text. A dotted name would emit a four-segment type the
// manifest pattern does not cover, and the refusal would come back from the
// platform as an opaque "not declared in manifest".
func TestEmitRejectsEventNamesThatAreNotOneSegment(t *testing.T) {
	s := emitScript("headphones", "-- permissions: emit\n")
	for _, bad := range []string{"", "battery.low", "with space", "star*", "$x"} {
		if err := s.bridgeEmit(bad, nil); err == nil {
			t.Errorf("event name %q must be refused", bad)
		}
	}
	// Not a special case worth carving out: `_platform` as the LAST segment
	// is `scripts.headphones._platform`, still squarely inside the namespace
	// the host declares. The reserved-namespace rule is about the first
	// segment, which a script never gets to choose.
}

// Same rule from the other side: the script name is a filename the USER chose,
// and it occupies the middle segment.
func TestEmitRejectsADottedScriptName(t *testing.T) {
	s := emitScript("my.notes", "-- permissions: emit\n")
	err := s.bridgeEmit("saved", nil)
	if err == nil {
		t.Fatal("a dotted script name must be refused")
	}
	if !strings.Contains(err.Error(), "rename the file") {
		t.Fatalf("refusal should say what to do about it, got: %v", err)
	}
}

func TestScriptEventOrigin(t *testing.T) {
	name, event, ok := scriptEventOrigin("scripts.headphones.charged")
	if !ok || name != "headphones" || event != "charged" {
		t.Fatalf("got (%q, %q, %v)", name, event, ok)
	}
	for _, notOurs := range []string{
		"_platform.app.focused",
		"scripts.headphones",
		"scripts.headphones.battery.low",
		"browser.tab.opened",
	} {
		if _, _, ok := scriptEventOrigin(notOurs); ok {
			t.Errorf("%q must not read as a script event", notOurs)
		}
	}
}

func TestPatternsOverlap(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		// A script's exact subscription against the manifest's pattern —
		// the case a plain map lookup used to call dead.
		{"scripts.*.*", "scripts.headphones.charged", true},
		{"scripts.*.*", "scripts.headphones.*", true},
		{"_platform.app.focused", "_platform.app.focused", true},
		{"scripts.*.*", "scripts.headphones", false},
		{"scripts.*.*", "browser.tab.opened", false},
		{"_platform.app.focused", "_platform.app.unfocused", false},
		// `**` is zero or more segments, on either side.
		{"scripts.*.*", "scripts.**", true},
		{"scripts.*.*", "**.charged", true},
		{"scripts.**", "scripts.*.*", true},
		{"_platform.**", "_platform.app.focused", true},
		{"scripts.*.*", "scripts.headphones.**", true},
		{"**", "anything.at.all", true},
		{"scripts.*.*", "browser.**", false},
		{"scripts.*.*", "**.a.b.c", false},
		{"_platform.app.*", "_platform.**.window.*", false},
	}
	for _, c := range cases {
		if got := patternsOverlap(c.a, c.b); got != c.want {
			t.Errorf("patternsOverlap(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestMatchesTopic(t *testing.T) {
	if !matchesTopic("scripts.headphones.*", "scripts.headphones.charged") {
		t.Error("a script's wildcard subscription should match its slice")
	}
	if matchesTopic("scripts.headphones.*", "scripts.notes.saved") {
		t.Error("a script's subscription must not catch another script's events")
	}
	// `*` is one segment — matching more would deliver events the platform's
	// own gate never sent.
	if matchesTopic("scripts.*", "scripts.headphones.charged") {
		t.Error("`*` must not swallow trailing segments")
	}
	// `**` is zero or more segments — the platform delivers on it, so the
	// fan-in must route on it.
	for _, c := range []struct {
		pattern, event string
		want           bool
	}{
		{"scripts.**", "scripts.headphones.charged", true},
		{"scripts.**", "scripts", true},
		{"scripts.headphones.**", "scripts.headphones", true},
		{"**.charged", "scripts.headphones.charged", true},
		{"scripts.**.charged", "scripts.charged", true},
		{"scripts.**", "scriptsx.headphones", false},
		{"scripts.**.charged", "scripts.headphones.saved", false},
		{"scripts.**b", "scripts.b", false},
	} {
		if got := matchesTopic(c.pattern, c.event); got != c.want {
			t.Errorf("matchesTopic(%q, %q) = %v, want %v", c.pattern, c.event, got, c.want)
		}
	}
}

// A script hearing its own emit is a loop with no natural bottom, and the
// platform's rate limit is a poor place to discover that.
func TestFanInSkipsTheEmittingScript(t *testing.T) {
	h := &Host{known: []string{"scripts.*.*"}, scripts: map[string]*Script{}}

	mk := func(name string) (*Script, *int) {
		count := 0
		s := &Script{
			name: name, skin: &luaSkin, host: h,
			grants:   parsePermissions("-- permissions: events\n", "--"),
			handlers: map[string]func() error{},
			events:   map[string][]func(payload any) error{},
			timers:   map[string]*time.Timer{},
		}
		s.events["scripts.*.*"] = []func(payload any) error{
			func(any) error { count++; return nil },
		}
		h.scripts[name] = s
		return s, &count
	}
	_, emitterSaw := mk("headphones")
	_, listenerSaw := mk("notes")

	h.FanIn("scripts.headphones.charged", pluginID, []byte(`{}`))

	if *emitterSaw != 0 {
		t.Errorf("the emitting script received its own event %d times", *emitterSaw)
	}
	if *listenerSaw != 1 {
		t.Errorf("the other script should have received it once, got %d", *listenerSaw)
	}
}

// A `scripts.<name>.<event>` names a script as its origin, and the callbacks
// it runs carry this plugin's grants — so one the platform says another plugin
// sent is dropped, not relayed as if the script had said it.
func TestFanInIgnoresScriptEventsFromAnotherSender(t *testing.T) {
	h := &Host{known: []string{"scripts.*.*"}, scripts: map[string]*Script{}}
	seen := 0
	s := &Script{
		name: "notes", skin: &luaSkin, host: h,
		grants:   parsePermissions("-- permissions: events\n", "--"),
		handlers: map[string]func() error{},
		events:   map[string][]func(payload any) error{},
		timers:   map[string]*time.Timer{},
	}
	s.events["scripts.headphones.*"] = []func(payload any) error{
		func(any) error { seen++; return nil },
	}
	h.scripts["notes"] = s

	for _, source := range []string{"impostor", "_platform", ""} {
		h.FanIn("scripts.headphones.charged", source, []byte(`{}`))
	}
	if seen != 0 {
		t.Fatalf("a script event from another sender reached a script %d times", seen)
	}
	h.FanIn("scripts.headphones.charged", pluginID, []byte(`{}`))
	if seen != 1 {
		t.Fatalf("the host's own script event should still arrive once, got %d", seen)
	}
}

// Platform events reach every subscriber, including a script that also emits.
func TestFanInDeliversPlatformEventsToEveryone(t *testing.T) {
	h := &Host{known: []string{"_platform.app.focused"}, scripts: map[string]*Script{}}
	seen := 0
	for _, name := range []string{"headphones", "notes"} {
		s := &Script{
			name: name, skin: &luaSkin, host: h,
			handlers: map[string]func() error{},
			events:   map[string][]func(payload any) error{},
			timers:   map[string]*time.Timer{},
		}
		s.events["_platform.app.focused"] = []func(payload any) error{
			func(any) error { seen++; return nil },
		}
		h.scripts[name] = s
	}
	h.FanIn("_platform.app.focused", "_platform", []byte(`{}`))
	if seen != 2 {
		t.Fatalf("both scripts should have seen the platform event, got %d", seen)
	}
}
