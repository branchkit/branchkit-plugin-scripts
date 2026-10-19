package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestJSScriptRegistersCommandsAndEvents(t *testing.T) {
	s, err := bootScript(t, &jsSkin, "test", `// permissions: events
command("note this", () => {});
command(["volume", ["up", "down"]], () => {});
on("_platform.audio_devices.changed", (ev) => {});
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
	var action map[string]any
	if err := json.Unmarshal(s.commands[0].Action, &action); err != nil {
		t.Fatal(err)
	}
	p, _ := action["params"].(map[string]any)
	// One params dialect: the payload nests under "params".
	if action["type"] != "scripts.run" || p["script"] != "test" || p["handler"] != "h1" {
		t.Fatalf("action shape wrong: %#v", action)
	}
	if s.infos[1].phrase != "volume up/down" {
		t.Fatalf("pattern display wrong: %q", s.infos[1].phrase)
	}
}

func TestJSUndeclaredCallsAreRefused(t *testing.T) {
	for _, src := range []string{
		`dispatch("system.set_volume", { level: 30 });`,
		`on("_platform.app.focused", () => {});`,
		`hud("<div/>");`,
	} {
		s, err := bootScript(t, &jsSkin, "locked", src)
		if err == nil {
			t.Fatalf("want refusal for %s", src)
		} else if !strings.Contains(err.Error(), "permissions") {
			t.Fatalf("refusal should teach the fix, got: %v", err)
		}
		if len(s.events) != 0 {
			t.Fatal("refused on() must not record a subscription")
		}
	}
	// The teaching message uses the skin's comment marker.
	_, err := bootScript(t, &jsSkin, "locked", `hud("x")`)
	if err == nil || !strings.Contains(err.Error(), "`// permissions: hud`") {
		t.Fatalf("JS refusal should name the // header form, got: %v", err)
	}
}

func TestJSEventPayloadRoundtrip(t *testing.T) {
	s, err := bootScript(t, &jsSkin, "ev", `// permissions: events, hud
let seen = "";
on("_platform.audio_devices.changed", (ev) => {
  if (ev.kind === "added" && ev.names[1] === "b") { seen = "ok"; }
  if (seen !== "ok") { throw new Error("payload shape wrong: " + JSON.stringify(ev)); }
});
`)
	if err != nil {
		t.Fatal(err)
	}
	fn := s.events["_platform.audio_devices.changed"][0]
	if err := fn(map[string]any{"kind": "added", "names": []any{"a", "b"}}); err != nil {
		t.Fatal(err)
	}
}

func TestTSTranspilesAndRegisters(t *testing.T) {
	s, err := bootScript(t, &tsSkin, "typed", `// permissions: events
declare function command(pattern: string, fn: () => void): void;
declare function on(event: string, fn: (payload: unknown) => void): void;
const phrase: string = "note this";
command(phrase, (): void => {});
on("_platform.audio_devices.changed", (ev: unknown): void => {});
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.commands) != 1 || len(s.events) != 1 {
		t.Fatalf("TS registration wrong: %d commands, %d events", len(s.commands), len(s.events))
	}
}

func TestLintPerSkin(t *testing.T) {
	if err := lintJS("x", `command("ok", () => {});`); err != nil {
		t.Fatalf("valid JS flagged: %v", err)
	}
	if err := lintJS("x", `broken ((`); err == nil {
		t.Fatal("broken JS not flagged")
	}
	if err := lintTS("x", `const n: number = 1;`); err != nil {
		t.Fatalf("valid TS flagged: %v", err)
	}
	if err := lintTS("x", `const n: = ;`); err == nil {
		t.Fatal("broken TS not flagged")
	}
	if err := lintLua("x", `command("ok", function() end)`); err != nil {
		t.Fatalf("valid Lua flagged: %v", err)
	}
	if err := lintLua("x", `broken ((`); err == nil {
		t.Fatal("broken Lua not flagged")
	}
}
