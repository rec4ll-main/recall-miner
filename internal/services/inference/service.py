from __future__ import annotations

import hashlib
import http.client
import json
import math
import os
import re
import select
import socket
import ssl
import sys
import threading
import time
import urllib.parse
import urllib.request
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from generation_diagnostics import Trace, create_trace, observe
from concurrency import GenerationLimiter, GenerationOverloaded, get_generation_limiter, install_generation_limiter
from hosted_provider import ChutesConfig, HostedProviderError, completed_hosted_text, provider_terminal_usage
from hosted_accounting import (AGING_POLICY, MAX_CONCURRENT_LIMIT, HostedLedger, HostedAdmissionBlocked,
                               HostedRequestKeyConflict, HostedSlotsBusy)
from controlled_chutes import prepare_controlled, validate_typed, CONTRACT
from chunked_framing import (  # noqa: F401  re-exported for existing callers
    _CHUNK_LINE_LIMIT,
    _CHUNK_SIZE_RE,
    _chunk_line,
    _chunked_progress,
    _decode_chunked,
)
from grounding import (  # noqa: F401  re-exported for existing callers and patches
    GEN_MAX_CONTEXTS,
    GEN_MIN_SUPPORT,
    GEN_LINK_RATIO,
    ABSTAIN_ANSWER,
    SYSTEM_PROMPT,
    _ABSTAIN_PATTERNS,
    _MARKER_RE,
    _TOKEN_RE,
    _STOPWORDS,
    _stem,
    _NUMBER_WORDS,
    _PARAPHRASE_GROUPS,
    _raw_stem,
    _CANON,
    _canon,
    _content_tokens,
    select_generation_contexts,
    build_generation_messages,
    build_generation_prompt,
    _abstained,
    GEN_MIN_QUESTION_COVERAGE,
    GEN_CITATION_MIN_TOKENS,
    GEN_CITATION_MIN_OVERLAP,
    _FRAME_WORDS,
    _META_WORDS,
    _NUMBER_RE,
    _YEAR_RE,
    _CURRENCY_RE,
    _TEMPERATURE_RE,
    _TYPED_EVIDENCE,
    _CLAIM_SPLIT_RE,
    _PRICE_VALUE_RE,
    requested_price_covered,
    _WORD_RE,
    question_anchors,
    _SENTENCE_STARTERS,
    proper_noun_tokens,
    about_question_subject,
    question_coverage,
    _NEGATION_RE,
    _LEADING_YESNO_RE,
    _CLAUSE_SPLIT_RE,
    _negated,
    polarity_agrees,
    unsupported_numbers,
    _split_claims,
    _marker_indexes,
    citation_supports,
    ground_answer,
    GROUNDING_KEYS,
)

import importlib


def _load_on_first_use(module: str, name: str):
    """Constructor for `module.name`, imported only when first called.

    The ML stack (torch, sentence-transformers, transformers) adds about 400 MB of memory.
    The Chutes sidecar runs this file too but never embeds or reranks locally, so it must
    not pay for it. Tests replace these module attributes with fakes.
    """
    def construct(*args, **kwargs):
        return getattr(importlib.import_module(module), name)(*args, **kwargs)
    construct.__name__ = name
    return construct


SentenceTransformer = _load_on_first_use("sentence_transformers", "SentenceTransformer")
CrossEncoder = _load_on_first_use("sentence_transformers", "CrossEncoder")


EMBED_MODEL = os.getenv("EMBED_MODEL", "BAAI/bge-small-en-v1.5")
RERANK_MODEL = os.getenv("RERANK_MODEL", "cross-encoder/ms-marco-MiniLM-L-2-v2")
GEN_MODEL = os.getenv("GEN_MODEL", "Qwen/Qwen2.5-0.5B-Instruct")
DEVICE = os.getenv("MODEL_DEVICE", "cpu")


# Dispatch base URL, set only from the Chutes config below. A legacy GEN_OPENAI_URL in the
# environment is never read into it, so it can never be dispatched to.
GEN_OPENAI_URL = ""
_CHAT_COMPLETIONS_PATH = "/chat/completions"


def chat_completions_url(base: str) -> str:


    parts = urllib.parse.urlsplit(str(base or "").strip())
    path = re.sub(r"/{2,}", "/", parts.path).rstrip("/")
    if not path.endswith(_CHAT_COMPLETIONS_PATH):
        path += _CHAT_COMPLETIONS_PATH
    return urllib.parse.urlunsplit((parts.scheme, parts.netloc, path, parts.query, ""))
GEN_OPENAI_MODEL = os.getenv("GEN_OPENAI_MODEL", "") or GEN_MODEL
# Hosted provider budget. The default (58 s) is the miner's default generation deadline
# (GENERATION_BACKEND_TIMEOUT_SECONDS), so min(this, deadline_ms) uses the miner's whole budget.
# Both fit the Recall API's 60 s scoring window: a later answer is not scored.
GEN_OPENAI_TIMEOUT = float(os.getenv("GEN_OPENAI_TIMEOUT_SECONDS", "58"))
# Per-attempt provider budget (default 25 s). A first attempt with no response by then is
# abandoned (it stays pending) and one new attempt follows within the remaining request budget
# (about 56 s in all, so the retry gets about 31 s), so a stalled provider request does not
# fail the whole request and the retry can still answer inside the scoring window.
# The request deadline (deadline_ms, else GEN_OPENAI_TIMEOUT from admission) stays the hard cap.
GEN_ATTEMPT_TIMEOUT_DEFAULT = 25.0
_ATTEMPT_TIMEOUT_ENV = os.getenv("GEN_ATTEMPT_TIMEOUT_SECONDS", "").strip()
try:
    GEN_ATTEMPT_TIMEOUT = float(_ATTEMPT_TIMEOUT_ENV) if _ATTEMPT_TIMEOUT_ENV else GEN_ATTEMPT_TIMEOUT_DEFAULT
except ValueError:
    GEN_ATTEMPT_TIMEOUT = float("nan")  # reported as an unavailable deadline, never dispatched
# A legacy GEN_OPENAI_URL or a non-Chutes GENERATION_CONNECTOR refuses generation only: the
# process starts and keeps serving embedding/rerank, Chutes stays disabled (its credential is
# not read), /generate answers 503 generation_disabled and nothing is dispatched.
UNSUPPORTED_GENERATION_CONFIG = bool(os.getenv("GEN_OPENAI_URL")) or os.getenv("GENERATION_CONNECTOR", "chutes") != "chutes"
if UNSUPPORTED_GENERATION_CONFIG:
    print(json.dumps({"event": "generation_refused", "reason": "only the Chutes generation connector is supported",
                      "action": "unset GEN_OPENAI_URL and set GENERATION_CONNECTOR=chutes; embedding and rerank keep serving"}),
          file=sys.stderr, flush=True)
    CHUTES_CONFIG = ChutesConfig()
else:
    CHUTES_CONFIG = ChutesConfig.from_env(os.environ)
CHUTES_PROVIDER_OUTPUT_DEFAULT = True
CONTROLLED_PROFILE = None
HOSTED_LEDGER = None
HOSTED_LEDGER_ERROR = "hosted ledger not configured"
# CHUTES_LEDGER_ID is optional. Empty (or the old template value pending-init): the sidecar
# creates the ledger in /state on first start and opens the one it finds. A ledger id
# (UUID) pins the ledger: a different file is refused as an identity mismatch.
LEDGER_ID_PLACEHOLDERS = ("", "pending-init")
LEDGER_ID_INVALID = "CHUTES_LEDGER_ID must be a ledger id (UUID) or left empty"


def _is_ledger_id(value) -> bool:
    try:
        uuid.UUID(value)
    except (ValueError, TypeError, AttributeError):
        return False
    return True


# Chutes sidecar concurrency: one operator setting. CHUTES_MAX_CONCURRENT (1..16, default 4) is
# how many generation requests this sidecar sends to Chutes at once. It sets the generation
# limiter to N active requests with up to 2*N waiting for a slot, and the ledger to N active
# hosted attempts (one slot lock each). An empty value means unset.
CHUTES_MAX_CONCURRENT_DEFAULT = 4
CHUTES_QUEUE_PER_REQUEST = 2
CHUTES_MAX_CONCURRENT_INVALID = "CHUTES_MAX_CONCURRENT must be a whole number from 1 to %d" % MAX_CONCURRENT_LIMIT
CHUTES_ACQUIRE_TIMEOUT_INVALID = "GEN_ACQUIRE_TIMEOUT_SECONDS must be a number of seconds"
# The ledger's one brake mode (carried-unknown-aging-v1): an unknown outcome stops counting
# after five minutes (the ledger minimum; a provider request ends within the ~56 s budget). That policy accepts only the retained outstanding bound two; admission is
# then N active attempts plus at most hosted_accounting.MAX_CARRIED_UNKNOWN carried unknowns.
CHUTES_UNKNOWN_AGE_OUT_SECONDS = 300
CHUTES_LEDGER_BOUND = 2
# Settings CHUTES_MAX_CONCURRENT replaced: name -> (replacement, what happens now). Accepted for
# one release in the Chutes sidecar's environment: each non-empty one is logged once at startup
# and ignored. The CPU inference service still reads GEN_MAX_INFLIGHT / GEN_MAX_QUEUE.
_CONCURRENCY = ("CHUTES_MAX_CONCURRENT",
                "ignored; CHUTES_MAX_CONCURRENT sets how many requests go to Chutes at once")
_BRAKE = ("none", "ignored; an unknown Chutes outcome always stops counting after five minutes "
                  "(the ledger keeps the record)")
RETIRED_CHUTES_SETTINGS = {
    "GEN_MAX_INFLIGHT": _CONCURRENCY,
    "GEN_MAX_QUEUE": _CONCURRENCY,
    "HOSTED_MAX_OUTSTANDING": _CONCURRENCY,
    "HOSTED_MAX_CONCURRENT": _CONCURRENCY,
    "HOSTED_RECOVERY_POLICY": _BRAKE,
    "HOSTED_UNKNOWN_AGE_OUT_SECONDS": _BRAKE,
}
# Earlier policies with no age-out. Set explicitly they are still ignored, with this addition.
_RETIRED_POLICIES = ("legacy", "carried-unknown-v1")


def chutes_concurrency_settings(env) -> tuple:
    """(N, acquire_timeout_seconds, "") for the Chutes sidecar, or (None, None, plain reason)
    when CHUTES_MAX_CONCURRENT or GEN_ACQUIRE_TIMEOUT_SECONDS is invalid. GEN_ACQUIRE_TIMEOUT_SECONDS
    keeps its earlier meaning (queue wait cap, default 25 s)."""
    raw = (env.get("CHUTES_MAX_CONCURRENT") or "").strip()
    if not raw:
        concurrent = CHUTES_MAX_CONCURRENT_DEFAULT
    elif re.fullmatch(r"[0-9]{1,3}", raw) and 1 <= int(raw) <= MAX_CONCURRENT_LIMIT:
        concurrent = int(raw)
    else:
        return None, None, CHUTES_MAX_CONCURRENT_INVALID
    try:
        timeout = float(env.get("GEN_ACQUIRE_TIMEOUT_SECONDS", "25") or "25")
    except ValueError:
        return None, None, CHUTES_ACQUIRE_TIMEOUT_INVALID
    if not math.isfinite(timeout):
        return None, None, CHUTES_ACQUIRE_TIMEOUT_INVALID
    return concurrent, max(0.0, timeout), ""


def warn_retired_chutes_settings(env, stream=None) -> list:
    """Log one warning per retired setting set (non-empty) in env; returns their names.

    Values are never logged."""
    stream = sys.stderr if stream is None else stream
    found = [name for name in RETIRED_CHUTES_SETTINGS if (env.get(name) or "").strip()]
    for name in found:
        replacement, detail = RETIRED_CHUTES_SETTINGS[name]
        if name == "HOSTED_RECOVERY_POLICY" and env.get(name, "").strip() in _RETIRED_POLICIES:
            detail += "; the policies without an age-out are no longer available"
        event = {"event": "setting_ignored", "setting": name, "replacement": replacement,
                 "detail": detail, "accepted_until": "next release"}
        print(json.dumps(event), file=stream, flush=True)
    return found


if CHUTES_CONFIG.enabled:
    GEN_OPENAI_URL = CHUTES_CONFIG.base_url
    GEN_OPENAI_MODEL = CHUTES_CONFIG.model
    # One operator setting sizes the Chutes sidecar (see chutes_concurrency_settings). The
    # settings it replaced are ignored here, with a warning; the CPU inference service (Chutes
    # disabled) never reaches this block and keeps its GEN_MAX_* limiter.
    warn_retired_chutes_settings(os.environ)
    _chutes_concurrent, _chutes_acquire_timeout, _chutes_settings_error = chutes_concurrency_settings(os.environ)
    # N active, 2*N waiting. With an invalid setting generation is refused before the limiter,
    # so the default size installed then only shows on /health.
    _chutes_slots = _chutes_concurrent or CHUTES_MAX_CONCURRENT_DEFAULT
    install_generation_limiter(GenerationLimiter(
        _chutes_slots, CHUTES_QUEUE_PER_REQUEST * _chutes_slots,
        acquire_timeout_seconds=25.0 if _chutes_acquire_timeout is None else _chutes_acquire_timeout))
    if _chutes_settings_error:
        # Blocks generation readiness with this reason; embedding/rerank keep serving.
        HOSTED_LEDGER_ERROR = _chutes_settings_error
        print(json.dumps({"event": "generation_refused", "reason": _chutes_settings_error,
                          "action": "embedding and rerank keep serving"}), file=sys.stderr, flush=True)
    elif os.getenv("CHUTES_LEDGER_PATH"):
        # Open (or, on first start, create) the ledger; every strict check still applies.
        # Failure blocks generation only; embedding/rerank keep serving.
        # Admission after an unknown outcome is always the self-recovering brake: a pending
        # attempt older than CHUTES_UNKNOWN_AGE_OUT_SECONDS (five minutes) stops holding admission;
        # the row stays pending (the possible-spend record is kept). The ledger file records no
        # policy, so a ledger written under an earlier policy opens unchanged.
        try:
            ledger_id = os.getenv("CHUTES_LEDGER_ID", "").strip()
            ledger_opts = dict(max_outstanding=CHUTES_LEDGER_BOUND, recovery_policy=AGING_POLICY,
                               unknown_age_out_seconds=CHUTES_UNKNOWN_AGE_OUT_SECONDS,
                               max_concurrent=_chutes_concurrent)
            if ledger_id in LEDGER_ID_PLACEHOLDERS:
                HOSTED_LEDGER = HostedLedger.open_or_create(os.environ["CHUTES_LEDGER_PATH"], **ledger_opts)
                if HOSTED_LEDGER.created:
                    print(json.dumps({"event": "hosted_ledger_created", "path": os.environ["CHUTES_LEDGER_PATH"]}),
                          file=sys.stderr, flush=True)
            elif _is_ledger_id(ledger_id):
                HOSTED_LEDGER = HostedLedger.open_existing(os.environ["CHUTES_LEDGER_PATH"], ledger_id,
                                                           **ledger_opts)
            else:
                print(json.dumps({"event": "generation_refused", "reason": LEDGER_ID_INVALID,
                                  "action": "embedding and rerank keep serving"}), file=sys.stderr, flush=True)
                raise HostedAdmissionBlocked(LEDGER_ID_INVALID)
            HOSTED_LEDGER_ERROR = ""
        except HostedAdmissionBlocked as exc:
            HOSTED_LEDGER_ERROR = str(exc)
# Attempts whose request is still running in this process. Only these are
# excluded from "unresolved hold"; an attempt leaves this set when its request
# exits, recorded terminal or not, and a still-pending one then holds admission.
_HOSTED_IN_FLIGHT = set()
_HOSTED_IN_FLIGHT_LOCK = threading.Lock()


def hosted_available() -> bool:
    with _HOSTED_IN_FLIGHT_LOCK:
        own = frozenset(_HOSTED_IN_FLIGHT)
    return HOSTED_LEDGER.available(in_flight=own)


EMBED_BATCH_SIZE = int(os.getenv("EMBED_BATCH_SIZE", "64"))
RERANK_BATCH_SIZE = int(os.getenv("RERANK_BATCH_SIZE", "32"))


embedder: SentenceTransformer | None = None
reranker: CrossEncoder | None = None
generator: object | None = None  # local generation is unsupported; kept for callers
gen_tokenizer: object | None = None


_GENERATE_IMPL = None


def _rerank_scorer(query: str, texts: list) -> list:
    return [float(x) for x in get_reranker().predict([(query, t) for t in texts], batch_size=RERANK_BATCH_SIZE)]


_RERANK_UNAVAILABLE = False


def _select_contexts(query: str, contexts: list) -> list:
    """Contexts shown to the model. Reranks with the local cross-encoder when it is available;
    the hosted sidecar runs offline without it, so a load failure keeps the caller's order
    (the API sends contexts ranked by retrieval similarity) instead of failing the request."""
    global _RERANK_UNAVAILABLE
    if not _RERANK_UNAVAILABLE:
        try:
            return select_generation_contexts(query, contexts, scorer=_rerank_scorer)
        except Exception as exc:
            _RERANK_UNAVAILABLE = True
            # Logged once per process: from here on contexts keep the caller's order.
            print(json.dumps({"event": "reranker_unavailable", "error": type(exc).__name__,
                              "fallback": "caller_order"}), flush=True)
    return select_generation_contexts(query, contexts)


class GenerationCanceled(Exception):
    pass


def _deadline_remaining(deadline: float) -> float:
    return deadline - time.monotonic()


def _wait_readable(sock, timeout: float) -> bool:
    pending = getattr(sock, "pending", None)
    if pending is not None and pending() > 0:
        return True
    readable, _, _ = select.select([sock], [], [], max(0.0, timeout))
    return bool(readable)


_WOULD_BLOCK = (BlockingIOError, ssl.SSLWantReadError, ssl.SSLWantWriteError)


_CONTENT_LENGTH_RE = re.compile(r"[0-9]{1,10}")


class DeadlineExhaustedBeforeDispatch(TimeoutError):
    """Request budget spent before the ledger reservation: nothing reserved or sent."""


class HostedConnectFailed(HostedProviderError):
    """Connect (DNS, TCP, TLS) failed before the ledger reservation: nothing reserved or sent."""


def _raise_if_abort_or_timeout(should_abort, deadline: float) -> None:
    if should_abort is not None and should_abort():
        raise GenerationCanceled("client disconnected during inference; backend request closed")
    if _deadline_remaining(deadline) <= 0:
        raise TimeoutError(f"generation backend timed out after {GEN_OPENAI_TIMEOUT:g}s")


def _recv_until(sock, marker: bytes, should_abort, deadline: float, poll_seconds: float, limit: int = 65536) -> bytes:
    buf = bytearray()
    sock.setblocking(False)
    try:
        while marker not in buf:
            _raise_if_abort_or_timeout(should_abort, deadline)
            if not _wait_readable(sock, min(poll_seconds, _deadline_remaining(deadline))):
                continue
            try:
                chunk = sock.recv(4096)
            except _WOULD_BLOCK:
                continue
            if chunk == b"":
                raise GenerationCanceled("backend closed during header wait")
            buf.extend(chunk)
            if len(buf) > limit:
                raise RuntimeError("generation backend headers exceed limit")
    finally:
        try:
            sock.setblocking(True)
        except OSError:
            pass
    return bytes(buf)


def _recv_exact(sock, n: int, should_abort, deadline: float, poll_seconds: float) -> bytes:
    buf = bytearray()
    sock.setblocking(False)
    try:
        while len(buf) < n:
            _raise_if_abort_or_timeout(should_abort, deadline)
            if not _wait_readable(sock, min(poll_seconds, _deadline_remaining(deadline))):
                continue
            try:
                chunk = sock.recv(min(65536, n - len(buf)))
            except _WOULD_BLOCK:
                continue
            if chunk == b"":
                raise GenerationCanceled("backend closed during body read")
            buf.extend(chunk)
    finally:
        try:
            sock.setblocking(True)
        except OSError:
            pass
    return bytes(buf)


def generate_via_openai(messages: list, max_tokens: int, temperature, should_abort=None, poll_seconds: float = 0.1,
                        require_complete=False, response_schema=None, response_validator=None,
                        deadline_seconds=None, deadline_at=None, request_key=None, attempt_report=None,
                        exempt_attempt=None) -> tuple:
    # exempt_attempt: for a post-dispatch retry only, the request's own stalled first attempt,
    # passed to the ledger's begin() (which accepts it only for that request's retry key).
    # attempt_report (optional dict) receives what this call did, for the caller's retry rule:
    # 'attempt' once reserved, 'sent' once the marker committed, 'cause' (the original
    # exception), 'terminal_write' if a terminal write was in progress, 'released' once local
    # ownership ended normally.
    if not CHUTES_CONFIG.enabled:
        raise HostedProviderError("Chutes generation is disabled; no alternate connector")
    try:
        if HOSTED_LEDGER is None:
            raise HostedAdmissionBlocked("durable hosted accounting required")
        if not math.isfinite(GEN_OPENAI_TIMEOUT) or GEN_OPENAI_TIMEOUT <= 0:
            raise HostedProviderError("positive finite hosted deadline required")
        if response_schema is not None and not callable(response_validator):
            raise HostedProviderError("schema response validator required before dispatch")
        # deadline_seconds is the caller's remaining budget (Go miner deadline minus
        # margin); it can only shorten GEN_OPENAI_TIMEOUT, never extend it.
        if deadline_seconds is not None and (isinstance(deadline_seconds, bool) or not isinstance(deadline_seconds, (int, float))
                                             or not math.isfinite(deadline_seconds) or deadline_seconds <= 0):
            raise HostedProviderError("request deadline budget exhausted before dispatch")
        # deadline_at is the request's absolute time.monotonic() deadline, fixed at
        # receipt and carried through queueing and context preparation.
        if deadline_at is not None and (isinstance(deadline_at, bool) or not isinstance(deadline_at, (int, float))
                                        or not math.isfinite(deadline_at)):
            raise HostedProviderError("invalid request deadline")
        return _generate_via_openai(messages, max_tokens, temperature, should_abort, poll_seconds,
                                   require_complete, response_schema, response_validator, deadline_seconds,
                                   deadline_at, request_key, attempt_report, exempt_attempt)
    except DeadlineExhaustedBeforeDispatch:
        raise
    except GenerationCanceled:
        raise GenerationCanceled("hosted generation canceled; remote completion unknown") from None
    except TimeoutError:
        raise TimeoutError("hosted generation deadline exceeded; remote completion unknown") from None
    except HostedProviderError:
        raise
    except HostedAdmissionBlocked:
        raise
    except Exception:
        raise HostedProviderError("hosted transport or response failure; no automatic retry") from None


def _generate_via_openai(messages: list, max_tokens: int, temperature, should_abort=None, poll_seconds: float = 0.1,
                         require_complete=False, response_schema=None, response_validator=None,
                         deadline_seconds=None, deadline_at=None, request_key=None, attempt_report=None,
                         exempt_attempt=None) -> tuple:
    report = {} if attempt_report is None else attempt_report






       
    payload = {
        "model": GEN_OPENAI_MODEL,
        "messages": messages,
        "max_tokens": int(max_tokens) if max_tokens is not None else None,
        "temperature": float(temperature) if temperature is not None else 0.0,
    }
    request_headers = {"Content-Type": "application/json", "Connection": "close"}
    if CHUTES_CONFIG.enabled:
        payload, auth_headers = CHUTES_CONFIG.request(messages, max_tokens, temperature,
            response_schema=response_schema, require_complete=require_complete)
        request_headers.update(auth_headers)
    body = json.dumps(payload).encode("utf-8")
    url = urllib.parse.urlsplit(chat_completions_url(GEN_OPENAI_URL))
    conn_cls = http.client.HTTPSConnection if url.scheme == "https" else http.client.HTTPConnection
    budget = GEN_OPENAI_TIMEOUT if deadline_seconds is None else min(GEN_OPENAI_TIMEOUT, deadline_seconds)
    now = time.monotonic()
    deadline = now + budget if deadline_at is None else min(now + budget, deadline_at)
    budget = deadline - now
    if budget <= 0:
        raise DeadlineExhaustedBeforeDispatch("deadline budget spent before dispatch; not sent")
                                                                           
                                                                         
    conn = conn_cls(url.hostname, url.port, timeout=max(0.05, budget))
    attempt = None
    trace = create_trace(Trace, budget)
    terminal_write_pending = False
    try:
        # Connect (DNS, TCP, TLS) before the reservation: a failure here sent no
        # request, so it is reported unsent and leaves no hold.
        try:
            conn.connect()
        except Exception:
            raise HostedConnectFailed("hosted connection failed before dispatch; not sent") from None
        # Last check before the reservation; budget spent here is reported unsent.
        try:
            _raise_if_abort_or_timeout(should_abort, deadline)
        except TimeoutError:
            raise DeadlineExhaustedBeforeDispatch("deadline budget spent before dispatch; not sent") from None
        path = url.path + (("?" + url.query) if url.query else "")
        if CHUTES_CONFIG.enabled:
            observe(trace, 'update', stage='ledger_begin')
            # A caller-supplied request key is bound to the exact provider body: the same key
            # is never dispatched twice, and reuse with a different body is refused.
            if request_key is None:
                attempt = HOSTED_LEDGER.begin()
            elif exempt_attempt is None:
                attempt = HOSTED_LEDGER.begin(request_key, hashlib.sha256(body).hexdigest())
            else:
                attempt = HOSTED_LEDGER.begin(request_key, hashlib.sha256(body).hexdigest(),
                                              exempt_attempt=exempt_attempt)
            report['attempt'] = attempt
            observe(trace, 'attempt', attempt)
            with _HOSTED_IN_FLIGHT_LOCK:
                _HOSTED_IN_FLIGHT.add(attempt)
                                                                             
                                                                          
                                                                          
            _raise_if_abort_or_timeout(should_abort, deadline)
            # Committed before the first request byte; if this fails nothing is sent and the
            # reserved row stays pending (counted, never treated as resolved).
            HOSTED_LEDGER.mark_sent(attempt)
            report['sent'] = True
            # The marker commit can wait on locks and disk: cancellation or the deadline may
            # have arrived meanwhile. Re-check immediately before sending. A refusal here
            # sends nothing; the row stays pending and is reported possibly sent (no clear, no retry).
            _raise_if_abort_or_timeout(should_abort, deadline)
        observe(trace, 'update', stage='send', dispatch_started=True)
        conn.request("POST", path, body=body, headers=request_headers)
        observe(trace, 'update', stage='headers')
        sock = conn.sock
        if sock is None:
            raise RuntimeError("generation backend connection has no socket")
                                                                                  
                                                                               
        while True:
            _raise_if_abort_or_timeout(should_abort, deadline)
            if _wait_readable(sock, min(poll_seconds, _deadline_remaining(deadline))):
                break
        header_blob = _recv_until(sock, b"\r\n\r\n", should_abort, deadline, poll_seconds)
        header_text, _, _ = header_blob.partition(b"\r\n\r\n")
                                                                               
        leftover = header_blob[len(header_text) + 4:]
        status_line, _, header_rest = header_text.partition(b"\r\n")
        parts = status_line.split(None, 2)
        if len(parts) < 2 or not parts[1].isdigit():
            raise RuntimeError(f"generation backend bad status line: {status_line!r}")
        status = int(parts[1])
        observe(trace, 'update', headers_complete=True, stage='framing')
        observe(trace, 'update', http_status=status if 100 <= status <= 599 else 0)
        headers = http.client.HTTPMessage()
        for line in header_rest.split(b"\r\n"):
            if b":" in line:
                k, _, v = line.partition(b":")
                headers[k.decode("latin-1")] = v.strip().decode("latin-1")
        # Only explicit, unambiguous framing establishes a complete response; a
        # close-delimited, conflicting or malformed one stays uncertain (held).
        encodings = headers.get_all("Transfer-Encoding") or []
        lengths = headers.get_all("Content-Length") or []
        if encodings and lengths:
            raise RuntimeError("generation backend conflicting body framing")
        if encodings and (len(encodings) != 1 or encodings[0].strip().lower() != "chunked"):
            raise RuntimeError("generation backend unsupported transfer encoding")
        if not encodings and not lengths:
            raise RuntimeError("generation backend response without complete framing")
        if lengths and (len({v.strip() for v in lengths}) != 1 or not _CONTENT_LENGTH_RE.fullmatch(lengths[0].strip())):
            raise RuntimeError("generation backend malformed Content-Length")
        length_hdr = lengths[0].strip() if lengths else None
        observe(trace, 'update', stage='body')
        if length_hdr is None:
                                                                               
            # Finish as soon as the framing is complete; EOF is not required, and
            # EOF before completion stays uncertain.
            buf = bytearray(leftover)
            raw = _chunked_progress(buf)
            if raw is None:
                sock.setblocking(False)
                try:
                    while raw is None:
                        _raise_if_abort_or_timeout(should_abort, deadline)
                        if not _wait_readable(sock, min(poll_seconds, _deadline_remaining(deadline))):
                            continue
                        try:
                            chunk = sock.recv(65536)
                        except _WOULD_BLOCK:
                            continue
                        if chunk == b"":
                            raise RuntimeError("generation backend truncated chunked body")
                        buf.extend(chunk)
                        if len(buf) > 16 * 1024 * 1024:
                            raise RuntimeError("generation backend body exceeds 16 MiB")
                        raw = _chunked_progress(buf)
                finally:
                    try:
                        sock.setblocking(True)
                    except OSError:
                        pass
        else:
            need = int(length_hdr)
            if need < 0 or need > 16 * 1024 * 1024:
                raise RuntimeError(f"generation backend Content-Length out of range: {need}")
            if len(leftover) > need:
                raise RuntimeError("generation backend sent data beyond Content-Length")
            if len(leftover) == need:
                raw = leftover
            else:
                raw = leftover + _recv_exact(sock, need - len(leftover), should_abort, deadline, poll_seconds)
        observe(trace, 'update', body_complete=True, stage='http_status')
        if status != 200:
            if CHUTES_CONFIG.enabled:
                if 400 <= status < 600 and status != 408:
                    # Complete 4xx or 5xx response: the provider refused the request or failed
                    # on its side (owner decision 2026-11-06: a 5xx is a definite server
                    # failure). Terminal, usage unreported (unknown), recorded as
                    # provider_rejected with its HTTP status, so a provider outage costs only
                    # the failed calls and never holds admission. A 408, a timeout, a dropped
                    # connection or an incomplete response stays pending (may still be running).
                    observe(trace, 'update', stage='ledger_complete', outcome='provider_rejected')
                    terminal_write_pending = True
                    HOSTED_LEDGER.complete(attempt, "provider_rejected", http_status=status)
                    terminal_write_pending = False
                    observe(trace, 'update', ledger='complete')
                raise HostedProviderError("hosted rate limited" if status == 429 else "hosted HTTP failure")
            raise RuntimeError(f"generation backend HTTP {status}")
        observe(trace, 'update', stage='json_parse')
        out = json.loads(raw.decode("utf-8"))
        observe(trace, 'envelope', out)
        observe(trace, 'update', stage='final_validation')
        if CHUTES_CONFIG.enabled:
            reported = provider_terminal_usage(out)
            try:
                result = completed_hosted_text(out, CHUTES_CONFIG.model)
                if response_schema is not None:
                    try:
                        if response_validator(json.loads(result[0])) is False:
                            raise ValueError("schema validator rejected response")
                    except Exception:
                        raise HostedProviderError("hosted response failed schema validation") from None
            except HostedProviderError:
                # Finished envelope that fails local checks: terminal, never delivered.
                if reported is not None:
                    observe(trace, 'update', stage='ledger_complete', outcome='completed_unaccepted')
                    terminal_write_pending = True
                    HOSTED_LEDGER.complete(attempt, "provider_completed_unaccepted", http_status=200, usage=reported)
                    terminal_write_pending = False
                    observe(trace, 'update', ledger='complete')
                raise
            observe(trace, 'update', stage='ledger_complete')
            terminal_write_pending = True
            HOSTED_LEDGER.complete(attempt, http_status=200, usage=reported)
            terminal_write_pending = False
            observe(trace, 'update', ledger='complete', outcome='accepted', stage='return')
            return result
    except Exception as exc:
        # This state is independent of diagnostic availability. A completion
        # call that did not return may have committed: never label it a provider
        # timeout/cancellation or alter/retry the durable operation.
        report['cause'] = exc
        report['terminal_write'] = terminal_write_pending
        if terminal_write_pending:
            observe(trace, 'update', outcome='terminal_write_uncertain')
        elif isinstance(exc, TimeoutError):
            observe(trace, 'update', outcome='timeout')
        elif isinstance(exc, GenerationCanceled):
            observe(trace, 'update', outcome='canceled')
        raise
    finally:
        observe(trace, 'emit')
        try:
            conn.close()
        finally:
            if attempt is not None:
                try:
                    HOSTED_LEDGER.finish_local(attempt)
                    report['released'] = True
                finally:
                    with _HOSTED_IN_FLIGHT_LOCK:
                        _HOSTED_IN_FLIGHT.discard(attempt)
    text = str(((out.get("choices") or [{}])[0].get("message") or {}).get("content") or "")
    usage = out.get("usage") or {}
    return text.strip(), int(usage.get("prompt_tokens") or 0), int(usage.get("completion_tokens") or 0)


# Bounded retries for one incoming generation request. Pre-dispatch failures (connect/TLS
# before the reservation, every hosted slot busy, limiter overloaded) spent nothing and are
# retried after these pauses while the request deadline leaves RETRY_PRE_DISPATCH_MIN_SECONDS.
# A post-dispatch timeout or connection drop leaves its attempt pending exactly as before (never
# cleared or resent); at most one new attempt follows, under a new request key, and only while
# RETRY_POST_DISPATCH_MIN_SECONDS remain. A used request key is never retried.
RETRY_BACKOFF_SECONDS = (0.25, 0.5, 1.0)
RETRY_PRE_DISPATCH_MIN_SECONDS = 5.0
RETRY_POST_DISPATCH_MIN_SECONDS = 15.0
RETRY_KEY_SUFFIX = ":r1"
_REQUEST_KEY_RE = re.compile(r"[A-Za-z0-9._:-]{1,128}")


def _attempt_timeout_valid() -> bool:
    return GEN_ATTEMPT_TIMEOUT is None or (math.isfinite(GEN_ATTEMPT_TIMEOUT) and GEN_ATTEMPT_TIMEOUT > 0)


class _RetryBudget:
    """Retry allowance and hard deadline of one incoming request."""

    def __init__(self, deadline_at, should_abort=None):
        # Without a caller deadline the hard cap is one configured budget from admission.
        self.deadline = deadline_at if deadline_at is not None else time.monotonic() + GEN_OPENAI_TIMEOUT
        self.should_abort = should_abort
        self.pre_dispatch = 0
        self.post_dispatch = 0
        self.retries = 0

    def _log(self, kind, exc):
        self.retries += 1
        # No keys, bodies or messages: the exception class only.
        print(json.dumps({"event": "generation_retry", "kind": kind, "attempt": self.retries,
                          "reason": type(exc).__name__}), flush=True)

    def _aborted(self) -> bool:
        try:
            return self.should_abort is not None and bool(self.should_abort())
        except Exception:
            return True

    def pre(self, exc) -> bool:
        """Pause and return True when a pre-dispatch failure may be retried."""
        if self.pre_dispatch >= len(RETRY_BACKOFF_SECONDS) or self._aborted():
            return False
        pause = RETRY_BACKOFF_SECONDS[self.pre_dispatch]
        if not _deadline_remaining(self.deadline) - pause >= RETRY_PRE_DISPATCH_MIN_SECONDS:
            return False
        self.pre_dispatch += 1
        self._log("pre_dispatch", exc)
        time.sleep(pause)
        return True

    def post(self, exc) -> bool:
        """True when one new attempt may follow a post-dispatch failure."""
        if self.post_dispatch >= 1 or self._aborted():
            return False
        if not _deadline_remaining(self.deadline) >= RETRY_POST_DISPATCH_MIN_SECONDS:
            return False
        self.post_dispatch += 1
        self._log("post_dispatch", exc)
        return True


def _pre_dispatch_failure(exc, report) -> bool:
    # Nothing was reserved: a connect failure, or every slot busy (a refusal that reserved nothing).
    return "attempt" not in report and isinstance(exc, (HostedConnectFailed, HostedSlotsBusy))


def _post_dispatch_failure(exc, report) -> bool:
    # Sent, then the provider timed out or the connection dropped (OSError covers resets and TLS
    # failures; GenerationCanceled here is the backend closing, a client abort is excluded by the
    # budget). A terminal write in progress, an HTTP status, a malformed or rejected response,
    # or a failed local release is never retried.
    cause = report.get("cause")
    return (bool(report.get("sent")) and report.get("released") is True and not report.get("terminal_write")
            and isinstance(exc, (TimeoutError, GenerationCanceled, HostedProviderError))
            and not isinstance(exc, DeadlineExhaustedBeforeDispatch)
            and isinstance(cause, (OSError, GenerationCanceled)))


def generate_with_retries(messages: list, max_tokens: int, temperature, *, budget: _RetryBudget,
                          deadline_at=None, request_key=None, **kwargs) -> tuple:
    """generate_via_openai under the request's retry budget. The first attempt runs exactly as a
    direct call (same deadline arguments); later attempts are capped by the hard deadline."""
    key = request_key
    first = True
    dispatched_failure = None
    # The stalled first attempt, once a post-dispatch retry follows: it stays pending, but the
    # retry's own admission does not count it (one row, that retry key only; see the ledger).
    exempt = {}
    while True:
        report = {}
        try:
            return generate_via_openai(messages, max_tokens, temperature,
                                       deadline_seconds=GEN_ATTEMPT_TIMEOUT,
                                       deadline_at=deadline_at if first else budget.deadline,
                                       request_key=key, attempt_report=report, **exempt, **kwargs)
        except Exception as exc:
            first = False
            if _pre_dispatch_failure(exc, report) and budget.pre(exc):
                continue
            if _post_dispatch_failure(exc, report) and dispatched_failure is None:
                # Only under a recovery policy, which can admit a new attempt while this one
                # stays pending, and only with a key that can be extended without truncation.
                retry_key = None if key is None else key + RETRY_KEY_SUFFIX
                if (retry_key is not None and _REQUEST_KEY_RE.fullmatch(retry_key)
                        and getattr(HOSTED_LEDGER, "recovery_policy", "legacy") != "legacy"
                        and budget.post(exc)):
                    dispatched_failure, key = exc, retry_key
                    exempt = {"exempt_attempt": report["attempt"]}
                    continue
            if dispatched_failure is not None and "attempt" not in report:
                # The retry was refused before any reservation (slots, holds, key conflict,
                # connect, deadline): the request's outcome is the dispatched attempt's failure.
                raise dispatched_failure from None
            raise


def set_generate_impl_for_tests(impl):
    global _GENERATE_IMPL
    _GENERATE_IMPL = impl


def peer_disconnected(handler: BaseHTTPRequestHandler) -> bool:


    sock = getattr(handler, "connection", None)
    if sock is None:
        sock = getattr(handler, "request", None)
    if sock is None:
        return False
    try:
        readable, _, _ = select.select([sock], [], [], 0)
        if not readable:
            return False

        flags = socket.MSG_PEEK
        if hasattr(socket, "MSG_DONTWAIT"):
            flags |= socket.MSG_DONTWAIT
        was_blocking = sock.getblocking()
        try:
            if not hasattr(socket, "MSG_DONTWAIT"):
                sock.setblocking(False)
            data = sock.recv(1, flags)
        finally:
            if not hasattr(socket, "MSG_DONTWAIT"):
                sock.setblocking(was_blocking)
        return data == b""
    except (BlockingIOError, InterruptedError):
        return False
    except OSError:

        return True


def client_should_abort(handler: BaseHTTPRequestHandler) -> bool:


    if bool(getattr(handler, "_generation_abort", False)):
        return True
    return peer_disconnected(handler)


def get_embedder() -> SentenceTransformer:
    global embedder
    if embedder is None:
        embedder = SentenceTransformer(
            EMBED_MODEL,
            device=DEVICE,
            trust_remote_code=False,
        )
    return embedder


def get_reranker() -> CrossEncoder:
    global reranker
    if reranker is None:
        reranker = CrossEncoder(
            RERANK_MODEL,
            device=DEVICE,
            trust_remote_code=False,
        )
    return reranker


def get_generator():
    raise HostedProviderError("local generation is unsupported; model files remain preserved")


def generation_readiness() -> tuple:
    """(ready, fixed reason) for the generate registration's UPSTREAM_HEALTH_PATH.

    Not-ready while a durable unresolved hold exists, so the miner stops
    advertising a capability it would refuse; the hold itself is never cleared here.
    """
    if not CHUTES_CONFIG.enabled:
        return False, "generation disabled"
    if HOSTED_LEDGER is None:
        return False, HOSTED_LEDGER_ERROR or "hosted ledger not configured"
    if not math.isfinite(GEN_OPENAI_TIMEOUT) or GEN_OPENAI_TIMEOUT <= 0 or not _attempt_timeout_valid():
        return False, "hosted deadline unavailable"
    try:
        CHUTES_CONFIG.request([{"role": "user", "content": ""}], 1, 0)
    except Exception:
        return False, "hosted configuration invalid"
    try:
        if not hosted_available():
            with _HOSTED_IN_FLIGHT_LOCK:
                busy = bool(_HOSTED_IN_FLIGHT)
            return False, "hosted request in progress" if busy else "unresolved hosted attempt held"
    except Exception:
        return False, "hosted ledger unreadable"
    return True, ""


def loaded_reranker_identity(instance) -> dict:
    """Report loaded model config only; never infer identity from cache or registry.

    This is loader metadata, not cryptographic weight attestation. Local paths and
    missing identity stay unknown. Revision is only the loader-resolved commit.
    """
    config = getattr(getattr(instance, "model", None), "config", None)
    name = getattr(config, "_name_or_path", None)
    if not isinstance(name, str) or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*", name):
        return {}
    result = {"model": name}
    revision = getattr(config, "_commit_hash", None)
    if isinstance(revision, str) and re.fullmatch(r"[0-9a-f]{40}", revision):
        result["model_revision"] = revision
    return result


class Handler(BaseHTTPRequestHandler):
    def do_GET(self) -> None:
        if self.path == "/health":
            stats = get_generation_limiter().stats()
            self.write_json(200, {
                "status": "ok",
                "embedding_model": EMBED_MODEL,
                "rerank_model": RERANK_MODEL,
                "gen_model": GEN_OPENAI_MODEL if GEN_OPENAI_URL else GEN_MODEL,
                "gen_backend": "chutes" if CHUTES_CONFIG.enabled else "disabled",
                "device": DEVICE,
                "generation_limits": {
                    "max_inflight": stats.max_inflight,
                    "max_queue": stats.max_queue,
                    "active": stats.active,
                    "queued": stats.queued,
                    "process_local": True,
                },
            })
            return
        if self.path == "/health/generation":
            ready, reason = generation_readiness()
            self.write_json(200 if ready else 503, {
                "status": "ok" if ready else "unavailable",
                "reason": reason,
                "connector": "chutes",
                "model": CHUTES_CONFIG.model if CHUTES_CONFIG.enabled else None,
                "hosted_deadline_seconds": GEN_OPENAI_TIMEOUT,
            })
            return
        self.write_json(404, {"detail": "not found"})

    def do_POST(self) -> None:
        try:
            if self.path == "/embed":
                self.handle_embed()
                return
            if self.path == "/rerank":
                self.handle_rerank()
                return
            if self.path == "/generate":
                self.handle_generate()
                return
            self.write_json(404, {"detail": "not found"})
        except Exception as exc:
            self.write_json(500, {"detail": str(exc)})

    def handle_embed(self) -> None:
        payload = self.read_json()
        inputs = payload.get("inputs", "")
        single = isinstance(inputs, str)
        texts = [inputs] if single else list(inputs or [])
        vectors = get_embedder().encode(
            texts,
            batch_size=EMBED_BATCH_SIZE,
            normalize_embeddings=True,
            convert_to_numpy=True,
            show_progress_bar=False,
        ).tolist()
        self.write_json(200, vectors[0] if single else vectors)

    def handle_rerank(self) -> None:
        payload = self.read_json()
        query = str(payload.get("query", ""))
        texts = list(payload.get("texts") or [])
        executing_model = get_reranker()
        identity = loaded_reranker_identity(executing_model)
        scores = executing_model.predict([(query, text) for text in texts], batch_size=RERANK_BATCH_SIZE)
        ranked = sorted(enumerate(float(score) for score in scores), key=lambda item: item[1], reverse=True)
        self.write_json(200, [{"index": index, "score": score, **identity} for index, score in ranked])

    def handle_generate(self) -> None:


        try:
            payload = self.read_json()
        except Exception as exc:
            self.write_json(400, {"error": "invalid_json", "detail": str(exc)})
            return

        if not CHUTES_CONFIG.enabled:
            self.write_json(503, {"error": "generation_disabled"})
            return
        if payload.get("model") not in (None,"",CHUTES_CONFIG.model):
            self.write_json(400, {"error": "unsupported_generation_model"})
            return
        # Optional caller idempotency key from the trusted internal hop (the authenticated
        # miner). Its end-to-end meaning depends on the API contract; absent means no dedup.
        request_key = payload.get("request_key")
        if request_key is not None and (not isinstance(request_key, str)
                                        or not re.fullmatch(r"[A-Za-z0-9._:-]{1,128}", request_key)):
            self.write_json(400, {"error": "invalid_request_key"})
            return
        # Optional caller budget (the Go miner sends its remaining deadline minus a
        # margin) so this sidecar always reaches its own terminal before Go cancels.
        request_deadline = None
        if "deadline_ms" in payload:
            deadline_ms = payload.get("deadline_ms")
            if isinstance(deadline_ms, bool) or not isinstance(deadline_ms, int) or deadline_ms <= 0:
                self.write_json(400, {"error": "invalid_deadline"})
                return
            request_deadline = time.monotonic() + deadline_ms / 1000.0
        controlled = None
        if "probe_contract" in payload:
            try:
                if CHUTES_CONFIG.schema_mode != "json_schema":
                    raise ValueError("structured capability unavailable")
                controlled=prepare_controlled(payload,CHUTES_CONFIG.model,CONTROLLED_PROFILE)
            except ValueError:
                self.write_json(409, {"error": "controlled_profile_not_admitted"})
                return
        limiter = get_generation_limiter()
        budget = _RetryBudget(request_deadline, lambda: client_should_abort(self))
        if CHUTES_CONFIG.enabled:
            try:

                if HOSTED_LEDGER is None:
                    raise HostedAdmissionBlocked("hosted admission unavailable")
                # Every slot busy reserved nothing and may clear soon (bounded pre-dispatch
                # retry); any other hold refuses at once.
                while not hosted_available():
                    if not (HOSTED_LEDGER.busy() and budget.pre(HostedSlotsBusy("hosted active owner present"))):
                        raise HostedAdmissionBlocked("hosted admission unavailable")
                CHUTES_CONFIG.request([{"role": "user", "content": ""}], 1, 0)
                if not math.isfinite(GEN_OPENAI_TIMEOUT) or GEN_OPENAI_TIMEOUT <= 0 or not _attempt_timeout_valid():
                    raise HostedAdmissionBlocked("hosted deadline unavailable")
            except Exception:
                self.write_json(409, {"error": "hosted_admission_blocked"})
                return
        while True:
            try:
                limiter.acquire(should_abort=lambda: client_should_abort(self))
                break
            except GenerationOverloaded as exc:
                if budget.pre(exc):
                    continue
                self.write_json(503, {"error": "generation_overloaded", "detail": str(exc)})
                return

        held = True
        try:

            if client_should_abort(self):
                self.write_json(503, {"error": "generation_aborted", "detail": "client disconnected before inference"})
                return
            if request_deadline is not None:
                if request_deadline - time.monotonic() <= 0:
                    # Budget spent queueing: nothing was reserved or sent, so no hold.
                    self.write_json(504, {"error": "generation_deadline_exhausted",
                                          "detail": "deadline budget spent before dispatch; not sent"})
                    return

            query = str(payload.get("query", ""))
            contexts = list(payload.get("contexts") or [])
            max_tokens = int(payload.get("max_tokens", 0) or 0) or 256
            max_tokens = min(max_tokens, 1024)
            if CHUTES_CONFIG.enabled and CHUTES_PROVIDER_OUTPUT_DEFAULT:
                max_tokens = None
            temperature = payload.get("temperature")

            if controlled is not None:
                messages,schema=controlled
                answer,inputs,outputs=generate_with_retries(messages,None,temperature,budget=budget,
                    should_abort=lambda:client_should_abort(self),response_schema=schema,
                    response_validator=validate_typed,deadline_at=request_deadline,request_key=request_key)
                self.write_json(200,{"probe_contract":CONTRACT,"typed_response":answer,
                    "model":CHUTES_CONFIG.model,"usage":{"prompt_tokens":inputs,
                    "completion_tokens":outputs,"total_tokens":inputs+outputs}})
                return

            if _GENERATE_IMPL is not None:
                shown = select_generation_contexts(query, contexts)
                prompt = build_generation_prompt(query, shown)
                result = _GENERATE_IMPL(prompt, max_tokens, temperature, contexts)
                self.write_json(200, result)
                return

            shown = _select_contexts(query, contexts)
            if GEN_OPENAI_URL:
                try:
                    answer, input_token_count, output_token_count = generate_with_retries(
                        build_generation_messages(query, shown), max_tokens, temperature, budget=budget,
                        should_abort=lambda: client_should_abort(self), deadline_at=request_deadline,
                        request_key=request_key)
                except GenerationCanceled as exc:
                    self.write_json(503, {"error": "generation_aborted", "detail": str(exc)})
                    return
                except DeadlineExhaustedBeforeDispatch:
                    raise
                except TimeoutError as exc:
                    self.write_json(504, {"error": "generation_backend_timeout", "detail": str(exc)})
                    return
                except HostedRequestKeyConflict as exc:
                    self.write_json(409, {"error": "duplicate_request_key", "detail": str(exc)})
                    return
                except HostedAdmissionBlocked:
                    self.write_json(409, {"error": "hosted_admission_blocked"})
                    return
                grounded = ground_answer(answer, query, shown)
                self.write_json(200, {
                    "answer": grounded["answer"],
                    "citations": grounded["citations"],
                    "abstained": grounded["abstained"],
                    "grounding": {k: grounded[k] for k in GROUNDING_KEYS if k in grounded},
                    "model": GEN_OPENAI_MODEL,
                    "usage": {
                        "prompt_tokens": input_token_count,
                        "completion_tokens": output_token_count,
                        "total_tokens": input_token_count + output_token_count,
                    },
                })
                return

            gen, tok = get_generator()
            prompt = build_generation_prompt(query, shown, tok)
            gen_kwargs: dict = {"max_new_tokens": max_tokens, "pad_token_id": tok.eos_token_id}
            if temperature is not None:
                temp = float(temperature)
                if temp > 0:
                    gen_kwargs["do_sample"] = True
                    gen_kwargs["temperature"] = temp
            inputs = tok(prompt, return_tensors="pt").to(gen.device)
            input_token_count = inputs.input_ids.shape[1]
            outputs = gen.generate(**inputs, **gen_kwargs)
            generated_ids = outputs[0][input_token_count:]
            answer = tok.decode(generated_ids, skip_special_tokens=True).strip()
            output_token_count = generated_ids.shape[0]

            grounded = ground_answer(answer, query, shown)
            self.write_json(200, {
                "answer": grounded["answer"],
                "citations": grounded["citations"],
                "abstained": grounded["abstained"],
                "grounding": {k: grounded[k] for k in GROUNDING_KEYS if k in grounded},
                "model": GEN_MODEL,
                "usage": {
                    "prompt_tokens": input_token_count,
                    "completion_tokens": output_token_count,
                    "total_tokens": input_token_count + output_token_count,
                },
            })
        except GenerationCanceled as exc:
            self.write_json(503, {"error": "generation_aborted", "detail": str(exc)})
        except DeadlineExhaustedBeforeDispatch:
            # Spent in queueing or context preparation: nothing was reserved or sent.
            self.write_json(504, {"error": "generation_deadline_exhausted",
                                  "detail": "deadline budget spent before dispatch; not sent"})
        except TimeoutError as exc:
            self.write_json(504, {"error": "generation_backend_timeout", "detail": str(exc)})
        except HostedRequestKeyConflict as exc:
            self.write_json(409, {"error": "duplicate_request_key", "detail": str(exc)})
        except HostedAdmissionBlocked:
            self.write_json(409, {"error": "hosted_admission_blocked"})
        except Exception as exc:
            self.write_json(500, {"error": "generation_failed", "detail": str(exc)})
        finally:
            if held:
                limiter.release()

    def read_json(self) -> dict:
        length = int(self.headers.get("Content-Length", "0") or "0")
        if length == 0:
            return {}
        return json.loads(self.rfile.read(length).decode("utf-8"))

    def write_json(self, status: int, payload: object) -> None:
        body = json.dumps(payload, separators=(",", ":")).encode("utf-8")
        try:
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        except (BrokenPipeError, ConnectionResetError):

            self.close_connection = True

    def log_message(self, format: str, *args: object) -> None:
        return


def main() -> None:
    addr = os.getenv("LISTEN_ADDR", ":9200")
    host, raw_port = addr.rsplit(":", 1)
    host = host or "0.0.0.0"
    port = int(raw_port)
    ThreadingHTTPServer((host, port), Handler).serve_forever()


if __name__ == "__main__":
    main()

