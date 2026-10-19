package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/branchkit/plugin-sdk-go"
)

// The language-neutral halves of the bridged functions: permission
// gates, registration bookkeeping, and the SDK calls the host makes as
// itself. Each skin's globals convert values in its own type system and
// delegate here; a returned error is raised in-language by the skin
// (Lua error, JS exception), so refusals abort the calling script code
// the same way everywhere.
//
// None of these lock s.mu: they only ever run with the VM already
// serialized — during boot (before the script is published to the host
// map) or inside call() (which holds s.mu).

// registerCommand records a voice command: pattern is the neutral form
// (a string, or []any of words and []any alternatives groups), run is a
// closure invoking the script's callback. Returns the handler id — the
// out-of-process skin hands it to the child so invocations can name the
// callback; the embedded skins ignore it.
func (s *Script) registerCommand(pattern any, mode, enters string, run func() error) (string, error) {
	slots, err := patternSlots(pattern)
	if err != nil {
		return "", fmt.Errorf("command: %v", err)
	}
	for what, v := range map[string]string{"mode name": mode, "enters mode name": enters} {
		if v == "" {
			continue
		}
		if err := checkSegment(what, v); err != nil {
			return "", fmt.Errorf("command: %v", err)
		}
	}
	s.nextID++
	id := fmt.Sprintf("h%d", s.nextID)
	s.handlers[id] = run
	b := branchkit.Command(slots...).
		Action("scripts.run", map[string]any{"script": s.name, "handler": id}).
		Category("Scripts")
	// A mode-bound command matches only while this script holds that mode.
	// Requiring the gate is what makes the mode worth entering — and because
	// the gate is exclusive, entering it also narrows the engine grammar to
	// exactly these commands.
	//
	// No `mode` grant needed to DECLARE one: a command that can only ever
	// match inside a gate this script cannot enter without the grant is
	// inert, and gating both would just double the ceremony.
	if mode != "" {
		b = b.RequiresTags(modeCollection + "." + s.modeTag(mode))
	}
	// The mode this command ENTERS, distinct from the one it requires.
	// Declaring it is what lets the platform graft a one-breath chain
	// ("enter review mark it" in a single segment) and — the half that
	// actually makes the utterance work — apply the tag INSIDE resolve(),
	// so the rest of the transcript is matched with the mode already live.
	// A handler-side mode() call cannot do that: dispatch happens after
	// matching, so the tag would arrive too late for its own utterance.
	if enters != "" {
		b = b.SetsTags(modeCollection + "." + s.modeTag(enters))
	}
	spec := b.Build()
	s.commands = append(s.commands, spec)
	phrase, runnable := patternDisplay(pattern)
	s.infos = append(s.infos, cmdInfo{
		phrase: phrase, handler: id, runnable: runnable, mode: mode, enters: enters,
	})
	return id, nil
}

// resolveModeExits fills in `clears_tags` on every entering command with this
// script's OTHER mode tags, so entering by voice leaves the same state behind
// as entering through mode() — which REPLACES rather than adds.
//
// Statically knowable, and that is the whole reason this works: a script's
// mode universe is the union of the `{mode}` and `{enters}` values across its
// own registrations, and the command group is pushed only after the file has
// finished loading, so by now every registration has been seen.
//
// Called once per load, between boot and the command push.
func (s *Script) resolveModeExits() {
	universe := map[string]bool{}
	for _, info := range s.infos {
		for _, m := range []string{info.mode, info.enters} {
			if m != "" {
				universe[s.modeTag(m)] = true
			}
		}
	}
	if len(universe) < 2 {
		return // nothing to clear: at most one mode in play
	}
	for i, info := range s.infos {
		if info.enters == "" {
			continue
		}
		entering := s.modeTag(info.enters)
		others := make([]string, 0, len(universe)-1)
		for tag := range universe {
			if tag != entering {
				others = append(others, modeCollection+"."+tag)
			}
		}
		sort.Strings(others) // stable pushes; the set is order-free
		s.commands[i].ClearsTags = others
	}
}

// registerEvent subscribes a script callback to a platform event. Needs
// the `events` grant (or the exact `events:<pattern>`); a pattern outside
// the host manifest's consumed set can never fire, so it's flagged.
func (s *Script) registerEvent(pattern string, fn func(payload any) error) error {
	if !s.grants.allows("events", pattern) {
		return fmt.Errorf("on(%q): not declared — add `%s permissions: events` (or `events:%s`) to the top of this file",
			pattern, s.skin.commentPrefix, pattern)
	}
	if !s.host.reachable(pattern) {
		s.lastErr = fmt.Sprintf("on(%q): outside the host manifest's consumed events — this will never fire", pattern)
		branchkit.Logf("scripts", "[%s] %s", s.name, s.lastErr)
	}
	s.events[pattern] = append(s.events[pattern], fn)
	return nil
}

// bridgeDispatch fires an action through the host's dispatch grant and
// returns the action's result value (nil when the action produced none).
// Needs the matching `dispatch:<prefix>` grant (or bare `dispatch`). A
// refused call returns the teaching error; a platform-side failure is soft
// (recorded on the row, nil result, the script continues) — dispatch
// callers overwhelmingly don't check, unlike call's.
//
// Soft-failing is still right, but it used to be UNASKABLE. Once dispatch
// started returning the action's result, `nil` meant both "the action
// produced no value" (the common case — most actions produce none) and "it
// did not run", and a script branching on the result could not tell them
// apart. That is the false-green shape this codebase has been bitten by
// three times. `failed()` is the question; the answer is recorded here.
func (s *Script) bridgeDispatch(actionType string, params map[string]any) (any, error) {
	s.dispatchErr = ""
	prefix, _, _ := strings.Cut(actionType, ".")
	if !s.grants.allows("dispatch", prefix) {
		return nil, fmt.Errorf("dispatch(%q): not declared — add `%s permissions: dispatch:%s` to the top of this file",
			actionType, s.skin.commentPrefix, prefix)
	}
	// The platform's Action is an internally-tagged enum with exactly two
	// public variants, `plugin` and `sequence` — there is no variant per
	// action type. So the tag is the literal "plugin" and the dotted type
	// goes in `ActionType`, with parameters NESTED under `Params` rather
	// than spread alongside it.
	//
	// Building `{"type": "<action_type>", ...params}` instead produced
	// `unknown variant` on every single call, which is what this did until
	// 2026-08-25. `branchkit.Action` is generated from the enum as of
	// 2026-09-19 and there is no longer a field that mistake could go in.
	if params == nil {
		params = map[string]any{}
	}
	encoded, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("dispatch: %v", err)
	}
	resp, err := s.host.plugin.Dispatch(branchkit.DispatchRequest{
		Action: branchkit.Action{
			Type:       branchkit.ActionKindPlugin,
			ActionType: &actionType,
			Params:     encoded,
		},
	})
	if err != nil {
		s.lastErr = fmt.Sprintf("dispatch(%s): %v", actionType, err)
		s.dispatchErr = s.lastErr
		branchkit.Logf("scripts", "[%s] %s", s.name, s.lastErr)
		return nil, nil
	}
	if resp == nil || len(resp.Result) == 0 {
		return nil, nil
	}
	var out any
	if json.Unmarshal(resp.Result, &out) != nil {
		return nil, nil
	}
	return out, nil
}

// bridgeFailed reports why the most recent dispatch() failed, or nil if it
// succeeded — the half of a soft failure a script can act on.
//
// Scoped to the LAST dispatch, not to the script's lifetime: cleared on
// entry to every dispatch(), so `failed()` immediately after one answers
// about that one. Anything wider would be a mood ring.
//
// Deliberately NOT a second return value. Multiple returns are natural in
// Lua and Python, awkward in JavaScript, and this host has four skins that
// must present one API; a reader is the shape all four express identically.
// It is also why dispatch() keeps its signature — existing scripts do not
// have to care.
//
// `call()` has no counterpart and needs none: its errors are HARD, so a
// failure never returns at all.
func (s *Script) bridgeFailed() any {
	if s.dispatchErr == "" {
		return nil
	}
	return s.dispatchErr
}

// bridgeCall calls a platform method (native.*, input.*, …) through the
// host's SDK connection and returns the decoded result. Gated by the
// `call` header grant against the embedded method→privilege table —
// bare `call` reaches every privilege bucket, `call:<privilege>` one.
// Unknown methods are refused (fail-closed against a stale table), and
// three method classes are refused regardless of the header: the
// `shell`/`security` privileges, and the privilege-less plugin-protocol
// methods (the host's own control plane). The shell/security refusal is a
// DEFAULT, not a boundary (another plugin may expose shell as an action);
// the platform's per-method check stays the real boundary.
// Errors are HARD, unlike dispatch's soft-fail: results feed control
// flow, and a silently-nil result is a wrong branch, not a missed side
// effect. Note the surface is NOT read-only — a granted bucket reaches
// its mutating methods too (input.type_text, native.set_window_position).
// The privilege bucket is the consent unit; that is why this is `call`
// and not `query`.
func (s *Script) bridgeCall(method string, params map[string]any) (any, error) {
	priv, known := methodPrivilege(method)
	if !known {
		return nil, fmt.Errorf("call(%q): unknown method", method)
	}
	switch priv {
	case "shell", "security":
		return nil, fmt.Errorf("call(%q): not available to scripts", method)
	case "":
		// Refused — but NOT because a missing privilege means unguarded.
		// `privilege: null` means governed on a different axis: collection
		// writes by each collection's `writers` policy against the caller,
		// reads by interaction grants the user toggles. Privileges are
		// simply the wrong axis for state.
		//
		// The refusal stands anyway, because this table cannot tell those
		// apart from the host's own control plane — `commands.push`,
		// `hud.create_channel`, `pipelines.inject`, `control.signal` —
		// where a raw call corrupts the host's model (a bare commands.push
		// bypasses the per-script group that makes hot reload work).
		// Curated projections are the answer for both: command/on/hud/
		// dispatch for the control plane, record/records/put/drop and
		// store/stored for collections, each applying the rules this
		// coarse gate cannot express.
		return nil, fmt.Errorf("call(%q): not reachable this way — collections go through record/records/put/drop, and the host's own machinery through command/on/hud/dispatch", method)
	default:
		if !s.grants.allows("call", priv) {
			return nil, fmt.Errorf("call(%q): not declared — add `%s permissions: call:%s` (or bare `call`) to the top of this file",
				method, s.skin.commentPrefix, priv)
		}
	}
	if params == nil {
		params = map[string]any{}
	}
	var raw json.RawMessage
	err := s.host.plugin.Call(method, params, &raw)
	if err != nil && strings.Contains(err.Error(), "-32003") {
		// The header granted it but the platform denied the HOST: the
		// privilege is optional in the manifest and not yet approved.
		// File a privileges.request (attributed to this script) so the
		// ask lands as an Approve/Dismiss to-do on the Plugins page, and
		// teach from the returned status. A grant reloads this script
		// automatically.
		switch s.host.notePrivilegeNeeded(priv, s.name, s.name+s.skin.ext) {
		case "granted":
			// Race: approved between the refusal and the request. Retry.
			err = s.host.plugin.Call(method, params, &raw)
		case "denied":
			return nil, fmt.Errorf("call(%s): the '%s' privilege was dismissed earlier — grant it under Settings → Plugins → Scripts → Privileges to change that", method, priv)
		default: // "pending", or the request itself failed
			return nil, fmt.Errorf("call(%s): awaiting approval — '%s' for Scripts is a pending request on the Plugins page; this script resumes when granted", method, priv)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("call(%s): %v", method, err)
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("call(%s): decoding result: %v", method, err)
	}
	return out, nil
}

// scriptEventPrefix is the namespace every script emit lands under. The
// host owns it: the manifest declares `scripts.*.*`, so a script can name
// the last segment and nothing else.
const scriptEventPrefix = "scripts"

// modeCollection holds the exclusive tags scripts enter. Its records ARE the
// tags — that is what a `preset: tag` collection is — so "enter a mode" is a
// record write and "leave" is a delete. No new primitive; the mechanism was
// fully built platform-side and merely unreachable from a script.
const modeCollection = "plugin.scripts.modes"

// modeTag namespaces a script's mode name so two scripts choosing "review"
// do not silently share one gate. A hyphen, not a dot: dots delimit the tag
// namespace this collection owns, and a dotted record name would read as a
// deeper prefix.
//
// A script never sees this string — it says mode("review") and asks for a
// command in "review" — so the encoding is the host's business. Cross-script
// sharing is deliberately NOT expressible: the point of the feature is a
// script owning the spoken surface, and a pasted script quietly stealing
// another's mode is exactly the sloppiness the header exists to catch.
func (s *Script) modeTag(name string) string {
	return s.name + "-" + name
}

// bridgeMode enters an exclusive mode: while it is held, only commands
// requiring it match, and the engine grammar narrows to them. Needs the
// `mode` grant (or the exact `mode:<name>`) — entering suppresses every
// other command in the system, which is a large act for one call.
//
// Replaces this script's current mode rather than stacking: "mode" is
// singular per script, and two exclusive gates held at once would open both
// scripts' gated commands, which is not what either asked for.
//
// The user is never trapped. The collection is not `escape_exempt`, so the
// global escape command clears it like any other exclusive gate, and
// teardown clears it too — a reloaded or deleted script cannot leave the
// spoken surface narrowed to commands that no longer exist.
func (s *Script) bridgeMode(name string) error {
	if !s.grants.allows("mode", name) {
		return fmt.Errorf("mode(%q): not declared — add `%s permissions: mode` (or `mode:%s`) to the top of this file",
			name, s.skin.commentPrefix, name)
	}
	if err := checkSegment("mode name", name); err != nil {
		return fmt.Errorf("mode(%q): %v", name, err)
	}
	if err := checkSegment("script name", s.name); err != nil {
		return fmt.Errorf("mode(%q): %v — rename the file to use modes", name, err)
	}
	s.host.clearModes(s.name)
	tag := s.modeTag(name)
	if err := s.host.plugin.Put(modeCollection, tag, map[string]any{"name": tag}); err != nil {
		s.lastErr = fmt.Sprintf("mode(%s): %v", name, err)
		branchkit.Logf("scripts", "[%s] %s", s.name, s.lastErr)
	}
	return nil
}

// bridgeClearMode leaves whatever mode this script holds. Idempotent, and
// ungated on purpose: getting OUT of an exclusive gate must never be the
// call that gets refused.
func (s *Script) bridgeClearMode() {
	s.host.clearModes(s.name)
}

// bridgeEmit announces an event on the platform bus as
// `scripts.<script>.<event>`, needing the `emit` grant (or the exact
// `emit:<event>`).
//
// This is what makes composition real rather than aspirational. Without it
// a script could react and act but never announce, so script→script was
// impossible and script→plugin worked only where some plugin happened to
// define a dispatchable action type. There was no "say it happened and let
// whoever cares respond", which is the shape the bus exists to provide.
//
// The namespace is not negotiable from a script. The host emits under a
// prefix it owns, on the hosted thing's behalf, exactly as dispatch() loans
// the host's identity under a header gate — and `source` on the wire stays
// `scripts`, because the platform authenticates the plugin, not the file.
// The script's name rides in `on_behalf_of`, which is a label and not a
// principal. A subscriber reading it knows precisely what it is worth: a
// claim by `scripts`, which is the honest amount.
//
// Failure is SOFT, like dispatch and unlike call: an announcement nobody
// happened to want is not a reason to abort the calling code. A refused
// grant stays a hard in-language error.
func (s *Script) bridgeEmit(event string, payload map[string]any) error {
	if !s.grants.allows("emit", event) {
		return fmt.Errorf("emit(%q): not declared — add `%s permissions: emit` (or `emit:%s`) to the top of this file",
			event, s.skin.commentPrefix, event)
	}
	if err := checkSegment("event name", event); err != nil {
		return fmt.Errorf("emit(%q): %v", event, err)
	}
	// The script's own name is a filename the user chose, and it occupies a
	// segment of the event type. A dotted stem would silently emit a
	// four-segment type the manifest pattern does not cover, and the refusal
	// would arrive as an opaque "not declared in manifest" from the platform.
	if err := checkSegment("script name", s.name); err != nil {
		return fmt.Errorf("emit(%q): %v — rename the file to emit events", event, err)
	}
	if payload == nil {
		payload = map[string]any{}
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("emit(%s): %v", event, err)
	}
	eventType := fmt.Sprintf("%s.%s.%s", scriptEventPrefix, s.name, event)
	if err := s.host.plugin.EventsEmit(branchkit.EventsEmitRequest{EventType: eventType, Data: data}); err != nil {
		s.lastErr = fmt.Sprintf("emit(%s): %v", event, err)
		branchkit.Logf("scripts", "[%s] %s", s.name, s.lastErr)
	}
	return nil
}

// checkSegment rejects anything that cannot be one segment of an event type.
// Untrusted text either way — a script chose the event name and a user chose
// the filename — and both land in a namespace the host is answerable for.
func checkSegment(what, v string) error {
	if v == "" {
		return fmt.Errorf("%s is empty", what)
	}
	for _, r := range v {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-'
		if !ok {
			return fmt.Errorf("%s %q must be one segment of letters, digits, '_' or '-'", what, v)
		}
	}
	return nil
}

// --- Collections: the platform's general state primitive, projected -----------
//
// Four clauses, all host-internal. The platform already permits everything here — this is
// the host deciding which of its reach to lend, not a boundary being moved.
//
// The reason it looked like a platform gap: collections carry
// `privilege: null`, and the call() gate read that as "plugin-protocol
// control plane, refuse". But `null` there means CLASSIFIED ELSEWHERE, not
// unclassified — writes are governed by each collection's `writers` policy
// against the caller's identity, reads by interaction grants the user
// toggles. Privileges are simply the wrong axis for them. call() still
// refuses the privilege-less set, and correctly, because it cannot tell
// collections from `commands.push`; the rules live here instead, where the
// per-collection distinction is expressible.

// storeCollection is the host's own per-script persistence. Clause 4: the
// host owns one collection and namespaces it by script, which closes the
// per-hosted-thing storage gap with no platform surface at all and inherits
// reload/delete semantics for free.
const storeCollection = "plugin.scripts.store"

// hostOwnedPrefix is every collection this plugin introduces. Clause 3: a
// script must never write these. The concern is concrete, not theoretical —
// a script writing plugin.scripts.scripts could disable its siblings or
// clear their error state, and one writing plugin.scripts.modes could hold
// a gate it never declared. Reads are refused too: enumerating siblings and
// their failures is the host's bookkeeping, not a script's business.
const hostOwnedPrefix = "plugin.scripts."

// checkCollectionReach applies the clauses the platform cannot express, and
// leaves the ones it can to the platform.
//
// Host-side: the collection must be one the host declared in
// `consumes.collections` (the host does not lend reach it never asked the
// user for), and must not be the host's own (clause 3).
//
// Platform-side, deliberately: whether the collection accepts contributions
// at all. That is the `writers` policy, checked against CallerIdentity in
// the platform, and it is the authoritative answer — duplicating it here
// would be a second implementation to drift.
func (s *Script) checkCollectionReach(verb, collection string) error {
	if strings.HasPrefix(collection, hostOwnedPrefix) {
		return fmt.Errorf("%s(%q): this is the Scripts plugin's own state — use store()/stored() for a script's private data",
			verb, collection)
	}
	if !s.host.declaresCollection(collection) {
		return fmt.Errorf("%s(%q): not in the host manifest's consumed collections — reaching it needs a manifest edit (the static-manifest tradeoff)",
			verb, collection)
	}
	return nil
}

// entry projects a stored record into the neutral form scripts see.
//
// The ENVELOPE, not just the payload. Returning bare payloads was the first
// shape and it was wrong twice over: a script could list records and then
// had no way to address one (the id lives out here, and which payload field
// happens to equal it is knowledge only the OWNING plugin's manifest holds),
// and it could not tell its own contribution from a peer plugin's or from
// the user's on a collection that accepts several writers.
//
// Fields, and what each is worth to a script:
//
//	id            what `record(coll, id)` takes. Round-trips.
//	payload       the data.
//	writer        the plugin that CREATED this record — creation, not last
//	              touch, so a later write by someone else does not transfer
//	              it. `_user` means the person added it through Settings,
//	              `_platform` means the host. This is how a script avoids
//	              clobbering a record it does not own.
//	on_behalf_of  which hosted thing the writer was acting for. Absent for
//	              most reads today: the `data` and `named_entities` presets
//	              are backed by shared-overrides, which stores flat
//	              per-plugin blobs with no per-record envelope, so the
//	              label is dropped on write: a storage gap in the
//	              platform, not this host.
//	revision      monotonic per-record write counter; 0 where the backend
//	              keeps none.
//	timestamp_ms  last write, unix ms; 0 where the backend keeps none.
//
// `group` is deliberately not projected: it labels a writer's named
// replace-set, scripts cannot push one, and a field you can read but never
// set is furniture.
func entry(rec *branchkit.CollectionRecord) (any, error) {
	payload, err := decodeJSON(rec.Payload)
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"id":           rec.ID,
		"payload":      payload,
		"writer":       rec.Writer,
		"revision":     rec.Revision,
		"timestamp_ms": rec.TimestampMs,
	}
	// Absent rather than empty when the backend kept no label — "nobody
	// recorded one" and "recorded as blank" must not read the same.
	if rec.OnBehalfOf != nil && *rec.OnBehalfOf != "" {
		out["on_behalf_of"] = *rec.OnBehalfOf
	}
	return out, nil
}

// bridgeRecord reads one record from a collection. Clause 1: the host reads
// what it is granted to read, which is existing, user-visible machinery —
// nothing new to enforce, so the grant here is hygiene about which scripts
// use it.
//
// Returns the same entry shape `records()` yields, so a script can take one
// out of a list and pass its id straight back here. Two functions over the
// same data returning two shapes is the confusion worth spending a `.payload`
// to avoid.
func (s *Script) bridgeRecord(collection, id string) (any, error) {
	// Reach BEFORE grant, deliberately: neither refusal reach produces is
	// fixable by adding a header token, so leading with "add read:<x>"
	// would name a token that can never work.
	if err := s.checkCollectionReach("record", collection); err != nil {
		return nil, err
	}
	if !s.grants.allows("read", collection) {
		return nil, fmt.Errorf("record(%q): not declared — add `%s permissions: read` (or `read:%s`) to the top of this file",
			collection, s.skin.commentPrefix, collection)
	}
	rec, err := s.host.plugin.Get(collection, id)
	if err != nil {
		return nil, fmt.Errorf("record(%s, %s): %v", collection, id, err)
	}
	if rec == nil {
		return nil, nil
	}
	return entry(rec)
}

// bridgeRecords reads every record in a collection. Reads are HARD-failing
// like call(), not soft like dispatch: a read feeds control flow, and a
// silently-empty list is a wrong branch, not a missed side effect.
func (s *Script) bridgeRecords(collection string, opts map[string]any) (any, error) {
	if err := s.checkCollectionReach("records", collection); err != nil {
		return nil, err
	}
	if !s.grants.allows("read", collection) {
		return nil, fmt.Errorf("records(%q): not declared — add `%s permissions: read` (or `read:%s`) to the top of this file",
			collection, s.skin.commentPrefix, collection)
	}
	// `limit` bounds the read; `writer` scopes it to one contributor —
	// usually this plugin, which is how a script reads back its own
	// contributions to a shared collection without fetching everyone's and
	// filtering by hand.
	b := branchkit.NewListOpts()
	var limited bool
	if n, ok := numberOpt(opts, "limit"); ok {
		b = b.Limit(n)
		limited = true
	}
	if w, ok := opts["writer"].(string); ok && w != "" {
		b = b.Writer(w)
	}
	recs, total, err := s.host.plugin.ListPage(collection, b.Build())
	if err != nil {
		return nil, fmt.Errorf("records(%s): %v", collection, err)
	}
	// The platform caps an unbounded read at its own default. That cap is
	// logged platform-side, but the script author's surface is their row —
	// tell them there, where they will actually look, and only when they
	// did not ask for a limit themselves.
	if !limited && len(recs) < total {
		s.lastErr = fmt.Sprintf("records(%q): returned %d of %d — pass { limit = n } to page, or the read stays capped",
			collection, len(recs), total)
		branchkit.Logf("scripts", "[%s] %s", s.name, s.lastErr)
	}
	out := make([]any, 0, len(recs))
	for i := range recs {
		v, err := entry(&recs[i])
		if err != nil {
			return nil, fmt.Errorf("records(%s): %v", collection, err)
		}
		out = append(out, v)
	}
	return out, nil
}

// bridgePut contributes a record to a collection that accepts contributions.
// Clause 2: only what the host declares, and never what the host owns.
//
// Whether the collection actually accepts a write from this caller is the
// platform's `writers` policy and stays there — an `introducer_only`
// collection refuses, and the refusal surfaces as the error below.
func (s *Script) bridgePut(collection, id string, payload map[string]any) error {
	if err := s.checkCollectionReach("put", collection); err != nil {
		return err
	}
	if !s.grants.allows("write", collection) {
		return fmt.Errorf("put(%q): not declared — add `%s permissions: write` (or `write:%s`) to the top of this file",
			collection, s.skin.commentPrefix, collection)
	}
	if payload == nil {
		payload = map[string]any{}
	}
	if err := s.host.plugin.Put(collection, id, payload); err != nil {
		return fmt.Errorf("put(%s, %s): %v", collection, id, err)
	}
	return nil
}

// bridgeDrop deletes a record the script's writes created. Same clauses; the
// platform decides whether this caller may delete it.
func (s *Script) bridgeDrop(collection, id string) error {
	if err := s.checkCollectionReach("drop", collection); err != nil {
		return err
	}
	if !s.grants.allows("write", collection) {
		return fmt.Errorf("drop(%q): not declared — add `%s permissions: write` (or `write:%s`) to the top of this file",
			collection, s.skin.commentPrefix, collection)
	}
	if _, err := s.host.plugin.Delete(collection, id); err != nil {
		return fmt.Errorf("drop(%s, %s): %v", collection, id, err)
	}
	return nil
}

// storeKey namespaces a script's storage key. Same hyphen convention as
// mode tags, and the same reason: one script must not read or overwrite
// another's, and the encoding is the host's business because a script never
// sees it.
func (s *Script) storeKey(key string) string {
	return s.name + "-" + key
}

// bridgeStore saves a value under a key private to this script. A nil value
// deletes.
//
// Ungated, like command/timer/log: it reaches nothing outside the script's
// own namespace inside a collection the host owns, so there is no boundary
// for a header to disclose. The values are JSON-encoded because a collection
// field is typed and script values are not.
func (s *Script) bridgeStore(key string, value any) error {
	if err := checkSegment("store key", key); err != nil {
		return fmt.Errorf("store(%q): %v", key, err)
	}
	if err := checkSegment("script name", s.name); err != nil {
		return fmt.Errorf("store(%q): %v — rename the file to use storage", key, err)
	}
	id := s.storeKey(key)
	if value == nil {
		if _, err := s.host.plugin.Delete(storeCollection, id); err != nil {
			return fmt.Errorf("store(%s): %v", key, err)
		}
		return nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("store(%s): %v", key, err)
	}
	rec := map[string]any{"key": id, "value": string(encoded)}
	if err := s.host.plugin.Put(storeCollection, id, rec); err != nil {
		return fmt.Errorf("store(%s): %v", key, err)
	}
	return nil
}

// bridgeStored reads back what store() saved, or nil when nothing is there.
func (s *Script) bridgeStored(key string) (any, error) {
	if err := checkSegment("store key", key); err != nil {
		return nil, fmt.Errorf("stored(%q): %v", key, err)
	}
	rec, err := s.host.plugin.Get(storeCollection, s.storeKey(key))
	if err != nil {
		return nil, fmt.Errorf("stored(%s): %v", key, err)
	}
	if rec == nil {
		return nil, nil
	}
	var envelope struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(rec.Payload, &envelope); err != nil {
		return nil, fmt.Errorf("stored(%s): %v", key, err)
	}
	if envelope.Value == "" {
		return nil, nil
	}
	var out any
	if err := json.Unmarshal([]byte(envelope.Value), &out); err != nil {
		return nil, fmt.Errorf("stored(%s): %v", key, err)
	}
	return out, nil
}

// numberOpt reads a numeric option out of a script-supplied table. Every
// skin hands numbers over as float64 (JSON's only number type, and Lua's),
// so that is the one case worth converting.
func numberOpt(opts map[string]any, key string) (int, bool) {
	switch v := opts[key].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	}
	return 0, false
}

// decodeJSON turns a stored payload into the neutral form each skin converts
// into its own values.
func decodeJSON(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// --- Effects: the contention channel ------------------------------------------
//
// Effects are how several parties negotiate one shared resource —
// suppress_notifications, disable_screen_dim, suppress_huds and
// the ownership stack behind them. Hosted things could not participate at
// all, which made them an audience for contention rather than a party to it.
//
// The ceiling is the manifest's `provides.effects[*].asserts`, a closed set
// validated against the platform registry at load. Subtractive only, like
// every host gate: a header token for an undeclared effect gets nothing,
// because the plugin holds nothing to lend.
//
// Deliberately NOT declared: `suppress_keybinds`, which pauses every global
// hotkey including Option+G and Option+T — the recovery tools. The platform
// releases a dead PLUGIN's frames, but a script dying is not the plugin
// dying, so a script could pause every hotkey and leave no way back. Also
// `pause_microphone_indicator`: quietly hiding the system's mic light is not
// something a pasted script should be able to do. Both are defaults rather
// than boundaries — a plugin offering either as an action type, plus a user
// granting this host dispatch to it, routes around them legitimately.

// bridgeHold asserts an effect for as long as the script wants it. Needs the
// `effect` grant (or the exact `effect:<name>`).
//
// The result carries `enforced`, which callers must actually read: an effect
// whose OS handler has not landed still grants ownership, displacement
// events and queryability, but the system behavior does NOT happen. A script
// that assumes otherwise silently believes it muted notifications.
func (s *Script) bridgeHold(name string) (any, error) {
	// A name under this host's own namespace is a coordination name the
	// scripts themselves mint — self-coordination confers no capability
	// (the platform implements nothing for it and the definer is the
	// asserter), so the header gate would be self-referential. Platform
	// and other plugins' effects keep the gate: those are a real second
	// party. Mirrors emit, which gates by namespace rather than per name.
	selfMinted := strings.HasPrefix(name, "scripts.")
	if !selfMinted && !s.grants.allows("effect", name) {
		return nil, fmt.Errorf("hold(%q): not declared — add `%s permissions: effect` (or `effect:%s`) to the top of this file",
			name, s.skin.commentPrefix, name)
	}
	// Recorded BEFORE the assert would matter: `already_held` comes back true
	// when a SIBLING script already holds the plugin's frame, and this
	// script is just as much a holder for it. Reading the platform's answer
	// as "someone else has it, so I do not" is what would let that sibling's
	// release drop the frame out from under this one.
	alreadyMine := s.host.holdsEffect(name, s.name)
	out, err := s.host.plugin.AssertEffect(name)
	if err != nil {
		return nil, fmt.Errorf("hold(%s): %v", name, err)
	}
	if out.Granted {
		s.host.noteEffectHolder(name, s.name)
	}
	return map[string]any{
		"granted": out.Granted,
		// Reported at SCRIPT granularity, not the platform's plugin
		// granularity: "did I already have this" is the question a script is
		// actually asking, and answering with a sibling's state would be a
		// lie about its own.
		"already_held": alreadyMine,
		"displaced":    out.Displaced,
		"enforced":     out.Enforced,
	}, nil
}

// bridgeRelease gives an effect back. Ungated for the same reason
// clear_mode() is: letting go must never be the call that gets refused.
// Idempotent — releasing what you do not hold is not an error.
func (s *Script) bridgeRelease(name string) (any, error) {
	// Retract the PLUGIN's frame only when the last script lets go. A
	// script that never held it, or one whose sibling still wants it, gets
	// a truthful `retracted: false` rather than yanking a frame someone
	// else is relying on.
	if !s.host.dropEffectHolder(name, s.name) {
		return map[string]any{"retracted": false, "new_owner": ""}, nil
	}
	retracted, newOwner, err := s.host.plugin.RetractEffect(name)
	if err != nil {
		return nil, fmt.Errorf("release(%s): %v", name, err)
	}
	return map[string]any{"retracted": retracted, "new_owner": newOwner}, nil
}

// bridgeHolder reports whether this HOST holds the effect and who the
// effective owner is. Ungated: it is a read of state the platform already
// broadcasts to every holder on displacement.
//
// `active` answers at plugin granularity, not script granularity, and that
// is the honest answer — the platform's ownership stack has one frame per
// plugin, so two scripts asserting the same effect are one holder as far as
// it is concerned. `mine` is the host's own bookkeeping about which script
// asked last, and it is exactly as trustworthy as any host-side label.
func (s *Script) bridgeHolder(name string) (any, error) {
	active, owner, err := s.host.plugin.IsEffectActive(name)
	if err != nil {
		return nil, fmt.Errorf("holder(%s): %v", name, err)
	}
	return map[string]any{
		"active": active,
		"owner":  owner,
		"mine":   s.host.holdsEffect(name, s.name),
	}, nil
}

// toOptions coerces a skin's list-ish value into option strings. Numbers
// and booleans are accepted and stringified — a script listing 1, 2, 3 as
// choices means the digits, and refusing that would be pedantry.
func toOptions(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		switch x := it.(type) {
		case string:
			out = append(out, x)
		case nil:
			// skip
		default:
			out = append(out, fmt.Sprint(x))
		}
	}
	return out
}

// bridgeChoose asks the user to pick one of `options` and hands the chosen
// one to `deliver`. Needs the `choose` grant.
//
// Gated, and gated on its own token rather than folded into `hud`, because
// of what it takes rather than what it shows: a chooser holds an EXCLUSIVE
// gate, so while it is open the options are the only thing the user can
// say. That is the same act `mode` gates, and it should be as visible in
// the header.
//
// `deliver` receives the chosen option, or nil if nobody answered before
// the timeout. nil is a real answer — a callback that silently never fires
// leaves the script wedged with no way to know why.
func (s *Script) bridgeChoose(options []string, deliver func(pick any) error) error {
	if !s.grants.allows("choose", "") {
		return fmt.Errorf("choose(): not declared — add `%s permissions: choose` to the top of this file",
			s.skin.commentPrefix)
	}
	return s.host.beginChoose(s.name, options, func(pick any) error {
		// Back into the VM under the script's lock, and never after the
		// script has closed — the same discipline timers and event
		// handlers use, for the same reason: the answer can arrive at any
		// time, including after a reload.
		s.mu.Lock()
		closed := s.closed
		s.mu.Unlock()
		if closed {
			return nil
		}
		return deliver(pick)
	})
}

// bridgeHud pushes an HTML fragment to the scripts HUD window and shows
// it. Needs the `hud` grant.
func (s *Script) bridgeHud(htmlFragment string) error {
	if !s.grants.allows("hud", "") {
		return fmt.Errorf("hud(): not declared — add `%s permissions: hud` to the top of this file",
			s.skin.commentPrefix)
	}
	p := s.host.plugin
	if err := p.HUDPushFragment("scripts", "content", htmlFragment); err != nil {
		s.lastErr = fmt.Sprintf("hud: %v", err)
		branchkit.Logf("scripts", "[%s] %s", s.name, s.lastErr)
		return nil
	}
	if err := p.HUDShow(branchkit.HUDShowRequest{Channel: "scripts"}); err != nil {
		branchkit.Logf("scripts", "[%s] hud.show: %v", s.name, err)
	}
	return nil
}

// bridgeTimer schedules a timer and returns its id — the token cancel()
// takes. The callback re-enters the VM under the script's lock and is
// cancelled if the script reloads. A repeating timer re-arms only after
// the callback returns, so a slow callback can never overlap itself; a
// fired one-shot removes itself from the table so ids don't accumulate.
func (s *Script) bridgeTimer(seconds float64, repeat bool, run func() error) string {
	s.nextID++
	id := fmt.Sprintf("t%d", s.nextID)
	d := time.Duration(seconds * float64(time.Second))
	var fire func()
	fire = func() {
		_ = s.call(run)
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, live := s.timers[id]; !live || s.closed {
			return // cancelled from the callback, or torn down
		}
		if repeat {
			s.timers[id] = time.AfterFunc(d, fire)
		} else {
			delete(s.timers, id)
		}
	}
	s.timers[id] = time.AfterFunc(d, fire)
	return id
}

// bridgeCancel stops a timer by id. Unknown ids (already fired one-shots,
// double cancels) are a no-op — cancel is idempotent by design.
func (s *Script) bridgeCancel(id string) {
	if t, ok := s.timers[id]; ok {
		t.Stop()
		delete(s.timers, id)
	}
}

// bridgeLog writes a plugin log line tagged with the script's name.
func (s *Script) bridgeLog(msg string) {
	branchkit.Logf("scripts", "[%s] %s", s.name, msg)
}

// patternSlots builds command slots from the neutral pattern form: a
// string of words, or a sequence of words and alternatives groups.
func patternSlots(v any) ([]branchkit.PatternSlot, error) {
	switch t := v.(type) {
	case string:
		words := strings.Fields(t)
		if len(words) == 0 {
			return nil, fmt.Errorf("empty pattern")
		}
		slots := make([]branchkit.PatternSlot, 0, len(words))
		for _, w := range words {
			slots = append(slots, branchkit.Word(w))
		}
		return slots, nil
	case []any:
		if len(t) == 0 {
			return nil, fmt.Errorf("empty pattern")
		}
		slots := make([]branchkit.PatternSlot, 0, len(t))
		for i, e := range t {
			switch el := e.(type) {
			case string:
				slots = append(slots, branchkit.Word(el))
			case []any:
				alts := make([]string, 0, len(el))
				for _, a := range el {
					if w, ok := a.(string); ok {
						alts = append(alts, w)
					}
				}
				if len(alts) == 0 {
					return nil, fmt.Errorf("slot %d: empty alternatives group", i+1)
				}
				slots = append(slots, branchkit.OneOf(alts...))
			default:
				return nil, fmt.Errorf("slot %d: expected word or alternatives list", i+1)
			}
		}
		return slots, nil
	}
	return nil, fmt.Errorf("expected string or list pattern, got %T", v)
}

// patternDisplay renders a neutral pattern for the settings tab and
// reports whether the Run button can invoke it (capture tokens need the
// matcher to bind them, so those are display-only).
func patternDisplay(v any) (string, bool) {
	words := []string{}
	runnable := true
	add := func(w string) {
		if strings.HasPrefix(w, "<") {
			runnable = false
		}
		words = append(words, w)
	}
	switch t := v.(type) {
	case string:
		for _, w := range strings.Fields(t) {
			add(w)
		}
	case []any:
		for _, e := range t {
			switch el := e.(type) {
			case string:
				add(el)
			case []any:
				alts := []string{}
				for _, a := range el {
					if w, ok := a.(string); ok {
						alts = append(alts, w)
					}
				}
				add(strings.Join(alts, "/"))
			}
		}
	}
	return strings.Join(words, " "), runnable
}

// --- fetch -------------------------------------------------------------------

// Outbound HTTP for scripts.
//
// The grant here is REAL ENFORCEMENT, not the hygiene the rest of the header
// is. Every other capability is bounded by the platform underneath it — the
// platform refuses what the plugin may not do regardless of what a header
// says. Network is the one axis where the platform's boundary is COARSER than
// the per-script claim: host grants are per PLUGIN, so the platform sees
// "scripts may reach api.example.com" and cannot see which script asked. The
// narrowing therefore has to hold here or it does not hold at all.
//
// That is only defensible because no skin has another way out: Lua and JS have
// no network primitive in their VMs, and Python's raw sockets are denied by the
// sandbox even at the `hosts` tier, whose Seatbelt profile is `(deny network*)`
// plus the proxy's unix socket alone (measured 2026-09-20).

const (
	fetchDefaultTimeout = 5 * time.Second
	fetchMaxTimeout     = 30 * time.Second
	fetchMaxBody        = 5 << 20 // 5 MiB into a single-threaded VM is already generous
)

// fetchOptions is the neutral form of a skin's second argument.
type fetchOptions struct {
	Method  string
	Headers map[string]string
	// Secrets are headers whose value is a `secret_ref(name, prefix)` —
	// a NAME the platform substitutes, never a value the script holds.
	Secrets map[string]secretRef
	Body    any     // string sent raw; anything else JSON-encoded
	Timeout float64 // seconds; 0 means the default
}

// secretRef is what `secret_ref(name, prefix)` builds in every skin: a
// marker object `{__secret: name, prefix: prefix}`. The script never holds
// the value — it cannot, since the platform never returns one.
type secretRef struct {
	Name   string
	Prefix string
}

// platform is the plugin connection, or nil for a script with no host (unit
// tests build scripts that way).
func (s *Script) platform() *branchkit.Plugin {
	if s.host == nil {
		return nil
	}
	return s.host.plugin
}

// declaredSecrets maps each `secret:<name>@<host>` header line to its host.
// A script may only spend a secret it declared, and only at that host.
func (g grants) declaredSecrets() map[string]string {
	out := map[string]string{}
	for tok := range g.caps {
		rest, ok := strings.CutPrefix(tok, "secret:")
		if !ok {
			continue
		}
		name, host, ok := strings.Cut(rest, "@")
		if ok && name != "" && host != "" {
			out[name] = strings.ToLower(strings.TrimSuffix(host, "."))
		}
	}
	return out
}

// slotName is a script's secret in the platform's per-plugin store. The
// script's own file names the prefix, taken from the host's record of which
// script is calling — never from the script — so one script cannot name
// another's secret. Characters the store refuses become `-`.
func (s *Script) slotName(name string) string {
	clean := func(v string) string {
		return strings.Map(func(r rune) rune {
			if r < 128 && (r == '_' || r == '-' || r == '.' ||
				(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
				return r
			}
			return '-'
		}, v)
	}
	return "script." + clean(s.name) + "." + clean(name)
}

// requestNetwork asks the platform for what this script's header names:
// each exact `fetch:` host (a wildcard cannot be requested — it is requested
// per concrete host when a fetch reaches it) and, for each `secret:`, its
// host and a credential row. Everything arrives OFF on the Scripts plugin
// page; the user allows hosts there and pastes values there. Failures are
// logged, not fatal: a script without network still runs its other parts.
func (s *Script) requestNetwork() {
	p := s.platform()
	if p == nil {
		return
	}
	reason := "used by " + s.file()
	hosts := map[string]bool{}
	for tok := range s.grants.caps {
		if host, ok := strings.CutPrefix(tok, "fetch:"); ok && host != "*" && !strings.HasPrefix(host, "*.") && host != "none" {
			hosts[strings.ToLower(host)] = true
		}
	}
	for _, host := range s.grants.declaredSecrets() {
		hosts[host] = true
	}
	for host := range hosts {
		if _, err := p.NetworkRequestHost(branchkit.NetworkRequestHostRequest{Host: host, Reason: &reason}); err != nil {
			branchkit.Logf("scripts", "[%s] network request for %s: %v", s.name, host, err)
		}
	}
	for name, host := range s.grants.declaredSecrets() {
		label := s.file() + " — " + name
		if _, err := p.SecretsRequestSlot(branchkit.SecretsRequestSlotRequest{Host: host, Name: s.slotName(name), Label: &label}); err != nil {
			branchkit.Logf("scripts", "[%s] secret slot %s: %v", s.name, name, err)
		}
	}
}

// hostAllowedByGrants matches a host against the script's declared `fetch:`
// entries, with the same shape the platform's own allowlist uses
// (the platform's per-plugin network proxy): a bare `*` matches anything, `*.suffix`
// matches sub-domains but pointedly NOT the bare parent, everything else is an
// exact case-insensitive match. Keeping the two in step matters — a script
// author reads one rule and the platform enforces the other half of it.
func hostAllowedByGrants(host string, declared []string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, tok := range declared {
		name, entry, ok := strings.Cut(tok, ":")
		if !ok || name != "fetch" {
			continue
		}
		entry = strings.ToLower(entry)
		switch {
		case entry == "*":
			return true
		case strings.HasPrefix(entry, "*."):
			suffix := entry[2:]
			if len(host) > len(suffix)+1 && strings.HasSuffix(host, "."+suffix) {
				return true
			}
		case entry == host:
			return true
		}
	}
	return false
}

// bridgeFetch performs one outbound request on the calling script's behalf.
//
// Two failure kinds, deliberately different, because conflating them makes a
// typo look like an outage and an outage look like a permissions bug:
//
//   - REFUSED (host not declared, bad URL, redirect off the allowlist) and
//     TRANSPORT failure (timeout, DNS, connection refused) return an error,
//     which the skin raises in-language.
//   - Any HTTP status is a normal return. `status` is data; a script that
//     cares checks it.
func (s *Script) bridgeFetch(rawURL string, opt fetchOptions) (map[string]any, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("fetch(%q): not a valid absolute URL", rawURL)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("fetch(%q): only http and https, got %q", rawURL, u.Scheme)
	}
	declared := s.grants.list()
	if !hostAllowedByGrants(u.Hostname(), declared) {
		return nil, fmt.Errorf(
			"fetch(%s): host not declared — add `%s permissions: fetch:%s` to the top of this file",
			u.Hostname(), s.skin.commentPrefix, u.Hostname())
	}
	if len(opt.Secrets) > 0 {
		return s.fetchWithSecrets(u, opt)
	}
	// A host reached through a wildcard grant was never requested by name at
	// load; ask now (idempotent), so the user sees exactly this host on the
	// Scripts page rather than a refusal with nothing to switch on.
	if p := s.platform(); p != nil {
		reason := "used by " + s.file()
		if r, err := p.NetworkRequestHost(branchkit.NetworkRequestHostRequest{Host: u.Hostname(), Reason: &reason}); err == nil && r != nil && r.Status == "pending" {
			return nil, fmt.Errorf(
				"fetch(%s): waiting for you to allow %s on the Scripts plugin page (Settings → Plugins → Scripts)",
				u.Hostname(), u.Hostname())
		}
	}

	timeout := fetchDefaultTimeout
	if opt.Timeout > 0 {
		timeout = time.Duration(opt.Timeout * float64(time.Second))
		if timeout > fetchMaxTimeout {
			timeout = fetchMaxTimeout
		}
	}

	method := strings.ToUpper(strings.TrimSpace(opt.Method))
	if method == "" {
		method = http.MethodGet
	}

	var body io.Reader
	jsonBody := false
	switch v := opt.Body.(type) {
	case nil:
	case string:
		body = strings.NewReader(v)
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("fetch(%s): body is not encodable as JSON: %v", u.Hostname(), err)
		}
		body = strings.NewReader(string(encoded))
		jsonBody = true
	}

	req, err := http.NewRequest(method, rawURL, body)
	if err != nil {
		return nil, fmt.Errorf("fetch(%s): %v", u.Hostname(), err)
	}
	if jsonBody {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range opt.Headers {
		req.Header.Set(k, v)
	}

	// Redirects are NOT followed. A 302 from a declared host to an undeclared
	// one would otherwise walk straight around the grant above — and the
	// platform could not catch it either, since its allowlist is the union
	// across every script. A script that wants to follow one reads `status`
	// and the Location header and calls again, which puts the new host through
	// this same check. The default transport is the SDK's, already routed
	// through BRANCHKIT_PROXY at package init (plugin-sdk-go/proxy.go).
	client := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch(%s): %v", u.Hostname(), err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, fetchMaxBody))
	if err != nil {
		return nil, fmt.Errorf("fetch(%s): reading the response: %v", u.Hostname(), err)
	}

	headers := map[string]any{}
	for k := range resp.Header {
		headers[strings.ToLower(k)] = resp.Header.Get(k)
	}

	out := map[string]any{
		"status":  float64(resp.StatusCode),
		"body":    string(raw),
		"headers": headers,
	}
	// `json` is present only when the server said so AND it parses — a script
	// reading res.json gets a value or nil, never a half-decoded surprise.
	if ct := resp.Header.Get("Content-Type"); strings.Contains(strings.ToLower(ct), "json") {
		var parsed any
		if json.Unmarshal(raw, &parsed) == nil {
			out["json"] = parsed
		}
	}
	return out, nil
}

// fetchWithSecrets performs a fetch whose headers name secrets. The script
// never holds a value, so the request is handed to the PLATFORM
// (`http.request`): it substitutes each secret — only if it is bound to this
// exact host — and performs the request in its sandboxed fetch sidecar
// through this plugin's proxy. Redirects come back unfollowed, and any echo
// of a secret in the response arrives as "[redacted]".
func (s *Script) fetchWithSecrets(u *url.URL, opt fetchOptions) (map[string]any, error) {
	host := strings.ToLower(u.Hostname())
	if u.Scheme != "https" {
		return nil, fmt.Errorf("fetch(%s): a request carrying a secret must be https", host)
	}
	declared := s.grants.declaredSecrets()
	var headers []branchkit.HttpHeader
	for k, v := range opt.Headers {
		value := v
		headers = append(headers, branchkit.HttpHeader{Name: k, Value: &value})
	}
	for k, ref := range opt.Secrets {
		bound, ok := declared[ref.Name]
		if !ok {
			return nil, fmt.Errorf(
				"fetch(%s): secret %q not declared — add `%s permissions: secret:%s@%s` to the top of this file",
				host, ref.Name, s.skin.commentPrefix, ref.Name, host)
		}
		if bound != host {
			return nil, fmt.Errorf(
				"fetch(%s): secret %q is declared for %s, and is only ever sent there", host, ref.Name, bound)
		}
		slot, prefix := s.slotName(ref.Name), ref.Prefix
		headers = append(headers, branchkit.HttpHeader{Name: k, Secret: &slot, Prefix: &prefix})
	}
	var body *string
	switch v := opt.Body.(type) {
	case nil:
	case string:
		body = &v
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("fetch(%s): body is not encodable as JSON: %v", host, err)
		}
		text := string(encoded)
		body = &text
		ct := "application/json"
		headers = append(headers, branchkit.HttpHeader{Name: "Content-Type", Value: &ct})
	}
	method := strings.ToUpper(strings.TrimSpace(opt.Method))
	if method == "" {
		method = "GET"
	}
	timeoutMs := int(fetchDefaultTimeout / time.Millisecond)
	if opt.Timeout > 0 {
		timeoutMs = int(opt.Timeout * 1000)
	}
	p := s.platform()
	if p == nil {
		return nil, fmt.Errorf("fetch(%s): not connected to the platform", host)
	}
	resp, err := p.HttpRequest(branchkit.HttpRequestRequest{URL: u.String(), Body: body, Headers: headers, Method: &method, TimeoutMs: &timeoutMs})
	if err != nil {
		return nil, fmt.Errorf("fetch(%s): %v", host, err)
	}
	text := ""
	if resp.Body != nil {
		text = *resp.Body
	} else if raw, err := base64.StdEncoding.DecodeString(resp.BodyBase64); err == nil {
		text = string(raw)
	}
	respHeaders := map[string]any{}
	for _, h := range resp.Headers {
		respHeaders[strings.ToLower(h.Name)] = h.Value
	}
	out := map[string]any{
		"status":  float64(resp.Status),
		"body":    text,
		"headers": respHeaders,
	}
	if ct, _ := respHeaders["content-type"].(string); strings.Contains(strings.ToLower(ct), "json") {
		var parsed any
		if json.Unmarshal([]byte(text), &parsed) == nil {
			out["json"] = parsed
		}
	}
	return out, nil
}

// fetchOptionsFrom converts a skin's option table/object into the neutral
// form. Every skin hands over `map[string]any` — the conversion from its own
// type system is the skin's job, deciding what the keys MEAN is this one's, so
// the three cannot drift on defaults.
func fetchOptionsFrom(opts map[string]any) fetchOptions {
	out := fetchOptions{}
	if opts == nil {
		return out
	}
	if v, ok := opts["method"].(string); ok {
		out.Method = v
	}
	if v, ok := opts["timeout"].(float64); ok {
		out.Timeout = v
	}
	out.Body = opts["body"]
	if h, ok := opts["headers"].(map[string]any); ok {
		out.Headers = map[string]string{}
		for k, v := range h {
			switch tv := v.(type) {
			case string:
				out.Headers[k] = tv
			case map[string]any:
				// `secret_ref(name, prefix)` in any skin.
				if name, ok := tv["__secret"].(string); ok {
					prefix, _ := tv["prefix"].(string)
					if out.Secrets == nil {
						out.Secrets = map[string]secretRef{}
					}
					out.Secrets[k] = secretRef{Name: name, Prefix: prefix}
					continue
				}
				out.Headers[k] = fmt.Sprint(v)
			default:
				out.Headers[k] = fmt.Sprint(v)
			}
		}
	}
	return out
}
