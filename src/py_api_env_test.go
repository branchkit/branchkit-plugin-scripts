package main

import "testing"

// A hosted script gets none of the plugin's platform or proxy wiring: the
// proxy address was how a script could tunnel past its per-script host list.
func TestPyChildEnvDropsPlatformAndProxyVariables(t *testing.T) {
	got := pyChildEnv([]string{
		"PATH=/usr/bin",
		"BRANCHKIT_PROXY=unix:///tmp/p.sock",
		"BRANCHKIT_LISTEN_RELAY=npipe://x",
		"https_proxy=http://x",
		"HTTP_PROXY=http://x",
		"LANG=en_US.UTF-8",
	})
	want := map[string]bool{"PATH=/usr/bin": true, "LANG=en_US.UTF-8": true}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for _, kv := range got {
		if !want[kv] {
			t.Fatalf("unexpected %q in %v", kv, got)
		}
	}
}
