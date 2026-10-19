# BranchKit Python script shim — HOST-OWNED. Written by the Scripts plugin
# at boot and never user-editable; it versions with the host, not with any
# script. Defines the script globals, executes the user file, then
# serves invocations over an NDJSON stdio wire. Every global is a wire call
# the host answers — permission gates run host-side, and a refusal comes
# back as an error reply raised here as BridgeError. The child is never an
# enforcement point.

import json
import os
import sys
import traceback

# The wire owns the real stdio; user print() is redirected to log() below
# so script output can never corrupt the protocol.
_wire_out = sys.stdout
_wire_in = sys.stdin

_next_id = 0
_handlers = {}   # host handler id -> command callback
_event_cbs = {}  # host callback id -> event callback
_timer_cbs = {}  # host timer id -> timer callback (one-shot and repeating)


class BridgeError(Exception):
    """A refusal or failure returned by the host bridge."""


def _send(msg):
    _wire_out.write(json.dumps(msg) + "\n")
    _wire_out.flush()


def _read():
    line = _wire_in.readline()
    if not line:
        # Host closed the pipe: unload. Exit quietly.
        raise SystemExit(0)
    return json.loads(line)


def _rpc(fn, *args):
    global _next_id
    _next_id += 1
    cid = _next_id
    _send({"op": "call", "id": cid, "fn": fn, "args": list(args)})
    while True:
        msg = _read()
        op = msg.get("op")
        if op == "result" and msg.get("id") == cid:
            return msg.get("value")
        if op == "error" and msg.get("id") == cid:
            raise BridgeError(msg.get("message", "bridge error"))
        # The wire is strictly request/response (the host serializes per
        # script); anything else here is stale — skip rather than wedge.


class _LogWriter:
    """Replaces sys.stdout so print() lands in the plugin log."""

    def __init__(self):
        self._buf = ""

    def write(self, text):
        self._buf += text
        while "\n" in self._buf:
            line, self._buf = self._buf.split("\n", 1)
            if line:
                _rpc("log", line)
        return len(text)

    def flush(self):
        pass


# --- The script globals ------------------------------------------------------

def command(pattern, fn, mode=None, enters=None):
    hid = _rpc("command", pattern, mode or "", enters or "")
    _handlers[hid] = fn


def on(event, fn):
    cb = _rpc("on", event)
    _event_cbs[cb] = fn


def dispatch(action, params=None):
    return _rpc("dispatch", action, params or {})


def failed():
    """Why the last dispatch() failed, or None. dispatch() fails soft, so a
    None result means "no result" and "did not run" until you ask."""
    return _rpc("failed")


def call(method, params=None):
    return _rpc("call", method, params or {})


def hold(effect):
    return _rpc("hold", effect)


def release(effect):
    return _rpc("release", effect)


def holder(effect):
    return _rpc("holder", effect)


def record(collection, id):
    return _rpc("record", collection, id)


def records(collection, limit=None, writer=None):
    opts = {}
    if limit is not None:
        opts["limit"] = limit
    if writer is not None:
        opts["writer"] = writer
    return _rpc("records", collection, opts)


def put(collection, id, payload=None):
    _rpc("put", collection, id, payload or {})


def drop(collection, id):
    _rpc("drop", collection, id)


def store(key, value=None):
    _rpc("store", key, value)


def secret_ref(name, prefix=""):
    """A header value naming a secret this file declared (`secret:name@host`).
    The platform substitutes the value at the declared host; the script
    never holds it."""
    return {"__secret": name, "prefix": prefix}


def fetch(url, opts=None):
    return _rpc("fetch", url, opts or {})


def stored(key):
    return _rpc("stored", key)


def mode(name):
    _rpc("mode", name)


def clear_mode():
    _rpc("clear_mode")


def emit(event, payload=None):
    _rpc("emit", event, payload or {})


def choose(options, fn):
    """Ask the user to pick one of `options`.

    `fn` receives the chosen option, or None if nobody answered before the
    chooser timed out. None is a real answer, not a failure: the script gets
    to decide what "the user declined" means.
    """
    cb = _rpc("choose", list(options))
    _event_cbs[cb] = fn


def hud(html):
    _rpc("hud", html)


def timer(seconds, fn):
    cb = _rpc("timer", float(seconds))
    _timer_cbs[cb] = fn
    return cb


def every(seconds, fn):
    cb = _rpc("every", float(seconds))
    _timer_cbs[cb] = fn
    return cb


def cancel(timer_id):
    _rpc("cancel", timer_id)
    _timer_cbs.pop(timer_id, None)


def log(msg):
    _rpc("log", str(msg))


# --- Execution ---------------------------------------------------------------

def _error_message(script_name):
    etype, exc, tb = sys.exc_info()
    if isinstance(exc, BridgeError):
        return str(exc)
    frames = [f for f in traceback.extract_tb(tb) if f.filename == script_name]
    where = " (line %d)" % frames[-1].lineno if frames else ""
    return "".join(traceback.format_exception_only(etype, exc)).strip() + where


def _serve(script_name):
    while True:
        msg = _read()
        if msg.get("op") != "invoke":
            continue
        iid = msg.get("id")
        try:
            if "handler" in msg:
                _handlers[msg["handler"]]()
            elif "event_cb" in msg:
                _event_cbs[msg["event_cb"]](msg.get("payload"))
            elif "timer_cb" in msg:
                # Kept until cancel(): repeating timers fire again, and a
                # cancelled-in-flight fire lands as a benign no-op.
                cb = _timer_cbs.get(msg["timer_cb"])
                if cb:
                    cb()
            else:
                raise BridgeError("unknown invocation: %r" % (msg,))
        except SystemExit:
            raise
        except BaseException:
            _send({"op": "error", "id": iid, "message": _error_message(script_name)})
        else:
            _send({"op": "result", "id": iid})


def _main():
    path = sys.argv[1]
    script_name = os.path.basename(path)
    with open(path, "r", encoding="utf-8") as f:
        src = f.read()
    sys.stdout = _LogWriter()
    scope = {
        "__name__": "__main__",
        "__file__": path,
        "command": command,
        "on": on,
        "dispatch": dispatch,
        "failed": failed,
        "call": call,
        "choose": choose,
        "emit": emit,
        "hold": hold,
        "release": release,
        "holder": holder,
        "record": record,
        "records": records,
        "put": put,
        "drop": drop,
        "store": store,
        "stored": stored,
        "fetch": fetch,
        "mode": mode,
        "clear_mode": clear_mode,
        "hud": hud,
        "timer": timer,
        "every": every,
        "cancel": cancel,
        "log": log,
    }
    try:
        exec(compile(src, script_name, "exec"), scope)
    except SystemExit:
        raise
    except BaseException:
        # Partial-load semantics, same as the embedded skins: registrations
        # made before the error survive (they already happened host-side),
        # and the child keeps serving them.
        _send({"op": "loaded_error", "message": _error_message(script_name)})
    else:
        _send({"op": "loaded"})
    _serve(script_name)


if __name__ == "__main__":
    _main()
