package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/branchkit/plugin-sdk-go"
)

func collScript(name, header string, declared ...string) *Script {
	h := &Host{known: []string{"scripts.*.*"}}
	h.SetDeclaredCollections(declared)
	return &Script{
		name:     name,
		skin:     &luaSkin,
		host:     h,
		grants:   parsePermissions(header, "--"),
		handlers: map[string]func() error{},
		events:   map[string][]func(payload any) error{},
		timers:   map[string]*time.Timer{},
	}
}

// Reach is checked before the grant, so a refusal never names a token that
// could not fix it. "add write:plugin.scripts.scripts" is worse than useless:
// it points at a header line that will never work.
func TestHostOwnedRefusalWinsOverTheMissingGrant(t *testing.T) {
	s := collScript("notes", "-- permissions: read:apps\n", "apps")
	err := s.bridgePut("plugin.scripts.scripts", "notes", nil)
	if err == nil {
		t.Fatal("writing host state must be refused")
	}
	if strings.Contains(err.Error(), "permissions:") {
		t.Fatalf("refusal must not suggest a header token, got: %v", err)
	}
	if !strings.Contains(err.Error(), "store()/stored()") {
		t.Fatalf("refusal should point at the surface that IS for this, got: %v", err)
	}
}

func TestCollectionReadAndWriteNeedTheirOwnGrants(t *testing.T) {
	s := collScript("notes", "-- permissions: hud\n", "apps", "keybinds")

	if _, err := s.bridgeRecord("apps", "Safari"); err == nil ||
		!strings.Contains(err.Error(), "-- permissions: read") {
		t.Fatalf("read should be refused with the token to add, got: %v", err)
	}
	if _, err := s.bridgeRecords("apps", nil); err == nil ||
		!strings.Contains(err.Error(), "-- permissions: read") {
		t.Fatalf("records should be refused with the token to add, got: %v", err)
	}
	if err := s.bridgePut("keybinds", "x", nil); err == nil ||
		!strings.Contains(err.Error(), "-- permissions: write") {
		t.Fatalf("put should be refused with the token to add, got: %v", err)
	}
	if err := s.bridgeDrop("keybinds", "x"); err == nil ||
		!strings.Contains(err.Error(), "-- permissions: write") {
		t.Fatalf("drop should be refused with the token to add, got: %v", err)
	}

	// Read and write are separate grants: reading a collection must not
	// carry the right to change it.
	reader := collScript("notes", "-- permissions: read\n", "keybinds")
	if err := reader.bridgePut("keybinds", "x", nil); err == nil ||
		!strings.Contains(err.Error(), "permissions: write") {
		t.Fatalf("a read grant must not authorize a write, got: %v", err)
	}
}

// Clause 3, the real control-plane concern: a script writing
// plugin.scripts.scripts could disable its siblings or clear their error
// state, and one writing plugin.scripts.modes could hold a gate it never
// declared. Reads are refused too — sibling bookkeeping is not a script's
// business — and the refusal points at the surface that IS for private data.
func TestHostOwnedCollectionsAreUnreachable(t *testing.T) {
	s := collScript("notes", "-- permissions: read, write\n",
		"plugin.scripts.scripts", "plugin.scripts.modes", "plugin.scripts.store")

	for _, coll := range []string{
		"plugin.scripts.scripts",
		"plugin.scripts.modes",
		"plugin.scripts.store",
		"plugin.scripts.config",
	} {
		if err := s.bridgePut(coll, "x", nil); err == nil ||
			!strings.Contains(err.Error(), "store()/stored()") {
			t.Errorf("writing %s must be refused with a pointer to store(), got: %v", coll, err)
		}
		if _, err := s.bridgeRecord(coll, "x"); err == nil {
			t.Errorf("reading %s must be refused", coll)
		}
		if err := s.bridgeDrop(coll, "x"); err == nil {
			t.Errorf("dropping from %s must be refused", coll)
		}
	}
}

// Clause 2's middle: the host lends only reach it asked the user for. A
// collection outside consumes.collections needs a manifest edit — the same
// static-manifest tradeoff the consumed-event set already carries.
func TestUndeclaredCollectionsAreRefused(t *testing.T) {
	s := collScript("notes", "-- permissions: read, write\n", "apps")

	if _, err := s.bridgeRecord("browser_tabs", "1"); err == nil ||
		!strings.Contains(err.Error(), "manifest edit") {
		t.Fatalf("an undeclared collection should say what it needs, got: %v", err)
	}
	if err := s.bridgePut("browser_tabs", "1", nil); err == nil {
		t.Fatal("writing an undeclared collection must be refused")
	}
}

// The reach check is about the collection, not the verb, so all four verbs
// answer identically for the same collection.
func TestReachCheckIsUniformAcrossVerbs(t *testing.T) {
	s := collScript("notes", "-- permissions: read, write\n", "apps")
	for _, verb := range []string{"record", "records", "put", "drop"} {
		if err := s.checkCollectionReach(verb, "apps"); err != nil {
			t.Errorf("%s on a declared, unowned collection should pass: %v", verb, err)
		}
		if err := s.checkCollectionReach(verb, "plugin.scripts.scripts"); err == nil {
			t.Errorf("%s on a host-owned collection should fail", verb)
		}
		if err := s.checkCollectionReach(verb, "browser_tabs"); err == nil {
			t.Errorf("%s on an undeclared collection should fail", verb)
		}
	}
}

// The read projection carries the ENVELOPE, not just the payload. Bare
// payloads were the first shape and were wrong twice: a script could list
// records and then not address one (the id is out here, and which payload
// field equals it is knowledge only the owning plugin's manifest holds), and
// it could not tell its own contribution from a peer's or from the user's.
func TestEntryProjectsTheEnvelopeNotJustThePayload(t *testing.T) {
	label := "headphones.lua"
	e, err := entry(&branchkit.CollectionRecord{
		ID:          "k1",
		Payload:     []byte(`{"spoken":"1password"}`),
		Writer:      "system",
		Revision:    3,
		TimestampMs: 1234,
		OnBehalfOf:  &label,
	})
	if err != nil {
		t.Fatal(err)
	}
	m := e.(map[string]any)
	if m["id"] != "k1" || m["writer"] != "system" || m["revision"] != 3 || m["timestamp_ms"] != 1234 {
		t.Fatalf("envelope fields lost: %#v", m)
	}
	if m["on_behalf_of"] != label {
		t.Fatalf("actor label lost: %#v", m["on_behalf_of"])
	}
	payload := m["payload"].(map[string]any)
	if payload["spoken"] != "1password" {
		t.Fatalf("payload lost: %#v", payload)
	}
	// `group` is deliberately absent — scripts cannot push one, and a field
	// you can read but never set is furniture.
	if _, present := m["group"]; present {
		t.Error("group should not be projected")
	}
}

// "Nobody recorded a label" and "recorded as blank" must not read the same.
// The shared-overrides backend keeps no per-record envelope, so this is the
// common case, not an edge one.
func TestEntryOmitsAnAbsentActorLabel(t *testing.T) {
	for _, rec := range []*branchkit.CollectionRecord{
		{ID: "a", Payload: []byte(`{}`)},
		{ID: "b", Payload: []byte(`{}`), OnBehalfOf: new(string)},
	} {
		e, err := entry(rec)
		if err != nil {
			t.Fatal(err)
		}
		if _, present := e.(map[string]any)["on_behalf_of"]; present {
			t.Errorf("record %s should carry no label key", rec.ID)
		}
	}
}

// Clause 4: one host-owned collection, namespaced by script, so one script
// cannot read or overwrite another's.
func TestStoreKeysAreNamespacedByScript(t *testing.T) {
	a := collScript("headphones", "", "").storeKey("count")
	b := collScript("notes", "", "").storeKey("count")
	if a == b {
		t.Fatalf("two scripts' keys collided on %q", a)
	}
	if !strings.HasPrefix(a, "headphones-") {
		t.Fatalf("clearStore scans by the script prefix; %q would be missed", a)
	}
}

func TestStoreRejectsKeysThatAreNotOneSegment(t *testing.T) {
	s := collScript("notes", "", "")
	for _, bad := range []string{"", "a.b", "with space"} {
		if err := s.bridgeStore(bad, "v"); err == nil {
			t.Errorf("store key %q must be refused", bad)
		}
		if _, err := s.bridgeStored(bad); err == nil {
			t.Errorf("stored key %q must be refused", bad)
		}
	}
}

// Values are JSON-encoded into a string field because a collection field is
// typed and script values are not. The round-trip has to survive every shape
// a script can hand over, or store() silently narrows what scripts can keep.
func TestStoredValuesRoundTripThroughTheStringField(t *testing.T) {
	for _, v := range []any{
		"text",
		42.0,
		true,
		[]any{"a", "b"},
		map[string]any{"nested": map[string]any{"x": 1.0}},
	} {
		encoded, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		envelope, err := json.Marshal(map[string]any{"key": "notes-k", "value": string(encoded)})
		if err != nil {
			t.Fatal(err)
		}
		var back struct {
			Value string `json:"value"`
		}
		if err := json.Unmarshal(envelope, &back); err != nil {
			t.Fatal(err)
		}
		var out any
		if err := json.Unmarshal([]byte(back.Value), &out); err != nil {
			t.Fatalf("%#v did not survive the round trip: %v", v, err)
		}
		if got, want := mustJSON(t, out), mustJSON(t, v); got != want {
			t.Errorf("round trip changed %s into %s", want, got)
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
