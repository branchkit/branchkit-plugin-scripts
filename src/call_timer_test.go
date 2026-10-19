package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// waitFor polls cond until it holds or the deadline passes — timer tests
// assert on real wall-clock fires, so fixed sleeps would be either slow or
// flaky.
func waitFor(t *testing.T, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// phrases reads the registered command phrases under the script lock —
// timer callbacks register commands as their observable side effect.
func phrases(s *Script) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.infos))
	for _, i := range s.infos {
		out = append(out, i.phrase)
	}
	return out
}

func hasPhrase(s *Script, want string) bool {
	for _, p := range phrases(s) {
		if p == want {
			return true
		}
	}
	return false
}

func TestGrantsCallTokens(t *testing.T) {
	g := parsePermissions("-- permissions: call:apps, hud\n", "--")
	if !g.allows("call", "apps") {
		t.Fatal("call:apps not granted")
	}
	if g.allows("call", "windows") {
		t.Fatal("call:windows must not be granted by call:apps")
	}
	if len(g.unknown) != 0 {
		t.Fatalf("call token flagged unknown: %v", g.unknown)
	}
	bare := parsePermissions("-- permissions: call\n", "--")
	if !bare.allows("call", "apps") || !bare.allows("call", "windows") {
		t.Fatal("bare call must reach every privilege bucket")
	}
}

func TestMethodPrivilegeTable(t *testing.T) {
	if priv, ok := methodPrivilege("native.frontmost_app"); !ok || priv != "apps" {
		t.Fatalf("native.frontmost_app: want apps, got %q ok=%v", priv, ok)
	}
	if priv, ok := methodPrivilege("collection.put"); !ok || priv != "" {
		t.Fatalf("collection.put: want privilege-less, got %q ok=%v", priv, ok)
	}
	if _, ok := methodPrivilege("native.does_not_exist"); ok {
		t.Fatal("unknown method must not resolve")
	}
}

func TestLuaCallRefusals(t *testing.T) {
	// Ungranted: the refusal teaches the exact token.
	_, err := bootScript(t, &luaSkin, "q1", `call("native.frontmost_app")`)
	if err == nil || !strings.Contains(err.Error(), "call:apps") {
		t.Fatalf("refusal should name call:apps, got: %v", err)
	}
	// Unknown method: refused even with bare call (fail-closed table).
	_, err = bootScript(t, &luaSkin, "q2", "-- permissions: call\ncall(\"native.does_not_exist\")")
	if err == nil || !strings.Contains(err.Error(), "unknown method") {
		t.Fatalf("unknown method should be refused, got: %v", err)
	}
	// shell / security: no header token reaches them. These change the
	// guarantee class (arbitrary exec, the credential store) and are the
	// tier-3 discussion, not a header token.
	for _, method := range []string{"system.run_shell", "native.keychain_read"} {
		_, err := bootScript(t, &luaSkin, "q3", "-- permissions: call\ncall(\""+method+"\")")
		if err == nil || !strings.Contains(err.Error(), "not available to scripts") {
			t.Fatalf("%s should be refused outright, got: %v", method, err)
		}
	}
	// Privilege-less methods are also refused through call() — but the
	// refusal points at the curated projection rather than claiming the
	// method is off limits. `privilege: null` means governed on a different
	// axis (writers policy, interaction grants), not unguarded, and
	// collections ARE reachable — through record/records/put/drop, which
	// apply the per-collection rules this coarse gate cannot express.
	_, err = bootScript(t, &luaSkin, "q4", "-- permissions: call\ncall(\"collection.put\")")
	if err == nil || !strings.Contains(err.Error(), "record/records/put/drop") {
		t.Fatalf("collection.put should point at the projection, got: %v", err)
	}
	_, err = bootScript(t, &luaSkin, "q5", "-- permissions: call\ncall(\"commands.push\")")
	if err == nil || !strings.Contains(err.Error(), "command/on/hud/dispatch") {
		t.Fatalf("commands.push should point at the projection, got: %v", err)
	}
}

func TestJSCallRefusalUsesSlashMarker(t *testing.T) {
	_, err := bootScript(t, &jsSkin, "q", `call("native.battery");`)
	if err == nil || !strings.Contains(err.Error(), "`// permissions: call:system`") {
		t.Fatalf("JS refusal should name the // header form, got: %v", err)
	}
}

func TestLuaTimerIdAndCancel(t *testing.T) {
	s, err := bootScript(t, &luaSkin, "t", `
local id = timer(0.03, function() command("fired one", function() end) end)
if type(id) ~= "string" or id == "" then error("timer id missing, got " .. type(id)) end
cancel(id)
timer(0.03, function() command("fired two", function() end) end)
`)
	if err != nil {
		t.Fatal(err)
	}
	if !waitFor(t, func() bool { return hasPhrase(s, "fired two") }) {
		t.Fatal("uncancelled timer never fired")
	}
	if hasPhrase(s, "fired one") {
		t.Fatal("cancelled timer fired")
	}
	// WAIT for the table to drain rather than reading it the instant the
	// phrase appears. `fire` runs the callback FIRST and only then takes the
	// lock to delete the entry (bridge.go), so the phrase this test waits on
	// is published BEFORE the cleanup it is about to assert. On an unloaded
	// machine the gap is invisible; on a busy CI runner the goroutine is
	// descheduled in between and the table still holds 1.
	//
	// Observed on ubuntu-latest 2026-09-21 while windows-latest passed in the
	// same run — the asymmetry that says "timing", not "logic". The assertion
	// is unchanged: the table must empty. Only the assumption about WHEN it
	// empties, relative to an unrelated side effect, is gone.
	drained := waitFor(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.timers) == 0
	})
	if !drained {
		s.mu.Lock()
		n := len(s.timers)
		s.mu.Unlock()
		t.Fatalf("fired/cancelled timers should leave the table empty, got %d", n)
	}
}

func TestLuaEveryRepeatsUntilClosed(t *testing.T) {
	s, err := bootScript(t, &luaSkin, "e", `
every(0.02, function() command("tick", function() end) end)
`)
	if err != nil {
		t.Fatal(err)
	}
	ticks := func() int {
		n := 0
		for _, p := range phrases(s) {
			if p == "tick" {
				n++
			}
		}
		return n
	}
	if !waitFor(t, func() bool { return ticks() >= 2 }) {
		t.Fatalf("repeating timer did not repeat: %d fires", ticks())
	}
	// Teardown semantics: closing stops the re-arm chain.
	s.mu.Lock()
	for _, tm := range s.timers {
		tm.Stop()
	}
	s.closed = true
	s.mu.Unlock()
	seen := ticks()
	time.Sleep(60 * time.Millisecond)
	if ticks() != seen {
		t.Fatalf("closed script kept ticking: %d -> %d", seen, ticks())
	}
}

// privilegeDemand is what decides both the inverse view and whether a
// pending request is stale, so it must read headers off DISK (a script
// that never ran still counts) and honor the bare-`call` wildcard.
func TestPrivilegeDemandFromHeaders(t *testing.T) {
	dir := t.TempDir()
	write := func(name, src string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.lua", "-- permissions: call:power, hud\n")
	write("b.js", "// permissions: call:power, call:windows\n")
	write("wild.ts", "// permissions: call\n")
	write("none.lua", "-- permissions: hud\n")

	h := &Host{dir: dir, scripts: map[string]*Script{}}
	demand := h.privilegeDemand([]string{"power", "windows", "clipboard"}, nil)

	if got := demand["power"]; len(got) != 3 || got[0] != "a.lua" || got[1] != "b.js" || got[2] != "wild.ts" {
		t.Fatalf("power demand wrong: %v", got)
	}
	if got := demand["windows"]; len(got) != 2 || got[0] != "b.js" || got[1] != "wild.ts" {
		t.Fatalf("windows demand wrong: %v", got)
	}
	// Bare `call` demands the whole surface, so nothing is ever stale
	// while such a script is enabled.
	if got := demand["clipboard"]; len(got) != 1 || got[0] != "wild.ts" {
		t.Fatalf("bare call should claim every known privilege: %v", got)
	}
	if _, present := demand["hud"]; present {
		t.Fatal("non-call grants must not appear as platform privilege demand")
	}
}
