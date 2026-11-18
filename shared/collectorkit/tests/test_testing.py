"""Tests for collectorkit.testing: the recording page and the fake clock."""
from __future__ import annotations

import sys
import time
from datetime import date, datetime, timezone
from pathlib import Path
import types

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from collectorkit import testing  # noqa: E402
from collectorkit.testing import (  # noqa: E402
    NODE, Compute, Raises, Recorder, Script, Seq,
)


def test_a_chain_is_one_line_per_call_named_by_its_receiver():
    rec = Recorder()
    rec.page.locator("#a").first.click(timeout=5)
    rec.page.frame_locator("iframe").locator("b").fill("x")
    rec.page.keyboard.press("Tab")
    assert rec.trace == [
        "page.locator('#a')",
        "page.locator('#a').first.click(timeout=5)",
        "page.frame_locator('iframe')",
        "page.frame_locator('iframe').locator('b')",
        "page.frame_locator('iframe').locator('b').fill('x')",
        "page.keyboard.press('Tab')",
    ]


def test_an_unanswered_read_finds_nothing():
    page = Recorder().page
    assert page.locator("x").count() == 0
    assert page.locator("x").is_visible() is False
    assert page.locator("x").inner_text() == ""
    assert page.locator("x").get_attribute("href") is None
    assert page.evaluate("() => 1") is None
    assert page.locator("x").all() == []


def test_rules_match_by_text_or_predicate_and_the_last_one_wins():
    s = Script()
    s.on("count", "#a", 1)
    s.on("count", "#a", 2)
    s.on("evaluate", lambda c: "href" in c.arg, "https://x.test/")
    page = Recorder(s).page
    assert page.locator("#a").count() == 2
    assert page.locator("#b").count() == 0
    assert page.evaluate("() => location.href") == "https://x.test/"


def test_answers_in_turn_limited_raised_or_computed():
    s = Script()
    s.on("count", "#a", Seq(0, 1))
    s.on("click", "#b", Raises(RuntimeError("boom")), times=1)
    s.on("inner_text", None, Compute(lambda c: c.receiver.upper()))
    s.on("new_page", None, Compute(lambda c: c.rec.page))
    rec = Recorder(s)
    page = rec.page
    assert [page.locator("#a").count() for _ in range(3)] == [0, 1, 1]
    with pytest.raises(RuntimeError):
        page.locator("#b").click()
    page.locator("#b").click()               # the rule is used up
    assert page.locator("q").inner_text() == "PAGE.LOCATOR('Q')"
    assert rec.context.new_page() is page


def test_node_answer_is_a_new_object_of_the_same_kind():
    s = Script().on("evaluate_handle", None, NODE)
    handle = Recorder(s).page.evaluate_handle("() => document")
    handle.click()
    assert handle._rec.trace[-1] == "page.evaluate_handle('() => document').click()"


def test_goto_moves_the_page_and_listeners_hear_emitted_events():
    rec = Recorder(Script(url="https://x.test/start"))
    assert rec.page.url == "https://x.test/start"
    rec.page.goto("https://x.test/next")
    assert rec.page.url == "https://x.test/next"
    heard = []
    rec.page.on("request", heard.append)
    rec.emit("request", "r1")
    rec.page.remove_listener("request", heard.append)
    rec.emit("request", "r2")
    assert heard == ["r1"]


def test_a_download_is_saved_with_the_scripted_body(tmp_path):
    rec = Recorder(Script(download=b"a,b\n"), root=tmp_path)
    with rec.page.expect_download(timeout=10) as info:
        rec.page.locator("#dl").click()
    assert info.value.suggested_filename == "download.bin"
    info.value.save_as(str(tmp_path / "out" / "f.csv"))
    assert (tmp_path / "out" / "f.csv").read_bytes() == b"a,b\n"
    assert rec.trace[-1] == (
        "page.expect_download(timeout=10).value.save_as('<root>/out/f.csv')")


def test_arguments_render_stably():
    rec = Recorder(root="/r")
    rec.page.evaluate("x" * 300, {"k": [1, b"ab", Path("/r/p")]})
    rec.page.on("load", lambda: None)
    long_arg = rec.trace[0].split("(", 1)[1].split(",", 1)[0]
    assert long_arg.startswith("<text ") and len(long_arg) == 19
    assert rec.trace[0].endswith("{'k': [1, <bytes 2>, '<root>/p']})")
    assert rec.trace[1] == "page.on('load', <fn>)"


def test_a_walk_that_never_ends_stops(monkeypatch):
    monkeypatch.setattr(testing, "MAX_CALLS", 10)
    page = Recorder().page
    with pytest.raises(RuntimeError, match="trace passed"):
        while page.locator("x").count() == 0:
            pass


def test_the_clock_moves_only_when_the_code_waits(monkeypatch):
    clock = testing.FakeClock(start=100.0).install(monkeypatch)
    rec = Recorder(clock=clock)
    deadline = time.monotonic() + 2
    while time.monotonic() < deadline:
        time.sleep(0.5)
    rec.page.wait_for_timeout(1500)
    assert time.time() == 103.5
    assert rec.trace == ["sleep(0.5)"] * 4 + ["page.wait_for_timeout(1500)"]


def test_freeze_today_rebinds_only_the_named_modules(monkeypatch):
    frozen, other = types.ModuleType("frozen"), types.ModuleType("other")
    for m in (frozen, other):
        m.date, m.datetime = date, datetime
    testing.freeze_today(monkeypatch, date(2026, 1, 15), [frozen])
    assert frozen.date.today() == date(2026, 1, 15)
    assert frozen.datetime.now(timezone.utc) == datetime(
        2026, 1, 15, 12, tzinfo=timezone.utc)
    assert isinstance(date(2020, 1, 1), frozen.date)
    assert other.date is date
