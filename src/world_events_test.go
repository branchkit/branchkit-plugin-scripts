package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	branchkit "github.com/branchkit/plugin-sdk-go"
)

// privilegeAsks answers privileges.request with a fixed status and records
// every privilege asked for: the platform's half of the ask, and nothing
// else.
type privilegeAsks struct {
	status  string
	granted []string // privileges privileges.list reports held
	asked   []string
}

func (f *privilegeAsks) Call(method string, params any, result any) error {
	if method == branchkit.MethodPrivilegesList {
		entries := []map[string]any{}
		for _, p := range f.granted {
			entries = append(entries, map[string]any{"privilege": p, "granted": true})
		}
		raw, _ := json.Marshal(map[string]any{"privileges": entries})
		return json.Unmarshal(raw, result)
	}
	if method == branchkit.MethodPrivilegesRequest {
		if req, ok := params.(*branchkit.PrivilegesRequestRequest); ok {
			f.asked = append(f.asked, req.Privilege)
		}
		raw, _ := json.Marshal(map[string]any{"status": f.status})
		return json.Unmarshal(raw, result)
	}
	return branchkit.ErrDetached
}

func worldScript(asks *privilegeAsks) *Script {
	p := branchkit.NewDetachedPlugin()
	p.Methods = branchkit.NewMethods(asks)
	return &Script{
		name: "focus",
		skin: &luaSkin,
		host: &Host{
			plugin:     p,
			known:      []string{"_platform.window.*", "_platform.app.*"},
			privWaits:  map[string]map[string]bool{},
			worldWaits: map[string]bool{},
		},
		grants:   parsePermissions("-- permissions: events\n", "--"),
		handlers: map[string]func() error{},
		events:   map[string][]func(payload any) error{},
		timers:   map[string]*time.Timer{},
	}
}

func TestReachesWorldEvents(t *testing.T) {
	for pattern, want := range map[string]bool{
		"_platform.window.focused": true,
		"_platform.window.*":       true,
		"_platform.world.updated":  true,
		"_platform.**":             true,
		"**":                       true,
		"_platform.app.launched":   false,
		"_platform.app.*":          false,
		"scripts.*.*":              false,
	} {
		if got := reachesWorldEvents(pattern); got != want {
			t.Errorf("reachesWorldEvents(%q) = %v, want %v", pattern, got, want)
		}
	}
}

// on() of a window event asks for events.world once per load, attributed to
// the script, records the script as waiting (so the grant reloads it), and
// says on its row that it is awaiting approval. Before, such a script was
// quiet with no visible reason until the user found the grant.
func TestOnAWindowEventAsksForEventsWorld(t *testing.T) {
	asks := &privilegeAsks{status: "pending"}
	s := worldScript(asks)
	for _, pattern := range []string{"_platform.window.focused", "_platform.window.closed"} {
		if err := s.registerEvent(pattern, func(any) error { return nil }); err != nil {
			t.Fatalf("on(%q): %v", pattern, err)
		}
	}
	if len(asks.asked) != 1 || asks.asked[0] != worldEventPrivilege {
		t.Fatalf("want one ask for %s, got %v", worldEventPrivilege, asks.asked)
	}
	if !s.host.worldWaits["focus"] {
		t.Fatal("the script must wait on the grant, so the grant reloads it")
	}
	if !strings.Contains(s.lastErr, "awaiting approval") {
		t.Fatalf("the row should say it is awaiting approval, got %q", s.lastErr)
	}
}

// Other events ask for nothing, and a held grant leaves the row quiet.
func TestOnOtherEventsOrAHeldGrantSaysNothing(t *testing.T) {
	asks := &privilegeAsks{status: "pending"}
	s := worldScript(asks)
	if err := s.registerEvent("_platform.app.launched", func(any) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(asks.asked) != 0 {
		t.Fatalf("a non-world event must not ask, got %v", asks.asked)
	}

	// Either privilege that admits the world family is enough: the platform
	// delivers these events to a holder of `windows` too.
	for _, priv := range []string{"events.world", "windows"} {
		held := &privilegeAsks{status: "pending", granted: []string{priv}}
		s = worldScript(held)
		if err := s.registerEvent("_platform.window.focused", func(any) error { return nil }); err != nil {
			t.Fatal(err)
		}
		if len(held.asked) != 0 || s.lastErr != "" {
			t.Fatalf("holding %s: want no ask and no note, got %v / %q", priv, held.asked, s.lastErr)
		}
	}
}

// An error already on the row (a header typo) is not replaced by the note.
func TestTheAskDoesNotHideARowError(t *testing.T) {
	s := worldScript(&privilegeAsks{status: "pending"})
	s.lastErr = "permissions header: unknown grant"
	if err := s.registerEvent("_platform.window.focused", func(any) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if s.lastErr != "permissions header: unknown grant" {
		t.Fatalf("the earlier error must stay, got %q", s.lastErr)
	}
	if !s.host.worldWaits["focus"] {
		t.Fatal("a later grant must reload the waiting script")
	}
}

// A pending events.world request stays wanted while an enabled script's
// header names a world event, or while a script's on() waits on it;
// otherwise withdrawStaleRequests would give back the very request on()
// just filed. A bare `events` header alone says nothing about which events.
func TestWorldEventHeadersDemandEventsWorld(t *testing.T) {
	dir := t.TempDir()
	write := func(name, src string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("bare.lua", "-- permissions: events\n")
	write("window.lua", "-- permissions: events:_platform.window.*\n")
	write("apps.lua", "-- permissions: events:_platform.app.*\n")
	write("waiting.lua", "-- permissions: events\n")
	h := &Host{dir: dir, worldWaits: map[string]bool{}}
	h.noteWorldWait("waiting")
	got := h.privilegeDemand(nil, map[string]scriptState{})[worldEventPrivilege]
	want := []string{"waiting.lua", "window.lua"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events.world demand = %v, want %v", got, want)
	}
}

// A reload starts with no world-event wait, and clearing it leaves a call()
// wait on `windows` alone: the grant must still reload that script.
func TestALoadForgetsOnlyTheWorldWait(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "focus.lua"), []byte("-- permissions: events\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := &Host{dir: dir, privWaits: map[string]map[string]bool{}, worldWaits: map[string]bool{}}
	h.noteWorldWait("focus")
	h.privWaits["windows"] = map[string]bool{"focus": true} // a call() wait
	h.clearWorldWait("focus")
	if h.worldWaits["focus"] {
		t.Fatal("the world wait must be gone")
	}
	if !h.privWaits["windows"]["focus"] {
		t.Fatal("the call() wait must stay")
	}
	if got := h.privilegeDemand(nil, map[string]scriptState{})[worldEventPrivilege]; len(got) != 0 {
		t.Fatalf("no events.world demand left, got %v", got)
	}
}
