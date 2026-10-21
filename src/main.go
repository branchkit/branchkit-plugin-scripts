package main

import (
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/branchkit/plugin-sdk-go"
)

//go:embed settings.css
var scriptsCSS string

func main() {
	plugin := branchkit.NewPlugin()
	host := NewHost(plugin)

	HandleRun(plugin, func(p RunParams, _ *branchkit.OnActionRequest) (any, error) {
		script, handler := "", ""
		if p.Script != nil {
			script = *p.Script
		}
		if p.Handler != nil {
			handler = *p.Handler
		}
		return nil, host.Invoke(script, handler)
	})

	// The receiving end of an open chooser: the matcher captured one of the
	// codewords the host published, and the value it carries routes back to
	// the script that asked.
	HandlePickChoice(plugin, func(p PickChoiceParams, _ *branchkit.OnActionRequest) (any, error) {
		choice := ""
		if p.Choice != nil {
			choice = *p.Choice
		}
		return nil, host.resolveChoice(choice)
	})

	// Fan-in: one listener per manifest-consumed pattern, read from
	// plugin.json itself so the two can't drift. Scripts can only subscribe
	// within this set — widening it is a manifest edit (the static-manifest
	// vs dynamic-interests tradeoff the design doc records).
	patterns, collections := consumedFromManifest()
	host.SetKnownEvents(patterns)
	host.SetDeclaredCollections(collections)
	for _, pattern := range patterns {
		if strings.Contains(pattern, "*") {
			// A wildcard subscription carries many event types, so the
			// listener has to be told which one arrived — FanIn matches the
			// CONCRETE type against each script's own subscription. This is
			// how a script hears another script: the manifest consumes
			// `scripts.*.*`, and a script asks for its slice of that.
			plugin.OnPattern(pattern, func(eventType string, payload json.RawMessage) {
				host.FanIn(eventType, plugin.CurrentEventOrigin().Source, payload)
			})
			continue
		}
		pat := pattern
		plugin.On(pat, func(payload json.RawMessage) {
			host.FanIn(pat, plugin.CurrentEventOrigin().Source, payload)
		})
	}

	// Auto-resume: when the user approves a privilege this host requested
	// (Plugins page to-do or the manual toggle), reload exactly the scripts
	// whose call() calls were waiting on it. The grant is live — no plugin
	// restart — so the reload is all it takes.
	plugin.OnPrivilegeGranted(func(ev branchkit.PrivilegeGrantedEventParams) {
		if ev.PluginID == "scripts" {
			host.PrivilegeGranted(ev.Privilege)
		}
	})

	plugin.SettingsCSS(scriptsCSS)
	plugin.Page("scripts", func(_ *branchkit.RenderSettingsRequest) (string, error) {
		return host.renderSettingsTab()
	})

	registerSettingsHandlers(plugin, host)

	plugin.OnReady(func() { host.Start() })

	plugin.Run()
}

// consumedFromManifest reads the host's own plugin.json for what it consumes.
// Read from the file rather than hardcoded so the manifest and the gates
// cannot drift: the manifest is what the user approved, and it is what the
// platform enforces.
func consumedFromManifest() (patterns []string, collections []string) {
	dir := os.Getenv("BRANCHKIT_PLUGIN_DIR")
	data, err := os.ReadFile(filepath.Join(dir, "plugin.json"))
	if err != nil {
		branchkit.Logf("scripts", "reading plugin.json: %v", err)
		return nil, nil
	}
	var manifest struct {
		Consumes struct {
			Events []struct {
				Pattern string `json:"pattern"`
			} `json:"events"`
			Collections []string `json:"collections"`
		} `json:"consumes"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		branchkit.Logf("scripts", "parsing plugin.json: %v", err)
		return nil, nil
	}
	patterns = make([]string, 0, len(manifest.Consumes.Events))
	for _, e := range manifest.Consumes.Events {
		patterns = append(patterns, e.Pattern)
	}
	return patterns, manifest.Consumes.Collections
}
