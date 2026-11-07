package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	branchkit "github.com/branchkit/plugin-sdk-go"
)

// Two scripts whose names and keys split the same hyphenated text at
// different places name different records, and every id reads back.
func TestScopedNamesNeverCollide(t *testing.T) {
	a := collScript("focus", "", "").storeKey("timer-x")
	b := collScript("focus-timer", "", "").storeKey("x")
	if a == b {
		t.Fatalf("both scripts' keys are %q", a)
	}
	ma := modeScript("a", "-- permissions: mode\n").modeTag("b-c")
	mb := modeScript("a-b", "-- permissions: mode\n").modeTag("c")
	if ma == mb {
		t.Fatalf("both scripts' modes are %q", ma)
	}
	for _, c := range [][2]string{{"focus", "timer-x"}, {"focus-timer", "x"}, {"9lives", "a"}, {"a", "1-a-b"}} {
		script, name, ok := splitScoped(scopedName(c[0], c[1]))
		if !ok || script != c[0] || name != c[1] {
			t.Errorf("splitScoped(scopedName(%q, %q)) = %q, %q, %v", c[0], c[1], script, name, ok)
		}
	}
	for _, id := range []string{"focus-x", "x", "3-ab", "0--x", "-1-a-x"} {
		if _, _, ok := splitScoped(id); ok {
			t.Errorf("splitScoped(%q) read an id not in the scoped form", id)
		}
	}
}

// memStore answers the collection calls the store uses, from memory.
type memStore struct {
	records map[string]json.RawMessage // id → payload, one collection
}

func (m *memStore) Call(method string, params any, result any) error {
	var out any
	switch method {
	case branchkit.MethodCollectionList:
		recs := []map[string]any{}
		ids := make([]string, 0, len(m.records))
		for id := range m.records {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			recs = append(recs, map[string]any{"id": id, "payload": m.records[id]})
		}
		out = map[string]any{"records": recs, "total": len(recs)}
	case branchkit.MethodCollectionPut:
		req := params.(*branchkit.CollectionPutRequest)
		for _, e := range req.Entries {
			m.records[e.ID] = e.Payload
		}
		out = map[string]any{"count": len(req.Entries)}
	case branchkit.MethodCollectionDeleteRecords:
		req := params.(*branchkit.CollectionDeleteRecordsRequest)
		n := 0
		for _, id := range req.Ids {
			if _, ok := m.records[id]; ok {
				delete(m.records, id)
				n++
			}
		}
		out = map[string]any{"deleted": n, "already_absent": len(req.Ids) - n}
	case branchkit.MethodCollectionFetch:
		req := params.(*branchkit.CollectionFetchRequest)
		if payload, ok := m.records[req.ID]; ok {
			out = map[string]any{"record": map[string]any{"id": req.ID, "payload": payload}}
		} else {
			out = map[string]any{}
		}
	default:
		return branchkit.ErrDetached
	}
	raw, _ := json.Marshal(out)
	return json.Unmarshal(raw, result)
}

// A mode entered by voice is written by the platform from the command's
// `sets_tags`, so its record holds only `name`, the scoped id, and no
// `script` field. The id still names its owner: teardown must clear it and
// the Settings row must show it, while another script's voice-entered mode
// stays put.
func TestVoiceEnteredModesBelongToTheScriptTheirIdNames(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"focus.lua", "focus-timer.lua"} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rec := func(fields map[string]any) json.RawMessage {
		raw, _ := json.Marshal(fields)
		return raw
	}
	focus := modeScript("focus", "-- permissions: mode\n").modeTag("review")
	timer := modeScript("focus-timer", "-- permissions: mode\n").modeTag("review")
	store := &memStore{records: map[string]json.RawMessage{
		focus: rec(map[string]any{"name": focus}),
		timer: rec(map[string]any{"name": timer}),
	}}
	p := branchkit.NewDetachedPlugin()
	p.Methods = branchkit.NewMethods(store)
	h := &Host{plugin: p, dir: dir}

	modes, _, _ := h.heldByScript()
	if modes["focus"] != "review" || modes["focus-timer"] != "review" {
		t.Errorf("held modes = %v, want both scripts in review", modes)
	}

	h.clearModes("focus")
	if _, ok := store.records[focus]; ok {
		t.Errorf("clearModes(focus) left its voice-entered mode %q", focus)
	}
	if _, ok := store.records[timer]; !ok {
		t.Errorf("clearModes(focus) removed focus-timer's mode %q", timer)
	}
}
