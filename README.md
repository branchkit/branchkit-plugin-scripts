# BranchKit Scripts Plugin

Host for user scripts — the rung between authoring a command in the settings
UI and writing a full plugin. Personal glue ("when headphones connect, set
volume to 30%") lives here.

## How it works

Drop a file in the plugin's data dir under `scripts/`
(`…/BranchKit*/plugin-data/scripts/scripts/`). The host watches the folder and
hot-reloads on save — no manifest, no build, no restart.

Four languages, told apart by extension:

| Extension | Runtime |
|---|---|
| `.lua` | gopher-lua, in-process |
| `.js` | Goja, in-process |
| `.ts` | esbuild lowers to JS, then Goja |
| `.py` | a supervised CPython subprocess, one per file |

The embedded skins run inside this plugin's process; the Python skin runs as a
child inside this plugin's sandbox, on the managed CPython runtime (`branchkit-cli
runtime install python`). Either way the platform sees exactly one plugin, and
**a script's capability ceiling IS this plugin's manifest**.

The language is a skin: the bridged functions are the invariant, and each skin
supplies only the boot, a lint parser, and its comment syntax. `just
check-skin-parity` in the app repo fails when a function is missing from any
skin.

## The permissions header

A comment line near the top of the file declares what the script may do:

```lua
-- permissions: dispatch:audio, call:apps, emit, mode, hud
```

Parsed textually before any code runs — a comment cannot be computed or
reassigned by the code it governs, and enforcement has to exist before
top-level statements execute. Use the file's own comment marker: `--` in Lua,
`//` in JS/TS, `#` in Python.

**Undeclared means refused.** A missing grant raises in-language, with a
message naming the exact line to paste. That is what makes the header worth
anything: a script pasted from a forum does not get to opt out by omitting it.

| Grant | Allows |
|---|---|
| `dispatch` / `dispatch:<prefix>` | `dispatch("<prefix>.…", …)` |
| `call` / `call:<privilege>` | `call("<method>", …)` for methods in that privilege bucket |
| `events` / `events:<pattern>` | `on("<pattern>", …)` |
| `emit` / `emit:<event>` | `emit("<event>", …)` |
| `mode` / `mode:<name>` | `mode("<name>")` |
| `effect` / `effect:<name>` | `hold("<name>")` |
| `read` / `read:<collection>` | `record` / `records` |
| `write` / `write:<collection>` | `put` / `drop` |
| `choose` | `choose(…)` |
| `hud` | `hud(…)` |
| `fetch:<host>` | `fetch("https://<host>/…")` — see the carve-out below |
| `secret:<name>@<host>` | `secret_ref("<name>")` in a `fetch` header — see "Keeping an API key" |

`command`, `timer`, `every`, `cancel`, `log`, `store`, `stored`,
`clear_mode`, `release`, `holder` and `failed` are ungated. They are inert alone — a command
handler still needs a grant to do anything — and letting go of a mode or an
effect must never be the call that gets refused.

**This is hygiene, not a sandbox — with one exception.** The platform bounds
the plugin; the plugin polices its scripts. Nothing in a header changes what
the Scripts plugin as a whole may do.

**`fetch` is the exception, and it is real enforcement.** Every other
capability is bounded by the platform underneath it, so a header that lies
still cannot exceed what the plugin may do. Network is the one axis where the
platform's boundary is *coarser* than the per-script claim: host grants are
per plugin, so the platform sees "Scripts may reach api.example.com" and
cannot see which script asked. The narrowing holds here or not at all. It is
defensible because no skin has another way out — Lua and JS have no network
primitive, and Python's raw sockets are refused by the sandbox.

## Script API

Shown in Lua. JS and TypeScript are identical except `clear_mode` is
`clearMode` and options are an object literal; Python takes the same options
as keyword arguments.

### Commands and events

```lua
-- A voice command. Pattern is a string, or a list of slots where a nested
-- list is an alternatives group: { "volume", {"up", "down"} }.
command("note this", function()
  hud("<div>" .. os.date() .. ": noted</div>")
end)

-- A platform event. Must fall within the patterns this plugin's manifest
-- consumes; anything outside is flagged on the script's settings row.
on("_platform.audio_devices.changed", function(ev)
  if ev.kind == "added" and ev.name and ev.name:find("AirPods") then
    dispatch("audio.volume_down")
  end
end)
```

### Acting

```lua
dispatch("audio.volume_down")                  -- fire an action; returns its result
failed()                                        -- why the last dispatch failed, or nil
call("native.frontmost_app")                    -- call a platform method; returns its result
hud("<div>hello</div>")                         -- push HTML to the scripts HUD window
log("message")                                  -- plugin log, tagged with this script's name
```

### Reaching the network

```lua
-- permissions: fetch:api.open-meteo.com

local res = fetch("https://api.open-meteo.com/v1/forecast?latitude=45.5&longitude=-122.7&current=temperature_2m")
if res.status == 200 then
  log("it is " .. res.json.current.temperature_2m)
end

-- Options: method, headers, body, timeout (seconds).
-- A table or dict body is JSON-encoded and sets the content type.
fetch("https://notes.example.com/capture", {
  method = "POST",
  body = { text = "note to self" },
  headers = { ["X-Note"] = "1" },
})
```

Returns `status`, `body`, `headers`, and `json` — the last present only when
the response both declares a JSON content type and parses, so reading it gives
a value or nil and never a half-decoded surprise.

Two failures, deliberately different. A host you did not declare, a
non-http(s) URL, a timeout or a dead connection **raise** in your language —
the first of those names the exact line to paste. Any HTTP status is a normal
return, so a 500 is `res.status`, not an exception.

**Redirects are not followed.** A 302 from a declared host to an undeclared
one would walk straight around your grant, and the platform could not catch it
either, since its allowlist is the union across every script. Read `status`
and the `location` header and call `fetch` again if you want to follow one —
that puts the new host through the same check.

Responses are capped at 5 MiB and requests default to a 5 second timeout (30
maximum).

**Every host starts switched off.** When a script loads, the Scripts plugin
asks for each `fetch:` host it names; each appears on the Scripts plugin page
(Settings → Plugins → Scripts) with its switch off until you allow it. A
wildcard like `fetch:*.example.com` is asked for one concrete host at a time,
the first time a fetch reaches it — until you allow it, that fetch fails with a
message naming the switch.

### Keeping an API key

Never paste a key into a script. Declare it, and the script only ever holds
its NAME:

```lua
-- permissions: fetch:api.openweathermap.org, secret:key@api.openweathermap.org

local res = fetch("https://api.openweathermap.org/data/2.5/weather?q=Portland", {
  headers = { Authorization = secret_ref("key", "Bearer ") },
})
```

- `secret:<name>@<host>` puts a **Credentials** row on the Scripts plugin page:
  "probe.lua — key, sent only to api.openweathermap.org". You paste the value
  there. It goes straight into BranchKit's encrypted store; neither the script
  nor the Scripts plugin ever sees it.
- `secret_ref("<name>", "<prefix>")` (`secretRef` in JS/TS) is a header value
  naming that secret. BranchKit substitutes it and performs the request itself,
  in a sandboxed helper — only over https, only to the host the line names,
  never following a redirect. If the server echoes the key back, the response
  your script sees says `[redacted]`.
- Each script's secrets are its own: a secret is stored under the script's
  file name, so another script cannot use it by naming it.

`hold()` returns `granted` as well as `enforced`, and they answer different
questions. `granted: false` means the user has turned this effect off for the
scripts host on the Plugins page — nothing is wrong, and your script should
carry on without it. `enforced: false` means you own the effect but this
build does not yet apply the behaviour. Check `granted` first.

`dispatch` fails soft (nil result, error on the row) because dispatch callers
overwhelmingly do not check. `call` fails hard, because a read feeds control
flow and a silently-nil read is a wrong branch rather than a missed side
effect.

Soft does not mean unaskable. Most actions produce no result, so a nil from
`dispatch` means "no value" far more often than "did not run" — and the two
are indistinguishable without asking. `failed()` asks, about the most recent
`dispatch` only:

```lua
dispatch("placement.to_screen", { screen = "left" })
if failed() then
  log("no screen that way: " .. failed())
end
```

`call` has no counterpart and needs none: it raises, so a failure never
returns at all.

### Minting your own coordination name

`hold("scripts.<name>")` asserts a name the scripts namespace mints at
runtime — no registry entry, no manifest edit, no grant, no `effect` header
line. It is SignalOnly: the platform tracks who holds it and announces
ownership changes, and implements nothing else. Useful when a PLUGIN needs to
contend or coordinate with your script over one name; two scripts
coordinating with each other should reach for `store` and `emit` instead —
the ownership stack is per plugin, so sibling scripts collapse to one holder
and the stack adds nothing the host is not already doing.

### Announcing

```lua
emit("charged", { level = 42 })   -- becomes scripts.<this script>.charged
```

Other scripts and plugins can subscribe. The event's `source` on the wire is
`scripts` — the platform authenticates the plugin, not the file — and this
script's name travels in `on_behalf_of`. A script never receives its own
emits.

### Modes

An exclusive mode narrows the whole spoken surface to the commands gated on
it, for as long as it is held.

```lua
command("enter review", function() log("in review") end, { enters = "review" })
command("mark it", function() log("marked") end, { mode = "review" })
```

`{enters}` is what lets both halves be said in one breath — "enter review
mark it" — because the platform applies the tag while matching the rest of
the utterance. `mode("review")` and `clear_mode()` do the same thing from a
handler, a timer or an event, but a handler runs after matching, so a mode
entered that way only affects the *next* utterance.

Two exits, so nothing traps you: the global escape command clears it, and so
does editing, disabling or deleting the script.

While a mode is held, ungated commands are suppressed. That includes the
entry command for a *different* mode — to switch, gate the switching command
on the mode you are leaving: `{ mode = "review", enters = "editing" }`.
Exclusive gates union rather than conflict, so if another script or plugin
also holds one, both command sets are live.

### Collections

```lua
local apps = records("apps")             -- every entry
local one  = record("apps", "safari")    -- one entry, or nil
put("_platform.bindings", "mine", { … }) -- contribute a record
drop("_platform.bindings", "mine")       -- remove one
```

Both reads return entries, not bare payloads:

```lua
{ id = "safari", payload = { … }, writer = "system",
  revision = 2, timestamp_ms = 1787537629348 }
```

`writer` names the plugin that created the record — `_user` means the person
added it through Settings — which is how you avoid clobbering something you
do not own. `revision` and `timestamp_ms` are `0` on collections whose
storage keeps no per-record envelope.

Reachable collections are the ones this plugin declares in
`consumes.collections`; widening that set is a manifest edit. The plugin's own
`plugin.scripts.*` collections are not reachable at all — use `store` for
private data.

### Private storage

```lua
store("count", { runs = 3 })   -- nil value deletes
local back = stored("count")
```

Keys are namespaced per script, so two scripts cannot read or overwrite each
other's. Records are deleted with the script.

### Effects

```lua
local r = hold("suppress_notifications")
-- r.granted, r.already_held, r.displaced, r.enforced
release("suppress_notifications")
holder("suppress_notifications")   -- { active, owner, mine }
```

**Check `enforced`.** An effect whose OS handler has not landed still grants
ownership, displacement events and queryability — but the system behaviour
does not happen.

Several scripts may hold one effect; the plugin's frame is released when the
last one lets go. Teardown releases what a script held, so a script edited
away mid-hold cannot leave Do Not Disturb on with nothing left to turn it off.

### Asking the user

```lua
-- permissions: choose
choose({"red", "green", "blue"}, function(pick)
  if pick == nil then
    log("nobody answered")
  else
    log("picked " .. pick)
  end
end)
```

Each option gets a spoken codeword, and the HUD shows both — `[harp] red`.
Say the codeword; the callback receives the option *you* listed, not the
codeword.

A chooser holds an **exclusive** gate: while it is open the options are the
only thing that matches, so an unrelated command can't fire while the user
is mid-answer. That is why it has its own grant rather than riding on `hud`
— it takes the spoken surface, which showing HTML does not.

Three ways it ends, and the script hears about all of them: an answer, the
global escape command, or a 45-second timeout. The last two deliver `nil`.
`nil` is a real answer — "the user declined" is information, and a callback
that silently never fires leaves a script waiting forever with no way to
know why.

One chooser at a time, across all scripts. The gate and the option list are
plugin-wide, so a second one would replace the first's options underneath
its gate — the user would see one question and answer another. A second
call is refused and names the script holding it.

### Timers

```lua
local t = timer(5, function() log("five seconds later") end)  -- one-shot
local e = every(30, function() log("every thirty") end)       -- repeating
cancel(t)
```

A repeating timer re-arms only after its callback returns, so a slow callback
cannot overlap itself. Reload cancels everything.

## What this host does not offer

There is no bridged function for shell execution. That is a statement about
this host, not about scripts: it becomes reachable if a plugin exposes it as an
action type and you grant this host dispatch to that plugin. The Scripts plugin's own refusals are defaults, not boundaries — the
platform's sandbox and per-method privilege checks are the boundaries.

## License

MIT. See [LICENSE](LICENSE).
