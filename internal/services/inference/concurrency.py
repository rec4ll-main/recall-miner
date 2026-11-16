"""Process-local generation admission at the actual model.generate boundary.

ThreadingHTTPServer fans out one thread per HTTP request. Without a bound,
concurrent /generate calls all enter model.generate and overload the process.
This module admits a finite number of active generations and optionally queues
a finite number of waiters; further callers are rejected immediately.

Semantics (documented for operators / #65):
- Limits are **process-local** to this inference-service backend only. They do
  not bound other miner processes or deployment-wide capacity.
- A slot is held from successful acquire until release after model.generate
  returns (success or exception). Disconnect/timeout on the HTTP client does
  not free the slot while generate is still running.
- Queued waiters may expire/cancel **before** inference starts; they never take
  an active slot in that case. Already-running work keeps its slot until end.
- There is **no fairness guarantee**: a new arrival can take a free inflight
  slot while older callers are still queued (no FIFO hand-off).
- ThreadingHTTPServer accept/connection setup and body read/write sit outside
  this limiter — it is not a complete bounded transport design.
- Restart clears in-memory counters (no durable queue).
- The Chutes generation sidecar does not read GEN_MAX_INFLIGHT / GEN_MAX_QUEUE:
  service.py installs its limiter from CHUTES_MAX_CONCURRENT (N active, 2*N
  queued) with install_generation_limiter(). from_env() stays the CPU
  inference service's limiter, unchanged.
"""

from __future__ import annotations

import os
import threading
import time
from dataclasses import dataclass
from typing import Callable


class GenerationOverloaded(Exception):
    """Raised when active+queued capacity is exhausted or queue wait expires."""


@dataclass(frozen=True)
class GenerationLimiterStats:
    active: int
    queued: int
    max_inflight: int
    max_queue: int
    admitted: int
    rejected: int
    completed: int
    expired: int


class GenerationLimiter:
    def __init__(self, max_inflight: int, max_queue: int, *, acquire_timeout_seconds: float = 25.0):
        if max_inflight < 1:
            raise ValueError("max_inflight must be >= 1")
        if max_queue < 0:
            raise ValueError("max_queue must be >= 0")
        self._max_inflight = max_inflight
        self._max_queue = max_queue
        self._acquire_timeout = acquire_timeout_seconds
        self._cond = threading.Condition()
        self._active = 0
        self._queued = 0
        self._admitted = 0
        self._rejected = 0
        self._completed = 0
        self._expired = 0

    @classmethod
    def from_env(cls) -> "GenerationLimiter":
        # Default acquire timeout is finite so queued work cannot silently
        # outlive the miner's generation deadline (GENERATION_BACKEND_TIMEOUT_SECONDS)
        # and then start model work after capacity frees.
        max_inflight = int(os.getenv("GEN_MAX_INFLIGHT", "1") or "1")
        max_queue = int(os.getenv("GEN_MAX_QUEUE", "2") or "2")
        timeout = float(os.getenv("GEN_ACQUIRE_TIMEOUT_SECONDS", "25") or "25")
        return cls(max_inflight=max(1, max_inflight), max_queue=max(0, max_queue), acquire_timeout_seconds=max(0.0, timeout))

    def stats(self) -> GenerationLimiterStats:
        with self._cond:
            return GenerationLimiterStats(
                active=self._active,
                queued=self._queued,
                max_inflight=self._max_inflight,
                max_queue=self._max_queue,
                admitted=self._admitted,
                rejected=self._rejected,
                completed=self._completed,
                expired=self._expired,
            )

    def acquire(self, should_abort: Callable[[], bool] | None = None) -> None:
        """Block until an inflight slot is available, or raise GenerationOverloaded.

        Waiters count against max_queue. When the queue is full, the acquire
        timeout expires, or should_abort() becomes true while waiting, the
        caller is rejected **without** entering model.generate and without
        holding an active slot.
        """
        deadline = None
        if self._acquire_timeout > 0:
            deadline = time.monotonic() + self._acquire_timeout

        with self._cond:
            if should_abort is not None and should_abort():
                self._expired += 1
                self._rejected += 1
                raise GenerationOverloaded("generation queue aborted before acquire")

            if self._active < self._max_inflight:
                self._active += 1
                self._admitted += 1
                return

            if self._queued >= self._max_queue:
                self._rejected += 1
                raise GenerationOverloaded(
                    f"generation overloaded: active={self._active}/{self._max_inflight} "
                    f"queued={self._queued}/{self._max_queue}"
                )

            self._queued += 1
            try:
                while self._active >= self._max_inflight:
                    if should_abort is not None and should_abort():
                        self._expired += 1
                        self._rejected += 1
                        raise GenerationOverloaded("generation queue aborted while waiting")
                    if deadline is None:
                        self._cond.wait(timeout=0.05)
                        continue
                    remaining = deadline - time.monotonic()
                    if remaining <= 0:
                        self._expired += 1
                        self._rejected += 1
                        raise GenerationOverloaded(
                            f"generation acquire timed out: active={self._active}/{self._max_inflight} "
                            f"queued={self._queued}/{self._max_queue}"
                        )
                    self._cond.wait(timeout=min(remaining, 0.05))
                # Capacity is free, but the waiter may have woken after its
                # deadline or after the client disconnected. Recheck BOTH
                # cancellation and expiry immediately before admission.
                if should_abort is not None and should_abort():
                    self._expired += 1
                    self._rejected += 1
                    raise GenerationOverloaded("generation queue aborted before inference")
                if deadline is not None and time.monotonic() >= deadline:
                    self._expired += 1
                    self._rejected += 1
                    raise GenerationOverloaded(
                        f"generation acquire timed out before admission: "
                        f"active={self._active}/{self._max_inflight} "
                        f"queued={self._queued}/{self._max_queue}"
                    )
                self._active += 1
                self._admitted += 1
            finally:
                self._queued -= 1

    def release(self) -> None:
        """Release an inflight slot. Must be called exactly once per successful acquire."""
        with self._cond:
            if self._active <= 0:
                raise RuntimeError("generation limiter release without matching acquire")
            self._active -= 1
            self._completed += 1
            self._cond.notify()


_LIMITER: GenerationLimiter | None = None
_LIMITER_LOCK = threading.Lock()


def get_generation_limiter() -> GenerationLimiter:
    global _LIMITER
    with _LIMITER_LOCK:
        if _LIMITER is None:
            _LIMITER = GenerationLimiter.from_env()
        return _LIMITER


def install_generation_limiter(limiter: GenerationLimiter | None) -> None:
    """Replace the process-wide limiter (None: rebuilt lazily from the environment)."""
    global _LIMITER
    with _LIMITER_LOCK:
        _LIMITER = limiter


def reset_generation_limiter_for_tests(limiter: GenerationLimiter | None = None) -> None:
    """Test helper: replace or clear the process-wide limiter."""
    install_generation_limiter(limiter)
