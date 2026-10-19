package main

import (
	"strings"
	"testing"
	"time"
)

func chooseScript(name, header string) *Script {
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

// A chooser holds an EXCLUSIVE gate — while it is open the options are the
// only thing the user can say. That is as large an act as entering a mode,
// so it is declared like one.
func TestChooseRefusedWithoutTheGrant(t *testing.T) {
	s := chooseScript("ask", "-- permissions: hud\n")
	err := s.bridgeChoose([]string{"a", "b"}, func(any) error { return nil })
	if err == nil {
		t.Fatal("choose without the grant must be refused")
	}
	if !strings.Contains(err.Error(), "permissions: choose") {
		t.Fatalf("refusal should name the token to add, got: %v", err)
	}
	// `hud` must not quietly stand in for it: a chooser takes the spoken
	// surface, which showing HTML does not.
	if s.grants.allows("choose", "") {
		t.Fatal("the hud grant must not imply choose")
	}
}

// Two open choosers would share one gate and one option collection, so the
// second would silently replace the first's options underneath its gate —
// the user would see one question and answer another.
func TestSecondChooserIsRefusedAndNamesTheHolder(t *testing.T) {
	h := &Host{}
	h.chooseMu.Lock()
	h.chooser = &openChooser{script: "first", byCode: map[string]string{"harp": "a"}}
	h.chooseMu.Unlock()

	err := h.beginChoose("second", []string{"b"}, func(any) error { return nil })
	if err == nil {
		t.Fatal("a second chooser must be refused, not queued")
	}
	if !strings.Contains(err.Error(), "first") {
		t.Fatalf("the refusal must name who holds it, got: %v", err)
	}
}

// The pool bounds how many options can be spoken. Failing loudly beats
// cycling codewords, which would make two options answer to one word.
func TestChooseRefusesMoreOptionsThanCodewords(t *testing.T) {
	h := &Host{}
	many := make([]string, len(choiceCodewordPool)+1)
	for i := range many {
		many[i] = "opt"
	}
	if err := h.beginChoose("ask", many, func(any) error { return nil }); err == nil {
		t.Fatal("more options than codewords must be refused")
	}
	if err := h.beginChoose("ask", nil, func(any) error { return nil }); err == nil {
		t.Fatal("an empty chooser must be refused")
	}
}

// A codeword maps back to the option the script listed, not to itself —
// the script asked about "red", so "red" is what it must hear.
func TestPickReturnsTheOptionNotTheCodeword(t *testing.T) {
	h := &Host{}
	got := make(chan any, 1)
	h.chooser = &openChooser{
		script:  "ask",
		byCode:  map[string]string{"harp": "red", "arch": "green"},
		deliver: func(pick any) error { got <- pick; return nil },
	}
	c, pick, err := h.takeChoice("arch")
	if err != nil {
		t.Fatalf("resolve failed: %v", err)
	}
	if pick != "green" {
		t.Fatalf("got %q, want the option value \"green\"", pick)
	}
	if err := c.deliver(pick); err != nil {
		t.Fatal(err)
	}
	if v := <-got; v != "green" {
		t.Fatalf("callback got %v, want \"green\"", v)
	}
	if h.chooser != nil {
		t.Fatal("answering must close the chooser — a replayable one is a stuck gate")
	}
}

// An unknown codeword must not consume the chooser: the user is still
// waiting to answer, and dropping their question on a mishear is worse
// than ignoring the mishear.
func TestUnknownCodewordLeavesTheChooserOpen(t *testing.T) {
	h := &Host{}
	h.chooser = &openChooser{
		script:  "ask",
		byCode:  map[string]string{"harp": "red"},
		deliver: func(any) error { return nil },
	}
	if _, _, err := h.takeChoice("kite"); err == nil {
		t.Fatal("an option that is not in the chooser must error")
	}
	if h.chooser == nil {
		t.Fatal("a mishear must not close the chooser")
	}
}

// nil is a real answer. A callback that silently never fires leaves the
// script waiting forever with no way to know why.
func TestTimeoutDeliversNil(t *testing.T) {
	h := &Host{}
	got := make(chan any, 1)
	c := &openChooser{
		script:  "ask",
		byCode:  map[string]string{"harp": "red"},
		deliver: func(pick any) error { got <- pick; return nil },
	}
	h.chooser = c
	if !h.takeExpired(c) {
		t.Fatal("an open chooser must be claimable by its own timer")
	}
	if err := c.deliver(nil); err != nil {
		t.Fatal(err)
	}
	if v := <-got; v != nil {
		t.Fatalf("timeout should deliver nil, got %v", v)
	}
	if h.chooser != nil {
		t.Fatal("timeout must clear the chooser")
	}
}

// A stale timer firing after someone answered must not deliver a second,
// contradictory answer.
func TestExpiryOfASupersededChooserIsANoop(t *testing.T) {
	h := &Host{}
	stale := &openChooser{script: "ask", deliver: func(any) error {
		t.Error("a superseded chooser must not deliver")
		return nil
	}}
	current := &openChooser{script: "other", byCode: map[string]string{}}
	h.chooser = current
	if h.takeExpired(stale) {
		t.Fatal("a superseded chooser must not be claimable")
	}
	if h.chooser != current {
		t.Fatal("expiring a stale chooser must leave the current one alone")
	}
}

func TestToOptionsStringifiesScalars(t *testing.T) {
	got := toOptions([]any{"a", float64(2), true, nil})
	want := []string{"a", "2", "true"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
