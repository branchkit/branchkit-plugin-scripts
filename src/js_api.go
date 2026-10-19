package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/dop251/goja"
	esbuild "github.com/evanw/esbuild/pkg/api"
)

// The JavaScript and TypeScript skins: one Goja runtime per file, `//`
// comment headers. TypeScript rides the same runtime — esbuild (a pure-Go
// library) transpiles at load, so .ts gets the identical script globals and
// hot reload with no external toolchain. Bridge errors are thrown as JS
// exceptions so a refusal aborts the calling code like any other error.

var jsSkin = skin{
	ext:           ".js",
	lang:          "javascript",
	label:         "JavaScript",
	commentPrefix: "//",
	template:      jsTemplate,
	boot:          bootJS,
	lint:          lintJS,
}

var tsSkin = skin{
	ext:           ".ts",
	lang:          "typescript",
	label:         "TypeScript",
	commentPrefix: "//",
	template:      tsTemplate,
	boot:          bootTS,
	lint:          lintTS,
}

func bootJS(s *Script, path string) error {
	src, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return runJS(s, filepath.Base(path), string(src))
}

func bootTS(s *Script, path string) error {
	src, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	js, err := transpileTS(filepath.Base(path), string(src))
	if err != nil {
		return err
	}
	return runJS(s, filepath.Base(path), js)
}

// transpileTS lowers TypeScript to the JS the shared runtime executes.
// esbuild without minification keeps the line structure close to the
// source, so runtime error positions stay usable.
func transpileTS(name, src string) (string, error) {
	res := esbuild.Transform(src, esbuild.TransformOptions{
		Loader:     esbuild.LoaderTS,
		Target:     esbuild.ES2017,
		Sourcefile: name,
	})
	if len(res.Errors) > 0 {
		msgs := make([]string, 0, len(res.Errors))
		for _, e := range res.Errors {
			loc := ""
			if e.Location != nil {
				loc = fmt.Sprintf("%s:%d:%d: ", name, e.Location.Line, e.Location.Column)
			}
			msgs = append(msgs, loc+e.Text)
		}
		return "", fmt.Errorf("%s", joinLines(msgs))
	}
	return string(res.Code), nil
}

func joinLines(lines []string) string {
	out := ""
	for i, l := range lines {
		if i > 0 {
			out += "\n"
		}
		out += l
	}
	return out
}

func runJS(s *Script, chunkName, src string) error {
	rt := goja.New()
	s.closeVM = func() { rt.Interrupt("script unloaded") }

	// throw converts a bridge refusal into a JS exception; goja rethrows
	// a panicked *goja.Object as-is inside the script. Thrown as a plain
	// JS Error so the message reads clean in the row (NewGoError prefixes
	// every message with "GoError:").
	newError, _ := goja.AssertConstructor(rt.GlobalObject().Get("Error"))
	throw := func(err error) {
		if err == nil {
			return
		}
		if newError != nil {
			if o, cerr := newError(nil, rt.ToValue(err.Error())); cerr == nil {
				panic(o)
			}
		}
		panic(rt.ToValue(err.Error()))
	}
	callFn := func(fn goja.Callable, args ...goja.Value) error {
		_, err := fn(goja.Undefined(), args...)
		return err
	}
	mustFn := func(v goja.Value, what string) goja.Callable {
		fn, ok := goja.AssertFunction(v)
		if !ok {
			throw(fmt.Errorf("%s: expected a function", what))
		}
		return fn
	}

	set := func(name string, impl func(call goja.FunctionCall) goja.Value) {
		_ = rt.Set(name, impl)
	}
	set("command", func(call goja.FunctionCall) goja.Value {
		pattern := call.Argument(0).Export()
		fn := mustFn(call.Argument(1), "command")
		mode, enters := "", ""
		if len(call.Arguments) >= 3 {
			if opts, ok := call.Argument(2).Export().(map[string]any); ok {
				mode, _ = opts["mode"].(string)
				enters, _ = opts["enters"].(string)
			}
		}
		_, err := s.registerCommand(pattern, mode, enters, func() error { return callFn(fn) })
		throw(err)
		return goja.Undefined()
	})
	set("on", func(call goja.FunctionCall) goja.Value {
		pattern := call.Argument(0).String()
		fn := mustFn(call.Argument(1), "on")
		throw(s.registerEvent(pattern, func(payload any) error {
			return callFn(fn, rt.ToValue(payload))
		}))
		return goja.Undefined()
	})
	set("dispatch", func(call goja.FunctionCall) goja.Value {
		actionType := call.Argument(0).String()
		var params map[string]any
		if len(call.Arguments) >= 2 {
			params, _ = call.Argument(1).Export().(map[string]any)
		}
		result, err := s.bridgeDispatch(actionType, params)
		throw(err)
		return rt.ToValue(result)
	})
	set("failed", func(goja.FunctionCall) goja.Value {
		return rt.ToValue(s.bridgeFailed())
	})
	set("call", func(call goja.FunctionCall) goja.Value {
		method := call.Argument(0).String()
		var params map[string]any
		if len(call.Arguments) >= 2 {
			params, _ = call.Argument(1).Export().(map[string]any)
		}
		result, err := s.bridgeCall(method, params)
		throw(err)
		return rt.ToValue(result)
	})
	set("record", func(call goja.FunctionCall) goja.Value {
		result, err := s.bridgeRecord(call.Argument(0).String(), call.Argument(1).String())
		throw(err)
		return rt.ToValue(result)
	})
	set("records", func(call goja.FunctionCall) goja.Value {
		var opts map[string]any
		if len(call.Arguments) >= 2 {
			opts, _ = call.Argument(1).Export().(map[string]any)
		}
		result, err := s.bridgeRecords(call.Argument(0).String(), opts)
		throw(err)
		return rt.ToValue(result)
	})
	set("put", func(call goja.FunctionCall) goja.Value {
		payload, _ := call.Argument(2).Export().(map[string]any)
		throw(s.bridgePut(call.Argument(0).String(), call.Argument(1).String(), payload))
		return goja.Undefined()
	})
	set("drop", func(call goja.FunctionCall) goja.Value {
		throw(s.bridgeDrop(call.Argument(0).String(), call.Argument(1).String()))
		return goja.Undefined()
	})
	set("store", func(call goja.FunctionCall) goja.Value {
		var value any
		if len(call.Arguments) >= 2 && !goja.IsUndefined(call.Argument(1)) && !goja.IsNull(call.Argument(1)) {
			value = call.Argument(1).Export()
		}
		throw(s.bridgeStore(call.Argument(0).String(), value))
		return goja.Undefined()
	})
	set("stored", func(call goja.FunctionCall) goja.Value {
		result, err := s.bridgeStored(call.Argument(0).String())
		throw(err)
		return rt.ToValue(result)
	})
	set("hold", func(call goja.FunctionCall) goja.Value {
		result, err := s.bridgeHold(call.Argument(0).String())
		throw(err)
		return rt.ToValue(result)
	})
	set("release", func(call goja.FunctionCall) goja.Value {
		result, err := s.bridgeRelease(call.Argument(0).String())
		throw(err)
		return rt.ToValue(result)
	})
	set("holder", func(call goja.FunctionCall) goja.Value {
		result, err := s.bridgeHolder(call.Argument(0).String())
		throw(err)
		return rt.ToValue(result)
	})
	set("mode", func(call goja.FunctionCall) goja.Value {
		throw(s.bridgeMode(call.Argument(0).String()))
		return goja.Undefined()
	})
	// secretRef("name", "Bearer ") — see secret_ref in the Lua skin.
	set("secretRef", func(call goja.FunctionCall) goja.Value {
		prefix := ""
		if len(call.Arguments) >= 2 && !goja.IsUndefined(call.Argument(1)) {
			prefix = call.Argument(1).String()
		}
		return rt.ToValue(map[string]any{"__secret": call.Argument(0).String(), "prefix": prefix})
	})
	set("fetch", func(call goja.FunctionCall) goja.Value {
		var opts map[string]any
		if o, ok := call.Argument(1).Export().(map[string]any); ok {
			opts = o
		}
		result, err := s.bridgeFetch(call.Argument(0).String(), fetchOptionsFrom(opts))
		throw(err)
		return rt.ToValue(result)
	})
	set("clearMode", func(call goja.FunctionCall) goja.Value {
		s.bridgeClearMode()
		return goja.Undefined()
	})
	set("emit", func(call goja.FunctionCall) goja.Value {
		event := call.Argument(0).String()
		var payload map[string]any
		if len(call.Arguments) >= 2 {
			payload, _ = call.Argument(1).Export().(map[string]any)
		}
		throw(s.bridgeEmit(event, payload))
		return goja.Undefined()
	})
	set("choose", func(call goja.FunctionCall) goja.Value {
		options := toOptions(call.Argument(0).Export())
		fn := mustFn(call.Argument(1), "choose")
		throw(s.bridgeChoose(options, func(pick any) error {
			return callFn(fn, rt.ToValue(pick))
		}))
		return goja.Undefined()
	})
	set("hud", func(call goja.FunctionCall) goja.Value {
		throw(s.bridgeHud(call.Argument(0).String()))
		return goja.Undefined()
	})
	set("timer", func(call goja.FunctionCall) goja.Value {
		seconds := call.Argument(0).ToFloat()
		fn := mustFn(call.Argument(1), "timer")
		return rt.ToValue(s.bridgeTimer(seconds, false, func() error { return callFn(fn) }))
	})
	set("every", func(call goja.FunctionCall) goja.Value {
		seconds := call.Argument(0).ToFloat()
		fn := mustFn(call.Argument(1), "every")
		return rt.ToValue(s.bridgeTimer(seconds, true, func() error { return callFn(fn) }))
	})
	set("cancel", func(call goja.FunctionCall) goja.Value {
		s.bridgeCancel(call.Argument(0).String())
		return goja.Undefined()
	})
	set("log", func(call goja.FunctionCall) goja.Value {
		s.bridgeLog(call.Argument(0).String())
		return goja.Undefined()
	})

	_, err := rt.RunScript(chunkName, src)
	return err
}

func lintJS(name, src string) error {
	_, err := goja.Compile(name+".js", src, false)
	return err
}

func lintTS(name, src string) error {
	_, err := transpileTS(name+".ts", src)
	return err
}

const jsTemplate = `// New BranchKit script. Saved changes load instantly — no build, no restart.
//
// The permissions line declares what this script may call; undeclared
// dispatch/call/on/hud calls are refused, and the reason shows on this
// script's row in Settings.
// Grants: dispatch[:prefix], call[:privilege], events[:pattern],
//         emit[:event], mode[:name], effect[:name],
//         read[:collection], write[:collection], hud, choose.
// permissions: hud
//
// API (each call forwards through the Scripts plugin's grants):
//   command(pattern, fn, opts) voice command; pattern "note this" or ["volume", ["up","down"]]
//                             opts { mode: "review" } gates it on that mode,
//                             { enters: "review" } makes it the way in
//                             (say both halves in one breath)
//   on(event, fn)             platform event, e.g. "_platform.audio_devices.changed"
//   dispatch(action, params)  fire an action, e.g. dispatch("audio.volume_up"); params optional; returns its result
//   failed()                  why the last dispatch failed, or null (it fails soft)
//   emit(event, payload)      announce something as scripts.<this script>.<event>, for other scripts and plugins
//   record(coll, id)          read one entry, or null; entry.payload holds the data
//   records(coll, opts)       read entries; opts { limit: 50, writer: "scripts" }
//                             each entry is
//                             { id, payload, writer, revision, timestamp_ms }
//                             writer names the plugin that created it ("_user" = you)
//   put(coll, id, payload)    contribute a record to a collection that accepts contributions
//   drop(coll, id)            delete one you contributed
//   store(key, value)         save something private to this script (null deletes)
//   stored(key)               read it back
//   hold(effect)              hold a system effect, e.g. hold("suppress_notifications"); check .granted then .enforced
//                             hold("scripts.<name>") mints your own coordination name — no grant needed
//   release(effect)           give it back
//   holder(effect)            who holds it now
//   mode(name)                enter an exclusive mode; only this script's commands for it match
//   clearMode()               leave it (the escape command also works)
//   call(method, params)      call a platform method, e.g. call("native.frontmost_app") with call:apps
//   hud(html)                 push HTML to the scripts HUD window
//   timer(seconds, fn)        one-shot timer; returns an id for cancel(id)
//   every(seconds, fn)        repeating timer; returns an id for cancel(id)
//   cancel(id)                stop a timer
//   log(msg)                  plugin log line, tagged with this script's name

command("my command", () => {
  hud("<div class='bk-panel'><div class='bk-phrase'>it works</div></div>");
});
`

const tsTemplate = `// New BranchKit script. Saved changes load instantly — no build, no restart.
//
// The permissions line declares what this script may call; undeclared
// dispatch/call/on/hud calls are refused, and the reason shows on this
// script's row in Settings.
// Grants: dispatch[:prefix], call[:privilege], events[:pattern],
//         emit[:event], mode[:name], effect[:name],
//         read[:collection], write[:collection], hud, choose.
// permissions: hud
//
// API (each call forwards through the Scripts plugin's grants):
//   command(pattern, fn, opts) voice command; pattern "note this" or ["volume", ["up","down"]]
//                             opts { mode: "review" } gates it on that mode,
//                             { enters: "review" } makes it the way in
//                             (say both halves in one breath)
//   on(event, fn)             platform event, e.g. "_platform.audio_devices.changed"
//   dispatch(action, params)  fire an action, e.g. dispatch("audio.volume_up"); params optional; returns its result
//   failed()                  why the last dispatch failed, or null (it fails soft)
//   emit(event, payload)      announce something as scripts.<this script>.<event>, for other scripts and plugins
//   record(coll, id)          read one entry, or null; entry.payload holds the data
//   records(coll, opts)       read entries; opts { limit: 50, writer: "scripts" }
//                             each entry is
//                             { id, payload, writer, revision, timestamp_ms }
//                             writer names the plugin that created it ("_user" = you)
//   put(coll, id, payload)    contribute a record to a collection that accepts contributions
//   drop(coll, id)            delete one you contributed
//   store(key, value)         save something private to this script (null deletes)
//   stored(key)               read it back
//   hold(effect)              hold a system effect, e.g. hold("suppress_notifications"); check .granted then .enforced
//                             hold("scripts.<name>") mints your own coordination name — no grant needed
//   release(effect)           give it back
//   holder(effect)            who holds it now
//   mode(name)                enter an exclusive mode; only this script's commands for it match
//   clearMode()               leave it (the escape command also works)
//   call(method, params)      call a platform method, e.g. call("native.frontmost_app") with call:apps
//   hud(html)                 push HTML to the scripts HUD window
//   timer(seconds, fn)        one-shot timer; returns an id for cancel(id)
//   every(seconds, fn)        repeating timer; returns an id for cancel(id)
//   cancel(id)                stop a timer
//   log(msg)                  plugin log line, tagged with this script's name

declare function command(pattern: string | (string | string[])[], fn: () => void, opts?: { mode?: string; enters?: string }): void;
declare function on(event: string, fn: (payload: unknown) => void): void;
declare function dispatch(action: string, params?: Record<string, unknown>): unknown;
declare function failed(): string | null;
declare function emit(event: string, payload?: Record<string, unknown>): void;
interface Entry { id: string; payload: unknown; writer: string; revision: number; timestamp_ms: number; on_behalf_of?: string }
declare function record(collection: string, id: string): Entry | null;
declare function records(collection: string, opts?: { limit?: number; writer?: string }): Entry[];
declare function put(collection: string, id: string, payload: Record<string, unknown>): void;
declare function drop(collection: string, id: string): void;
declare function store(key: string, value?: unknown): void;
declare function stored(key: string): unknown;
declare function hold(effect: string): { granted: boolean; already_held: boolean; displaced: string; enforced: boolean };
declare function release(effect: string): { retracted: boolean; new_owner: string };
declare function holder(effect: string): { active: boolean; owner: string; mine: boolean };
declare function mode(name: string): void;
declare function clearMode(): void;
declare function call(method: string, params?: Record<string, unknown>): unknown;
declare function hud(html: string): void;
declare function timer(seconds: number, fn: () => void): string;
declare function every(seconds: number, fn: () => void): string;
declare function cancel(id: string): void;
declare function log(msg: string): void;

command("my command", (): void => {
  hud("<div class='bk-panel'><div class='bk-phrase'>it works</div></div>");
});
`
