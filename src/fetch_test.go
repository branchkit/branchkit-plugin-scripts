package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// scriptWithGrants builds the minimum a bridgeFetch call touches: the header's
// parsed grants and the skin's comment prefix (which the refusal message
// quotes back, so the author is told the exact line to paste in THEIR
// language).
func scriptWithGrants(t *testing.T, header string) *Script {
	t.Helper()
	return &Script{
		name:   "probe",
		skin:   &luaSkin,
		grants: parsePermissions(header, luaSkin.commentPrefix),
	}
}

func TestHostAllowedByGrants(t *testing.T) {
	cases := []struct {
		declared []string
		host     string
		want     bool
		why      string
	}{
		{[]string{"fetch:api.example.com"}, "api.example.com", true, "exact"},
		{[]string{"fetch:api.example.com"}, "API.Example.COM", true, "case-insensitive"},
		{[]string{"fetch:api.example.com"}, "api.example.com.", true, "trailing root dot"},
		{[]string{"fetch:api.example.com"}, "evil.com", false, "different host"},
		{[]string{"fetch:api.example.com"}, "other.example.com", false, "sibling is not covered"},
		{[]string{"fetch:*.example.com"}, "api.example.com", true, "wildcard subdomain"},
		{[]string{"fetch:*.example.com"}, "deep.api.example.com", true, "wildcard is not one label"},
		{[]string{"fetch:*.example.com"}, "example.com", false, "wildcard must NOT match the bare parent"},
		{[]string{"fetch:*.example.com"}, "notexample.com", false, "suffix match must respect the dot"},
		{[]string{"fetch:*"}, "anything.at.all", true, "bare star"},
		{[]string{"call:network", "hud"}, "api.example.com", false, "other capabilities grant nothing here"},
		{[]string{}, "api.example.com", false, "nothing declared"},
	}
	for _, c := range cases {
		if got := hostAllowedByGrants(c.host, c.declared); got != c.want {
			t.Errorf("%s: hostAllowedByGrants(%q, %v) = %v, want %v", c.why, c.host, c.declared, got, c.want)
		}
	}
}

func TestFetchRefusesUndeclaredHost(t *testing.T) {
	s := scriptWithGrants(t, "-- permissions: fetch:api.example.com\n")
	_, err := s.bridgeFetch("https://evil.com/steal", fetchOptions{})
	if err == nil {
		t.Fatal("an undeclared host must be refused")
	}
	// The refusal has to name the line to paste, in the script's own comment
	// syntax — that is what makes the header teachable rather than a wall.
	for _, want := range []string{"evil.com", "-- permissions: fetch:evil.com"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal should contain %q, got: %v", want, err)
		}
	}
}

func TestFetchRefusesNonHTTPSchemes(t *testing.T) {
	s := scriptWithGrants(t, "-- permissions: fetch:*\n")
	for _, u := range []string{"file:///etc/passwd", "ftp://example.com/x", "not a url"} {
		if _, err := s.bridgeFetch(u, fetchOptions{}); err == nil {
			t.Errorf("%q must be refused", u)
		}
	}
}

// The one a grant alone cannot defend: a declared host answering 302 with a
// Location pointing somewhere undeclared. Following it would walk around the
// per-script grant, and the platform could not catch it either — its allowlist
// is the union across every script, so another script's host would do.
func TestFetchDoesNotFollowRedirects(t *testing.T) {
	var reachedElsewhere bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reachedElsewhere = true
		w.WriteHeader(200)
	}))
	defer elsewhere.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL, http.StatusFound)
	}))
	defer redirector.Close()

	s := scriptWithGrants(t, fmt.Sprintf("-- permissions: fetch:%s\n", hostOf(t, redirector.URL)))
	res, err := s.bridgeFetch(redirector.URL, fetchOptions{})
	if err != nil {
		t.Fatalf("the declared host itself must still be reachable: %v", err)
	}
	if reachedElsewhere {
		t.Fatal("followed a redirect to an undeclared host — the grant was bypassed")
	}
	if res["status"].(float64) != 302 {
		t.Errorf("the redirect should be returned as data, got status %v", res["status"])
	}
}

func TestFetchReturnsStatusAsDataNotError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()

	s := scriptWithGrants(t, fmt.Sprintf("-- permissions: fetch:%s\n", hostOf(t, srv.URL)))
	res, err := s.bridgeFetch(srv.URL, fetchOptions{})
	// A 500 is a condition the script handles, not a refusal it cannot.
	if err != nil {
		t.Fatalf("an HTTP status must not be an error: %v", err)
	}
	if res["status"].(float64) != 500 {
		t.Errorf("status = %v, want 500", res["status"])
	}
}

func TestFetchParsesJSONOnlyWhenDeclaredAndValid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/good":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"temp": 12.5}`)
		case "/lying":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `not json at all`)
		default:
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, `{"temp": 12.5}`)
		}
	}))
	defer srv.Close()
	s := scriptWithGrants(t, fmt.Sprintf("-- permissions: fetch:%s\n", hostOf(t, srv.URL)))

	res, err := s.bridgeFetch(srv.URL+"/good", fetchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	obj, ok := res["json"].(map[string]any)
	if !ok || obj["temp"].(float64) != 12.5 {
		t.Errorf("json should be parsed, got %#v", res["json"])
	}

	// Says JSON, is not: the script gets nil rather than a half-decoded value.
	res, _ = s.bridgeFetch(srv.URL+"/lying", fetchOptions{})
	if _, present := res["json"]; present {
		t.Error("unparseable body must not produce a json field")
	}
	// Is JSON, does not say so: body is there, json is not invented.
	res, _ = s.bridgeFetch(srv.URL+"/plain", fetchOptions{})
	if _, present := res["json"]; present {
		t.Error("a non-json content type must not be parsed")
	}
	if !strings.Contains(res["body"].(string), "temp") {
		t.Error("body should still carry the text")
	}
}

func TestFetchCapsResponseSize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := strings.Repeat("x", 1<<20)
		for i := 0; i < 8; i++ { // 8 MiB, over the 5 MiB cap
			fmt.Fprint(w, chunk)
		}
	}))
	defer srv.Close()

	s := scriptWithGrants(t, fmt.Sprintf("-- permissions: fetch:%s\n", hostOf(t, srv.URL)))
	res, err := s.bridgeFetch(srv.URL, fetchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(res["body"].(string)); got > fetchMaxBody {
		t.Errorf("body was %d bytes, cap is %d", got, fetchMaxBody)
	}
}

func TestFetchSendsJSONBodyAndHeaders(t *testing.T) {
	var gotBody, gotType, gotCustom, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 256)
		n, _ := r.Body.Read(buf)
		gotBody, gotType, gotCustom, gotMethod = string(buf[:n]), r.Header.Get("Content-Type"), r.Header.Get("X-Note"), r.Method
	}))
	defer srv.Close()

	s := scriptWithGrants(t, fmt.Sprintf("-- permissions: fetch:%s\n", hostOf(t, srv.URL)))
	_, err := s.bridgeFetch(srv.URL, fetchOptionsFrom(map[string]any{
		"method":  "post",
		"body":    map[string]any{"text": "note to self"},
		"headers": map[string]any{"X-Note": "1"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != "POST" {
		t.Errorf("method = %q, want POST (case-normalised)", gotMethod)
	}
	if !strings.Contains(gotBody, "note to self") {
		t.Errorf("body = %q", gotBody)
	}
	if gotType != "application/json" {
		t.Errorf("a structured body should set the content type, got %q", gotType)
	}
	if gotCustom != "1" {
		t.Errorf("custom header lost, got %q", gotCustom)
	}
}

func hostOf(t *testing.T, rawURL string) string {
	t.Helper()
	trimmed := strings.TrimPrefix(rawURL, "http://")
	host, _, _ := strings.Cut(trimmed, ":")
	return host
}
