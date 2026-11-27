"""Credential redaction for everything a run writes to disk.

The pure half of the `--debug` captures: string and JSON rewriting, no I/O
beyond the HAR rewrite, and no Playwright import (:class:`SessionMask`
reads the jar of the page it is handed). :mod:`collectorkit.debugcap`
re-exports every public name, so a capture site reads them from there.

Two complementary passes run throughout:

  * by NAME: a value is masked because of where it sits, the header,
    query parameter, form field or JSON key that names it a credential.
    That is the only way to catch a secret the site issues at runtime, or
    one typed by hand that no process ever saw.
  * by VALUE: :func:`secret_redactor` masks every spelling of the
    credentials a run knows, wherever they appear.

A serialized DOM gets its own rule (:func:`scrub_dom`): a password input
loses its value whatever the run knows.
"""
from __future__ import annotations

import contextlib
import functools
import json
import logging
import os
import re
import urllib.parse
from pathlib import Path


# Query parameters whose value is a credential. fred puts its API key in the
# query string (collectors/fred/AGENTS.md §2), and a captured URL would
# otherwise persist it to disk.
_SECRET_PARAMS = frozenset({
    "api_key", "apikey", "key", "token", "access_token", "refresh_token",
    "id_token", "secret", "client_secret", "password", "passwd", "pwd",
    "sig", "signature", "auth", "authorization", "session", "sessionid",
})

REDACTED = "<redacted>"

# Header names whose VALUE is a credential. The query-parameter set covers
# most of them (`apikey`, `authorization`, `token`, …); the rest are
# header-only — cookies, which carry the live session, and the CSRF token a
# SPA echoes back on every mutating call. A source that names its CSRF
# header dynamically (`x-csrft<N>`) is out of reach of any static list and
# is left to the value redactor.
_SECRET_HEADERS = frozenset(_SECRET_PARAMS | {
    "cookie", "set-cookie", "x-api-key", "x-auth-token", "proxy-authorization",
    "x-csrf-token", "x-xsrf-token",
})

# The names a credential can arrive under ANYWHERE in a recorded exchange.
# Header, query parameter and form field are three places for one name, not
# three vocabularies: a sign-in POST carries its CSRF token as a body field
# while the SPA that follows sends the same token as a header, and a key
# read from `x-api-key` on one call rides the query string on the next.
# Keyed per place, each name would be masked only where it was first seen,
# so the widest set applies wherever a name is what identifies a value.
_SECRET_NAMES = frozenset(_SECRET_HEADERS)

# A percent-escape, for re-spelling one in lower-case hex.
_PCT_ESCAPE_RE = re.compile(r"%[0-9A-Fa-f]{2}")


def redact_url(url: str) -> str:
    """Return `url` with credential-bearing query parameters masked.

    Keeps the parameter NAME — that a request carried an `api_key` is the
    diagnostic; its value is the secret. A URL that cannot be parsed is
    dropped entirely rather than guessed at: an unparseable string is
    precisely the case where a naive mask could miss.
    """
    try:
        parts = urllib.parse.urlsplit(url)
    except ValueError:
        return REDACTED
    if not parts.query:
        return url
    pairs = urllib.parse.parse_qsl(parts.query, keep_blank_values=True)
    masked = [(k, REDACTED if k.lower() in _SECRET_PARAMS else v)
              for k, v in pairs]
    return urllib.parse.urlunsplit(
        parts._replace(query=urllib.parse.urlencode(masked)))


def safe_error(exc: BaseException) -> str:
    """An exception rendered safely enough to persist.

    A transport client's exception is not a one-line message. Playwright's
    request errors append a ``Call log:`` block reproducing every request
    header — which for an authenticated session means the API key and the
    whole cookie jar. Anything that writes ``str(exc)`` into a manifest, a
    log file, or a captured artefact therefore writes a live credential
    to disk.

    This keeps what diagnoses (the exception class and its first line,
    which carries the failure and the timeout) and drops what leaks
    (everything from the call log on), then masks credential-bearing query
    parameters in whatever URL survives.
    """
    first = safe_text(str(exc).split("\n", 1)[0]).strip()
    return f"{type(exc).__name__}: {first}" if first else type(exc).__name__


def safe_text(text: str) -> str:
    """`text` with every Playwright call log cut off and the
    credential-bearing parameters of every URL in it masked.

    A call log runs from its ``Call log:`` line to the end of the message
    that holds it, so everything from there on goes. The rest of a message
    stays as it was, line breaks included: it names the failure. A URL
    with nothing to mask keeps its spelling."""
    marker = text.find("Call log:")
    if marker >= 0:
        text = text[:marker].rstrip()

    def mask(m):
        url = m.group(0)
        return redact_url(url) if _carries_secret(url) else url

    return re.sub(r"https?://\S+", mask, text)


def _carries_secret(url: str) -> bool:
    """Whether redact_url would mask a parameter of `url` (or the whole
    of a URL it cannot parse)."""
    try:
        query = urllib.parse.urlsplit(url).query
    except ValueError:
        return True
    return any(k.lower() in _SECRET_PARAMS
               for k, _ in urllib.parse.parse_qsl(query, keep_blank_values=True))


def _chain(exc: BaseException) -> list[BaseException]:
    """`exc` and the exceptions it was raised from or while handling, the
    oldest first, as a traceback prints them."""
    chain: list[BaseException] = []
    seen: set[int] = set()
    while exc is not None and id(exc) not in seen:
        seen.add(id(exc))
        chain.append(exc)
        exc = exc.__cause__ or (None if exc.__suppress_context__
                                else exc.__context__)
    return chain[::-1]


def leaks(exc: BaseException) -> bool:
    """Whether printing `exc` or an exception of its chain as Python does
    would print a call log or a URL secret."""
    return any(safe_text(str(e)) != str(e) for e in _chain(exc))


def safe_traceback(exc: BaseException) -> str:
    """The traceback Python prints for `exc`, with each message of its
    chain passed through :func:`safe_text`.

    The frames stay: they name files, lines and code, never a value. A
    note added to an exception is left out, since it can hold anything."""
    import traceback

    parts: list[str] = []
    chain = _chain(exc)
    for i, e in enumerate(chain):
        if i:
            parts.append(
                "\nThe above exception was the direct cause of the "
                "following exception:\n\n" if e.__cause__ is chain[i - 1]
                else "\nDuring handling of the above exception, another "
                "exception occurred:\n\n")
        if e.__traceback__ is not None:
            parts.append("Traceback (most recent call last):\n")
            parts.extend(traceback.format_tb(e.__traceback__))
        message = safe_text(str(e))
        parts.append(f"{type(e).__qualname__}: {message}\n" if message
                     else f"{type(e).__qualname__}\n")
    return "".join(parts)


def redact_headers(headers) -> dict:
    """Header mapping with credential-bearing values masked by NAME.

    The complement of :func:`redact_url`: that masks a secret whose value
    is known from the query parameter it sits in, this masks one whose
    value is only knowable from the header it arrived in — an ``apikey``
    a site issues at runtime, a ``cookie`` a session sets. Keeping the
    name and dropping the value preserves the diagnostic (that the
    request carried a key) without the key.
    """
    if not headers:
        return {}
    try:
        items = headers.items()
    except AttributeError:
        return {}
    return {k: (REDACTED if k.lower() in _SECRET_HEADERS else v)
            for k, v in items}


# Masking a value where it sits and hunting the same string through the
# bodies around it are two different moves, and only the first is certain.
# A value is masked in place because of where it sits — every one, however
# short. Chasing it elsewhere is an inference, and on a short value a wrong
# one: a `session=1`, or an `?auth=0`, would blank every `1` or `0` in the
# response body beside it and leave a capture that reads as redacted while
# showing nothing. So the echo pass has a length floor, and the case the
# floor would otherwise leave exposed — a short credential posted in a
# body — is covered by name instead: a form field is masked because the
# field names it (`_mask_urlencoded`), a JSON member because its key does
# (`_mask_json_secrets`), neither by chasing the value.
_MIN_ECHOED_SECRET = 8


def _echo(found, value) -> None:
    """Record `value` for the echo pass, if it is long enough to chase."""
    if isinstance(value, str) and len(value) >= _MIN_ECHOED_SECRET:
        found.add(value)


def _mask_har_pairs(pairs, names, found) -> None:
    """Mask the `value` of every {name, value} pair a credential name
    selects, recording what was masked in `found`.

    ``names=None`` masks every pair — what the cookie arrays need, since a
    cookie jar IS the session whatever the cookies are called.
    """
    if not isinstance(pairs, list):
        return
    for pair in pairs:
        if not isinstance(pair, dict):
            continue
        if names is None or str(pair.get("name", "")).lower() in names:
            _echo(found, pair.get("value"))
            pair["value"] = REDACTED


def _mask_json_secrets(node, found) -> bool:
    """Mask credential-named values anywhere in a parsed JSON body,
    recording what was masked in `found`. Returns whether anything was.

    The complement of the ``postData.params`` pass: Playwright parses
    ``params`` for a form-encoded body only, so a JSON or GraphQL sign-in
    — the normal shape for the SPAs these harnesses target — arrives as
    ``text`` alone and the parsed pass finds nothing to mask. Walks
    dicts and lists, so a credential nested under a wrapper object is
    reached too.

    A member the key selects is replaced whatever it holds, an object or
    an array included: a `session` that is a whole object is a session
    the same way a `session` that is a string is, and keeping its
    interior because the key happened to hold structure would be the one
    reading that leaks. Only a string is worth chasing elsewhere, and
    only above the floor.
    """
    hit = False
    if isinstance(node, dict):
        # Assigning to an existing key does not resize the dict, so the
        # rewrite is safe to do while iterating it.
        for key, value in node.items():
            if str(key).lower() in _SECRET_NAMES:
                _echo(found, value)
                node[key] = REDACTED
                hit = True
            elif _mask_json_secrets(value, found):
                hit = True
    elif isinstance(node, list):
        for item in node:
            if _mask_json_secrets(item, found):
                hit = True
    return hit


def _mask_urlencoded(text: str, found) -> str | None:
    """A form-urlencoded body with its credential-named fields masked, or
    None when it carries none.

    Two passes reach a form body and neither reaches it alone. The
    ``postData.params`` pass masks by name, but only where the recorder
    parsed the body into ``params`` — a body it did not recognise arrives
    as ``text`` with no parsed twin. The echo pass masks by value, but a
    value below the floor is not chased, and a sign-in field is exactly
    where a short credential sits. Masking the field by NAME here covers
    both: the field names the credential whatever the recorder did with
    the body, and however short the value is.

    Only the credential fields are rewritten; every other field keeps the
    spelling it was recorded in, so a body that merely claims to be a form
    is left as it is rather than re-encoded into something it never was.
    Returning None says nothing was masked, which leaves the original text
    for the echo pass.
    """
    out, hit = [], False
    for field in text.split("&"):
        name, sep, value = field.partition("=")
        if sep and urllib.parse.unquote_plus(name).lower() in _SECRET_NAMES:
            # Both spellings: the value as it was recorded, and what it
            # decodes to. Either can be the one echoed elsewhere.
            _echo(found, value)
            _echo(found, urllib.parse.unquote_plus(value))
            out.append(f"{name}={REDACTED}")
            hit = True
        else:
            out.append(field)
    return "&".join(out) if hit else None


def _mask_body_by_name(text: str, mime, found) -> str:
    """`text` with every field or member a credential NAME selects masked,
    recording what was masked in `found`.

    The body's own syntax says what the names are: a form's field names, a
    JSON object's keys. A body in neither shape has no names to read, so it
    is returned as it is and left to the value-based mask.
    """
    kind = str(mime or "").lower()
    if "urlencoded" in kind:
        masked = _mask_urlencoded(text, found)
        return text if masked is None else masked
    if "json" in kind:
        try:
            body = json.loads(text)
        except (ValueError, RecursionError):
            return text
        if _mask_json_secrets(body, found):
            return json.dumps(body)
    return text


def redact_body(text, mime=None, redact=None):
    """A request body with its credentials masked by NAME and by VALUE.

    The body-shaped counterpart to :func:`redact_headers`. `redact` masks
    the credentials the caller was handed; the field or key a value sits
    under masks the ones only the wire can name — the sign-in form's own
    password field on a run where the value was typed by hand, and nothing
    in the process ever saw it. A form-urlencoded body is masked field by
    field and a JSON one member by member at any depth; a body in neither
    shape gets the value-based mask alone.

    What the name pass masks is then masked wherever the same body spells
    it again — a token in a JSON member and again in a free-text field
    beside it — which is the echo pass :func:`_redact_har_entry` runs for
    the same reason. It stays body-local: only values this body named are
    echoed, so one body's secret can never leak into another's mask.

    Best effort and never raising, like the rest of this module: a body
    that does not parse is returned with the value mask applied and
    nothing else.
    """
    if not isinstance(text, str) or not text:
        return text
    found: set = set()
    text = _mask_body_by_name(text, mime, found)
    if found:
        text = secret_redactor(*sorted(found))(text)
    return redact(text) if redact is not None else text


def _as_dict(value) -> dict:
    return value if isinstance(value, dict) else {}


def _redact_har_entry(entry: dict, redact) -> None:
    """Mask one HAR entry in place, in the two complementary passes the
    JSONL logs use: by NAME wherever the header, parameter, form field or
    JSON key a value sits in is the only thing that identifies it, then by
    VALUE for every secret either the caller or the first pass knows."""
    request = _as_dict(entry.get("request"))
    response = _as_dict(entry.get("response"))
    post = _as_dict(request.get("postData"))
    # Each {name, value} array with the names whose value is a credential.
    # None masks every pair, which is what the cookie arrays need.
    pair_lists = (
        (request.get("headers"), _SECRET_NAMES),
        (response.get("headers"), _SECRET_NAMES),
        (request.get("cookies"), None),
        (response.get("cookies"), None),
        (request.get("queryString"), _SECRET_NAMES),
        (post.get("params"), _SECRET_NAMES),
    )
    found: set = set()
    for pairs, names in pair_lists:
        _mask_har_pairs(pairs, names, found)
    # A body carries its own copy of what the pass above masked, in a
    # spelling that pass never touched — and, where the recorder filled no
    # `params`, the only copy there is. Each shape is masked by the name
    # its own syntax gives the value.
    raw = post.get("text")
    if isinstance(raw, str):
        post["text"] = _mask_body_by_name(raw, post.get("mimeType"), found)
    # What the first pass masked is echoed in the raw spellings beside it:
    # postData.text holds the same body the params were parsed from, and a
    # token minted in one response rides the next request's URL.
    echo = secret_redactor(*sorted(found)) if found else None

    def clean(value):
        if not isinstance(value, str):
            return value
        if redact is not None:
            value = redact(value)
        return echo(value) if echo is not None else value

    for pairs, _ in pair_lists:
        if not isinstance(pairs, list):
            continue
        for pair in pairs:
            if isinstance(pair, dict):
                pair["value"] = clean(pair.get("value"))
    # Both fields on both halves. The spec puts `url` on the request and
    # `redirectURL` on the response, but a recorder that also writes the
    # other spelling would otherwise leave a URL — and the hop carrying a
    # one-time token in its query string is exactly the one a sign-in
    # emits.
    for message in (request, response):
        for field in ("url", "redirectURL"):
            if isinstance(message.get(field), str):
                message[field] = redact_url(clean(message[field]))
    if isinstance(post.get("text"), str):
        post["text"] = clean(post["text"])
    content = _as_dict(response.get("content"))
    if isinstance(content.get("text"), str):
        if str(content.get("encoding", "")).lower() == "base64":
            # A value-based mask cannot see inside base64, and
            # :func:`secret_variants` deliberately does not pretend to
            # cover an opaque re-encoding. Passing such a body through
            # would leave it whole in a file that now reads as redacted,
            # which is worse than not having it: the body goes, and that
            # there was one stays.
            content["text"] = "<base64 body, not redacted>"
            content.pop("encoding", None)
        else:
            content["text"] = clean(content["text"])


def redact_har(path: Path, redact=None, *, log: logging.Logger) -> bool:
    """Rewrite a Playwright-written HAR in place with its credentials out.

    Playwright records the HAR itself, and records it whole: the login POST
    body (as ``text`` AND as parsed ``params``), every request and response
    header, the cookie jar, the query string. Nothing the capturing code
    passes to Playwright narrows that — ``record_har_content="omit"`` blanks
    only ``content.text`` and leaves the parsed body, the URLs and the
    cookies untouched — so the file is cleaned after the fact instead. It
    exists only once the context has closed, which is where this belongs.

    Two passes, the same complementary pair the JSONL logs use: by NAME for
    a value only the header, parameter, form field or JSON key it sits in
    identifies (a session cookie, a bearer token, a CSRF token, an API key
    the site issued at runtime), and by VALUE for the credentials `redact`
    knows plus whatever the first pass turned up. A base64 response body is
    the one thing dropped rather than masked: no value-based pass can see
    inside it, so leaving it would put a whole body in a file that reads as
    redacted.

    Best effort, like every capture here: an unreadable or unparseable file
    is left alone with a warning rather than taking down the run that
    produced it, and so is one whose rewrite fails part-way — the original
    survives, holding what it always held. The rewrite is compact rather
    than re-indented — a HAR is read through a viewer, and a capture with
    bodies in it is large. Returns whether the file was rewritten.
    """
    p = Path(path)
    if not p.is_file():
        return False
    try:
        har = json.loads(p.read_text(encoding="utf-8"))
        entries = har["log"]["entries"]
        if not isinstance(entries, list):
            raise ValueError("log.entries is not a list")
    except (OSError, ValueError, KeyError, TypeError, RecursionError) as e:
        log.warning("HAR left as it is and NOT redacted — it still holds "
                    "every credential it recorded: %s: %s", p, e)
        return False
    tmp = p.with_name(p.name + ".redacting")
    try:
        for entry in entries:
            if isinstance(entry, dict):
                _redact_har_entry(entry, redact)
        tmp.write_text(json.dumps(har), encoding="utf-8")
        os.replace(tmp, p)
    except (OSError, ValueError, TypeError, RecursionError) as e:
        log.warning("HAR rewrite failed, so it is NOT redacted — it still "
                    "holds every credential it recorded: %s: %s", p, e)
        with contextlib.suppress(OSError):
            tmp.unlink()
        return False
    return True


# An <input> tag, with whatever attributes it carries. Serializing a live DOM
# can emit the typed value as a `value=` attribute.
#
# Attribute values are skipped over as units rather than scanned for the next
# `>`: a password containing `>` closes the tag early for a naive `[^>]*`
# matcher, and the credential then survives the scrub — which is the one
# input this exists to catch.
_INPUT_TAG_RE = re.compile(
    r"""<input\b(?:[^>"']|"[^"]*"|'[^']*')*>""", re.I)
_PASSWORD_TYPE_RE = re.compile(r"""\btype\s*=\s*(["']?)password\1""", re.I)
# `\b` would also anchor inside a hyphenated name, so `data-value=` on a
# password input lost the very attribute a selector may be pinned to. A
# hyphen is not a word character, so the name is anchored by hand.
_VALUE_ATTR_RE = re.compile(
    r"""(?<![\w-])value\s*=\s*("[^"]*"|'[^']*'|[^\s>]*)""", re.I)


def scrub_dom(html: str, redact=None, *, also_blank=()) -> str:
    """Strip credentials from a serialized DOM before it is written out.

    Two independent defences, because each catches what the other cannot:

    * Every ``type="password"`` input loses its ``value`` attribute. This
      holds even when the capturing code has no idea what the password is
      — a harness run with ``--no-prefill``, where the human typed it, is
      exactly the case a value-based redactor cannot cover, and exactly
      the case a discovery session is most likely to be run in.
    * `redact`, when given (a :func:`secret_redactor`), masks the known
      credentials anywhere else in the markup — a hidden field, an inline
      script, a data attribute.

    `also_blank` names markers (matched case-insensitively anywhere in
    the tag) of further inputs whose value goes too. A one-time-code box
    is a plain text input holding one digit, so neither defence above
    reaches it; the id or name it carries does.

    A DOM snapshot is the record selectors are pinned from, so it is kept
    whole apart from these; nothing else is rewritten.
    """
    if not html:
        return html
    markers = tuple(m.lower() for m in also_blank)

    def blank(match):
        # Blank `value` on a password or marked input, leave every other
        # input alone.
        tag = match.group(0)
        if not (_PASSWORD_TYPE_RE.search(tag)
                or any(m in tag.lower() for m in markers)):
            return tag
        return _VALUE_ATTR_RE.sub('value=""', tag)

    html = _INPUT_TAG_RE.sub(blank, html)
    return redact(html) if redact is not None else html


def secret_variants(secret: str) -> tuple[str, ...]:
    """Every spelling `secret` can take in a request or a serialized DOM.

    A credential does not reach the wire verbatim. A login form body is
    ``application/x-www-form-urlencoded``, so every reserved character is
    percent-encoded; a JSON body escapes quotes, backslashes and (with
    ``ensure_ascii``) non-ASCII; and a serialized DOM — the one artefact
    that always spells it that way — entity-escapes ``&``, ``"`` and the
    angle brackets. A redactor that only knows the raw string therefore
    masks the username — which is usually alphanumeric and survives
    encoding unchanged — while writing a password full of punctuation to
    disk in full.

    Returned longest-first, so a longer spelling is masked before a shorter
    one can match inside it. Duplicates are dropped, which is the common
    case for an alphanumeric secret whose spellings all coincide.

    Base64 and other opaque re-encodings are deliberately NOT covered — a
    value the page transformed before sending is unrecognisable here, and
    pretending otherwise would sell false assurance. What the wire formats
    above carry, this catches.
    """
    if not secret:
        return ()
    out = {secret}
    for enc in (urllib.parse.quote(secret, safe=""),
                urllib.parse.quote_plus(secret)):
        out.add(enc)
        # Percent-escapes are hex, and clients disagree on its case
        # (%5E vs %5e). Both spellings decode to the same byte, so both
        # have to be masked.
        out.add(_PCT_ESCAPE_RE.sub(lambda m: m.group(0).lower(), enc))
    # json.dumps quotes the string; strip the quotes it added.
    out.add(json.dumps(secret)[1:-1])
    out.add(json.dumps(secret, ensure_ascii=False)[1:-1])
    # A serialized DOM (`page.content()` is outerHTML) entity-escapes `&`
    # and U+00A0 always, `"` in an attribute value, and `<`/`>` in a text
    # node — and, in serializers newer than the Firefox the pinned Camoufox
    # ships, in attributes too. `'` is never escaped by a serializer, so
    # there is no `&#39;` spelling to carry. `&` is replaced first, so the
    # entities the later replacements introduce are not re-escaped.
    amp = secret.replace("&", "&amp;").replace("\xa0", "&nbsp;")
    text = amp.replace("<", "&lt;").replace(">", "&gt;")
    out.add(text)                          # text node
    out.add(amp.replace('"', "&quot;"))    # attribute value
    out.add(text.replace('"', "&quot;"))   # attribute value, newer serializer
    return tuple(sorted(out, key=len, reverse=True))


def secret_redactor(*secrets: str, placeholder: str = REDACTED):
    """A ``str -> str`` masker for every spelling of every `secret`.

    Built once per run and applied to headers, request bodies and response
    bodies alike. Falsy secrets are skipped; with none left the returned
    function is the identity, so a harness running without credentials pays
    nothing.

    Deliberately over-redacts rather than under-redacts: a short secret can
    mask innocuous text, which costs a debug capture some legibility, while
    the opposite failure writes a credential to disk.
    """
    variants: list[str] = []
    seen: set[str] = set()
    for secret in secrets:
        for variant in secret_variants(secret):
            if variant not in seen:
                seen.add(variant)
                variants.append(variant)
    variants.sort(key=len, reverse=True)
    if not variants:
        return lambda value: value

    def redact(value):
        if not value:
            return value
        for variant in variants:
            value = value.replace(variant, placeholder)
        return value

    return redact


@functools.cache
def env_redactor(user_env: str, pass_env: str):
    """The run's credential mask, built once from the environment.

    A login flow sources its env file before the first capture, so the
    credentials are in the environment by the time this is first asked
    for, and the cache keeps it one build per run. On a run that was
    given none — the credentials are typed by hand — this is the
    identity, and the password-input blanking in :func:`scrub_dom` is
    what covers the markup instead.
    """
    return secret_redactor(os.environ.get(user_env, ""),
                           os.environ.get(pass_env, ""))


def session_redactor(cookies, *extra: str):
    """A :func:`secret_redactor` for the session a run was handed.

    A download run is given no password. What it is given is a lifted
    session, and that session IS its credential: an SPA that prints its
    CSRF token into a `<meta>` tag, or bootstraps its state into an inline
    script, puts the live session straight into a captured DOM. `cookies`
    is a Playwright cookie jar — `context.cookies()`, or the BYO list a
    run loads from disk — and `extra` carries any credential the caller
    knows besides.

    Selection is by LENGTH, not by name: a session cookie is named
    whatever the source chose, so no fleet-wide name list can find it,
    while a value too short to be a token is a locale, a consent flag, a
    bucket id — and masking `en` would blank the letters out of the very
    markup the capture exists to show. `extra` is not filtered that way; a
    credential the caller names is not a guess.
    """
    try:
        jar = list(cookies or ())
    except TypeError:
        # Never fatal: a jar in a shape this cannot read yields a mask for
        # `extra` alone rather than an exception out of a diagnostics path.
        jar = []
    values = [c["value"] for c in jar
              if isinstance(c, dict) and isinstance(c.get("value"), str)
              and len(c["value"]) >= _MIN_ECHOED_SECRET]
    return secret_redactor(*values, *extra)


class SessionMask:
    """The run's session mask, built once, on the first capture.

    :func:`session_redactor` needs a jar, and a harness driving a
    PERSISTENT browser profile has none to hand at wiring time: the
    capture path is written before the browser exists, and the profile's
    cookies belong to the browser process, not to a state file the run
    loaded. Reading the jar at the first capture — which happens after a
    navigation, or the capture would have nothing to show — reads it when
    it is populated, and caching the result keeps it one read per run.

    A harness whose session came from a state file or a BYO cookie list
    has the jar before it opens a browser and calls
    :func:`session_redactor` directly; this exists for the ones that do
    not. `extra` carries any credential known besides the session.
    """

    def __init__(self, *extra: str) -> None:
        self._extra = extra
        self._redact = None

    def for_page(self, page):
        """The mask, reading `page`'s jar the first time it is asked.

        Never raises: a jar that cannot be read yields a mask that masks
        the known `extra` alone, which is worse than the full one and far
        better than a capture that fails.
        """
        if self._redact is None:
            try:
                cookies = page.context.cookies()
            except Exception:  # noqa: BLE001 — diagnostics never break a run
                cookies = ()
            self._redact = session_redactor(cookies, *self._extra)
        return self._redact


# A run of hex long enough to be an id rather than a word. Session-scoped
# ids (a challenge page's url id, document tokens) are what make a captured
# URL unsafe to paste into a bug report; the PATH is what diagnoses a moved
# endpoint, so the shape is kept and the ids are not.
_HEX_ID_RE = re.compile(r"[0-9a-f]{8,}", re.I)


def url_shape(url: str) -> str:
    """A URL reduced to its shape: credential params masked, ids elided."""
    return _HEX_ID_RE.sub("<id>", redact_url(url))
