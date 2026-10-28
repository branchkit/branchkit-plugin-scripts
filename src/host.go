package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/branchkit/plugin-sdk-go"
)

// Host owns the script directory: one script instance per file — an
// embedded VM, or a supervised subprocess for Python — loaded at start and
// hot-reloaded on save. Scripts never talk to the platform — every bridged
// function forwards to an SDK call the host makes as itself.
//
// The language is a skin: the bridged functions are the invariant, and
// each skin supplies only the boot, a lint parser, and its comment
// syntax. Everything else — grants, state records, rows, the editor — is
// language-neutral. Dispatch is by file extension, the one selector every
// editor and human already understands.
type Host struct {
	plugin *branchkit.Plugin
	dir    string
	// settings mirrors plugin.scripts.config; the Editor row reads it.
	settings *branchkit.SettingsMirror[Config]
	// installedEditors' one-minute cache.
	editorsMu   sync.Mutex
	editorsAt   time.Time
	editorsList [][2]string // {bundle id, label}

	// known is the manifest's consumed-event patterns, fixed before Start;
	// on() warns when a script subscribes outside their reach.
	known []string

	// declared is the manifest's consumed-collection set, fixed before
	// Start. The host lends scripts only reach it asked the user for; a
	// collection outside this set needs a manifest edit, which is the
	// static-manifest tradeoff the consumed-event set already carries.
	declared map[string]bool

	// lifeMu serializes lifecycle transitions (load/stop/unload) across
	// their entry points — watcher scan, settings handlers, and the
	// collection reconcile — which otherwise race: two concurrent loads
	// both publish a VM and the loser leaks with its timers live.
	lifeMu sync.Mutex

	mu      sync.RWMutex
	scripts map[string]*Script // key: filename without extension

	// privWaits records which scripts' call() calls were refused for a
	// platform privilege the host hasn't been granted — the requests filed
	// via privileges.request. When _platform.privilege.granted arrives for
	// one, exactly those scripts reload and resume.
	reqMu     sync.Mutex
	privWaits map[string]map[string]bool // privilege -> script names

	// chooseMu guards the single open chooser. Plugin-wide, not per-script:
	// the choice collection and its exclusive gate are one apiece, so two
	// choosers cannot coexist without the second silently replacing the
	// first's options underneath its gate.
	chooseMu sync.Mutex
	chooser  *openChooser

	// effMu guards effects: which scripts want each effect the host holds.
	//
	// A SET per effect, not a single holder, because the platform's
	// ownership stack has one frame per PLUGIN. Two scripts asserting the
	// same effect are one holder as far as it is concerned, so without a
	// refcount here the second script's assert is an idempotent no-op and
	// the FIRST script's release would silently pull the frame out from
	// under it. The host asserts on the empty→held transition and retracts
	// only when the last script lets go — the same rule the platform itself
	// applies across plugins, one rung down.
	//
	// Bookkeeping, not enforcement: the platform grants at plugin
	// granularity and this changes nothing about that.
	effMu   sync.Mutex
	effects map[string]map[string]bool // effect name -> scripts holding it
}

// skin is one hosted language. boot creates the VM (or spawns the
// supervising subprocess), installs the bridged globals as closures on
// the Script, and runs the file; lint syntax-checks a buffer with the same
// parser boot will use.
type skin struct {
	ext           string // ".lua"
	lang          string // new_script selector: "lua"
	label         string // display name: "Lua"
	commentPrefix string // line-comment marker the permissions header uses
	template      string // what new_script writes
	boot          func(s *Script, path string) error
	lint          func(name, src string) error
}

// skins in dispatch order. Extensions are unique; names are not namespaced
// by language (foo.lua and foo.js would collide on the script name), so
// scanOnce keeps the first extension it sees and flags the duplicate.
var skins = []*skin{&luaSkin, &jsSkin, &tsSkin, &pySkin}

func skinForExt(ext string) *skin {
	for _, sk := range skins {
		if sk.ext == ext {
			return sk
		}
	}
	return nil
}

func skinForLang(lang string) *skin {
	for _, sk := range skins {
		if sk.lang == lang {
			return sk
		}
	}
	return nil
}

// findScript resolves a script name to its on-disk file, trying each
// skin's extension in dispatch order.
func (h *Host) findScript(name string) (string, *skin, bool) {
	for _, sk := range skins {
		p := filepath.Join(h.dir, name+sk.ext)
		if _, err := os.Stat(p); err == nil {
			return p, sk, true
		}
	}
	return "", nil, false
}

// Script is one loaded file: its VM (behind closures), its registered
// handlers, and its status. VMs are not thread-safe; mu serializes every
// entry (action handlers, event fan-in, timers).
type Script struct {
	name string
	skin *skin
	host *Host

	// grants is parsed from the file's comment header before the VM runs
	// and is immutable afterwards — readable without mu.
	grants grants

	mu       sync.Mutex
	closed   bool
	closeVM  func()
	handlers map[string]func() error // handler id -> command callback
	nextID   int
	events   map[string][]func(payload any) error // event pattern -> callbacks
	// worldAsked: this load already asked for events.world (askForWorldEvents).
	worldAsked bool
	timers     map[string]*time.Timer // timer id -> live timer; cancel() and fired one-shots remove
	commands   []branchkit.CommandSpec
	infos      []cmdInfo // settings-tab display metadata, parallel to handlers
	lastErr    string
	// dispatchErr is why the most recent dispatch() soft-failed, read back
	// by failed(). Separate from lastErr, which is the SETTINGS ROW's error
	// and is written by load failures, refusals and every other bridged
	// call — a script asking "did my dispatch work" must not be answered
	// with someone else's problem.
	dispatchErr string
}

// file is the script's name as it appears on disk. This is the actor label
// the host stamps on every platform call it makes for this script, and the
// name that shows up in the audit log, on its records, and on its emits.
// WITH the extension deliberately: 'headphones.lua' reads as a file, bare
// 'headphones' reads as a mystery label.
func (s *Script) file() string {
	return s.name + s.skin.ext
}

// cmdInfo is what the settings tab shows for one registered command: the
// spoken phrase and, when the pattern has no capture tokens, a handler id
// the Run button can invoke directly.
type cmdInfo struct {
	phrase   string
	handler  string
	runnable bool
	// mode is the script's mode name this command is gated on, "" when it
	// matches in free context. Shown on the row so "why doesn't this
	// command work" has a visible answer.
	mode string
	// enters is the mode this command puts the script INTO, "" for most.
	// Distinct from mode: a command can require one and enter another.
	enters string
}

// --- Per-script permission header --------------------------------------------

// grants is a script's declared permission set, parsed from the leading
// comment block before the VM runs the file. The header is a comment, not
// code, deliberately: code could compute or reassign its own declaration,
// and enforcement must exist before top-level statements execute.
//
// This is the host policing its scripts (hygiene against a pasted script
// doing more than it claims), never a sandbox — the platform bounds the
// host; nothing here changes what the scripts plugin as a whole may do.
type grants struct {
	// `declared` used to sit here — whether a permissions line existed at all,
	// "for the settings row". The settings row never showed it.
	caps    map[string]bool
	unknown []string // tokens with an unrecognized capability name
}

// allows reports whether the named capability with qualifier arg is
// granted — either the bare capability ("dispatch") or the exact
// qualified form ("dispatch:system").
func (g grants) allows(name, arg string) bool {
	if g.caps[name] {
		return true
	}
	return arg != "" && g.caps[name+":"+arg]
}

// list returns the declared tokens, sorted, for the settings row.
func (g grants) list() []string {
	out := make([]string, 0, len(g.caps))
	for c := range g.caps {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// parsePermissions scans a script's leading comment block (line comments
// and blank lines; the first code line ends it) for `permissions: a, b`
// lines behind the skin's comment marker (`--` in Lua, `//` in JS/TS).
// Multiple lines accumulate. Tokens whose capability name isn't one the
// host gates are collected as unknown — a typo that silently never
// matched would read as a broken gate.
func parsePermissions(src, commentPrefix string) grants {
	g := grants{caps: map[string]bool{}}
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if !strings.HasPrefix(trimmed, commentPrefix) {
			break
		}
		body := strings.TrimSpace(strings.TrimPrefix(trimmed, commentPrefix))
		if !strings.HasPrefix(body, "permissions:") {
			continue
		}
		rest := strings.TrimPrefix(body, "permissions:")
		for _, tok := range strings.FieldsFunc(rest, func(r rune) bool {
			return r == ',' || r == ' ' || r == '\t'
		}) {
			name, _, _ := strings.Cut(tok, ":")
			switch name {
			case "dispatch", "events", "hud", "call", "emit", "mode", "effect", "read", "write", "choose", "fetch", "secret":
				g.caps[tok] = true
			default:
				g.unknown = append(g.unknown, tok)
			}
		}
	}
	return g
}

func NewHost(p *branchkit.Plugin) *Host {
	h := &Host{
		plugin:    p,
		dir:       filepath.Join(branchkit.PluginDataDir(), "scripts"),
		settings:  branchkit.Settings[Config](p, settingsCollection),
		scripts:   make(map[string]*Script),
		privWaits: map[string]map[string]bool{},
		effects:   map[string]map[string]bool{},
	}
	// The state collection is the source of truth for which scripts run.
	// The settings toggle converges synchronously, but any other writer
	// (Collections tab, overrides) reaches the VMs through this path.
	p.Subscribe(stateCollection, func(branchkit.CollectionUpdatedEventParams) { h.reconcile() })
	return h
}

// --- Per-script host state ---------------------------------------------------

// stateCollection holds one record per script file: whether it runs and
// the script's last error. Writes here are also what re-render the settings
// tab (its dependency set includes every provided collection), which is how
// a runtime VM death reaches the row without a page reload.
const stateCollection = "plugin.scripts.scripts"

// pluginID is this plugin's own id, as the platform knows it — what a
// displacement payload names when the frame that was lost was ours.
const pluginID = "scripts"

type scriptState struct {
	Script    string `json:"script"`
	Enabled   bool   `json:"enabled"`
	LastError string `json:"last_error,omitempty"`
}

// getState reads one script's record. ok=false means no record exists yet
// (a file the host hasn't loaded before) — enabled, no error.
func (h *Host) getState(name string) (scriptState, bool) {
	st := scriptState{Script: name, Enabled: true}
	rec, err := h.plugin.Get(stateCollection, name)
	if err != nil || rec == nil {
		return st, false
	}
	if json.Unmarshal(rec.Payload, &st) != nil {
		return scriptState{Script: name, Enabled: true}, false
	}
	return st, true
}

// listStates reads every script record, keyed by script name.
func (h *Host) listStates() map[string]scriptState {
	recs, err := h.plugin.ListAll(stateCollection)
	if err != nil {
		branchkit.Logf("scripts", "state list: %v", err)
		return nil
	}
	out := make(map[string]scriptState, len(recs))
	for _, r := range recs {
		st := scriptState{Enabled: true}
		if json.Unmarshal(r.Payload, &st) == nil && st.Script != "" {
			out[st.Script] = st
		}
	}
	return out
}

// setEnabled is the toggle behind the settings-row button: record write plus
// VM flip, fused, so the re-render the write wakes already shows the final
// state (the SetUser shape, applied to a domain collection).
func (h *Host) setEnabled(name string, enabled bool) error {
	st, _ := h.getState(name)
	st.Enabled = enabled
	if err := h.plugin.Put(stateCollection, name, st); err != nil {
		return err
	}
	h.lifeMu.Lock()
	defer h.lifeMu.Unlock()
	if enabled {
		h.loadLocked(name)
	} else {
		h.stopLocked(name, "disabled")
	}
	return nil
}

// publishError mirrors a script's latest error into its record. Compare
// first: a handler failing on every timer tick must not write on every
// tick. Creating the missing record doubles as first-sight registration.
func (h *Host) publishError(name, msg string) {
	st, ok := h.getState(name)
	if ok && st.LastError == msg {
		return
	}
	st.LastError = msg
	if err := h.plugin.Put(stateCollection, name, st); err != nil {
		branchkit.Logf("scripts", "[%s] state write: %v", name, err)
	}
}

// reconcile converges VM state onto the records: enabled but not loaded →
// load (if the file still exists); disabled but loaded → stop. Idempotent —
// it also runs on the host's own writes and settles in one pass.
func (h *Host) reconcile() {
	h.lifeMu.Lock()
	defer h.lifeMu.Unlock()
	loaded := h.snapshot()
	for name, st := range h.listStates() {
		_, isLoaded := loaded[name]
		switch {
		case st.Enabled && !isLoaded:
			if _, _, ok := h.findScript(name); ok {
				h.loadLocked(name)
			}
		case !st.Enabled && isLoaded:
			h.stopLocked(name, "disabled")
		}
	}
	// A script that just went away (deleted, disabled, header edited) may
	// have been the only reason a request was pending. Outside the VM
	// work, still under lifeMu — this is RPC, not VM manipulation.
	go h.withdrawStaleRequests()
}

// notePrivilegeNeeded files a privileges.request for a platform privilege
// a script's call() needs but the host hasn't been granted, and records
// the script so the grant can reload exactly the scripts that were
// waiting. The reason names the file WITH its extension — 'headphones.lua'
// reads as a file on the Plugins page; bare 'headphones' read as a
// mystery label. Returns the request status ("pending", "denied",
// "granted") or "" when the request itself failed (older platform, wire
// trouble) — the caller falls back to the manual-approval teaching
// message.
func (h *Host) notePrivilegeNeeded(privilege, script, file string) string {
	return h.notePrivilegeNeededFor(privilege, script, fmt.Sprintf("script '%s' uses call:%s", file, privilege))
}

// notePrivilegeNeededFor is notePrivilegeNeeded with the reason the request
// carries spelled out by the caller: on() asks for an event family, not for
// a call.
func (h *Host) notePrivilegeNeededFor(privilege, script, reason string) string {
	h.reqMu.Lock()
	if h.privWaits[privilege] == nil {
		h.privWaits[privilege] = map[string]bool{}
	}
	h.privWaits[privilege][script] = true
	h.reqMu.Unlock()

	res, err := h.plugin.PrivilegesRequest(branchkit.PrivilegesRequestRequest{Privilege: privilege, Reason: &reason})
	if err != nil {
		branchkit.Logf("scripts", "[%s] privileges.request(%s): %v", script, privilege, err)
		return ""
	}
	return res.Status
}

// noteWaiting records that a script waits on a privilege without filing a
// request for it, so its grant reloads the script (PrivilegeGranted).
func (h *Host) noteWaiting(privilege, script string) {
	h.reqMu.Lock()
	defer h.reqMu.Unlock()
	if h.privWaits[privilege] == nil {
		h.privWaits[privilege] = map[string]bool{}
	}
	h.privWaits[privilege][script] = true
}

// privilegeStatus is one entry of the host's own grant state, from
// privileges.list — what lets each script row say whether its query:*
// tokens are actually usable right now.
type privilegeStatus struct {
	// Required privileges are the host's own floor (it doesn't run without
	// them) — always granted, never relinquishable.
	Required bool `json:"required"`
	Granted  bool `json:"granted"`
	Pending  bool `json:"pending"`
	Denied   bool `json:"denied"`
}

// privilegeDemand maps a platform privilege to the ENABLED scripts whose
// headers declare it (bare `call` demands everything, so it lands under
// every privilege the host knows about). Derived from disk, not from the
// waits map: a header edit or a delete must change the answer even for
// scripts that never ran. Sorted, for stable rendering.
func (h *Host) privilegeDemand(known []string, states map[string]scriptState) map[string][]string {
	demand := map[string][]string{}
	for _, name := range h.scriptFiles() {
		if st, ok := states[name]; ok && !st.Enabled {
			continue
		}
		path, sk, ok := h.findScript(name)
		if !ok {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		g := parsePermissions(string(src), sk.commentPrefix)
		file := name + sk.ext
		for tok := range g.caps {
			if tok == "call" {
				// Bare grant: demands the whole surface.
				for _, priv := range known {
					demand[priv] = append(demand[priv], file)
				}
				continue
			}
			if priv, isCall := strings.CutPrefix(tok, "call:"); isCall {
				demand[priv] = append(demand[priv], file)
			}
			// A grant naming a window or world event pattern demands
			// events.world, which on() asks for (worldEventPrivilege). A bare
			// `events` grant says nothing about which events; a script
			// holding one demands it only while on() is waiting on it (below).
			if pattern, isEvents := strings.CutPrefix(tok, "events:"); isEvents && reachesWorldEvents(pattern) {
				demand[worldEventPrivilege] = append(demand[worldEventPrivilege], file)
			}
		}
		h.reqMu.Lock()
		waiting := h.privWaits[worldEventPrivilege][name]
		h.reqMu.Unlock()
		if waiting && !slices.Contains(demand[worldEventPrivilege], file) {
			demand[worldEventPrivilege] = append(demand[worldEventPrivilege], file)
		}
	}
	for priv := range demand {
		sort.Strings(demand[priv])
		demand[priv] = slices.Compact(demand[priv])
	}
	return demand
}

// withdrawStaleRequests gives back pending requests no enabled script asks
// for anymore — a deleted, disabled, or edited script must not leave a
// to-do sitting on the Plugins page forever. Only PENDING requests are
// withdrawn automatically: auto-returning live grants on script churn
// would thrash the user's consent, so those are surfaced on the tab with
// an explicit Give back button instead.
func (h *Host) withdrawStaleRequests() {
	status := h.privilegeStatuses()
	if len(status) == 0 {
		return
	}
	known := make([]string, 0, len(status))
	for priv := range status {
		known = append(known, priv)
	}
	demand := h.privilegeDemand(known, h.listStates())
	for priv, st := range status {
		if !st.Pending || len(demand[priv]) > 0 {
			continue
		}
		h.relinquish(priv)
		branchkit.Logf("scripts", "withdrew pending request for '%s' — no enabled script uses it", priv)
	}
}

// relinquish gives one optional privilege back to the platform and drops
// any local wait bookkeeping for it.
func (h *Host) relinquish(privilege string) string {
	h.reqMu.Lock()
	delete(h.privWaits, privilege)
	h.reqMu.Unlock()
	res, err := h.plugin.PrivilegesRelinquish(branchkit.PrivilegesRelinquishRequest{Privilege: privilege})
	if err != nil {
		branchkit.Logf("scripts", "privileges.relinquish(%s): %v", privilege, err)
		return ""
	}
	return res.Status
}

// privilegeStatuses reads the host's live privilege state, keyed by
// privilege name. Empty map (older platform, wire trouble) renders as no
// annotations — never as wrong ones.
func (h *Host) privilegeStatuses() map[string]privilegeStatus {
	entries, err := h.plugin.PrivilegesList()
	if err != nil {
		branchkit.Logf("scripts", "privileges.list: %v", err)
		return nil
	}
	out := make(map[string]privilegeStatus, len(entries))
	for _, e := range entries {
		out[e.Privilege] = privilegeStatus{
			Required: e.Required,
			Granted:  e.Granted,
			Pending:  e.Pending,
			Denied:   e.Denied,
		}
	}
	return out
}

// PrivilegeGranted reloads the scripts whose call() calls were waiting on
// this privilege — the receiving end of _platform.privilege.granted, so a
// script resumes the moment the user clicks Approve.
func (h *Host) PrivilegeGranted(privilege string) {
	h.reqMu.Lock()
	waiting := h.privWaits[privilege]
	delete(h.privWaits, privilege)
	h.reqMu.Unlock()
	if len(waiting) == 0 {
		return
	}
	h.lifeMu.Lock()
	defer h.lifeMu.Unlock()
	for name := range waiting {
		branchkit.Logf("scripts", "[%s] privilege '%s' granted — reloading", name, privilege)
		h.loadLocked(name)
	}
}

// noteEffectHolder records that a script now wants an effect.
func (h *Host) noteEffectHolder(effect, script string) {
	h.effMu.Lock()
	defer h.effMu.Unlock()
	if h.effects[effect] == nil {
		h.effects[effect] = map[string]bool{}
	}
	h.effects[effect][script] = true
}

// dropEffectHolder forgets one script's claim and reports whether the effect
// is now unwanted — the signal to actually retract the plugin's frame.
func (h *Host) dropEffectHolder(effect, script string) (last bool) {
	h.effMu.Lock()
	defer h.effMu.Unlock()
	holders := h.effects[effect]
	if holders == nil || !holders[script] {
		// Not a holder, so nothing to give back. Releasing what you never
		// took must not retract the frame a sibling is relying on.
		return false
	}
	delete(holders, script)
	if len(holders) == 0 {
		delete(h.effects, effect)
		return true
	}
	return false
}

// dropEffect forgets every claim on an effect and returns who held it. Used
// on displacement, where the frame is gone whether or not anyone let go.
func (h *Host) dropEffect(effect string) []string {
	h.effMu.Lock()
	defer h.effMu.Unlock()
	holders := h.effects[effect]
	delete(h.effects, effect)
	out := make([]string, 0, len(holders))
	for script := range holders {
		out = append(out, script)
	}
	sort.Strings(out)
	return out
}

// holdsEffect reports whether a script is among an effect's holders.
func (h *Host) holdsEffect(effect, script string) bool {
	h.effMu.Lock()
	defer h.effMu.Unlock()
	return h.effects[effect][script]
}

// releaseEffects gives back everything a script holds. Called from teardown.
//
// The load-bearing case is suppress_notifications: a script that mutes
// notifications and is then edited, disabled or deleted would otherwise
// leave Do Not Disturb on with nothing left to turn it off. The platform
// releases a dead PLUGIN's frames, but a script dying is not the plugin
// dying — that gap is the host's to close, and this is where.
func (h *Host) releaseEffects(script string) {
	h.effMu.Lock()
	held := []string{}
	for effect, holders := range h.effects {
		if holders[script] {
			held = append(held, effect)
		}
	}
	h.effMu.Unlock()
	for _, effect := range held {
		if !h.dropEffectHolder(effect, script) {
			continue // a sibling still wants it
		}
		if _, _, err := h.plugin.RetractEffect(effect); err != nil {
			branchkit.Logf("scripts", "[%s] releasing %s: %v", script, effect, err)
		}
	}
}

// SetDeclaredCollections records the manifest's consumed collections. Call
// before Start.
func (h *Host) SetDeclaredCollections(names []string) {
	h.declared = make(map[string]bool, len(names))
	for _, n := range names {
		h.declared[n] = true
	}
}

// declaresCollection reports whether the host asked the user for reach into
// this collection.
func (h *Host) declaresCollection(name string) bool {
	return h.declared[name]
}

// SetKnownEvents records the manifest's consumed patterns. Call before Start.
func (h *Host) SetKnownEvents(patterns []string) {
	h.known = append(h.known[:0], patterns...)
}

// reachable reports whether a script's subscription pattern could ever fire:
// does it overlap any pattern the host's manifest consumes? Both sides may
// carry `*` (and `**`) now that scripts subscribe to each other
// (`scripts.*.*` in the manifest, `scripts.headphones.*` or `scripts.**` in a
// script), so this is an overlap question, not a match one — a plain lookup
// would call every wildcard subscription dead.
func (h *Host) reachable(pattern string) bool {
	for _, known := range h.known {
		if patternsOverlap(known, pattern) {
			return true
		}
	}
	return false
}

// patternsOverlap reports whether two subscription patterns could ever name
// the same event: whether some topic matches both. Both sides use the
// platform's subscription grammar — `*` is exactly one segment, `**` zero or
// more — so arity alone no longer decides it (`scripts.**` overlaps
// `scripts.*.*`, and `**.charged` does too). Mirrors the platform's
// validation of the same manifest (its `topic::subscription_overlaps_*`).
//
// ok[i][j] is "some topic suffix is matched by both a[i:] and b[j:]", filled
// from the ends backwards. A `**` either matches nothing or absorbs one
// segment the other side produces; when both sides are `**`, the state where
// one has moved on and the other is still open already covers the shorter
// run. O(len(a) × len(b)).
func patternsOverlap(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	la, lb := len(as), len(bs)
	w := lb + 1
	ok := make([]bool, (la+1)*w)
	ok[la*w+lb] = true
	for i := la; i >= 0; i-- {
		for j := lb; j >= 0; j-- {
			if i == la && j == lb {
				continue
			}
			aAny := i < la && as[i] == "**"
			bAny := j < lb && bs[j] == "**"
			r := false
			if aAny {
				r = r || ok[(i+1)*w+j]
				if j < lb && !bAny {
					r = r || ok[i*w+j+1]
				}
			}
			if bAny {
				r = r || ok[i*w+j+1]
				if i < la && !aAny {
					r = r || ok[(i+1)*w+j]
				}
			}
			if i < la && j < lb && !aAny && !bAny &&
				(as[i] == "*" || bs[j] == "*" || as[i] == bs[j]) {
				r = r || ok[(i+1)*w+j+1]
			}
			ok[i*w+j] = r
		}
	}
	return ok[0]
}

// matchesTopic reports whether a concrete event type matches a subscription
// pattern, in the platform's grammar: `*` is exactly one segment, `**` zero
// or more, anything else literal. Used to fan a delivered event in to the
// script callbacks that asked for it, so it must agree with the platform's
// delivery gate — which is what delivered the event in the first place.
func matchesTopic(pattern, eventType string) bool {
	if pattern == eventType {
		return true
	}
	if !strings.Contains(pattern, "*") {
		return false
	}
	pat, evt := strings.Split(pattern, "."), strings.Split(eventType, ".")
	// Greedy, backtracking to the most recent `**`: what lies between two
	// `**` has a fixed length, so the latest one is the only one worth
	// retrying.
	p, e := 0, 0
	resumeP, resumeE := -1, -1
	for e < len(evt) {
		switch {
		case p < len(pat) && pat[p] == "**":
			resumeP, resumeE = p+1, e
			p++
		case p < len(pat) && (pat[p] == "*" || pat[p] == evt[e]):
			p++
			e++
		case resumeP >= 0:
			resumeE++
			p, e = resumeP, resumeE
		default:
			return false
		}
	}
	for ; p < len(pat); p++ {
		if pat[p] != "**" {
			return false
		}
	}
	return true
}

// Start loads every script and begins watching the directory. Called from
// OnReady so command pushes land on a live platform.
func (h *Host) Start() {
	if err := os.MkdirAll(h.dir, 0o755); err != nil {
		branchkit.Logf("scripts", "creating %s: %v", h.dir, err)
		return
	}
	h.publishChooserCommand()
	// Nothing is being asked yet, so anything left in the chooser's state is
	// a previous process's. The exclusive GATE is the platform's to release
	// — `preset: tag` means `held_by_writer`, so a host that dies has its
	// tags dropped for it — but the OPTION records are `preset:
	// named_entities`, which is not held, so a crash mid-chooser strands up
	// to twelve codewords in the Layer-1 grammar union with nothing alive
	// that would ever clear them.
	//
	// The rule this follows: every projected gate needs an exit that does
	// not depend on the thing being hosted. Here the thing it must not
	// depend on is the host's own previous life.
	h.reapChoiceRecords()
	seen := map[string]time.Time{}
	h.scanOnce(seen)
	h.reapOrphanRecords()
	go h.watch(seen)
	branchkit.Logf("scripts", "hosting %s", h.dir)
}

// reapOrphanRecords drops state records whose file no longer exists —
// a file deleted while the host wasn't running never took the unload path.
func (h *Host) reapOrphanRecords() {
	for name := range h.listStates() {
		if _, _, ok := h.findScript(name); !ok {
			if _, err := h.plugin.Delete(stateCollection, name); err != nil {
				branchkit.Logf("scripts", "[%s] orphan state delete: %v", name, err)
			} else {
				branchkit.Logf("scripts", "[%s] orphan state record reaped", name)
			}
		}
	}
}

// watch polls modtimes once a second — hot reload without a dependency.
// Continues on the seen map the boot scan built: re-deriving it from the
// loaded set would miss disabled scripts (no VM) and re-stop them once.
func (h *Host) watch(seen map[string]time.Time) {
	for range time.Tick(time.Second) {
		h.scanOnce(seen)
	}
}

// scanOnce diffs the directory against seen (name -> modtime), loading new
// and changed files and unloading deleted ones. seen is mutated in place.
func (h *Host) scanOnce(seen map[string]time.Time) {
	entries, err := os.ReadDir(h.dir)
	if err != nil {
		return
	}
	h.lifeMu.Lock()
	defer h.lifeMu.Unlock()
	present := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := filepath.Ext(e.Name())
		if skinForExt(ext) == nil {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ext)
		if present[name] {
			branchkit.Logf("scripts", "[%s] duplicate script extensions on disk — using the first, ignoring %s", name, e.Name())
			continue
		}
		present[name] = true
		info, err := e.Info()
		if err != nil {
			continue
		}
		if prev, ok := seen[name]; !ok || info.ModTime().After(prev) {
			seen[name] = info.ModTime()
			h.loadLocked(name)
		}
	}
	gone := false
	for name := range seen {
		if !present[name] {
			delete(seen, name)
			h.unloadLocked(name)
			gone = true
		}
	}
	// A deleted script may have been the only demand behind a pending
	// request. (Header EDITS reach the same check through reconcile, which
	// the reload's state write triggers.)
	if gone {
		go h.withdrawStaleRequests()
	}
}

// scriptFiles lists the script files on disk (any skin), sorted — the
// settings tab's row source, since a disabled script has no VM and so no
// entry in scripts.
func (h *Host) scriptFiles() []string {
	entries, err := os.ReadDir(h.dir)
	if err != nil {
		return nil
	}
	names := []string{}
	seen := map[string]bool{}
	for _, e := range entries {
		ext := filepath.Ext(e.Name())
		if !e.IsDir() && skinForExt(ext) != nil {
			name := strings.TrimSuffix(e.Name(), ext)
			if !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	return names
}

func (h *Host) snapshot() map[string]*Script {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make(map[string]*Script, len(h.scripts))
	for k, v := range h.scripts {
		out[k] = v
	}
	return out
}

// load (re)loads one script file — the lifeMu-locked entry for settings
// handlers; the scan and reconcile loops call loadLocked directly.
func (h *Host) load(name string) {
	h.lifeMu.Lock()
	defer h.lifeMu.Unlock()
	h.loadLocked(name)
}

// loadLocked (caller holds lifeMu) tears down any prior VM, runs the file
// in a fresh one, and replaces the script's command group with whatever it
// registered. A script that errors mid-file keeps whatever it registered
// before the error, plus its error state for the settings tab. A disabled
// record short-circuits to stop — the file stays listed, the VM never runs.
func (h *Host) loadLocked(name string) {
	if st, _ := h.getState(name); !st.Enabled {
		// Ensure no VM and no lingering command group (a plugin restart
		// does not clear groups pushed by the previous process).
		h.stopLocked(name, "disabled")
		return
	}
	h.teardown(name)

	path, sk, ok := h.findScript(name)
	if !ok {
		return
	}
	s := &Script{
		name:     name,
		skin:     sk,
		host:     h,
		handlers: map[string]func() error{},
		events:   map[string][]func(payload any) error{},
		timers:   map[string]*time.Timer{},
	}
	// Load-time work is done for this script too: top-level code can emit,
	// dispatch and push HUD, and the command push and error publish below
	// are on its behalf. Opened here, before the header is even parsed, so
	// nothing in the load path is unlabeled.
	defer branchkit.ActOnBehalfOf(s.file())()

	// The permission header must be in force before top-level code runs.
	if src, err := os.ReadFile(path); err == nil {
		s.grants = parsePermissions(string(src), sk.commentPrefix)
	}
	// Ask the platform for what the header names on the network: each exact
	// `fetch:` host, and each `secret:` slot with its host. All of it lands
	// OFF on the Scripts plugin page until the user allows it — the header
	// is the request, never the grant.
	s.requestNetwork()
	if len(s.grants.unknown) > 0 {
		s.lastErr = fmt.Sprintf("permissions header: unknown grant %q — the host gates dispatch[:prefix], call[:privilege], events[:pattern], emit[:event], mode[:name], effect[:name], read[:collection], write[:collection], hud, choose, fetch:host, secret:name@host",
			strings.Join(s.grants.unknown, ", "))
		branchkit.Logf("scripts", "[%s] %s", name, s.lastErr)
	}

	// boot runs the file before the script is published to h.scripts, so
	// nothing can enter the VM concurrently while it executes.
	if err := sk.boot(s, path); err != nil {
		s.lastErr = err.Error()
		branchkit.Logf("scripts", "[%s] load error: %v", name, err)
	} else {
		branchkit.Logf("scripts", "[%s] loaded (%s): %d commands, %d event subscriptions",
			name, sk.lang, len(s.handlers), len(s.events))
	}
	loadErr := s.lastErr // safe to read: s is not yet published

	h.mu.Lock()
	h.scripts[name] = s
	h.mu.Unlock()

	// Every registration has now been seen, so the script's mode universe is
	// known and entering commands can be told what to clear on the way in.
	s.resolveModeExits()

	if _, err := branchkit.PushCommandGroup(h.plugin, "script:"+name, s.commands); err != nil {
		branchkit.Logf("scripts", "[%s] command push: %v", name, err)
	}

	// Publish the load outcome — sets the row's error, or clears a stale
	// one on a clean reload. Also creates the record on first sight.
	h.publishError(name, loadErr)
}

// stopLocked (caller holds lifeMu) tears down a script's VM and retracts
// its command group, keeping the file and its state record — the disable
// half of the toggle.
func (h *Host) stopLocked(name, why string) {
	h.teardown(name)
	if _, err := branchkit.PushCommandGroup(h.plugin, "script:"+name, nil); err != nil {
		branchkit.Logf("scripts", "[%s] command retract: %v", name, err)
	}
	branchkit.Logf("scripts", "[%s] %s", name, why)
}

// unload removes a deleted script: stop plus dropping its state record.
func (h *Host) unload(name string) {
	h.lifeMu.Lock()
	defer h.lifeMu.Unlock()
	h.unloadLocked(name)
}

func (h *Host) unloadLocked(name string) {
	h.stopLocked(name, "unloaded")
	if _, err := h.plugin.Delete(stateCollection, name); err != nil {
		branchkit.Logf("scripts", "[%s] state delete: %v", name, err)
	}
	// Private storage goes with the script. Keeping it would leave records
	// no surface can reach — the key namespace is derived from the file
	// name, so a file that no longer exists owns data nothing can read,
	// write, or show the user.
	go h.clearStore(name)
}

// clearStore drops a script's private storage records.
func (h *Host) clearStore(script string) {
	recs, err := h.plugin.ListAll(storeCollection)
	if err != nil {
		branchkit.Logf("scripts", "[%s] store list: %v", script, err)
		return
	}
	prefix := script + "-"
	for _, r := range recs {
		var st struct {
			Key string `json:"key"`
		}
		if json.Unmarshal(r.Payload, &st) != nil || !strings.HasPrefix(st.Key, prefix) {
			continue
		}
		if _, err := h.plugin.Delete(storeCollection, st.Key); err != nil {
			branchkit.Logf("scripts", "[%s] store clear: %v", script, err)
		}
	}
}

// onChildExit routes an out-of-process skin's child dying on its own onto
// the existing error paths: command group retracted, row error published,
// next save or enable restarts. A stale script (already replaced or torn
// down — its child's death is old news) is ignored.
func (h *Host) onChildExit(name string, s *Script, msg string) {
	h.lifeMu.Lock()
	h.mu.RLock()
	current := h.scripts[name] == s
	h.mu.RUnlock()
	if current {
		h.stopLocked(name, "process exited")
	}
	h.lifeMu.Unlock()
	if current {
		h.publishError(name, msg)
	}
}

// heldByScript reports what a script is currently HOLDING, as opposed to
// what its header declares — the mode it is in, the effects it has taken,
// and how many private keys it has stored.
//
// Declared and held are different questions and the row was only ever
// answering the first. Holding is the state that changes what the rest of
// the system does: a script in a mode has narrowed the spoken surface for
// everyone, and a script holding suppress_notifications has muted the
// machine. "What did this script claim" and "what is it doing to me right
// now" belong on the same surface.
//
// Read-only and best-effort: a failed read renders as nothing held rather
// than as an error, because this is an annotation on a row, not the row.
func (h *Host) heldByScript() (modes map[string]string, effects map[string][]string, stored map[string]int) {
	modes, effects, stored = map[string]string{}, map[string][]string{}, map[string]int{}

	if recs, err := h.plugin.ListAll(modeCollection); err == nil {
		for _, r := range recs {
			var st struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(r.Payload, &st) != nil {
				continue
			}
			// Records are `<script>-<mode>`; split on the first hyphen, which
			// is the host's own encoding, so a mode name containing one is
			// still attributed to the right script.
			if script, mode, ok := strings.Cut(st.Name, "-"); ok {
				modes[script] = mode
			}
		}
	}

	h.effMu.Lock()
	for effect, holders := range h.effects {
		for script := range holders {
			effects[script] = append(effects[script], effect)
		}
	}
	h.effMu.Unlock()
	for script := range effects {
		sort.Strings(effects[script])
	}

	if recs, err := h.plugin.ListAll(storeCollection); err == nil {
		for _, r := range recs {
			var st struct {
				Key string `json:"key"`
			}
			if json.Unmarshal(r.Payload, &st) != nil {
				continue
			}
			if script, _, ok := strings.Cut(st.Key, "-"); ok {
				stored[script]++
			}
		}
	}
	return modes, effects, stored
}

// clearModes drops every mode record a script holds. Called by mode() before
// entering a new one, by clear_mode(), and by teardown.
//
// The teardown call is the load-bearing one: a script that enters an
// exclusive mode and is then edited, disabled or deleted would otherwise
// leave the spoken surface narrowed to commands that no longer exist. The
// user could still say the escape word, but "my voice control stopped
// working because I saved a file" is not a recovery story worth shipping.
func (h *Host) clearModes(script string) {
	recs, err := h.plugin.ListAll(modeCollection)
	if err != nil {
		branchkit.Logf("scripts", "[%s] mode list: %v", script, err)
		return
	}
	prefix := script + "-"
	for _, r := range recs {
		var st struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(r.Payload, &st) != nil || !strings.HasPrefix(st.Name, prefix) {
			continue
		}
		if _, err := h.plugin.Delete(modeCollection, st.Name); err != nil {
			branchkit.Logf("scripts", "[%s] mode clear: %v", script, err)
		}
	}
}

// teardown stops a script's timers and closes its VM, if loaded.
func (h *Host) teardown(name string) {
	h.mu.Lock()
	s, ok := h.scripts[name]
	if ok {
		delete(h.scripts, name)
	}
	h.mu.Unlock()
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.timers {
		t.Stop()
	}
	s.closed = true
	if s.closeVM != nil {
		s.closeVM()
	}
	// Outside the VM lock — this is RPC, not VM manipulation — and after
	// `closed`, so nothing the script does on the way out can re-enter.
	go func() {
		h.clearModes(name)
		h.releaseEffects(name)
		// A chooser whose script is gone can never be answered, and its
		// gate is exclusive — leaving it up narrows the spoken surface to
		// options belonging to a script that no longer exists.
		h.closeChoose(name, "script torn down")
	}()
}

// Invoke runs a script's command handler — the receiving end of the
// scripts.run action the host attaches to every pushed command.
func (h *Host) Invoke(script, handler string) error {
	h.mu.RLock()
	s := h.scripts[script]
	h.mu.RUnlock()
	if s == nil {
		return fmt.Errorf("script %q not loaded", script)
	}
	return s.call(func() error {
		fn := s.handlers[handler]
		if fn == nil {
			return fmt.Errorf("[%s] handler not found", s.name)
		}
		return fn()
	})
}

// FanIn delivers an event to every script callback whose subscription
// pattern matches its type.
//
// Keyed on the CONCRETE type, not on the manifest pattern that let it
// through: one manifest pattern (`scripts.*.*`) now carries many event
// types, and a script subscribing to `scripts.headphones.*` must see its
// slice of them and nothing else.
//
// A script never receives its own emits. Not a special case for its own
// sake — a script reacting to itself is a loop with no natural bottom, and
// the platform's rate limit is a poor place to discover that.
//
// source is the sender the platform put on the notification. A
// `scripts.<name>.<event>` event is honoured only when it is this plugin:
// the host reads <name> as the emitting script's identity, and every callback
// it runs executes with this plugin's grants, so an event that merely LOOKS
// like a script's is a way to run scripts' code on another plugin's say-so.
// The platform binds the `scripts.` namespace to this plugin at manifest
// validation; this is the host not depending on that alone.
func (h *Host) FanIn(eventType, source string, payload json.RawMessage) {
	if strings.HasPrefix(eventType, scriptEventPrefix+".") && source != pluginID {
		branchkit.Logf(pluginID, "ignoring %s from %q: only %s emits script events",
			eventType, source, pluginID)
		return
	}
	var decoded any
	_ = json.Unmarshal(payload, &decoded)
	self, _, isScriptEvent := scriptEventOrigin(eventType)
	// Displacement is broadcast: every plugin subscribed hears every
	// displacement, including ones about other plugins. Narrow it to the one
	// script that actually lost its frame, mirroring what the SDK's own
	// OnEffectDisplaced helper does one level up. Without this, "you lost
	// the effect" would arrive at scripts that never held it.
	var only map[string]bool
	if eventType == effectDisplacedEvent {
		lost, effect := h.displacedScripts(payload)
		if effect == "" {
			return // another plugin's displacement; not ours to relay
		}
		if len(lost) == 0 {
			// Ours, but whoever held it is gone. The claim is already
			// dropped; there is nobody left to tell.
			return
		}
		only = map[string]bool{}
		for _, script := range lost {
			only[script] = true
		}
	}
	for name, s := range h.snapshot() {
		if isScriptEvent && name == self {
			continue
		}
		if only != nil && !only[name] {
			continue
		}
		s.mu.Lock()
		var fns []func(payload any) error
		for pattern, subs := range s.events {
			if matchesTopic(pattern, eventType) {
				fns = append(fns, subs...)
			}
		}
		s.mu.Unlock()
		for _, fn := range fns {
			fn := fn
			_ = s.call(func() error { return fn(decoded) })
		}
	}
}

// effectDisplacedEvent is the platform's "you lost the effect" broadcast.
const effectDisplacedEvent = "_platform.effect.displaced"

// displacedScript reads a displacement payload and answers which of the
// host's scripts lost the frame, dropping the bookkeeping as it goes.
//
// Returns ("", "") when the displacement is about another plugin — the host
// then relays nothing. Returns ("", <effect>) when it IS about this plugin
// but no script is on file: the host held the effect for a script that has
// since gone, so there is nobody left to tell.
func (h *Host) displacedScripts(payload json.RawMessage) (scripts []string, effect string) {
	var ev struct {
		DisplacedOwner string `json:"displaced_owner"`
		Effect         string `json:"effect"`
	}
	if json.Unmarshal(payload, &ev) != nil || ev.DisplacedOwner != pluginID {
		return nil, ""
	}
	// The frame is gone for the whole plugin, so EVERY script that wanted it
	// lost — and the bookkeeping must not claim otherwise.
	return h.dropEffect(ev.Effect), ev.Effect
}

// scriptEventOrigin splits `scripts.<name>.<event>` into its parts. ok is
// false for anything else (every platform event).
func scriptEventOrigin(eventType string) (script, event string, ok bool) {
	parts := strings.Split(eventType, ".")
	if len(parts) != 3 || parts[0] != scriptEventPrefix {
		return "", "", false
	}
	return parts[1], parts[2], true
}

// call locks the VM and invokes run (a closure the language bridge built
// around a script callback). Errors are recorded on the script, logged,
// and published to the state record so the settings row shows them —
// never fatal to the host.
func (s *Script) call(run func() error) error {
	// Everything the host does from here — the bridged calls the callback
	// makes, and the error publish below — is done for this script, so it
	// carries the script's name into whatever the platform records. The
	// label is inert: it changes nothing about what the host may do, only
	// what the audit trail can say about it.
	defer branchkit.ActOnBehalfOf(s.file())()
	err, lastErr := s.callLocked(run)
	// Published outside the VM lock: publishError is a collection RPC. A
	// bridged function (dispatch, hud) may have recorded an error without
	// failing the call, so publish whatever the VM exited with.
	if lastErr != "" {
		s.host.publishError(s.name, lastErr)
	}
	return err
}

func (s *Script) callLocked(run func() error) (error, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ""
	}
	if err := run(); err != nil {
		s.lastErr = err.Error()
		branchkit.Logf("scripts", "[%s] %v", s.name, err)
		return err, s.lastErr
	}
	return nil, s.lastErr
}

// --- Chooser -----------------------------------------------------------
//
// A script asks the user a question and gets the answer back. Modelled on
// the voice plugin's tie disambiguator, which is the one
// chooser in the fleet that actually works end to end: a transient
// named_entities collection supplies the spoken codewords, a static command
// captures one, and an exclusive tag holds the spoken surface while the user
// answers.
//
// It is deliberately NOT built on the platform's `selection.*` operations.
// Those look like exactly this feature and are unreachable: nothing calls
// `selection.set` or `selection.pick`, nothing subscribes to
// `_platform.selection.picked`, and `match_selection_direct` has no caller
// outside its own tests. Building on them would render a HUD with no way
// to answer it.

const (
	choiceCollection = "plugin.scripts.choices"
	choosingTag      = "plugin.scripts.choosing"

	// The record id a single-cardinality tag collection stores under. The
	// platform coerces to this; passing anything else silently produces a
	// record you cannot then address by the id you chose.
	choosingTagID = "singleton"

	// How long a chooser may hold the spoken surface. An exclusive gate
	// with no exit is a trap, and the two exits a mode has are not enough
	// here: the escape command requires the user to know a chooser is why
	// nothing works, and teardown requires them to edit the file. So the
	// host closes it on its own, and the script hears about it.
	//
	// Longer than a tie's lifetime (one utterance) because the questions
	// differ: a tie asks which of two commands you meant while you are
	// mid-sentence; a chooser asks something a person is entitled to think
	// about for a moment.
	chooseTimeout = 45 * time.Second
)

// choiceCodewordPool supplies the spoken badges. Borrowed verbatim from
// the tie chooser rather than invented: the words are already curated to
// be acoustically distinct and unlikely to collide with command words, and
// two pools would be two things to keep good.
var choiceCodewordPool = []string{
	"harp", "arch", "drum", "bell", "fern", "kite",
	"lake", "moon", "nest", "pearl", "quill", "rune",
}

// openChooser is the one chooser that may be open at a time.
//
// One, not one per script: the gate and the choice collection are
// plugin-wide, so a second chooser would overwrite the first's options
// while its gate still claimed the surface — the user would see one
// question and answer another. A second call is REFUSED and says who holds
// it, rather than queued: a script blocked behind an unrelated script's
// unanswered question is a worse failure than an honest error.
type openChooser struct {
	script  string
	byCode  map[string]string // codeword -> the option value handed back
	deliver func(pick any) error
	timer   *time.Timer
}

// publishChooserCommand registers the one static command that answers an
// open chooser: a capture over the option collection, gated on the chooser
// tag so it is inert whenever nothing is being asked.
//
// Pushed as a command GROUP like every script's commands, rather than
// living in a `commands.json`, so this plugin has ONE way commands reach
// the platform. A manifest file would have needed `PushCommands`, a second
// entry point the host does not otherwise use — and the failure mode of
// forgetting it is silent: the gate goes up, the options publish, and
// nothing matches, which looks exactly like a recognition problem.
func (h *Host) publishChooserCommand() {
	spec := branchkit.Command(branchkit.Capture("choice", choiceCollection)).
		Action("scripts.pick_choice", map[string]any{"choice": "{choice}"}).
		RequiresTags(choosingTag).
		ClearsTags(choosingTag).
		Category("Scripts").
		Description("Answer a chooser a script opened").
		Build()
	if _, err := branchkit.PushCommandGroup(h.plugin, "chooser", []branchkit.CommandSpec{spec}); err != nil {
		branchkit.Logf("scripts", "publish chooser command: %v", err)
	}
}

// beginChoose stands up the transient choice grammar and opens the gate.
func (h *Host) beginChoose(script string, options []string, deliver func(pick any) error) error {
	if len(options) == 0 {
		return fmt.Errorf("choose(): needs at least one option")
	}
	if len(options) > len(choiceCodewordPool) {
		return fmt.Errorf("choose(): %d options exceeds the %d available codewords — "+
			"a spoken chooser stops being usable well before this",
			len(options), len(choiceCodewordPool))
	}

	h.chooseMu.Lock()
	if h.chooser != nil {
		owner := h.chooser.script
		h.chooseMu.Unlock()
		return fmt.Errorf("choose(): %q is already waiting for an answer — "+
			"only one chooser can hold the spoken surface at a time", owner)
	}
	c := &openChooser{script: script, byCode: map[string]string{}, deliver: deliver}
	entries := make([]branchkit.CollectionPutEntry, 0, len(options))
	for i, opt := range options {
		code := choiceCodewordPool[i]
		c.byCode[code] = opt
		payload, err := json.Marshal(map[string]any{"codeword": code, "pick": code})
		if err != nil {
			h.chooseMu.Unlock()
			return fmt.Errorf("choose(): %v", err)
		}
		entries = append(entries, branchkit.CollectionPutEntry{ID: code, Payload: payload})
	}
	h.chooser = c
	h.chooseMu.Unlock()

	if _, err := h.plugin.PutMany(choiceCollection, entries); err != nil {
		h.closeChoose(script, "could not publish options")
		return fmt.Errorf("choose(): %v", err)
	}
	// The gate goes up AFTER the options exist. The other order leaves a
	// window where the surface is narrowed to commands that are not
	// matchable yet, which reads to the user as voice control dying.
	// "singleton" is the id a single-cardinality tag collection stores under
	// — the platform coerces to it regardless of what is passed. Naming it
	// explicitly matches the convention the voice plugin's equivalent gate
	// uses, and makes the retraction below obviously symmetric.
	if err := h.plugin.Put(choosingTag, choosingTagID, map[string]any{}); err != nil {
		h.closeChoose(script, "could not open the gate")
		return fmt.Errorf("choose(): %v", err)
	}

	h.renderChooser(script, options)

	h.chooseMu.Lock()
	if h.chooser == c {
		c.timer = time.AfterFunc(chooseTimeout, func() { h.expireChoose(c) })
	}
	h.chooseMu.Unlock()
	return nil
}

// renderChooser shows the options and the word to say for each.
//
// The codeword is the answer, so it leads. Showing only the option text
// would leave the user guessing at what to say — which is the failure mode
// of every voice chooser that renders like a mouse menu.
//
// Published as a `choices` output state, not HTML: the platform draws it
// from the HUD kit, and every other renderer (speech above all) gets the
// same question. As raw HTML it was a spoken question nobody who cannot see
// the screen would ever hear.
func (h *Host) renderChooser(script string, options []string) {
	words := make([]string, len(options))
	items := make([]branchkit.OutputItem, len(options))
	for i, opt := range options {
		word := choiceCodewordPool[i]
		words[i] = word
		say, subtitle := word, opt
		items[i] = branchkit.OutputItem{
			ID:       word,
			Title:    word,
			Subtitle: &subtitle,
			Phrase:   word + ", " + opt,
			Action:   &branchkit.OutputAction{Say: &say},
		}
	}
	footer := "say a word, or wait"
	doc := branchkit.OutputState{
		Channel:  "scripts",
		Kind:     branchkit.OutputKindChoices,
		Title:    script,
		Phrase:   "Choose one: say " + joinWords(words) + ".",
		Sections: []branchkit.OutputSection{{Items: items}},
		Footer:   &footer,
		Progress: &branchkit.OutputProgress{
			RemainingMs: int(chooseTimeout / time.Millisecond),
			TotalMs:     int(chooseTimeout / time.Millisecond),
		},
		Urgency: branchkit.OutputUrgencyNotable,
		Locale:  "en",
		V:       1,
	}
	if _, err := h.plugin.OutputState(branchkit.OutputStateRequest{State: doc}); err != nil {
		branchkit.Logf("scripts", "[%s] chooser render: %v", script, err)
		return
	}
	if err := h.plugin.HUDShow(branchkit.HUDShowRequest{Channel: "scripts"}); err != nil {
		branchkit.Logf("scripts", "[%s] chooser show: %v", script, err)
	}
}

// joinWords reads a list aloud: "harp", "harp or arch", "harp, arch or drum".
func joinWords(w []string) string {
	switch len(w) {
	case 0:
		return ""
	case 1:
		return w[0]
	}
	return strings.Join(w[:len(w)-1], ", ") + " or " + w[len(w)-1]
}

// expireChoose closes a chooser nobody answered and tells the script so.
// The script hears nil, which is a real answer — "the user declined" is
// information, and a callback that simply never fires is not.
func (h *Host) expireChoose(c *openChooser) {
	if !h.takeExpired(c) {
		return
	}
	h.retractChoiceState()
	branchkit.Logf("scripts", "[%s] chooser timed out after %s", c.script, chooseTimeout)
	if err := c.deliver(nil); err != nil {
		branchkit.Logf("scripts", "[%s] chooser timeout callback: %v", c.script, err)
	}
}

// takeChoice is the state half of answering: it validates the codeword
// against the open chooser and, if it matches, consumes the chooser and
// hands back what to deliver.
//
// Split from the RPC half so the rules that matter — a codeword maps to
// the option the script listed, an answer is single-use, a mishear does
// NOT consume the question — are decided in one place with no I/O in it.
func (h *Host) takeChoice(codeword string) (*openChooser, string, error) {
	h.chooseMu.Lock()
	defer h.chooseMu.Unlock()

	c := h.chooser
	if c == nil {
		return nil, "", errNoChooser
	}
	pick, ok := c.byCode[codeword]
	if !ok {
		// Deliberately leaves the chooser open. The user is still waiting
		// to answer, and throwing their question away on a mishear is
		// worse than ignoring the mishear.
		return nil, "", fmt.Errorf("pick_choice: %q is not an option in the open chooser", codeword)
	}
	h.chooser = nil
	if c.timer != nil {
		c.timer.Stop()
	}
	return c, pick, nil
}

// takeExpired consumes the chooser only if it is still the open one. A
// timer that fires just after the user answered must not deliver a second,
// contradictory answer to a question already resolved.
func (h *Host) takeExpired(c *openChooser) bool {
	h.chooseMu.Lock()
	defer h.chooseMu.Unlock()
	if h.chooser != c {
		return false
	}
	h.chooser = nil
	return true
}

// errNoChooser is sentinel rather than formatted because resolveChoice
// branches on it: only "nothing is open" warrants sweeping the leftover
// gate, and matching on message text would be a bug waiting for a reword.
var errNoChooser = fmt.Errorf("pick_choice: no chooser is open")

// resolveChoice is the receiving end of the scripts.pick_choice action:
// the user spoke a codeword and the matcher captured it.
func (h *Host) resolveChoice(codeword string) error {
	c, pick, err := h.takeChoice(codeword)
	if err != nil {
		// The gate and the collection can outlive the chooser if a previous
		// close half-failed. Sweep them so the surface unsticks rather than
		// staying narrowed to a question nobody is asking.
		if errors.Is(err, errNoChooser) {
			h.retractChoiceState()
		}
		return err
	}
	// The matched command carries clears_tags for the gate, so the platform
	// has already dropped it. The options are ours to clean up.
	h.retractChoiceState()
	return c.deliver(pick)
}

// closeChoose abandons the named script's chooser, if it owns the open one.
func (h *Host) closeChoose(script, reason string) {
	h.chooseMu.Lock()
	c := h.chooser
	if c == nil || c.script != script {
		h.chooseMu.Unlock()
		return
	}
	h.chooser = nil
	if c.timer != nil {
		c.timer.Stop()
	}
	h.chooseMu.Unlock()
	h.retractChoiceState()
	branchkit.Logf("scripts", "[%s] chooser closed: %s", script, reason)
}

// retractChoiceState drops the options, the gate and the rendering. All are
// attempted even if an earlier one fails: leaving the gate up is what
// strands the user.
func (h *Host) retractChoiceState() {
	h.reapChoiceRecords()
	// A chooser still on screen after it stopped accepting answers is worse
	// than no chooser: it tells the user to say words that now do nothing.
	if _, err := h.plugin.OutputClear(branchkit.OutputClearRequest{Channel: "scripts"}); err != nil {
		branchkit.Logf("scripts", "chooser clear: %v", err)
	}
	if err := h.plugin.HUDHide(branchkit.HUDHideRequest{Channel: "scripts"}); err != nil {
		branchkit.Logf("scripts", "chooser hide: %v", err)
	}
}

// reapChoiceRecords drops the option records and the gate, leaving the HUD
// alone.
//
// Split from the render half for the boot call, where there is no HUD to
// clear and asking would log a failure for a window nobody opened.
func (h *Host) reapChoiceRecords() {
	recs, err := h.plugin.ListAll(choiceCollection)
	if err != nil {
		branchkit.Logf("scripts", "chooser option list: %v", err)
	} else if len(recs) > 0 {
		ids := make([]string, 0, len(recs))
		for _, r := range recs {
			ids = append(ids, r.ID)
		}
		if _, _, err := h.plugin.DeleteMany(choiceCollection, ids); err != nil {
			branchkit.Logf("scripts", "chooser option clear: %v", err)
		}
	}
	// Delete every record rather than one known id. An exclusive gate left
	// up is the worst failure this feature has — the user's voice control
	// appears dead and the only way out is a command they have no reason to
	// think of. That is worth being id-agnostic about: this survives a
	// change in how tag collections key their records, which the first cut
	// did not. It used id "choosing" while the platform stores single-
	// cardinality tags under "singleton", so every delete matched nothing,
	// reported no error, and left the gate held after every timeout.
	//
	// The `existed` result is checked, not discarded. It is the only signal
	// distinguishing "cleared" from "found nothing to clear", and discarding
	// it is precisely what hid the bug. Unit tests cannot reach this seam
	// (no plugin under test) — verify retraction live.
	gates, err := h.plugin.ListAll(choosingTag)
	if err != nil {
		branchkit.Logf("scripts", "chooser gate list: %v", err)
	}
	for _, g := range gates {
		existed, err := h.plugin.Delete(choosingTag, g.ID)
		if err != nil {
			branchkit.Logf("scripts", "chooser gate clear %q: %v", g.ID, err)
		} else if !existed {
			branchkit.Logf("scripts", "chooser gate clear %q: nothing removed", g.ID)
		}
	}
}
