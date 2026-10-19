package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/branchkit/plugin-sdk-go"
	"github.com/branchkit/plugin-sdk-go/ui"
)

// The settings tab is the discoverable door to the scripts folder: list each
// script with its status, and put the file operations on buttons so nobody
// has to know the path. Open/reveal go through the platform's native
// operations (`filesystem` privilege); delete stays host-local because the
// folder is the host's own data dir.

// --- Editor preference -------------------------------------------------------

const settingsCollection = "plugin.scripts.config"

// Config mirrors the manifest's settings fields. Editor is a bundle id;
// empty means the system default handler for .lua files.
type Config struct {
	Editor string `json:"editor"`
}

func (h *Host) config() Config {
	if h.settings.Ready() {
		return h.settings.Get()
	}
	return Config{}
}

// installedEditors asks Launch Services which applications can open a
// .lua file — the OS's own answer to "what is a text editor here", no
// curated list to maintain. Cached a minute so re-renders don't rescan.
// Needs an existing .lua file for LS to inspect, so before the first
// script exists the row offers only System default.
func (h *Host) installedEditors() [][2]string {
	sample := ""
	if names := h.scriptFiles(); len(names) > 0 {
		if p, _, ok := h.findScript(names[0]); ok {
			sample = p
		}
	}
	if sample == "" {
		return nil
	}
	h.editorsMu.Lock()
	defer h.editorsMu.Unlock()
	if !h.editorsAt.IsZero() && time.Since(h.editorsAt) < time.Minute {
		return h.editorsList
	}
	apps, err := h.plugin.NativeAppsForPath(branchkit.NativeAppsForPathRequest{Path: sample})
	if err != nil {
		branchkit.Logf("scripts", "apps_for_path: %v", err)
		return h.editorsList
	}
	list := [][2]string{}
	seen := map[string]bool{}
	for _, a := range apps {
		if a.BundleID != "" && !seen[a.BundleID] {
			seen[a.BundleID] = true
			list = append(list, [2]string{a.BundleID, a.Name})
		}
	}
	h.editorsAt = time.Now()
	h.editorsList = list
	return list
}

// openInEditor opens path with the configured editor, falling back to the
// system default handler when none is set.
func (h *Host) openInEditor(path string) error {
	if ed := h.config().Editor; ed != "" {
		return h.plugin.NativeOpenWithApp(branchkit.NativeOpenWithAppRequest{BundleID: ed, Target: path})
	}
	return h.plugin.NativeOpenTarget(branchkit.NativeOpenTargetRequest{Target: path})
}

type scriptNameRequest struct {
	Name string `json:"name"`
}

// validName checks a UI-supplied script name. Names come from markup this
// plugin rendered, but the check keeps a crafted post from escaping the
// scripts folder.
func validName(name string) error {
	if name == "" || name != filepath.Base(name) || strings.ContainsAny(name, "/\\") || strings.HasPrefix(name, ".") {
		return fmt.Errorf("invalid script name %q", name)
	}
	return nil
}

// resolveScript validates a name and finds its file on disk.
func (h *Host) resolveScript(name string) (string, *skin, error) {
	if err := validName(name); err != nil {
		return "", nil, err
	}
	path, sk, ok := h.findScript(name)
	if !ok {
		return "", nil, fmt.Errorf("script %q not found", name)
	}
	return path, sk, nil
}

func registerSettingsHandlers(p *branchkit.Plugin, h *Host) {
	branchkit.HandleTyped(p, "open_script", func(req *scriptNameRequest) (any, error) {
		path, _, err := h.resolveScript(req.Name)
		if err != nil {
			return nil, err
		}
		return nil, h.openInEditor(path)
	})

	// Give back a granted privilege. Safe in the PLUGIN's own tab (unlike
	// Approve, which must stay in platform chrome): its only effect is
	// this plugin reducing its own capability — de-escalation needs no
	// consent.
	type privilegeRequest struct {
		Privilege string `json:"privilege"`
	}
	branchkit.HandleTyped(p, "relinquish_privilege", func(req *privilegeRequest) (any, error) {
		h.relinquish(req.Privilege)
		return nil, nil
	})

	type setEditorRequest struct {
		Editor string `json:"editor"`
	}
	branchkit.HandleTyped(p, "set_editor", func(req *setEditorRequest) (any, error) {
		return nil, h.settings.SetUser("editor", req.Editor)
	})

	// The confirm step lives in the PAGE (a Datastar signal toggles the
	// Delete button into Really delete?/Cancel), so this post only ever
	// arrives confirmed — and one window's half-finished delete can't
	// leak into another window's buttons. Server-side confirm state was
	// tried twice and produced a cross-window ghost both times.
	branchkit.HandleTyped(p, "delete_script", func(req *scriptNameRequest) (any, error) {
		path, _, err := h.resolveScript(req.Name)
		if err != nil {
			return nil, err
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
		h.unload(req.Name)
		return nil, nil
	})

	// Enable/disable: the record write is the source of truth, the VM flip
	// happens before this returns (setEnabled fuses them), so the re-render
	// the write wakes already shows the toggled state.
	type setEnabledRequest struct {
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
	}
	branchkit.HandleTyped(p, "set_script_enabled", func(req *setEnabledRequest) (any, error) {
		if _, _, err := h.resolveScript(req.Name); err != nil {
			return nil, err
		}
		return nil, h.setEnabled(req.Name, req.Enabled)
	})

	// The language choice happens here — the only moment the question
	// exists. From then on the file extension carries it, the selector
	// every editor and tool already understands.
	type newScriptRequest struct {
		Lang string `json:"lang"`
	}
	branchkit.HandleTyped(p, "new_script", func(req *newScriptRequest) (any, error) {
		sk := skinForLang(req.Lang)
		if sk == nil {
			sk = &luaSkin
		}
		name := "untitled"
		for i := 2; ; i++ {
			if _, _, ok := h.findScript(name); !ok {
				break
			}
			name = fmt.Sprintf("untitled-%d", i)
		}
		path := filepath.Join(h.dir, name+sk.ext)
		// Create only — the row appears in the list (watcher picks it up
		// within a second) and the user opens it from there when ready.
		return nil, os.WriteFile(path, []byte(sk.template), 0o644)
	})

	// Run one registered command's handler directly — the same scripts.run
	// path a voice match takes, minus the microphone. The click is the
	// user's consent; whatever the script does on match happens for real.
	type runRequest struct {
		Script  string `json:"script"`
		Handler string `json:"handler"`
	}
	branchkit.HandleTyped(p, "run_command", func(req *runRequest) (any, error) {
		if _, _, err := h.resolveScript(req.Script); err != nil {
			return nil, err
		}
		return nil, h.Invoke(req.Script, req.Handler)
	})

	type renameRequest struct {
		Name    string `json:"name"`
		NewName string `json:"new_name"`
	}
	branchkit.HandleTyped(p, "rename_script", func(req *renameRequest) (any, error) {
		// A rename never changes the language: the new name keeps the
		// script's extension.
		oldPath, sk, err := h.resolveScript(req.Name)
		if err != nil {
			return nil, err
		}
		newName := strings.TrimSuffix(strings.TrimSpace(req.NewName), sk.ext)
		if err := validName(newName); err != nil {
			return nil, err
		}
		if newName == req.Name {
			return nil, nil
		}
		if _, _, ok := h.findScript(newName); ok {
			return nil, fmt.Errorf("%s already exists", newName)
		}
		if err := os.Rename(oldPath, filepath.Join(h.dir, newName+sk.ext)); err != nil {
			return nil, err
		}
		// Converge now rather than on the next watcher tick, so the
		// re-render this method triggers already shows the new name.
		// unload drops the old state record, so carry enabled across —
		// renaming a disabled script must not silently re-enable it.
		st, hadState := h.getState(req.Name)
		h.unload(req.Name)
		if hadState && !st.Enabled {
			_ = h.setEnabled(newName, false)
		} else {
			h.load(newName)
		}
		return nil, nil
	})

	branchkit.HandleTyped(p, "open_folder", func(_ *struct{}) (any, error) {
		return nil, p.NativeOpenTarget(branchkit.NativeOpenTargetRequest{Target: h.dir})
	})

	// The in-tab editor.
	// Save writes the file and converges now rather than on the watcher tick,
	// so the re-render this post wakes already shows the reloaded script.
	type saveScriptRequest struct {
		Name    string `json:"name"`
		Content string `json:"content"`
	}
	branchkit.HandleTyped(p, "save_script", func(req *saveScriptRequest) (any, error) {
		path, _, err := h.resolveScript(req.Name)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte(req.Content), 0o644); err != nil {
			return nil, err
		}
		h.load(req.Name)
		return nil, nil
	})

	// Live lint: the editor posts the buffer as the user types; the
	// script's own skin checks it (the same parser that will load the
	// file), and the verdict rides settings.patch_signals back into the
	// row's diagnostics signal. No file is written and nothing executes.
	branchkit.HandleTyped(p, "lint_script", func(req *saveScriptRequest) (any, error) {
		_, sk, err := h.resolveScript(req.Name)
		if err != nil {
			return nil, err
		}
		var msgs []string
		if g := parsePermissions(req.Content, sk.commentPrefix); len(g.unknown) > 0 {
			msgs = append(msgs, fmt.Sprintf("permissions header: unknown grant %q", strings.Join(g.unknown, ", ")))
		}
		if err := sk.lint(req.Name, req.Content); err != nil {
			msgs = append(msgs, err.Error())
		}
		patch, err := json.Marshal(map[string]string{
			"l_" + ui.SignalName(req.Name): strings.Join(msgs, "\n"),
		})
		if err != nil {
			return nil, err
		}
		return nil, p.SettingsPatchSignals(branchkit.SettingsPatchSignalsRequest{Signals: string(patch)})
	})
}

// --- view model -------------------------------------------------------------

type skinView struct{ Lang, Label string }

type permTokenView struct {
	Token     string
	Note      string // live platform-side state for call:* tokens, "" when nothing to say
	NoteClass string
}

type cmdRowView struct {
	Runnable bool
	Handler  string
	Phrase   string
	Mode     string
}

type scriptRowView struct {
	Name, Ext, Sig string
	Enabled        bool
	StatusClass    string
	StatusGlyph    string
	Src            string // seeds the in-tab editor
	Perms          []permTokenView
	Commands       []cmdRowView
	Events         []string
	HeldMode       string
	HeldEffects    []string
	StoredKeys     int
	LastError      string
	// RegistersNothing: enabled, no commands, no events, no error.
	RegistersNothing bool
}

type privRowView struct {
	Name, State, StateClass, Note string
	Relinquishable                bool
}

type editorChoiceView struct {
	BundleID, Label string
	Current         bool
}

type scriptsTabView struct {
	Skins      []skinView
	Rows       []scriptRowView
	Privileges []privRowView
	Editors    []editorChoiceView
}

func (h *Host) renderSettingsTab() (string, error) {
	return branchkit.RenderComponent(ScriptsTab(h.settingsView()))
}

// settingsView gathers everything the tab draws. Rows come from disk (a
// disabled script has no VM to enumerate), state from the collection — a
// direct read, so this render observes any write that woke it, including
// the error a VM just died with. The SDK's render hook has already
// refreshed the settings mirror, so the Editor row observes its own write.
func (h *Host) settingsView() scriptsTabView {
	var v scriptsTabView
	for _, sk := range skins {
		v.Skins = append(v.Skins, skinView{Lang: sk.lang, Label: sk.label})
	}
	names := h.scriptFiles()
	states := h.listStates()
	scripts := h.snapshot()
	privStatus := h.privilegeStatuses()
	// One lookup for the whole table, not one per row: each of these is a
	// collection read, and the row count is unbounded.
	heldModes, heldEffects, storedKeys := h.heldByScript()

	for _, n := range names {
		st, hasState := states[n]
		enabled := !hasState || st.Enabled
		path, sk, ok := h.findScript(n)
		if !ok {
			continue
		}
		// The file content seeds the in-tab editor (fresh at open time:
		// disk changes re-render this button) and, for disabled scripts
		// with no VM, answers "what will this do if I enable it".
		src, _ := os.ReadFile(path)

		var perms grants
		row := scriptRowView{
			Name: n, Ext: sk.ext, Sig: ui.SignalName(n),
			Enabled: enabled, Src: string(src), LastError: st.LastError,
			HeldMode: heldModes[n], HeldEffects: heldEffects[n], StoredKeys: storedKeys[n],
			StatusClass: "sc-dot-ok", StatusGlyph: "●",
		}
		switch {
		case !enabled:
			row.StatusClass, row.StatusGlyph = "sc-dot-off", "○"
		case st.LastError != "":
			row.StatusClass = "sc-dot-err"
		}
		if sc := scripts[n]; sc != nil {
			perms = sc.grants // immutable after load
			sc.mu.Lock()
			for _, info := range sc.infos {
				row.Commands = append(row.Commands, cmdRowView{
					Runnable: info.runnable, Handler: info.handler, Phrase: info.phrase, Mode: info.mode,
				})
			}
			for pat := range sc.events {
				row.Events = append(row.Events, pat)
			}
			sc.mu.Unlock()
			sort.Strings(row.Events)
		} else {
			perms = parsePermissions(string(src), sk.commentPrefix)
		}
		for _, tok := range perms.list() {
			pv := permTokenView{Token: tok}
			if priv, ok := strings.CutPrefix(tok, "call:"); ok {
				if st, known := privStatus[priv]; known && !st.Granted {
					switch {
					case st.Pending:
						pv.Note, pv.NoteClass = "(pending your approval)", "sc-warn"
					case st.Denied:
						pv.Note, pv.NoteClass = "(dismissed — grant under Settings → Plugins)", "sc-danger"
					default:
						pv.Note, pv.NoteClass = "(not granted yet)", "sc-muted"
					}
				}
			}
			row.Perms = append(row.Perms, pv)
		}
		row.RegistersNothing = len(row.Commands) == 0 && len(row.Events) == 0 && st.LastError == ""
		v.Rows = append(v.Rows, row)
	}
	v.Privileges = h.privilegeRowsView(privStatus)
	v.Editors = h.editorChoicesView()
	return v
}

// privilegeRowsView lists only privileges that matter right now: an optional
// one the host holds or was asked for, or any privilege some script's call:
// token wants. REQUIRED privileges with no call demand are the host's own
// floor (dispatch, filesystem) — always granted, not relinquishable, and
// nothing to decide about here; the Plugins page lists them.
//
// Deliberately plugin-side, not platform-side: sub-plugin entities are
// internal to the host by the membership model, so only the host can render
// a live list.
func (h *Host) privilegeRowsView(status map[string]privilegeStatus) []privRowView {
	if len(status) == 0 {
		return nil // older platform, or the introspection call failed
	}
	known := make([]string, 0, len(status))
	for priv := range status {
		known = append(known, priv)
	}
	demand := h.privilegeDemand(known, h.listStates())
	names := make([]string, 0, len(status))
	for priv, st := range status {
		if len(demand[priv]) > 0 || (!st.Required && (st.Granted || st.Pending || st.Denied)) {
			names = append(names, priv)
		}
	}
	sort.Strings(names)
	rows := make([]privRowView, 0, len(names))
	for _, priv := range names {
		st := status[priv]
		r := privRowView{Name: priv, Relinquishable: st.Granted && !st.Required}
		switch {
		case st.Required:
			r.State = "always on (the host requires it)"
		case st.Granted:
			r.State, r.StateClass = "granted", "sc-state-ok"
		case st.Pending:
			r.State, r.StateClass = "pending your approval", "sc-state-pending"
		case st.Denied:
			r.State, r.StateClass = "dismissed", "sc-state-denied"
		default:
			r.State = "not granted"
		}
		if users := demand[priv]; len(users) > 0 {
			r.Note = strings.Join(users, ", ")
		} else {
			r.Note = "no script uses this"
		}
		rows = append(rows, r)
	}
	return rows
}

func (h *Host) editorChoicesView() []editorChoiceView {
	current := h.config().Editor
	choices := []editorChoiceView{{BundleID: "", Label: "System default", Current: current == ""}}
	for _, e := range h.installedEditors() {
		choices = append(choices, editorChoiceView{BundleID: e[0], Label: e[1], Current: e[0] == current})
	}
	return choices
}
