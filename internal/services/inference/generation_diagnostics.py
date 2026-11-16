"""Nonsecret, best-effort transport observations; never ledger authority.

Only locally generated correlation, hashed ledger ID, closed categories and numeric
metadata are emitted. No provider text, headers, URLs, model inputs or exception text.
Remaining budget is an observation estimate, never a new timeout.
"""
import hashlib
import json
import logging
import time
import uuid

_LOG = logging.getLogger('generation.transport')

class Trace:
    def __init__(self, budget):
        self.started = time.perf_counter()
        self.budget = budget
        self.data = dict(event='generation_transport_v1', correlation=uuid.uuid4().hex,
                        stage='connect', outcome='uncertain', ledger='not_reserved',
                        dispatch_started=False, headers_complete=False, body_complete=False)

    def update(self, **fields):
        self.data.update(fields)

    def attempt(self, attempt):
        self.data['attempt_sha256'] = hashlib.sha256(str(attempt).encode()).hexdigest()
        self.data['ledger'] = 'pending'

    def envelope(self, out):
        if not isinstance(out, dict):
            self.data['envelope'] = 'not_object'
            return
        self.data['envelope'] = 'object'
        self.data['usage_present'] = isinstance(out.get('usage'), dict)
        choices = out.get('choices')
        choice = choices[0] if isinstance(choices, list) and len(choices) == 1 and isinstance(choices[0], dict) else {}
        reason = choice.get('finish_reason')
        self.data['finish_reason'] = reason if reason in ('stop', 'length', 'content_filter', 'tool_calls') else 'missing_or_other'
        message = choice.get('message')
        self.data['refusal_present'] = bool(message.get('refusal')) if isinstance(message, dict) else False

    def emit(self):
        try:
            elapsed = max(0, time.perf_counter() - self.started)
            self.data['elapsed_ms'] = int(elapsed * 1000)
            self.data['remaining_budget_estimate_ms'] = max(0, int((self.budget - elapsed) * 1000))
            _LOG.warning('%s', json.dumps(self.data, sort_keys=True))
        except Exception:
            pass  # Telemetry must not affect transport or durable accounting.


def create_trace(factory, budget):
    try:
        return factory(budget)
    except Exception:
        return None


def observe(trace, method, *args, **kwargs):
    # Only diagnostic operations belong inside this boundary. Transport and
    # ledger calls must execute outside it and propagate their original errors.
    try:
        if trace is not None:
            getattr(trace, method)(*args, **kwargs)
    except Exception:
        pass
