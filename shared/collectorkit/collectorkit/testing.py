"""Test doubles for the browser walks: a page that records, a clock that fakes.

A walk talks to a provider through a Playwright page. A test of the walk
cannot reach the provider, so it hands the walk a `Recorder`'s page
instead. Every call the walk makes on it is one line of the recorder's
`trace`: on the page, on every locator, frame, keyboard, download or
response reached from it, and on the browser context. The line names the
receiver by the chain that reached it, so

    page.locator("#export").first.click(timeout=5000)

is written as exactly that. A `Script` answers the calls. A call no rule
answers gets a neutral default: no element is found, no text is read, a
navigation succeeds. Every other call returns a new object of the same
kind, so chains of any length work.

`FakeClock` replaces `time.sleep`, `time.monotonic` and `time.time` for
the test. Every sleep is a line of the same trace, and so is every
`page.wait_for_timeout`, so a trace pins the order of the waits between
the clicks as well. Time moves only when the walk waits, so a poll loop
ends at once.

A trace holds the receiver chains, the selectors, the routes, and the
arguments as the walk passed them. A long text argument (a JavaScript
body, say) is written as a digest, and a path under the test's root as
`<root>/...`. The trace of a walk, stored as a fixture, is the
mechanical proof that a later restructuring sent the same calls in the
same order.
"""

from __future__ import annotations

import hashlib
import time as _time
from dataclasses import dataclass
from datetime import date as _date
from datetime import datetime as _datetime
from pathlib import Path, PurePath

# Calls that read rather than act, and what they read when no rule
# answers: nothing is on the page.
DEFAULT_ANSWERS = {
    "count": 0,
    "is_visible": False,
    "is_enabled": False,
    "is_checked": False,
    "is_editable": False,
    "is_hidden": True,
    "is_disabled": True,
    "inner_text": "",
    "inner_html": "",
    "text_content": "",
    "input_value": "",
    "content": "",
    "title": "",
    "get_attribute": None,
    "evaluate": None,
    "query_selector": None,
    "query_selector_all": [],
    "eval_on_selector": None,
    "eval_on_selector_all": [],
    "all": [],
    "all_inner_texts": [],
    "all_text_contents": [],
    "cookies": [],
    "json": {},
    "text": "",
    "body": b"",
    "screenshot": b"",
    "all_headers": {},
    "storage_state": {},
}

# Attributes that Playwright exposes as properties, and what they read
# when no rule answers. `None` means a new object of the same kind (the
# locator `first`, the page `keyboard`, a download's `value`).
PROPERTIES = {
    "url": "",
    "frames": [],
    "pages": [],
    "status": 200,
    "ok": True,
    "headers": {},
    "method": "GET",
    "post_data": None,
    "suggested_filename": "download.bin",
    "first": None,
    "last": None,
    "keyboard": None,
    "mouse": None,
    "tracing": None,
    "request": None,
    "context": None,
    "main_frame": None,
    "value": None,
    "chromium": None,
    "firefox": None,
}

# A trace longer than this is a walk that never ends against the script.
MAX_CALLS = 50_000

# Text arguments longer than this are written as a digest.
LONG_TEXT = 200


class Raises:
    """A rule's answer: raise this exception."""

    def __init__(self, exc: BaseException):
        self.exc = exc


class Seq:
    """A rule's answer: these answers in turn; the last one repeats."""

    def __init__(self, *answers):
        self.answers = list(answers)


class Compute:
    """A rule's answer: whatever `fn(call)` returns, worked out at call
    time. `call.rec` is the recorder, so the function can move the page
    (`call.rec.url = ...`) or fire a listener (`call.rec.emit(...)`)."""

    def __init__(self, fn):
        self.fn = fn


# A rule's answer: a new object of the same kind, as an unanswered
# action gets.
NODE = object()


@dataclass
class Call:
    """One call as a rule sees it."""

    rec: Recorder
    receiver: str
    name: str
    args: tuple
    kwargs: dict
    line: str

    @property
    def arg(self):
        """The first positional argument, or None."""
        return self.args[0] if self.args else None


@dataclass
class _Rule:
    name: str
    match: object
    answer: object
    times: int | None

    def matches(self, call: Call) -> bool:
        if self.name != call.name or self.times == 0:
            return False
        if self.match is None:
            return True
        if callable(self.match):
            return bool(self.match(call))
        return str(self.match) in call.line


class Script:
    """How the provider answers a walk.

    `on(name, match, answer)` adds a rule. `name` is a method or a
    property (`url`, `suggested_filename`). `match` is None (any call),
    a text the call's trace line must contain, or a predicate on the
    `Call`. The rule added last is asked first, so a test can start
    from a shared script and override one answer. `times` limits how
    often a rule answers.

    `download` is the body every `save_as` writes, so a walk that reads
    back what it saved goes on.
    """

    def __init__(self, *, url: str = "about:blank", download: bytes = b""):
        self.url = url
        self.download = download
        self._rules: list[_Rule] = []

    def on(self, name: str, match=None, answer=NODE, *, times: int | None = None):
        self._rules.append(_Rule(name, match, answer, times))
        return self

    def rule_for(self, call: Call) -> _Rule | None:
        for rule in reversed(self._rules):
            if rule.matches(call):
                return rule
        return None


class Recorder:
    """The trace of one walk, and the objects the walk drives.

    `page` and `context` are the two roots a walk is handed; `node(name)`
    makes another (a browser, the Playwright handle). `root` is the
    directory paths in the trace are written relative to.
    """

    def __init__(self, script: Script | None = None, *, root=None,
                 clock: FakeClock | None = None):
        self.script = script or Script()
        self.root = str(root) if root is not None else None
        self.trace: list[str] = []
        self.url = self.script.url
        self.listeners: dict[str, list] = {}
        self.clock = clock
        if clock is not None:
            clock.trace = self.trace
        self.page = self.node("page")
        self.context = self.node("context")

    def node(self, path: str) -> _Node:
        return _Node(self, path)

    def emit(self, event: str, *args) -> None:
        """Call every listener registered for `event`, on whichever
        object it was registered, as the browser would."""
        for fn in list(self.listeners.get(event, ())):
            fn(*args)

    def render(self, value) -> str:
        """`value` as the trace writes an argument."""
        if isinstance(value, _Node):
            return value._path
        if isinstance(value, (PurePath, str)):
            text = str(value)
            if self.root and text.startswith(self.root):
                text = "<root>" + text[len(self.root):]
            if len(text) > LONG_TEXT or "\n" in text:
                digest = hashlib.sha256(text.encode()).hexdigest()[:12]
                return f"<text {digest}>"
            return repr(text)
        if isinstance(value, (bytes, bytearray)):
            return f"<bytes {len(value)}>"
        if isinstance(value, dict):
            return "{" + ", ".join(
                f"{self.render(k)}: {self.render(v)}"
                for k, v in value.items()) + "}"
        if isinstance(value, (list, tuple)):
            inner = ", ".join(self.render(v) for v in value)
            return f"[{inner}]" if isinstance(value, list) else f"({inner})"
        if callable(value):
            return "<fn>"
        return repr(value)

    def _answer(self, call: Call, rule: _Rule | None, default):
        if rule is None:
            return default
        if rule.times is not None:
            rule.times -= 1
        answer = rule.answer
        if isinstance(answer, Seq):
            answer = (answer.answers.pop(0) if len(answer.answers) > 1
                      else answer.answers[0])
        if isinstance(answer, Compute):
            answer = answer.fn(call)
        if isinstance(answer, Raises):
            raise answer.exc
        if answer is NODE:
            return default if isinstance(default, _Node) else self.node(
                call.line)
        return answer

    def _call(self, receiver: str, name: str, args: tuple, kwargs: dict):
        rendered = [self.render(a) for a in args]
        rendered += [f"{k}={self.render(v)}" for k, v in kwargs.items()]
        line = f"{receiver}.{name}({', '.join(rendered)})"
        self.trace.append(line)
        if len(self.trace) > MAX_CALLS:
            raise RuntimeError(f"trace passed {MAX_CALLS} calls")
        call = Call(self, receiver, name, args, kwargs, line)
        self._effect(call)
        default = (DEFAULT_ANSWERS[name] if name in DEFAULT_ANSWERS
                   else self.node(line))
        return self._answer(call, self.script.rule_for(call), default)

    def _property(self, receiver: str, name: str):
        line = f"{receiver}.{name}"
        call = Call(self, receiver, name, (), {}, line)
        default = PROPERTIES[name]
        if name == "url" and receiver in ("page", "page.main_frame"):
            default = self.url
        if default is None:
            default = self.node(line)
        return self._answer(call, self.script.rule_for(call), default)

    def _effect(self, call: Call) -> None:
        """What a call does to the recorder besides being recorded."""
        if call.name == "goto" and call.receiver == "page" and call.args:
            self.url = call.args[0]
        elif call.name in ("on", "once") and len(call.args) == 2:
            self.listeners.setdefault(call.args[0], []).append(call.args[1])
        elif call.name in ("remove_listener", "off") and len(call.args) == 2:
            fns = self.listeners.get(call.args[0], [])
            if call.args[1] in fns:
                fns.remove(call.args[1])
        elif call.name == "wait_for_timeout" and self.clock and call.args:
            self.clock.now += call.args[0] / 1000
        elif call.name == "save_as" and call.args:
            path = Path(call.args[0])
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(self.script.download)


class _Node:
    """A page, locator, frame, download, response or context: anything
    a walk reaches. Its attribute names are Playwright's, so the
    recorder's own state lives on the `Recorder`."""

    def __init__(self, rec: Recorder, path: str):
        object.__setattr__(self, "_rec", rec)
        object.__setattr__(self, "_path", path)

    def __getattr__(self, name):
        if name.startswith("__"):
            raise AttributeError(name)
        if name in PROPERTIES:
            return self._rec._property(self._path, name)

        def method(*args, **kwargs):
            return self._rec._call(self._path, name, args, kwargs)
        return method

    def __enter__(self):
        return self

    def __exit__(self, *_exc):
        return False

    def __repr__(self):
        return f"<{self._path}>"


class FakeClock:
    """A clock that moves only when the code under test waits.

    `install(monkeypatch)` puts it in place of `time.sleep`,
    `time.monotonic` and `time.time` for one test. A recorder it is
    handed to writes each sleep into the trace, between the calls.
    """

    def __init__(self, start: float = 1_000_000.0):
        self.now = start
        self.trace: list[str] = []

    def sleep(self, seconds) -> None:
        self.trace.append(f"sleep({seconds!r})")
        self.now += seconds

    def monotonic(self) -> float:
        return self.now

    def time(self) -> float:
        return self.now

    def install(self, monkeypatch) -> FakeClock:
        monkeypatch.setattr(_time, "sleep", self.sleep)
        monkeypatch.setattr(_time, "monotonic", self.monotonic)
        monkeypatch.setattr(_time, "time", self.time)
        return self


class _Frozen(type):
    """A frozen class still owns every instance of the real one, so an
    `isinstance` check in the frozen module reads as before."""

    def __instancecheck__(cls, obj):
        return isinstance(obj, cls.__mro__[1])


def freeze_today(monkeypatch, today: _date, modules) -> None:
    """Make `date.today()` and `datetime.now()` read `today` (at noon,
    in the zone asked for) in each of `modules`, by rebinding the
    module's own `date` and `datetime` names. Other modules keep the
    real clock."""
    y, m, d = today.year, today.month, today.day

    class FrozenDate(_date, metaclass=_Frozen):
        @classmethod
        def today(cls):
            return cls(y, m, d)

    class FrozenDatetime(_datetime, metaclass=_Frozen):
        @classmethod
        def now(cls, tz=None):
            return cls(y, m, d, 12, tzinfo=tz)

        @classmethod
        def utcnow(cls):
            return cls(y, m, d, 12)

    for module in modules:
        if getattr(module, "date", None) is _date:
            monkeypatch.setattr(module, "date", FrozenDate)
        if getattr(module, "datetime", None) is _datetime:
            monkeypatch.setattr(module, "datetime", FrozenDatetime)
