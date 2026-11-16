"""Synthetic enabled-Chutes admission for the in-tree tests (test-only; not in the image).

Installs a synthetic ChutesConfig (canonical base URL, explicit synthetic model,
synthetic credential), a disposable initialized ledger and the matching service
globals, then restores every one of them at cleanup. Production code is not
changed: the provider URL check still sees https://llm.chutes.ai/v1. Only the
test's HTTPSConnection class is replaced by a plain HTTP connection to a local
loopback backend, so cancellation and close are observed on a real socket.

No provider, credential file, live ledger or network beyond 127.0.0.1 is used.
"""

from __future__ import annotations

import http.client
import os
import tempfile
from unittest import mock

import service
from hosted_accounting import HostedLedger
from hosted_provider import ChutesConfig

SYNTHETIC_MODEL = "synthetic-org/synthetic-model"
SYNTHETIC_KEY = "synthetic-test-key"
CANONICAL_HOST = "llm.chutes.ai"
_GLOBALS = ("CHUTES_CONFIG", "GEN_OPENAI_URL", "GEN_OPENAI_MODEL", "GEN_OPENAI_TIMEOUT",
            "HOSTED_LEDGER", "HOSTED_LEDGER_ERROR", "_rerank_scorer", "_GENERATE_IMPL")


def completion(content, *, model=SYNTHETIC_MODEL, prompt_tokens=50, completion_tokens=12):
    """A complete current envelope: exact model, one choice, stop, non-negative integer usage."""
    return {"model": model,
            "choices": [{"finish_reason": "stop", "message": {"role": "assistant", "content": content}}],
            "usage": {"prompt_tokens": prompt_tokens, "completion_tokens": completion_tokens}}


class SyntheticChutes:
    """Install with ``SyntheticChutes(test, backend_port)``; cleanup is registered on ``test``."""

    def __init__(self, test, backend_port=None, *, timeout=10.0):
        self.requested = []      # (host, port) the transport asked HTTPSConnection for
        self.connections = []    # every connection object the transport created
        saved = {name: getattr(service, name) for name in _GLOBALS}
        in_flight = set(service._HOSTED_IN_FLIGHT)

        def restore():
            for name, value in saved.items():
                setattr(service, name, value)
            with service._HOSTED_IN_FLIGHT_LOCK:
                service._HOSTED_IN_FLIGHT.clear()
                service._HOSTED_IN_FLIGHT.update(in_flight)

        test.addCleanup(restore)
        tmp = tempfile.TemporaryDirectory()
        test.addCleanup(tmp.cleanup)
        self.ledger_path = os.path.join(tmp.name, "ledger.sqlite")
        ledger_id = HostedLedger.initialize(self.ledger_path)
        self.ledger = HostedLedger.open_existing(self.ledger_path, ledger_id)

        config = ChutesConfig(True, SYNTHETIC_MODEL, SYNTHETIC_KEY)
        service.CHUTES_CONFIG = config
        service.GEN_OPENAI_URL = config.base_url
        service.GEN_OPENAI_MODEL = config.model
        service.GEN_OPENAI_TIMEOUT = timeout
        service.HOSTED_LEDGER = self.ledger
        service.HOSTED_LEDGER_ERROR = ""

        if backend_port is not None:
            fixture = self

            class LoopbackConnection(http.client.HTTPConnection):
                def __init__(self, host, port=None, **kwargs):
                    fixture.requested.append((host, port))
                    super().__init__("127.0.0.1", backend_port, **kwargs)
                    fixture.connections.append(self)

            patcher = mock.patch.object(service.http.client, "HTTPSConnection", LoopbackConnection)
            patcher.start()
            test.addCleanup(patcher.stop)

    def pending(self):
        return self.ledger.pending()

    def admission_open(self):
        return service.hosted_available()
