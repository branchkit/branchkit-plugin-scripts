package main

import (
	"strings"
	"testing"
)

// `secret:name@host` is a known grant, and names exactly one host.
func TestSecretHeaderLinesAreGrantsWithOneHost(t *testing.T) {
	g := parsePermissions("-- permissions: fetch:api.w.test, secret:key@API.W.test\n", "--")
	if len(g.unknown) != 0 {
		t.Fatalf("secret: is a known grant, got unknown %v", g.unknown)
	}
	if got := g.declaredSecrets(); got["key"] != "api.w.test" || len(got) != 1 {
		t.Fatalf("declaredSecrets = %v", got)
	}
}

// A script's secret lives under its OWN file's prefix, and nothing a script
// passes can move it out of that namespace.
func TestSlotNamesAreScopedToTheCallingScript(t *testing.T) {
	s := &Script{name: "weather report"}
	if got := s.slotName("api/key"); got != "script.weather-report.api-key" {
		t.Fatalf("slotName = %q", got)
	}
	other := &Script{name: "other"}
	if s.slotName("k") == other.slotName("k") {
		t.Fatal("two scripts must not share a slot")
	}
}

// The refusals happen before anything reaches the platform: an undeclared
// secret, a secret declared for another host, or plain http.
func TestFetchWithASecretRefusesWhatTheHeaderDoesNotSay(t *testing.T) {
	cases := []struct {
		skin      *skin
		src, want string
	}{
		{&luaSkin, "-- permissions: fetch:api.w.test\n" +
			`fetch("https://api.w.test/x", {headers = {Authorization = secret_ref("key", "Bearer ")}})`,
			"not declared"},
		{&luaSkin, "-- permissions: fetch:api.w.test, secret:key@other.test\n" +
			`fetch("https://api.w.test/x", {headers = {Authorization = secret_ref("key", "Bearer ")}})`,
			"declared for other.test"},
		{&luaSkin, "-- permissions: fetch:api.w.test, secret:key@api.w.test\n" +
			`fetch("http://api.w.test/x", {headers = {Authorization = secret_ref("key")}})`,
			"must be https"},
		{&jsSkin, "// permissions: fetch:api.w.test, secret:key@other.test\n" +
			`fetch("https://api.w.test/x", {headers: {Authorization: secretRef("key", "Bearer ")}})`,
			"declared for other.test"},
	}
	for _, c := range cases {
		_, err := bootScript(t, c.skin, "probe", c.src)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want error containing %q, got %v", c.skin.lang, c.want, err)
		}
	}
}

// With everything declared, the request is handed to the platform (here,
// absent), never performed by the script host's own HTTP client.
func TestADeclaredSecretFetchGoesToThePlatform(t *testing.T) {
	_, err := bootScript(t, &luaSkin, "probe",
		"-- permissions: fetch:api.w.test, secret:key@api.w.test\n"+
			`fetch("https://api.w.test/x", {headers = {Authorization = secret_ref("key", "Bearer ")}})`)
	if err == nil || !strings.Contains(err.Error(), "not connected to the platform") {
		t.Fatalf("want the platform path, got %v", err)
	}
}
