package main

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/branchkit/plugin-sdk-go"
)

// The Python skin: CPython cannot embed in Go, so this is the first
// out-of-process skin — one supervised child per .py file, running the
// embedded bkshim.py under the managed runtime the manifest's
// `"runtimes": ["python"]` grant makes executable, inside this plugin's own
// sandbox. The skin contract is unchanged: boot spawns a process instead of
// creating a VM, and the shim's globals write NDJSON to a pipe instead
// of calling into an interpreter. The bridge stays the trust boundary —
// every shim call lands in the same bridge.go functions the embedded skins
// use, so permission gates run host-side and a refusal rides back over the
// pipe as a Python exception. One child per file mirrors VM-per-file:
// teardown is SIGTERM, hot reload is restart, and a crashed script cannot
// take its siblings down.

//go:embed bkshim.py
var bkshim []byte

var pySkin = skin{
	ext:           ".py",
	lang:          "python",
	label:         "Python",
	commentPrefix: "#",
	template:      pyTemplate,
	boot:          bootPy,
	lint:          lintPy,
}

// pyCallTimeout bounds one load, one handler invocation, and one lint —
// the per-call timeout the embedded skins never needed: a hung child must
// not wedge the host. The call errors onto the row; disable is the escape
// hatch.
const pyCallTimeout = 10 * time.Second

const pyRuntimeMissing = "Python runtime not installed — run: branchkit-cli runtime install python"

// pythonInterpreter resolves the managed CPython the sandbox grants exec
// on, via the BRANCHKIT_RUNTIMES_DIR the platform derives from the same
// policy expression as the grant. The host never downloads anything — no
// runtime on disk is a row error naming the install command, never a
// sandbox violation.
func pythonInterpreter() (string, error) {
	root := os.Getenv("BRANCHKIT_RUNTIMES_DIR")
	if root == "" {
		return "", errors.New(pyRuntimeMissing)
	}
	bin := filepath.Join(root, "python", "bin", "python3")
	if runtime.GOOS == "windows" {
		bin = filepath.Join(root, "python", "python.exe")
	}
	if _, err := os.Stat(bin); err != nil {
		return "", errors.New(pyRuntimeMissing)
	}
	return bin, nil
}

// ensureShim writes the embedded shim into the plugin's data dir once per
// host process — never user-editable, always the version this binary
// shipped with.
var shimOnce struct {
	sync.Once
	path string
	err  error
}

func ensureShim() (string, error) {
	shimOnce.Do(func() {
		dir := branchkit.PluginDataDir()
		if dir == "" {
			dir = filepath.Join(os.TempDir(), "branchkit-scripts-shim")
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			shimOnce.err = err
			return
		}
		shimOnce.path = filepath.Join(dir, "bkshim.py")
		shimOnce.err = os.WriteFile(shimOnce.path, bkshim, 0o644)
	})
	return shimOnce.path, shimOnce.err
}

// pyMsg is one NDJSON wire line, both directions. Four shapes: child→host
// bridge calls (op "call"), host→child invocations (op "invoke", target
// chosen by which id field is set), and paired result/error replies.
type pyMsg struct {
	Op      string `json:"op"`
	ID      int64  `json:"id,omitempty"`
	Fn      string `json:"fn,omitempty"`
	Args    []any  `json:"args,omitempty"`
	Value   any    `json:"value,omitempty"`
	Message string `json:"message,omitempty"`
	Handler string `json:"handler,omitempty"`
	EventCb string `json:"event_cb,omitempty"`
	TimerCb string `json:"timer_cb,omitempty"`
	Payload any    `json:"payload,omitempty"`
}

// pyProc supervises one script's child. The conversation is strictly
// serialized: boot runs before the script is published, and every later
// entry goes through s.call (which holds s.mu) — the same one-at-a-time
// contract the embedded VMs get, so no wire multiplexing is needed.
type pyProc struct {
	s     *Script
	cmd   *exec.Cmd
	stdin io.WriteCloser
	// lines carries parsed child stdout. Small buffer so a stale reply
	// from a timed-out call can land without blocking the reader; closed
	// on EOF.
	lines chan pyMsg

	stderrMu   sync.Mutex
	stderrLast string

	closeOnce sync.Once

	nextInvoke int64
	nextCb     int
}

func bootPy(s *Script, path string) error {
	python, err := pythonInterpreter()
	if err != nil {
		return err
	}
	shim, err := ensureShim()
	if err != nil {
		return fmt.Errorf("writing shim: %v", err)
	}

	p := &pyProc{s: s, lines: make(chan pyMsg, 8)}
	// -I (isolated): no PYTHONPATH, no user site-packages — the child sees
	// the managed stdlib and the script, nothing ambient.
	cmd := exec.Command(python, "-I", shim, path)
	cmd.Env = pyChildEnv(os.Environ())
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting python: %v", err)
	}
	p.cmd = cmd
	p.stdin = stdin
	stdoutDone := make(chan struct{})
	stderrDone := make(chan struct{})
	go p.readStdout(stdout, stdoutDone)
	go p.readStderr(stderr, stderrDone)
	go p.monitor(stdoutDone, stderrDone)
	s.closeVM = p.shutdown

	// Boot conversation: service registrations until the file finishes.
	deadline := time.After(pyCallTimeout)
	for {
		select {
		case msg, ok := <-p.lines:
			if !ok {
				return fmt.Errorf("python exited during load%s", p.stderrTail())
			}
			switch msg.Op {
			case "call":
				p.serviceCall(msg)
			case "loaded":
				return nil
			case "loaded_error":
				return errors.New(msg.Message)
			}
		case <-deadline:
			p.shutdown()
			return fmt.Errorf("script took longer than %s to load — process stopped", pyCallTimeout)
		}
	}
}

func (p *pyProc) readStdout(r io.Reader, done chan struct{}) {
	defer close(done)
	defer close(p.lines)
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if line = strings.TrimSpace(line); line != "" {
			var msg pyMsg
			if jerr := json.Unmarshal([]byte(line), &msg); jerr != nil {
				branchkit.Logf("scripts", "[%s] py wire noise: %.200s", p.s.name, line)
			} else {
				p.lines <- msg
			}
		}
		if err != nil {
			return
		}
	}
}

// readStderr forwards the child's stderr to the plugin log and keeps the
// last line for crash messages.
func (p *pyProc) readStderr(r io.Reader, done chan struct{}) {
	defer close(done)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		branchkit.Logf("scripts", "[%s] py stderr: %s", p.s.name, line)
		p.stderrMu.Lock()
		p.stderrLast = line
		p.stderrMu.Unlock()
	}
}

func (p *pyProc) stderrTail() string {
	p.stderrMu.Lock()
	defer p.stderrMu.Unlock()
	if p.stderrLast == "" {
		return ""
	}
	return " — " + p.stderrLast
}

// monitor reaps the child and routes an unexpected death onto the existing
// error paths: row error published, command group retracted, next save or
// enable restarts. Waits for both pipe readers first (exec.Cmd.Wait must
// not race in-flight reads).
func (p *pyProc) monitor(stdoutDone, stderrDone chan struct{}) {
	<-stdoutDone
	<-stderrDone
	err := p.cmd.Wait()
	s := p.s
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return // teardown-initiated exit
	}
	msg := "python process exited"
	if err != nil {
		msg = fmt.Sprintf("python process exited: %v", err)
	}
	msg += p.stderrTail()
	branchkit.Logf("scripts", "[%s] %s", s.name, msg)
	s.host.onChildExit(s.name, s, msg)
}

// shutdown is the skin's closeVM: EOF on stdin is the shim's unload
// signal, SIGTERM backs it up, kill after a grace period. Never blocks —
// teardown holds the script lock.
func (p *pyProc) shutdown() {
	p.closeOnce.Do(func() {
		_ = p.stdin.Close()
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Signal(syscall.SIGTERM)
			go func(proc *os.Process) {
				time.Sleep(2 * time.Second)
				_ = proc.Kill()
			}(p.cmd.Process)
		}
	})
}

func (p *pyProc) send(msg pyMsg) error {
	raw, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = p.stdin.Write(append(raw, '\n'))
	return err
}

// invoke runs one child-side callback and waits for its paired reply,
// servicing any nested bridge calls the callback makes meanwhile. Stale
// replies (from a call that previously timed out) are discarded by id.
func (p *pyProc) invoke(msg pyMsg) error {
	p.nextInvoke++
	msg.Op = "invoke"
	msg.ID = p.nextInvoke
	if err := p.send(msg); err != nil {
		return fmt.Errorf("python process gone: %v", err)
	}
	deadline := time.After(pyCallTimeout)
	for {
		select {
		case reply, ok := <-p.lines:
			if !ok {
				return errors.New("python process exited" + p.stderrTail())
			}
			switch reply.Op {
			case "call":
				p.serviceCall(reply)
			case "result":
				if reply.ID == msg.ID {
					return nil
				}
			case "error":
				if reply.ID == msg.ID {
					return errors.New(reply.Message)
				}
			}
		case <-deadline:
			return fmt.Errorf("no reply after %s — script busy or hung (disable it to stop the process)", pyCallTimeout)
		}
	}
}

// serviceCall answers one child→host bridge call by delegating to the
// language-neutral bridge — the permission gates and registration
// bookkeeping every skin shares.
func (p *pyProc) serviceCall(msg pyMsg) {
	value, err := p.dispatchCall(msg.Fn, msg.Args)
	reply := pyMsg{ID: msg.ID}
	if err != nil {
		reply.Op = "error"
		reply.Message = err.Error()
	} else {
		reply.Op = "result"
		reply.Value = value
	}
	if serr := p.send(reply); serr != nil {
		branchkit.Logf("scripts", "[%s] py reply: %v", p.s.name, serr)
	}
}

func (p *pyProc) dispatchCall(fn string, args []any) (any, error) {
	arg := func(i int) any {
		if i < len(args) {
			return args[i]
		}
		return nil
	}
	str := func(i int, what string) (string, error) {
		v, ok := arg(i).(string)
		if !ok {
			return "", fmt.Errorf("%s: expected a string, got %T", what, arg(i))
		}
		return v, nil
	}
	switch fn {
	case "command":
		mode, _ := arg(1).(string)
		enters, _ := arg(2).(string)
		var hid string
		id, err := p.s.registerCommand(arg(0), mode, enters, func() error {
			return p.invoke(pyMsg{Handler: hid})
		})
		if err != nil {
			return nil, err
		}
		hid = id
		return id, nil
	case "on":
		pattern, err := str(0, "on")
		if err != nil {
			return nil, err
		}
		p.nextCb++
		cb := fmt.Sprintf("e%d", p.nextCb)
		if err := p.s.registerEvent(pattern, func(payload any) error {
			return p.invoke(pyMsg{EventCb: cb, Payload: payload})
		}); err != nil {
			return nil, err
		}
		return cb, nil
	case "dispatch":
		actionType, err := str(0, "dispatch")
		if err != nil {
			return nil, err
		}
		var params map[string]any
		if raw := arg(1); raw != nil {
			var ok bool
			if params, ok = raw.(map[string]any); !ok {
				return nil, fmt.Errorf("dispatch(%q): params must be a dict", actionType)
			}
		}
		return p.s.bridgeDispatch(actionType, params)
	case "failed":
		return p.s.bridgeFailed(), nil
	case "call":
		method, err := str(0, "call")
		if err != nil {
			return nil, err
		}
		var params map[string]any
		if raw := arg(1); raw != nil {
			var ok bool
			if params, ok = raw.(map[string]any); !ok {
				return nil, fmt.Errorf("call(%q): params must be a dict", method)
			}
		}
		return p.s.bridgeCall(method, params)
	case "record":
		collection, err := str(0, "record")
		if err != nil {
			return nil, err
		}
		id, err := str(1, "record")
		if err != nil {
			return nil, err
		}
		return p.s.bridgeRecord(collection, id)
	case "records":
		collection, err := str(0, "records")
		if err != nil {
			return nil, err
		}
		opts, _ := arg(1).(map[string]any)
		return p.s.bridgeRecords(collection, opts)
	case "put":
		collection, err := str(0, "put")
		if err != nil {
			return nil, err
		}
		id, err := str(1, "put")
		if err != nil {
			return nil, err
		}
		payload, _ := arg(2).(map[string]any)
		return nil, p.s.bridgePut(collection, id, payload)
	case "drop":
		collection, err := str(0, "drop")
		if err != nil {
			return nil, err
		}
		id, err := str(1, "drop")
		if err != nil {
			return nil, err
		}
		return nil, p.s.bridgeDrop(collection, id)
	case "store":
		key, err := str(0, "store")
		if err != nil {
			return nil, err
		}
		return nil, p.s.bridgeStore(key, arg(1))
	case "stored":
		key, err := str(0, "stored")
		if err != nil {
			return nil, err
		}
		return p.s.bridgeStored(key)
	case "fetch":
		rawURL, err := str(0, "fetch")
		if err != nil {
			return nil, err
		}
		opts, _ := arg(1).(map[string]any)
		return p.s.bridgeFetch(rawURL, fetchOptionsFrom(opts))
	case "hold":
		name, err := str(0, "hold")
		if err != nil {
			return nil, err
		}
		return p.s.bridgeHold(name)
	case "release":
		name, err := str(0, "release")
		if err != nil {
			return nil, err
		}
		return p.s.bridgeRelease(name)
	case "holder":
		name, err := str(0, "holder")
		if err != nil {
			return nil, err
		}
		return p.s.bridgeHolder(name)
	case "mode":
		name, err := str(0, "mode")
		if err != nil {
			return nil, err
		}
		return nil, p.s.bridgeMode(name)
	case "clear_mode":
		p.s.bridgeClearMode()
		return nil, nil
	case "emit":
		event, err := str(0, "emit")
		if err != nil {
			return nil, err
		}
		var payload map[string]any
		if raw := arg(1); raw != nil {
			var ok bool
			if payload, ok = raw.(map[string]any); !ok {
				return nil, fmt.Errorf("emit(%q): payload must be a dict", event)
			}
		}
		return nil, p.s.bridgeEmit(event, payload)
	case "choose":
		// Reuses the event-callback channel rather than inventing a third:
		// the shape is identical (one id, one payload delivered later), and
		// the pick arrives asynchronously exactly as an event does.
		p.nextCb++
		cb := fmt.Sprintf("c%d", p.nextCb)
		if err := p.s.bridgeChoose(toOptions(arg(0)), func(pick any) error {
			return p.invoke(pyMsg{EventCb: cb, Payload: pick})
		}); err != nil {
			return nil, err
		}
		return cb, nil
	case "hud":
		html, err := str(0, "hud")
		if err != nil {
			return nil, err
		}
		return nil, p.s.bridgeHud(html)
	case "timer", "every":
		seconds, ok := arg(0).(float64)
		if !ok {
			return nil, fmt.Errorf("%s: expected seconds, got %T", fn, arg(0))
		}
		// The bridge id doubles as the wire callback id, same late-bind
		// trick as command's hid: the closure can't fire before this
		// serviceCall's enclosing lock releases.
		var cb string
		id := p.s.bridgeTimer(seconds, fn == "every", func() error {
			return p.invoke(pyMsg{TimerCb: cb})
		})
		cb = id
		return id, nil
	case "cancel":
		id, err := str(0, "cancel")
		if err != nil {
			return nil, err
		}
		p.s.bridgeCancel(id)
		return nil, nil
	case "log":
		msg, err := str(0, "log")
		if err != nil {
			return nil, err
		}
		p.s.bridgeLog(msg)
		return nil, nil
	}
	return nil, fmt.Errorf("unknown bridge call %q", fn)
}

// lintPy syntax-checks a buffer with a short-lived interpreter rather than
// asking the resident child — stateless, immune to a busy or dead child,
// works for disabled scripts, and compile() executes nothing. Runtime
// absent → the verdict says so.
func lintPy(name, src string) error {
	python, err := pythonInterpreter()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), pyCallTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-I", "-c",
		"import sys; compile(sys.stdin.read(), sys.argv[1], 'exec')", name+".py")
	cmd.Stdin = strings.NewReader(src)
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("lint timed out after %s", pyCallTimeout)
	}
	msg := pyLintMessage(string(out), name+".py")
	if msg == "" {
		return fmt.Errorf("lint: %v", runErr)
	}
	return errors.New(msg)
}

// pyLintMessage trims the interpreter's traceback down to the part about
// the user's buffer.
func pyLintMessage(out, filename string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i, l := range lines {
		if strings.Contains(l, `File "`+filename+`"`) {
			return strings.Join(lines[i:], "\n")
		}
	}
	return strings.TrimSpace(out)
}

const pyTemplate = `# New BranchKit script. Saved changes load instantly — no build, no restart.
#
# The permissions line declares what this script may call; undeclared
# dispatch/call/on/hud calls are refused, and the reason shows on this
# script's row in Settings.
# Grants: dispatch[:prefix], call[:privilege], events[:pattern],
#         emit[:event], mode[:name], effect[:name],
#         read[:collection], write[:collection], hud, choose.
# permissions: hud
#
# API (each call forwards through the Scripts plugin's grants):
#   command(pattern, fn, mode=None, enters=None)
#                             voice command; pattern "note this" or ["volume", ["up", "down"]]
#                             mode="review" gates it on that mode,
#                             enters="review" makes it the way in
#                             (say both halves in one breath)
#   on(event, fn)             platform event, e.g. "_platform.audio_devices.changed"
#   dispatch(action, params)  fire an action, e.g. dispatch("audio.volume_up"); params optional; returns its result
#   failed()                  why the last dispatch failed, or None (it fails soft)
#   emit(event, payload)      announce something as scripts.<this script>.<event>, for other scripts and plugins
#   record(coll, id)          read one entry, or None; entry["payload"] holds the data
#   records(coll, limit=None, writer=None)
#                             read entries; each is
#                             {id, payload, writer, revision, timestamp_ms}
#                             writer names the plugin that created it ("_user" = you)
#   put(coll, id, payload)    contribute a record to a collection that accepts contributions
#   drop(coll, id)            delete one you contributed
#   store(key, value)         save something private to this script (None deletes)
#   stored(key)               read it back
#   hold(effect)              hold a system effect, e.g. hold("suppress_notifications"); check ["granted"] then ["enforced"]
#                             hold("scripts.<name>") mints your own coordination name — no grant needed
#   release(effect)           give it back
#   holder(effect)            who holds it now
#   mode(name)                enter an exclusive mode; only this script's commands for it match
#   clear_mode()              leave it (the escape command also works)
#   call(method, params)      call a platform method, e.g. call("native.frontmost_app") with call:apps
#   hud(html)                 push HTML to the scripts HUD window
#   timer(seconds, fn)        one-shot timer; returns an id for cancel(id)
#   every(seconds, fn)        repeating timer; returns an id for cancel(id)
#   cancel(id)                stop a timer
#   log(msg)                  plugin log line, tagged with this script's name

def my_command():
    hud("<div class='bk-panel'><div class='bk-phrase'>it works</div></div>")

command("my command", my_command)
`

// pyChildEnv is the environment a hosted Python script runs with: the
// plugin's, minus everything that reaches the platform or the network.
//
// The child talks to this plugin over stdin/stdout and needs none of it. It
// used to inherit the whole environment, BRANCHKIT_PROXY included, inside the
// same sandbox that permits the proxy's socket — so a script could open a
// CONNECT tunnel to any host in the PLUGIN-wide allowlist, around the
// per-script `fetch:<host>` narrowing that is sold to script authors as real
// enforcement (2026-09-23 review). Dropping the address is half the fix; the
// sandbox still permits the socket, so a script that guessed the path could
// dial it. Closing that needs a narrower sandbox for the child.
func pyChildEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		upper := strings.ToUpper(k)
		if strings.HasPrefix(upper, "BRANCHKIT_") || strings.HasSuffix(upper, "_PROXY") {
			continue
		}
		out = append(out, kv)
	}
	return out
}
