package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// bootScript writes src to a temp file with the skin's extension and boots
// it the way loadLocked does, returning the Script and boot error.
func bootScript(t *testing.T, sk *skin, name, src string) (*Script, error) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name+sk.ext)
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Script{
		name:     name,
		skin:     sk,
		host:     &Host{known: []string{"_platform.audio_devices.changed", "scripts.*.*"}},
		grants:   parsePermissions(src, sk.commentPrefix),
		handlers: map[string]func() error{},
		events:   map[string][]func(payload any) error{},
		timers:   map[string]*time.Timer{},
	}
	return s, sk.boot(s, path)
}

func TestPatternSlotsFromString(t *testing.T) {
	slots, err := patternSlots("note this")
	if err != nil {
		t.Fatal(err)
	}
	if len(slots) != 2 {
		t.Fatalf("want 2 slots, got %d", len(slots))
	}
}

func TestPatternSlotsFromListWithAlternatives(t *testing.T) {
	slots, err := patternSlots([]any{"note", []any{"this", "that"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(slots) != 2 {
		t.Fatalf("want 2 slots, got %d", len(slots))
	}
}

func TestPatternSlotsRejectsEmpty(t *testing.T) {
	if _, err := patternSlots("  "); err == nil {
		t.Fatal("want error for empty pattern")
	}
	if _, err := patternSlots(3.0); err == nil {
		t.Fatal("want error for non-string/list pattern")
	}
}

func TestLuaGoRoundtrip(t *testing.T) {
	L := lua.NewState()
	defer L.Close()
	if err := L.DoString(`v = { level = 30, name = "AirPods", tags = {"a", "b"}, ok = true }`); err != nil {
		t.Fatal(err)
	}
	got := luaToGo(L.GetGlobal("v")).(map[string]any)
	if got["level"] != 30.0 || got["name"] != "AirPods" || got["ok"] != true {
		t.Fatalf("scalar fields wrong: %#v", got)
	}
	if tags := got["tags"].([]any); len(tags) != 2 || tags[0] != "a" {
		t.Fatalf("array field wrong: %#v", got["tags"])
	}

	// And back: a JSON event payload becomes a Lua table scripts can index.
	var payload any
	if err := json.Unmarshal([]byte(`{"kind":"added","name":"AirPods Pro","count":2,"nested":{"x":[1,2]}}`), &payload); err != nil {
		t.Fatal(err)
	}
	lv := goToLua(L, payload)
	L.SetGlobal("ev", lv)
	if err := L.DoString(`
		assert(ev.kind == "added")
		assert(ev.name:find("AirPods"))
		assert(ev.count == 2)
		assert(ev.nested.x[2] == 2)
	`); err != nil {
		t.Fatal(err)
	}
}

func TestLuaScriptRegistersCommandsAndEvents(t *testing.T) {
	s, err := bootScript(t, &luaSkin, "test", `-- permissions: events
command("note this", function() end)
command({ "volume", {"up", "down"} }, function() end)
on("_platform.audio_devices.changed", function(ev) end)
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.commands) != 2 || len(s.handlers) != 2 {
		t.Fatalf("want 2 commands, got %d specs / %d handlers", len(s.commands), len(s.handlers))
	}
	if len(s.events["_platform.audio_devices.changed"]) != 1 {
		t.Fatal("event subscription not recorded")
	}
}

func TestLuaCommandSpecActionShape(t *testing.T) {
	s, err := bootScript(t, &luaSkin, "vol", `command("mute now", function() end)`)
	if err != nil {
		t.Fatal(err)
	}
	var action map[string]any
	if err := json.Unmarshal(s.commands[0].Action, &action); err != nil {
		t.Fatal(err)
	}
	p, _ := action["params"].(map[string]any)
	// One params dialect: the payload nests under "params".
	if action["type"] != "scripts.run" || p["script"] != "vol" || p["handler"] != "h1" {
		t.Fatalf("action shape wrong: %#v", action)
	}
}

func TestParsePermissionsHeader(t *testing.T) {
	g := parsePermissions(`-- a demo script
--
-- permissions: dispatch:system, hud
-- permissions: events:_platform.app.focused

command("x", function() end)
-- permissions: dispatch  (past first code line: ignored)
`, "--")
	// The caps below prove the header was detected. There was a `g.declared`
	// check here too; that field recorded "a permissions line exists at all",
	// for a settings row that was never built, and nothing read it.
	for _, want := range []string{"dispatch:system", "hud", "events:_platform.app.focused"} {
		if !g.caps[want] {
			t.Fatalf("missing grant %q: %#v", want, g.caps)
		}
	}
	if g.caps["dispatch"] {
		t.Fatal("permissions line after first code line must be ignored")
	}
	if len(g.unknown) != 0 {
		t.Fatalf("unexpected unknown tokens: %v", g.unknown)
	}

	if g := parsePermissions(`print("no header")`, "--"); len(g.caps) != 0 || len(g.unknown) != 0 {
		t.Fatalf("headerless script should have empty grants: %#v", g)
	}
	if g := parsePermissions("-- permissions: dispatchh:system", "--"); len(g.unknown) != 1 {
		t.Fatalf("typo'd capability should be collected as unknown: %#v", g)
	}
	// JS/TS headers use the skin's comment marker.
	if g := parsePermissions("// permissions: hud, dispatch:system", "//"); !g.caps["hud"] || !g.caps["dispatch:system"] {
		t.Fatalf("slash-comment header not parsed: %#v", g)
	}
}

func TestGrantsAllows(t *testing.T) {
	g := grants{caps: map[string]bool{"dispatch:system": true, "events": true}}
	if !g.allows("dispatch", "system") || g.allows("dispatch", "input") {
		t.Fatal("qualified dispatch grant wrong")
	}
	if !g.allows("events", "_platform.app.focused") {
		t.Fatal("bare capability must allow any qualifier")
	}
	if g.allows("hud", "") {
		t.Fatal("undeclared capability must be refused")
	}
	wild := grants{caps: map[string]bool{"dispatch": true}}
	if !wild.allows("dispatch", "input") {
		t.Fatal("bare dispatch must allow any prefix")
	}
}

func TestLuaUndeclaredCallsAreRefused(t *testing.T) {
	// Each gated call raises before touching any SDK surface (host.plugin
	// is nil in bootScript — reaching it would panic, which is itself the
	// check that refusal happens first).
	for _, src := range []string{
		`dispatch("system.set_volume", { level = 30 })`,
		`on("_platform.app.focused", function() end)`,
		`hud("<div/>")`,
	} {
		s, err := bootScript(t, &luaSkin, "locked", src)
		if err == nil {
			t.Fatalf("want refusal for %s", src)
		} else if !strings.Contains(err.Error(), "permissions") {
			t.Fatalf("refusal should teach the fix, got: %v", err)
		}
		if len(s.events) != 0 {
			t.Fatal("refused on() must not record a subscription")
		}
	}

	// command/timer/log stay ungated.
	if _, err := bootScript(t, &luaSkin, "fine", `command("still fine", function() end)`); err != nil {
		t.Fatal(err)
	}
}
