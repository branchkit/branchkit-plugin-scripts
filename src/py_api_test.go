package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// pyTestRuntime stages the managed-runtime layout that pythonInterpreter()
// resolves, pointing at a system CPython — the shim is stdlib-only, so any
// serves. Skips when the machine has none. Teardown of the child rides each
// Script's closeVM.
//
// The layout is per-OS and so is the staging. Building only the unix shape
// made all six Python-skin tests fail on windows-latest with "runtime not
// installed": the resolver was right and the fixture was in the wrong place.
func pyTestRuntime(t *testing.T) {
	t.Helper()
	sys, err := exec.LookPath("python3")
	if err != nil {
		unstageable(t, "no python3 on PATH")
	}
	root := t.TempDir()

	if runtime.GOOS == "windows" {
		stageWindowsRuntime(t, root, sys)
	} else {
		bin := filepath.Join(root, "python", "bin")
		if err := os.MkdirAll(bin, 0o755); err != nil {
			t.Fatal(err)
		}
		// Unix CPython resolves its prefix through the symlink, so linking
		// the one file is enough.
		if err := os.Symlink(sys, filepath.Join(bin, "python3")); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("BRANCHKIT_RUNTIMES_DIR", root)
}

// unstageable ends the test when the fixture cannot put a usable interpreter
// where the resolver looks — skipping locally, FAILING under CI.
//
// The two contexts mean opposite things by the same condition. On a
// contributor's machine a missing or unstageable python3 is an absent
// optional toolchain, and failing their run over it would be rude. On CI the
// runner image guarantees Python, so the only way staging fails is that the
// fixture broke — and a skip there silently retires the coverage while the
// tick stays green, which is exactly how these six tests spent their whole
// life failing on Windows without anyone noticing.
func unstageable(t *testing.T, format string, args ...any) {
	t.Helper()
	// GitHub Actions sets CI=true; so does essentially every other runner.
	if os.Getenv("CI") != "" {
		t.Fatalf("python fixture could not be staged under CI: "+format, args...)
	}
	t.Skipf(format, args...)
}

// stageWindowsRuntime points <root>/python at the REAL install rather than
// faking one.
//
// Windows CPython is not relocatable: python.exe needs its python3XX.dll and
// Lib/ beside it, so copying the single file into a temp tree yields an
// interpreter that cannot start. A directory junction gives the real thing,
// and unlike a symlink `mklink /J` needs no elevation — which a CI runner
// does not have.
func stageWindowsRuntime(t *testing.T, root, sys string) {
	t.Helper()
	// Ask the interpreter where it lives rather than deriving it from the
	// resolved path: `python3` on PATH is often the Microsoft Store
	// app-execution alias, a reparse stub that is not the installation.
	out, err := exec.Command(sys, "-c", "import sys; print(sys.prefix)").Output()
	if err != nil {
		unstageable(t, "cannot read python's sys.prefix (Store alias?): %v", err)
	}
	prefix := strings.TrimSpace(string(out))
	if prefix == "" {
		unstageable(t, "python reported an empty sys.prefix")
	}

	link := filepath.Join(root, "python")
	// Registered AFTER t.TempDir's own cleanup, so it runs BEFORE it (LIFO):
	// the junction must be gone before RemoveAll walks root, or a cleanup
	// that followed the reparse point would delete the machine's Python.
	t.Cleanup(func() { _ = os.Remove(link) })
	if err := exec.Command("cmd", "/c", "mklink", "/J", link, prefix).Run(); err != nil {
		unstageable(t, "could not junction %s -> %s: %v", link, prefix, err)
	}
	if _, err := os.Stat(filepath.Join(link, "python.exe")); err != nil {
		unstageable(t, "junctioned prefix has no python.exe: %v", err)
	}
}

// closePy tears the child down the way teardown() would — without it the
// test process would accumulate live interpreters.
func closePy(s *Script) {
	s.mu.Lock()
	s.closed = true
	cv := s.closeVM
	s.mu.Unlock()
	if cv != nil {
		cv()
	}
}

func TestPyScriptRegistersCommandsAndEvents(t *testing.T) {
	pyTestRuntime(t)
	s, err := bootScript(t, &pySkin, "test", `# permissions: events
def noop():
    pass

command("note this", noop)
command(["volume", ["up", "down"]], noop)
on("_platform.audio_devices.changed", lambda ev: None)
`)
	defer closePy(s)
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

func TestPyUndeclaredCallsAreRefused(t *testing.T) {
	pyTestRuntime(t)
	for _, src := range []string{
		`dispatch("system.set_volume", {"level": 30})`,
		`on("_platform.app.focused", lambda ev: None)`,
		`hud("<div/>")`,
	} {
		s, err := bootScript(t, &pySkin, "locked", src)
		if err == nil {
			closePy(s)
			t.Fatalf("want refusal for %s", src)
		}
		if !strings.Contains(err.Error(), "permissions") {
			closePy(s)
			t.Fatalf("refusal should teach the fix, got: %v", err)
		}
		if len(s.events) != 0 {
			closePy(s)
			t.Fatal("refused on() must not record a subscription")
		}
		closePy(s)
	}
	// The teaching message uses the skin's comment marker.
	s, err := bootScript(t, &pySkin, "locked", `hud("x")`)
	defer closePy(s)
	if err == nil || !strings.Contains(err.Error(), "`# permissions: hud`") {
		t.Fatalf("Python refusal should name the # header form, got: %v", err)
	}
}

// A runtime refusal (undeclared dispatch from inside a handler) errors that
// invocation over the wire without killing the child — the next invocation
// still works.
func TestPyRuntimeRefusalAndRecovery(t *testing.T) {
	pyTestRuntime(t)
	s, err := bootScript(t, &pySkin, "rt", `count = [0]

def escalate():
    dispatch("system.set_volume", {"level": 30})

def benign():
    count[0] += 1
    log("ran %d" % count[0])

command("escalate", escalate)
command("benign", benign)
`)
	defer closePy(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.handlers["h1"](); err == nil || !strings.Contains(err.Error(), "dispatch:system") {
		t.Fatalf("undeclared dispatch inside a handler should refuse with the teaching line, got: %v", err)
	}
	if err := s.handlers["h2"](); err != nil {
		t.Fatalf("child should keep serving after a refusal: %v", err)
	}
}

func TestPyEventPayloadRoundtrip(t *testing.T) {
	pyTestRuntime(t)
	s, err := bootScript(t, &pySkin, "ev", `# permissions: events
def check(ev):
    if ev["kind"] != "added" or ev["names"][1] != "b":
        raise ValueError("payload shape wrong: %r" % (ev,))

on("_platform.audio_devices.changed", check)
`)
	defer closePy(s)
	if err != nil {
		t.Fatal(err)
	}
	fn := s.events["_platform.audio_devices.changed"][0]
	if err := fn(map[string]any{"kind": "added", "names": []any{"a", "b"}}); err != nil {
		t.Fatal(err)
	}
	if err := fn(map[string]any{"kind": "removed"}); err == nil {
		t.Fatal("child-side exception should surface as the invocation's error")
	}
}

// A handler error carries the script line, and the child keeps serving.
func TestPyHandlerErrorNamesTheLine(t *testing.T) {
	pyTestRuntime(t)
	s, err := bootScript(t, &pySkin, "boom", `def blow_up():
    raise RuntimeError("kaboom")

command("blow up", blow_up)
`)
	defer closePy(s)
	if err != nil {
		t.Fatal(err)
	}
	err = s.handlers["h1"]()
	if err == nil || !strings.Contains(err.Error(), "kaboom") || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("handler error should carry message and line, got: %v", err)
	}
}

func TestLintPy(t *testing.T) {
	pyTestRuntime(t)
	if err := lintPy("x", "def ok():\n    pass\n"); err != nil {
		t.Fatalf("valid Python flagged: %v", err)
	}
	err := lintPy("x", "def broken(:\n")
	if err == nil {
		t.Fatal("broken Python not flagged")
	}
	if !strings.Contains(err.Error(), "SyntaxError") {
		t.Fatalf("lint verdict should carry the SyntaxError, got: %v", err)
	}
}

// No runtime on disk: load and lint both say so, naming the install
// command — a row error, never a sandbox violation.
func TestPyMissingRuntime(t *testing.T) {
	t.Setenv("BRANCHKIT_RUNTIMES_DIR", t.TempDir())
	if err := lintPy("x", "pass"); err == nil || !strings.Contains(err.Error(), "runtime install python") {
		t.Fatalf("lint without a runtime should name the install command, got: %v", err)
	}
	s, err := bootScript(t, &pySkin, "norun", `log("hi")`)
	_ = s
	if err == nil || !strings.Contains(err.Error(), "runtime install python") {
		t.Fatalf("boot without a runtime should name the install command, got: %v", err)
	}
}
