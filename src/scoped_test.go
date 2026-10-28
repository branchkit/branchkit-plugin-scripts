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

// Records stored under the old `<script>-<key>` id move to the scoped id at
// start: by their owner field, or, older than that, by the longest script
// file name the id starts with. A record already moved, or one no script
// owns, stays where it is.
func TestStoreKeysMoveToTheScopedForm(t *testing.T) {
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
	store := &memStore{records: map[string]json.RawMessage{
		// The field says focus, though focus-timer is the longer prefix.
		"focus-timer-x": rec(map[string]any{"key": "focus-timer-x", "value": `1`, "script": "focus"}),
		// No field: the longest name decides.
		"focus-timer-y": rec(map[string]any{"key": "focus-timer-y", "value": `2`}),
		"gone-z":        rec(map[string]any{"key": "gone-z", "value": `3`}),
		"5-focus-w":     rec(map[string]any{"key": "5-focus-w", "value": `4`, "script": "focus", "name": "w"}),
	}}
	p := branchkit.NewDetachedPlugin()
	p.Methods = branchkit.NewMethods(store)
	h := &Host{plugin: p, dir: dir}
	h.migrateStoreKeys()

	ids := []string{}
	for id := range store.records {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	want := []string{"11-focus-timer-y", "5-focus-timer-x", "5-focus-w", "gone-z"}
	if len(ids) != len(want) {
		t.Fatalf("ids = %q, want %q", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("ids = %q, want %q", ids, want)
		}
	}
	var moved struct{ Key, Value, Script, Name string }
	_ = json.Unmarshal(store.records["5-focus-timer-x"], &moved)
	if moved.Key != "5-focus-timer-x" || moved.Value != "1" || moved.Script != "focus" || moved.Name != "timer-x" {
		t.Errorf("moved record = %+v", moved)
	}

	// The script reads its value back under the new id.
	s := collScript("focus", "", "")
	s.host = h
	got, err := s.bridgeStored("timer-x")
	if err != nil || got != float64(1) {
		t.Errorf("stored(timer-x) = %v, %v", got, err)
	}
}
