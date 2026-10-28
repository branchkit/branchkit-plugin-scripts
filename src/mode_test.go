package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func modeScript(name, header string) *Script {
	return &Script{
		name:     name,
		skin:     &luaSkin,
		host:     &Host{known: []string{"scripts.*.*"}},
		grants:   parsePermissions(header, "--"),
		handlers: map[string]func() error{},
		events:   map[string][]func(payload any) error{},
		timers:   map[string]*time.Timer{},
	}
}

// Entering an exclusive mode suppresses every other command in the system.
// That is a large enough act to declare.
func TestModeRefusedWithoutTheGrant(t *testing.T) {
	s := modeScript("review", "-- permissions: hud\n")
	err := s.bridgeMode("editing")
	if err == nil {
		t.Fatal("mode without the grant must be refused")
	}
	if !strings.Contains(err.Error(), "-- permissions: mode") {
		t.Fatalf("refusal should name the token to add, got: %v", err)
	}
}

func TestModeAcceptsBareAndQualifiedGrants(t *testing.T) {
	s := modeScript("notes", "-- permissions: mode:editing\n")
	if !s.grants.allows("mode", "editing") {
		t.Fatal("mode:editing should allow the editing mode")
	}
	if s.grants.allows("mode", "reviewing") {
		t.Fatal("mode:editing must not allow a different mode")
	}
	if !modeScript("notes", "-- permissions: mode\n").grants.allows("mode", "reviewing") {
		t.Fatal("bare mode should allow any mode name")
	}
}

// Two scripts choosing "review" must not silently share one gate: the point
// of the feature is a script owning the spoken surface, and a pasted script
// quietly stealing another's mode is the sloppiness the header exists to catch.
func TestModeTagIsNamespacedByScript(t *testing.T) {
	a := modeScript("headphones", "-- permissions: mode\n").modeTag("review")
	b := modeScript("notes", "-- permissions: mode\n").modeTag("review")
	if a == b {
		t.Fatalf("two scripts' modes collided on %q", a)
	}
	// Not a dot: dots delimit the tag namespace this collection owns, so a
	// dotted record name would read as a deeper prefix.
	if strings.Contains(a, ".") {
		t.Fatalf("mode tag %q must not introduce a dot", a)
	}
}

func TestModeRejectsNamesThatAreNotOneSegment(t *testing.T) {
	s := modeScript("notes", "-- permissions: mode\n")
	for _, bad := range []string{"", "a.b", "with space", "star*"} {
		if err := s.bridgeMode(bad); err == nil {
			t.Errorf("mode name %q must be refused", bad)
		}
	}
}

// A mode-bound command carries the gate; a free-context one carries none.
// Requiring the tag is what makes the mode worth entering.
func TestCommandBindsToTheScriptsModeGate(t *testing.T) {
	s := modeScript("notes", "-- permissions: mode\n")
	if _, err := s.registerCommand("save it", "editing", "", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.registerCommand("free form", "", "", func() error { return nil }); err != nil {
		t.Fatal(err)
	}

	gated, free := s.commands[0], s.commands[1]
	want := modeCollection + ".notes-editing"
	if len(gated.RequiresTags) != 1 || gated.RequiresTags[0] != want {
		t.Fatalf("gated command should require %q, got %v", want, gated.RequiresTags)
	}
	if len(free.RequiresTags) != 0 {
		t.Fatalf("ungated command should require nothing, got %v", free.RequiresTags)
	}
	if s.infos[0].mode != "editing" || s.infos[1].mode != "" {
		t.Fatalf("row metadata should carry the mode: %#v", s.infos)
	}
}

func TestCommandRejectsAModeNameThatIsNotOneSegment(t *testing.T) {
	s := modeScript("notes", "-- permissions: mode\n")
	if _, err := s.registerCommand("save it", "a.b", "", func() error { return nil }); err == nil {
		t.Fatal("a dotted mode name must be refused at registration")
	}
}

// `{enters}` is what the platform's one-breath graft pairs on: an entry is a
// command whose sets_tags names an exclusive tag, and a gated command is one
// whose requires_tags are a subset of it. Without it no pair is ever found
// and "enter review mark it" needs two utterances.
func TestEnteringCommandDeclaresTheModeTagItSets(t *testing.T) {
	s := modeScript("notes", "-- permissions: mode\n")
	if _, err := s.registerCommand("enter review", "", "review", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.registerCommand("mark it", "review", "", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	entry, gated := s.commands[0], s.commands[1]
	tag := modeCollection + ".notes-review"

	if len(entry.SetsTags) != 1 || entry.SetsTags[0] != tag {
		t.Fatalf("entry should set %q, got %v", tag, entry.SetsTags)
	}
	if len(entry.RequiresTags) != 0 {
		t.Fatalf("an entry must stay free-context or the graft skips it: %v", entry.RequiresTags)
	}
	// The pairing rule the platform applies, asserted here so a rename on
	// either side cannot quietly stop producing chains.
	for _, req := range gated.RequiresTags {
		if !contains(entry.SetsTags, req) {
			t.Fatalf("gated requires %q which the entry does not set — no chain", req)
		}
	}
}

// mode() REPLACES this script's mode; entering by voice must leave the same
// state behind, so an entering command clears the script's other modes.
func TestEnteringCommandClearsTheScriptsOtherModes(t *testing.T) {
	s := modeScript("notes", "-- permissions: mode\n")
	for _, c := range []struct{ pattern, mode, enters string }{
		{"enter review", "", "review"},
		{"enter editing", "", "editing"},
		{"mark it", "review", ""},
	} {
		if _, err := s.registerCommand(c.pattern, c.mode, c.enters, func() error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	s.resolveModeExits()

	review, editing := s.commands[0], s.commands[1]
	if len(review.ClearsTags) != 1 || review.ClearsTags[0] != modeCollection+".notes-editing" {
		t.Fatalf("entering review should clear editing, got %v", review.ClearsTags)
	}
	if len(editing.ClearsTags) != 1 || editing.ClearsTags[0] != modeCollection+".notes-review" {
		t.Fatalf("entering editing should clear review, got %v", editing.ClearsTags)
	}
	// Never clears the one it is entering.
	for _, c := range []int{0, 1} {
		if contains(s.commands[c].ClearsTags, s.commands[c].SetsTags[0]) {
			t.Fatal("an entering command must not clear the mode it enters")
		}
	}
}

// One mode in play means nothing to clear — no spurious tag ops on the wire.
func TestSingleModeScriptClearsNothing(t *testing.T) {
	s := modeScript("notes", "-- permissions: mode\n")
	if _, err := s.registerCommand("enter review", "", "review", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.registerCommand("mark it", "review", "", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	s.resolveModeExits()
	if len(s.commands[0].ClearsTags) != 0 {
		t.Fatalf("nothing to clear, got %v", s.commands[0].ClearsTags)
	}
}

func TestEntersModeNameIsValidatedLikeTheRest(t *testing.T) {
	s := modeScript("notes", "-- permissions: mode\n")
	if _, err := s.registerCommand("x", "", "a.b", func() error { return nil }); err == nil {
		t.Fatal("a dotted enters name must be refused at registration")
	}
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// The command's tag and the tag mode() writes have to be the same string, or
// the script enters a gate nothing is bound to and the commands look broken.
func TestCommandGateMatchesWhatModeWrites(t *testing.T) {
	s := modeScript("notes", "-- permissions: mode\n")
	if _, err := s.registerCommand("save it", "editing", "", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	fromCommand := s.commands[0].RequiresTags[0]
	fromMode := modeCollection + "." + s.modeTag("editing")
	if fromCommand != fromMode {
		t.Fatalf("command requires %q but mode() writes %q", fromCommand, fromMode)
	}
}

// The record's payload is what becomes the tag, so the id and the `name`
// field must agree — a mismatch would leave a record whose delete-by-id the
// host computes from `name` could never find.
func TestModeRecordIdAndNameAgree(t *testing.T) {
	s := modeScript("notes", "-- permissions: mode\n")
	tag := s.modeTag("editing")
	payload, err := json.Marshal(map[string]any{"name": tag})
	if err != nil {
		t.Fatal(err)
	}
	var back struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(payload, &back) != nil || back.Name != tag {
		t.Fatalf("record name should round-trip as the id: %q", back.Name)
	}
	if !strings.HasPrefix(back.Name, s.name+"-") {
		t.Fatalf("clearModes scans by the %q prefix; %q would be missed", s.name+"-", back.Name)
	}
}

// A record names its owning script, so tearing down `focus` leaves
// `focus-timer`'s modes and storage alone. A record written before the
// field existed falls back to the id prefix.
func TestRecordsBelongToTheScriptThatNamesItself(t *testing.T) {
	cases := []struct {
		owner, id, script string
		want              bool
	}{
		{"focus-timer", "focus-timer-review", "focus", false},
		{"focus-timer", "focus-timer-review", "focus-timer", true},
		{"focus", "focus-review", "focus", true},
		{"", "focus-review", "focus", true},  // legacy record
		{"", "other-review", "focus", false}, // legacy, someone else's
	}
	for _, c := range cases {
		if got := ownedBy(c.owner, c.id, c.script); got != c.want {
			t.Errorf("ownedBy(%q, %q, %q) = %v, want %v", c.owner, c.id, c.script, got, c.want)
		}
	}
}
