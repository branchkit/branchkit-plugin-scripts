package main

import (
	"fmt"
	"strings"

	lua "github.com/yuin/gopher-lua"
	luaparse "github.com/yuin/gopher-lua/parse"
)

// The Lua skin: gopher-lua VM per file, `--` comment headers. Each global
// converts Lua values to the neutral forms and delegates to the bridge;
// bridge errors are raised as Lua errors so a refusal aborts the calling
// code the same way a syntax error would.

var luaSkin = skin{
	ext:           ".lua",
	lang:          "lua",
	label:         "Lua",
	commentPrefix: "--",
	template:      luaTemplate,
	boot:          bootLua,
	lint:          lintLua,
}

func bootLua(s *Script, path string) error {
	L := lua.NewState()
	s.closeVM = L.Close

	raise := func(err error) {
		if err != nil {
			L.RaiseError("%v", err)
		}
	}
	callFn := func(fn *lua.LFunction, args ...lua.LValue) error {
		return L.CallByParam(lua.P{Fn: fn, NRet: 0, Protect: true}, args...)
	}

	L.SetGlobal("command", L.NewFunction(func(L *lua.LState) int {
		pattern := luaToGo(L.CheckAny(1))
		fn := L.CheckFunction(2)
		mode, enters := "", ""
		if L.GetTop() >= 3 {
			if opts, ok := luaToGo(L.CheckTable(3)).(map[string]any); ok {
				mode, _ = opts["mode"].(string)
				enters, _ = opts["enters"].(string)
			}
		}
		_, err := s.registerCommand(pattern, mode, enters, func() error { return callFn(fn) })
		raise(err)
		return 0
	}))
	L.SetGlobal("on", L.NewFunction(func(L *lua.LState) int {
		pattern := L.CheckString(1)
		fn := L.CheckFunction(2)
		raise(s.registerEvent(pattern, func(payload any) error {
			return callFn(fn, goToLua(L, payload))
		}))
		return 0
	}))
	L.SetGlobal("dispatch", L.NewFunction(func(L *lua.LState) int {
		actionType := L.CheckString(1)
		var params map[string]any
		if L.GetTop() >= 2 {
			params, _ = luaToGo(L.CheckTable(2)).(map[string]any)
		}
		result, err := s.bridgeDispatch(actionType, params)
		raise(err)
		L.Push(goToLua(L, result))
		return 1
	}))
	L.SetGlobal("failed", L.NewFunction(func(L *lua.LState) int {
		L.Push(goToLua(L, s.bridgeFailed()))
		return 1
	}))
	L.SetGlobal("call", L.NewFunction(func(L *lua.LState) int {
		method := L.CheckString(1)
		var params map[string]any
		if L.GetTop() >= 2 {
			params, _ = luaToGo(L.CheckTable(2)).(map[string]any)
		}
		result, err := s.bridgeCall(method, params)
		raise(err)
		L.Push(goToLua(L, result))
		return 1
	}))
	L.SetGlobal("record", L.NewFunction(func(L *lua.LState) int {
		result, err := s.bridgeRecord(L.CheckString(1), L.CheckString(2))
		raise(err)
		L.Push(goToLua(L, result))
		return 1
	}))
	L.SetGlobal("records", L.NewFunction(func(L *lua.LState) int {
		var opts map[string]any
		if L.GetTop() >= 2 {
			opts, _ = luaToGo(L.CheckTable(2)).(map[string]any)
		}
		result, err := s.bridgeRecords(L.CheckString(1), opts)
		raise(err)
		L.Push(goToLua(L, result))
		return 1
	}))
	L.SetGlobal("put", L.NewFunction(func(L *lua.LState) int {
		payload, _ := luaToGo(L.CheckTable(3)).(map[string]any)
		raise(s.bridgePut(L.CheckString(1), L.CheckString(2), payload))
		return 0
	}))
	L.SetGlobal("drop", L.NewFunction(func(L *lua.LState) int {
		raise(s.bridgeDrop(L.CheckString(1), L.CheckString(2)))
		return 0
	}))
	L.SetGlobal("store", L.NewFunction(func(L *lua.LState) int {
		var value any
		if L.GetTop() >= 2 {
			value = luaToGo(L.CheckAny(2))
		}
		raise(s.bridgeStore(L.CheckString(1), value))
		return 0
	}))
	// secret_ref("name", "Bearer ") — a header value NAMING a secret this
	// file declared (`secret:name@host`). The value is substituted by the
	// platform at the declared host; the script never holds it.
	L.SetGlobal("secret_ref", L.NewFunction(func(L *lua.LState) int {
		t := L.NewTable()
		t.RawSetString("__secret", lua.LString(L.CheckString(1)))
		t.RawSetString("prefix", lua.LString(L.OptString(2, "")))
		L.Push(t)
		return 1
	}))
	L.SetGlobal("fetch", L.NewFunction(func(L *lua.LState) int {
		var opts map[string]any
		if L.GetTop() >= 2 {
			opts, _ = luaToGo(L.CheckTable(2)).(map[string]any)
		}
		result, err := s.bridgeFetch(L.CheckString(1), fetchOptionsFrom(opts))
		raise(err)
		L.Push(goToLua(L, result))
		return 1
	}))
	L.SetGlobal("stored", L.NewFunction(func(L *lua.LState) int {
		result, err := s.bridgeStored(L.CheckString(1))
		raise(err)
		L.Push(goToLua(L, result))
		return 1
	}))
	L.SetGlobal("hold", L.NewFunction(func(L *lua.LState) int {
		result, err := s.bridgeHold(L.CheckString(1))
		raise(err)
		L.Push(goToLua(L, result))
		return 1
	}))
	L.SetGlobal("release", L.NewFunction(func(L *lua.LState) int {
		result, err := s.bridgeRelease(L.CheckString(1))
		raise(err)
		L.Push(goToLua(L, result))
		return 1
	}))
	L.SetGlobal("holder", L.NewFunction(func(L *lua.LState) int {
		result, err := s.bridgeHolder(L.CheckString(1))
		raise(err)
		L.Push(goToLua(L, result))
		return 1
	}))
	L.SetGlobal("mode", L.NewFunction(func(L *lua.LState) int {
		raise(s.bridgeMode(L.CheckString(1)))
		return 0
	}))
	L.SetGlobal("clear_mode", L.NewFunction(func(L *lua.LState) int {
		s.bridgeClearMode()
		return 0
	}))
	L.SetGlobal("emit", L.NewFunction(func(L *lua.LState) int {
		event := L.CheckString(1)
		var payload map[string]any
		if L.GetTop() >= 2 {
			payload, _ = luaToGo(L.CheckTable(2)).(map[string]any)
		}
		raise(s.bridgeEmit(event, payload))
		return 0
	}))
	L.SetGlobal("choose", L.NewFunction(func(L *lua.LState) int {
		options := toOptions(luaToGo(L.CheckTable(1)))
		fn := L.CheckFunction(2)
		raise(s.bridgeChoose(options, func(pick any) error {
			return callFn(fn, goToLua(L, pick))
		}))
		return 0
	}))
	L.SetGlobal("hud", L.NewFunction(func(L *lua.LState) int {
		raise(s.bridgeHud(L.CheckString(1)))
		return 0
	}))
	L.SetGlobal("timer", L.NewFunction(func(L *lua.LState) int {
		seconds := float64(L.CheckNumber(1))
		fn := L.CheckFunction(2)
		L.Push(lua.LString(s.bridgeTimer(seconds, false, func() error { return callFn(fn) })))
		return 1
	}))
	L.SetGlobal("every", L.NewFunction(func(L *lua.LState) int {
		seconds := float64(L.CheckNumber(1))
		fn := L.CheckFunction(2)
		L.Push(lua.LString(s.bridgeTimer(seconds, true, func() error { return callFn(fn) })))
		return 1
	}))
	L.SetGlobal("cancel", L.NewFunction(func(L *lua.LState) int {
		s.bridgeCancel(L.CheckString(1))
		return 0
	}))
	L.SetGlobal("log", L.NewFunction(func(L *lua.LState) int {
		s.bridgeLog(L.CheckString(1))
		return 0
	}))

	return L.DoFile(path)
}

func lintLua(name, src string) error {
	_, err := luaparse.Parse(strings.NewReader(src), name+".lua")
	return err
}

const luaTemplate = `-- New BranchKit script. Saved changes load instantly — no build, no restart.
--
-- The permissions line declares what this script may call; undeclared
-- dispatch/call/on/hud calls are refused, and the reason shows on this
-- script's row in Settings.
-- Grants: dispatch[:prefix], call[:privilege], events[:pattern],
--         emit[:event], mode[:name], effect[:name],
--         read[:collection], write[:collection], hud, choose.
-- permissions: hud
--
-- API (each call forwards through the Scripts plugin's grants):
--   command(pattern, fn, opts) voice command; pattern "note this" or {"volume", {"up","down"}}
--                             opts { mode = "review" } gates it on that mode,
--                             { enters = "review" } makes it the way in
--                             (say both halves in one breath)
--   on(event, fn)             platform event, e.g. "_platform.audio_devices.changed"
--   dispatch(action, params)  fire an action, e.g. dispatch("audio.volume_up"); params optional; returns its result
--   failed()                  why the last dispatch failed, or nil (it fails soft)
--   emit(event, payload)      announce something as scripts.<this script>.<event>, for other scripts and plugins
--   record(coll, id)          read one entry, or nil; entry.payload holds the data
--   records(coll, opts)       read entries; opts { limit = 50, writer = "scripts" }
--                             each entry is
--                             { id, payload, writer, revision, timestamp_ms }
--                             writer names the plugin that created it ("_user" = you)
--   put(coll, id, payload)    contribute a record to a collection that accepts contributions
--   drop(coll, id)            delete one you contributed
--   store(key, value)         save something private to this script (nil deletes)
--   stored(key)               read it back
--   hold(effect)              hold a system effect, e.g. hold("suppress_notifications"); check .granted then .enforced
--                             hold("scripts.<name>") mints your own coordination name — no grant needed
--   release(effect)           give it back
--   holder(effect)            who holds it now
--   mode(name)                enter an exclusive mode; only this script's commands for it match
--   clear_mode()              leave it (the escape command also works)
--   call(method, params)      call a platform method, e.g. call("native.frontmost_app") with call:apps
--   hud(html)                 push HTML to the scripts HUD window
--   timer(seconds, fn)        one-shot timer; returns an id for cancel(id)
--   every(seconds, fn)        repeating timer; returns an id for cancel(id)
--   cancel(id)                stop a timer
--   log(msg)                  plugin log line, tagged with this script's name

command("my command", function()
  hud("<div class='bk-panel'><div class='bk-phrase'>it works</div></div>")
end)
`

// goToLua converts a JSON-decoded value into a Lua value for event payloads.
func goToLua(L *lua.LState, v any) lua.LValue {
	switch t := v.(type) {
	case nil:
		return lua.LNil
	case bool:
		return lua.LBool(t)
	case float64:
		return lua.LNumber(t)
	case string:
		return lua.LString(t)
	case []any:
		tbl := L.NewTable()
		for _, e := range t {
			tbl.Append(goToLua(L, e))
		}
		return tbl
	case map[string]any:
		tbl := L.NewTable()
		for k, e := range t {
			tbl.RawSetString(k, goToLua(L, e))
		}
		return tbl
	}
	return lua.LString(fmt.Sprint(v))
}

// luaToGo converts a Lua value into the neutral form (JSON-marshalable Go
// values). Tables with sequential integer keys become arrays, others maps.
func luaToGo(v lua.LValue) any {
	switch t := v.(type) {
	case lua.LBool:
		return bool(t)
	case lua.LNumber:
		return float64(t)
	case lua.LString:
		return string(t)
	case *lua.LTable:
		if n := t.MaxN(); n > 0 {
			arr := make([]any, 0, n)
			for i := 1; i <= n; i++ {
				arr = append(arr, luaToGo(t.RawGetInt(i)))
			}
			return arr
		}
		m := map[string]any{}
		t.ForEach(func(k, val lua.LValue) {
			m[lua.LVAsString(k)] = luaToGo(val)
		})
		return m
	}
	return nil
}
