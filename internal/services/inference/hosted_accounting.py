"""Conservative local hosted dispatch fence; not API probe/reward accounting.

All writers sharing a provider allocation must use this same durable local DB.
No automatic replay is provided. An unknown attempt stays pending until the
aging policy stops counting it (the row itself stays pending) or an operator
reconciles it with reconcile_unknown() / `hosted_accounting.py reconcile`,
which records it as terminal with an unknown outcome. Nothing is ever deleted.

The service opens the ledger with open_existing(): the file must already have
been created once by an operator with initialize() and must carry the expected
ledger identity. A missing, unmounted, replaced or corrupt ledger therefore
blocks dispatch instead of silently starting empty and forgetting held work.

Within one running process the opened file is pinned by (st_dev, st_ino) and
by what this process has seen (attempt rows never shrink; attempts it began
stay present). A replaced path, including an older copy with the same ledger
identity, or an in-place rollback is refused for good, before and after each
connection is opened. That is not rollback proof across a restart: the
identity lives inside the file, so a restored older copy carries the same
identity, and a fresh process accepts it and can forget an unresolved
dispatch. Deployments must keep the ledger on protected persistent storage
with no unsupervised replace or restore, and keep serving disabled after any
ledger loss or restore until separately verified reconciliation. A restored
snapshot is never a clean ledger, and the reconcile command is not a substitute
for verifying one.

Continuity checks and this process's expectations are serialized per ledger
with the whole transaction, commit included. Expectations are published only
after a commit returns. A failed or uncertain commit publishes nothing: the
attempt stays held (pending) or, if the write did land, the mismatch refuses
dispatch; neither releases nor replays anything. Readiness waits for an
in-flight or waiting write only within READINESS_CHECK_WAIT_SECONDS (a waiting
read never delays the write), then reports not ready. Reads may overlap one
another (no write can run meanwhile); each connection's pin, identity and
continuity checks, which update this process's expectations, stay serialized.
Readiness waits at most READINESS_CHECK_WAIT_SECONDS in all, for a write or for
another connection's checks, and then reports not ready; that budget covers lock waiting only, not this
process's own filesystem or SQLite I/O, which has no hard bound here.

What holds admission is a pending attempt: one whose provider exchange may
still be running or whose terminal is unknown (timeout, cancel, transport
failure after send, 408, truncated or unparsable response, crash). A complete
provider response is terminal and is recorded as such, with its outcome kept
apart from success: 'validated' (the only outcome a caller may deliver),
'provider_completed_unaccepted' (finished envelope with reported usage that
failed local checks) or 'provider_rejected' (complete 4xx other than 408, or a
complete 5xx: the provider refused the request or failed on its side; the HTTP
status is kept).
Reported usage is provider-reported, not billing; absent usage is unknown,
never zero. A recorded terminal never replays or retries anything. Legacy
'complete' rows without an outcome row were validated under earlier code,
except rows with a hosted_attempt_reconciliations row: those were pending
attempts an operator closed with outcome 'unknown' (possible spend, no
delivered result). A pending row in this process's continuity check may
therefore move to 'complete' only through such a reconciliation.
The outcome table is additive: created by initialize(), or inside the first
terminal write on an older ledger; earlier code ignores it.

Legacy admission bounds all pending attempts by max_outstanding. Opt-in
carried-unknown-v1 permits up to max_concurrent locally owned active requests
(default one) plus at most two carried unknowns. Each active request holds one
slot lock; a pending row not owned by a slot of this process (another
process's active request included, since it cannot be told apart) counts as
carried. Unknown remote work may still execute: exposure is at most two plus
max_concurrent, not measured concurrency. A third unknown blocks admission, except that
a post-dispatch retry (key + ':r1') may leave its own stalled first attempt out of its
admission count (begin(exempt_attempt=...)); that row stays pending and counts for every
other admission. available(in_flight=...) lets the owning
process exclude its own still-running attempts, so a normal in-flight request
is not reported as an unresolved hold.

Writes require the rollback journal (journal_mode=delete), set once by
initialize(). No connection ever changes the journal mode; another mode is
reported and refused. status() is a separate read-only inspection.
"""
import json
import fcntl
import os
import re
import sqlite3
import stat
import sys
import threading
import time
import urllib.parse
import uuid
from contextlib import contextmanager


class HostedAdmissionBlocked(RuntimeError):
    pass


class HostedRequestKeyConflict(HostedAdmissionBlocked):
    """A request key was already used (never dispatched twice), or reused with a different body."""


class HostedSlotsBusy(HostedAdmissionBlocked):
    """Every active slot is held: nothing was reserved, so the caller may try again later."""


WRITE_JOURNAL_MODE = 'delete'
VALIDATED = 'validated'
OUTCOMES = (VALIDATED, 'provider_completed_unaccepted', 'provider_rejected')
# Opt-in policy: carried-unknown-v1 admission, except that a pending attempt older than the
# configured age-out (seconds, at least MIN_UNKNOWN_AGE_OUT_SECONDS) stops holding a slot.
AGING_POLICY = 'carried-unknown-aging-v1'
MIN_UNKNOWN_AGE_OUT_SECONDS = 300
# Recovery policies only: most attempts one process may own as active at once (one slot lock each).
MAX_CONCURRENT_LIMIT = 16
# Admission needs at most this many counted pending rows that no local slot owns.
MAX_CARRIED_UNKNOWN = 2
# Request key suffix of the one post-dispatch retry; begin(exempt_attempt=...) accepts only the
# pending attempt whose key is the retry key without this suffix.
RETRY_KEY_SUFFIX = ':r1'
# reconcile_unknown() never closes an attempt younger than this: the provider deadline is far
# shorter, so an attempt this old is no longer running in any process using this code.
MIN_RECONCILE_AGE_SECONDS = MIN_UNKNOWN_AGE_OUT_SECONDS
RECONCILED_OUTCOME = 'unknown'

_OUTCOMES_DDL = ('CREATE TABLE IF NOT EXISTS hosted_attempt_outcomes (id TEXT PRIMARY KEY REFERENCES hosted_attempts(id), '
                 'outcome TEXT NOT NULL CHECK(outcome IN (%s)), http_status INTEGER, prompt_tokens INTEGER, '
                 'completion_tokens INTEGER, recorded TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP)'
                 % ','.join("'%s'" % o for o in OUTCOMES))


# Additive: one row per attempt begun by this code. request_key (optional, UNIQUE) is the
# caller's durable deduplication key and request_digest the sha256 of the body it was bound
# to; sent=1 is committed immediately before the first request byte. A legacy pending
# attempt has no row here and is treated as possibly sent.
_REQUESTS_DDL = ('CREATE TABLE IF NOT EXISTS hosted_attempt_requests (id TEXT PRIMARY KEY REFERENCES hosted_attempts(id), '
                 'request_key TEXT UNIQUE, request_digest TEXT, sent INTEGER NOT NULL DEFAULT 0 CHECK(sent IN (0,1)), '
                 'CHECK(request_digest IS NULL OR request_key IS NOT NULL))')

# Additive: one row per pending attempt an operator reconciled as terminal with an unknown
# outcome (reconcile_unknown). The attempt row becomes 'complete' and stays, with its request
# row, as the record of possible spend; it has no hosted_attempt_outcomes row.
_RECONCILED_DDL = ("CREATE TABLE IF NOT EXISTS hosted_attempt_reconciliations (id TEXT PRIMARY KEY "
                   "REFERENCES hosted_attempts(id), outcome TEXT NOT NULL CHECK(outcome='%s'), "
                   "recorded TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP)" % RECONCILED_OUTCOME)


def _has_table(conn, name):
    return conn.execute("SELECT 1 FROM sqlite_master WHERE type='table' AND name=?", (name,)).fetchone() is not None


def _file_journal_mode(path):
    """Journal mode from the database header (bytes 18-19: 1 legacy, 2 WAL); never opens SQLite."""
    with open(path, 'rb') as f:
        header = f.read(100)
    if len(header) < 100 or not header.startswith(b'SQLite format 3\x00'):
        raise HostedAdmissionBlocked('hosted ledger unreadable')
    if header[18:20] == b'\x01\x01':
        return 'delete'
    return 'wal' if header[18:20] == b'\x02\x02' else 'unknown'


class HostedLedger:
    # Maximum attempts that may be pending (running anywhere or of unknown outcome) after
    # an admission, the new attempt included. 1 is the accepted behaviour: no new dispatch
    # while any attempt is pending. A larger value is a proposed policy only; it admits
    # distinct new attempts while older unresolved ones stay pending and counted, and it
    # says nothing about whether their remote work has ended.
    max_outstanding = 1

    def __init__(self, path, *, expected_id=None):
        if not path or path == ':memory:':
            raise HostedAdmissionBlocked('durable hosted accounting required')
        self.path = path
        self.expected_id = expected_id
        self._init_gate()
        self._init_slots(1)
        with self.connect() as conn:
            conn.execute('CREATE TABLE IF NOT EXISTS hosted_attempts '
                         '(id TEXT PRIMARY KEY, state TEXT NOT NULL CHECK(state IN '
                         '(\'pending\',\'complete\')), created TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP)')

    @classmethod
    def initialize(cls, path):
        """One-time operator step. Refuses an existing path; returns the new ledger identity."""
        if not path or path == ':memory:':
            raise HostedAdmissionBlocked('durable hosted accounting required')
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, 'O_NOFOLLOW', 0), 0o600)
        os.close(fd)
        ledger_id = str(uuid.uuid4())
        try:
            ledger = cls(path)
            with ledger.connect() as conn:
                if conn.execute('PRAGMA journal_mode=' + WRITE_JOURNAL_MODE).fetchone()[0] != WRITE_JOURNAL_MODE:
                    raise HostedAdmissionBlocked('hosted ledger journal mode unavailable')
                conn.execute('CREATE TABLE hosted_ledger_identity (singleton INTEGER PRIMARY KEY CHECK(singleton=1), '
                             'ledger_id TEXT NOT NULL, created TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP)')
                conn.execute('INSERT INTO hosted_ledger_identity(singleton, ledger_id) VALUES (1, ?)', (ledger_id,))
                conn.execute(_OUTCOMES_DDL)
        except BaseException:
            # The file was created by this call and holds no identity: remove it so a
            # later start can create the ledger cleanly instead of failing on it forever.
            try:
                os.unlink(path)
            except OSError:
                pass
            raise
        dir_fd = os.open(os.path.dirname(os.path.abspath(path)), os.O_RDONLY)
        try:
            os.fsync(dir_fd)
        finally:
            os.close(dir_fd)
        return ledger_id

    @staticmethod
    def stored_identity(path):
        """Read-only: the ledger identity recorded inside an existing ledger file."""
        try:
            st = os.lstat(path)
        except OSError:
            raise HostedAdmissionBlocked('hosted ledger missing') from None
        if not stat.S_ISREG(st.st_mode):
            raise HostedAdmissionBlocked('hosted ledger is not a regular file')
        uri = 'file:' + urllib.parse.quote(os.path.abspath(path)) + '?mode=ro'
        try:
            conn = sqlite3.connect(uri, uri=True, timeout=5)
            try:
                row = conn.execute('SELECT ledger_id FROM hosted_ledger_identity WHERE singleton=1').fetchone()
            finally:
                conn.close()
        except sqlite3.Error:
            raise HostedAdmissionBlocked('hosted ledger unreadable') from None
        if not row or not row[0]:
            raise HostedAdmissionBlocked('hosted ledger identity missing')
        return row[0]

    @classmethod
    def open_or_create(cls, path, **kwargs):
        """Service-side open with no operator step: create the ledger once, inside a
        directory that must already exist (a missing or unmounted state directory is still
        refused), then open it strictly with the identity stored in it. Every check of
        open_existing applies; the file is pinned for the life of the process. Returns the
        ledger with `created` set when this call created the file."""
        if not path or path == ':memory:':
            raise HostedAdmissionBlocked('durable hosted accounting required')
        directory = os.path.dirname(os.path.abspath(path))
        if not os.path.isdir(directory):
            raise HostedAdmissionBlocked('hosted ledger directory missing; refusing to create it')
        created = False
        if not os.path.lexists(path):
            try:
                cls.initialize(path)
                created = True
            except FileExistsError:
                pass  # created concurrently; open the one that exists
            except PermissionError:
                raise HostedAdmissionBlocked('hosted ledger or journal directory not writable by runtime UID') from None
            except OSError as exc:
                raise HostedAdmissionBlocked('hosted ledger could not be created: %s' % exc.strerror) from None
            except sqlite3.Error:
                raise HostedAdmissionBlocked('hosted ledger could not be created') from None
        ledger = cls.open_existing(path, cls.stored_identity(path), **kwargs)
        ledger.created = created
        return ledger

    @classmethod
    def open_existing(cls, path, expected_id, *, max_outstanding=1, recovery_policy="legacy",
                      unknown_age_out_seconds=None, max_concurrent=1):
        """Strict service-side open: never creates, never accepts another ledger."""
        if not path or path == ':memory:':
            raise HostedAdmissionBlocked('durable hosted accounting required')
        if not expected_id:
            raise HostedAdmissionBlocked('hosted ledger identity not configured')
        try:
            st = os.lstat(path)
        except OSError:
            raise HostedAdmissionBlocked('hosted ledger missing; refusing to create a new one') from None
        if not stat.S_ISREG(st.st_mode):
            raise HostedAdmissionBlocked('hosted ledger is not a regular file')
        directory = os.path.dirname(os.path.abspath(path))
        # The rollback journal is created beside the database, so the runtime UID
        # needs write access to the directory as well as the file.
        if not os.access(path, os.R_OK | os.W_OK) or not os.access(directory, os.W_OK | os.X_OK):
            raise HostedAdmissionBlocked('hosted ledger or journal directory not writable by runtime UID')
        if isinstance(max_outstanding, bool) or not isinstance(max_outstanding, int) or max_outstanding < 1:
            raise HostedAdmissionBlocked('invalid hosted outstanding-attempt bound')
        ledger = object.__new__(cls)
        ledger.path = path
        ledger.expected_id = expected_id
        if recovery_policy not in ('legacy', 'carried-unknown-v1', AGING_POLICY):
            raise HostedAdmissionBlocked('invalid hosted recovery policy')
        if recovery_policy != 'legacy' and max_outstanding != 2:
            raise HostedAdmissionBlocked('recovery policy requires retained bound two')
        if recovery_policy == AGING_POLICY:
            if (isinstance(unknown_age_out_seconds, bool) or not isinstance(unknown_age_out_seconds, int)
                    or unknown_age_out_seconds < MIN_UNKNOWN_AGE_OUT_SECONDS):
                raise HostedAdmissionBlocked('invalid hosted unknown age-out')
        elif unknown_age_out_seconds is not None:
            raise HostedAdmissionBlocked('unknown age-out requires the aging recovery policy')
        if (isinstance(max_concurrent, bool) or not isinstance(max_concurrent, int)
                or not 1 <= max_concurrent <= MAX_CONCURRENT_LIMIT):
            raise HostedAdmissionBlocked('invalid hosted concurrent-attempt bound')
        if recovery_policy == 'legacy' and max_concurrent != 1:
            raise HostedAdmissionBlocked('concurrent hosted attempts require a recovery policy')
        ledger.recovery_policy = recovery_policy
        ledger.unknown_age_out_seconds = unknown_age_out_seconds
        ledger._init_slots(max_concurrent)
        ledger.max_outstanding = max_outstanding
        ledger._pin = (st.st_dev, st.st_ino)
        ledger._rows_seen = 0
        ledger._last = None  # (attempt, state) this process last wrote
        ledger._broken = None
        ledger._init_gate()
        try:
            with ledger._serialized(), ledger.connect() as conn:
                # connect() has already checked pin, identity and continuity.
                if conn.execute('PRAGMA quick_check').fetchone()[0] != 'ok':
                    raise HostedAdmissionBlocked('hosted ledger failed integrity check')
        except sqlite3.Error:
            raise HostedAdmissionBlocked('hosted ledger unreadable') from None
        return ledger

    def _check_pin(self):
        try:
            st = os.lstat(self.path)
        except OSError:
            st = None
        if st is None or not stat.S_ISREG(st.st_mode) or (st.st_dev, st.st_ino) != self._pin:
            self._broken = 'hosted ledger file replaced while running'
        if self._broken:
            raise HostedAdmissionBlocked(self._broken)

    def _check_continuity(self, conn):
        # Rows are never deleted, so the highest rowid only grows.
        rows = conn.execute('SELECT coalesce(max(rowid), 0) FROM hosted_attempts').fetchone()[0]
        last_ok = self._last is None or conn.execute(
            'SELECT 1 FROM hosted_attempts WHERE id=? AND state=?', self._last).fetchone() is not None
        if not last_ok and self._last[1] == 'pending' and _has_table(conn, 'hosted_attempt_reconciliations'):
            # Forward only: this process's pending attempt was closed by an operator
            # reconciliation (complete + reconciliation row). A rollback never produces that.
            last_ok = conn.execute(
                "SELECT 1 FROM hosted_attempt_reconciliations r JOIN hosted_attempts a USING(id) "
                "WHERE r.id=? AND a.state='complete'", (self._last[0],)).fetchone() is not None
        if rows < self._rows_seen or not last_ok:
            self._broken = 'hosted ledger rolled back while running'
            raise HostedAdmissionBlocked(self._broken)
        self._rows_seen = rows

    def _check_write_mode(self, conn):
        if conn.execute('PRAGMA journal_mode').fetchone()[0] != WRITE_JOURNAL_MODE:
            raise HostedAdmissionBlocked('hosted ledger journal mode unsupported; not changed')

    def _check_identity(self, conn):
        try:
            row = conn.execute('SELECT ledger_id FROM hosted_ledger_identity WHERE singleton=1').fetchone()
        except sqlite3.Error:
            row = None
        if row is None or row[0] != self.expected_id:
            raise HostedAdmissionBlocked('hosted ledger identity mismatch')

    # Longest available() waits for another connection's pin/identity/continuity
    # checks before reporting not ready (lock waiting only, not own I/O).
    READINESS_CHECK_WAIT_SECONDS = 0.25
    # begin() rides out a readiness probe's brief hold of a slot lock instead of refusing a
    # real request; when every slot is genuinely active it still refuses once this wait expires.
    ACTIVE_ACQUIRE_WAIT_SECONDS = 1.0

    def _init_gate(self):
        self._gate = threading.Condition()
        self._readers = 0
        self._writing = False
        self._writers_waiting = 0
        self._checks = threading.Lock()
        self._readiness = threading.local()  # .deadline, set only inside available()

    @contextmanager
    def _serialized(self, *, write=False, blocking=True, wait_until=None):
        # A write is held across connect(), the body and the commit, exclusive of
        # every read, so no check ever sees an expectation whose transaction has not
        # committed. Reads share the gate; blocking=False never waits on a write, and
        # wait_until (a time.monotonic() deadline) waits for it at most until then.
        # A read that is still waiting holds no permit, so it never delays the write.
        with self._gate:
            if write:
                self._writers_waiting += 1
                try:
                    self._gate.wait_for(lambda: not self._writing and self._readers == 0)
                finally:
                    self._writers_waiting -= 1
                self._writing = True
            else:
                ready = lambda: not self._writing and not self._writers_waiting
                if wait_until is not None:
                    if not self._gate.wait_for(ready, timeout=max(0.0, wait_until - time.monotonic())):
                        raise HostedAdmissionBlocked('hosted ledger write in progress')
                elif not blocking and not ready():
                    raise HostedAdmissionBlocked('hosted ledger write in progress')
                self._gate.wait_for(ready)
                self._readers += 1
        try:
            yield
        finally:
            with self._gate:
                if write:
                    self._writing = False
                else:
                    self._readers -= 1
                self._gate.notify_all()

    @contextmanager
    def _check_state(self):
        # Serializes the checks that update _rows_seen and may latch _broken. Inside
        # available() (this thread's readiness deadline is set) it never waits past
        # that deadline for another connection's checks.
        deadline = getattr(self._readiness, 'deadline', None)
        if deadline is None:
            self._checks.acquire()
        elif not self._checks.acquire(timeout=max(0.0, deadline - time.monotonic())):
            raise HostedAdmissionBlocked('hosted ledger busy')
        try:
            yield
        finally:
            self._checks.release()

    @contextmanager
    def connect(self, *, write=False):
        strict = self.expected_id is not None
        if not strict:
            conn = sqlite3.connect(self.path, timeout=5)
        else:
            # mode=rw: a vanished or unmounted ledger errors instead of being recreated empty.
            with self._check_state():
                self._check_pin()
            uri = 'file:' + urllib.parse.quote(os.path.abspath(self.path)) + '?mode=rw'
            conn = sqlite3.connect(uri, uri=True, timeout=5)
        try:
            conn.execute('PRAGMA synchronous=FULL')
            with conn:
                if strict:
                    if write:
                        conn.execute('BEGIN IMMEDIATE')
                        self._check_write_mode(conn)
                    # Re-check after opening (and, for writes, after locking): the
                    # connection must hold the pinned file, not a swapped-in copy.
                    # Serialized: these update _rows_seen and may latch _broken.
                    with self._check_state():
                        self._check_pin()
                        self._check_identity(conn)
                        self._check_continuity(conn)
                yield conn
        finally:
            conn.close()

    def _recovery(self):
        return getattr(self, 'recovery_policy', 'legacy') in ('carried-unknown-v1', AGING_POLICY)

    def _counted_pending(self):
        """SQL condition and parameters for the pending rows that count toward admission.

        Every pending row counts, except under the aging policy, where a row older than the
        age-out no longer holds a slot. It stays pending and unknown: the record of possible
        spend is never cleared, only its claim on admission ends."""
        if getattr(self, 'recovery_policy', 'legacy') == AGING_POLICY:
            return ("state='pending' AND created > datetime('now', ?)",
                    (f"-{int(self.unknown_age_out_seconds)} seconds",))
        return ("state='pending'", ())

    def _init_slots(self, max_concurrent):
        # Slot 0 is <ledger>.active-lock (the single lock of earlier code); slot i > 0 is
        # <ledger>.active-lock.<i>. Each active attempt holds exactly one slot from begin() until
        # finish_local(). _owned maps attempt -> (slot, fd) for this process's attempts only.
        self.max_concurrent = max_concurrent
        self._slots_guard = threading.Lock()
        self._owned = {}

    def _slot_path(self, slot):
        return self.path + '.active-lock' + ('' if slot == 0 else '.%d' % slot)

    def _try_slot(self, slot):
        # One OS lock per slot of a protected local ledger; never unlink/replace these files.
        # A process crash releases local ownership, NOT remote uncertainty.
        # Returns the locked fd, or None when another holder (any process or thread) has it.
        path = self._slot_path(slot)
        fd = os.open(path, os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW | os.O_CLOEXEC, 0o600)
        try:
            st = os.fstat(fd)
            if not stat.S_ISREG(st.st_mode) or st.st_nlink != 1:
                raise HostedAdmissionBlocked('invalid hosted active lock')
            try:
                fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                os.close(fd)
                return None
            path_st = os.stat(path, follow_symlinks=False)
            if (st.st_dev, st.st_ino) != (path_st.st_dev, path_st.st_ino):
                raise HostedAdmissionBlocked('hosted active lock changed')
            return fd
        except BaseException:
            os.close(fd)
            raise

    def _acquire_slot(self, wait=0.0):
        # wait > 0 (begin) retries for at most that long; wait == 0 (readiness) tries each slot
        # once, so a probe can never delay or refuse a real request. Any invalid slot file
        # refuses outright instead of being skipped.
        deadline = time.monotonic() + max(0.0, wait)
        while True:
            for slot in range(self.max_concurrent):
                try:
                    fd = self._try_slot(slot)
                except Exception:
                    raise HostedAdmissionBlocked('hosted active owner unavailable') from None
                if fd is not None:
                    return (slot, fd)
            if time.monotonic() >= deadline:
                raise HostedSlotsBusy('hosted active owner present')
            time.sleep(0.01)

    def _release_slot(self, held):
        # Drops any ownership recorded for this (slot, fd), then the lock itself.
        with self._slots_guard:
            for attempt in [a for a, h in self._owned.items() if h is held]:
                del self._owned[attempt]
        os.close(held[1])

    def _owner(self, attempt):
        with self._slots_guard:
            return self._owned.get(attempt)

    def _owned_attempts(self):
        with self._slots_guard:
            return frozenset(self._owned)

    # Single-owner views kept for callers of the one-slot code (exact when max_concurrent is 1).
    @property
    def _active_attempt(self):
        with self._slots_guard:
            return next(iter(self._owned), None)

    @property
    def _active_fd(self):
        with self._slots_guard:
            held = next(iter(self._owned.values()), None)
        return None if held is None else held[1]

    def busy(self):
        """True only when every slot is held right now (recovery policies). Never waits and holds
        nothing afterwards; a refusal for this reason reserved nothing and may clear by itself."""
        if not self._recovery():
            return False
        try:
            self._release_slot(self._acquire_slot())
        except HostedSlotsBusy:
            return True
        except HostedAdmissionBlocked:
            return False
        return False

    def finish_local(self, attempt):
        """End ONLY local ownership after transport closes. Pending stays unknown.
        Write failure propagates; lock release never fabricates a terminal.
        A subsequent locked begin conservatively classifies orphaned phases.
        """
        if not self._recovery(): return
        held = self._owner(attempt)
        if held is None:
            raise HostedAdmissionBlocked('hosted local owner mismatch')
        # Ownership ends inside the write gate, so no begin() or available() ever sees this
        # attempt released from its slot but not yet marked unknown, or the reverse.
        with self._serialized(write=True):
            try:
                with self.connect(write=True) as conn:
                    conn.execute("UPDATE hosted_attempt_phases SET phase='unknown' WHERE id=? "
                                 "AND id IN (SELECT id FROM hosted_attempts WHERE state='pending')", (attempt,))
            finally:
                self._release_slot(held)

    @staticmethod
    def _phases(conn):
        conn.execute("CREATE TABLE IF NOT EXISTS hosted_attempt_phases (id TEXT PRIMARY KEY "
                     "REFERENCES hosted_attempts(id), phase TEXT NOT NULL CHECK(phase IN "
                     "('reserved','dispatched','unknown','terminal')))")

    def available(self, *, in_flight=()):
        """No unresolved hold. in_flight: attempts the caller's own running requests still own."""
        self._readiness.deadline = time.monotonic() + self.READINESS_CHECK_WAIT_SECONDS
        try:
            if self._recovery():
                # Busy test only: take and immediately drop one free slot (never waiting), so a
                # process with every slot active reports not ready but the probe holds nothing
                # while it reads. A slow read can therefore never delay or refuse a real begin().
                self._release_slot(self._acquire_slot())
            # A ledger write takes milliseconds (begin, sent marker, terminal): readiness waits
            # it out within the same deadline instead of reporting a false not-ready, which made
            # the miner drop generation until the sidecar's next registration. The wait holds no
            # permit, so it never delays the write. A refusal here (including an expired wait)
            # releases the read permit on the way out of _serialized.
            with self._serialized(wait_until=self._readiness.deadline), self.connect() as conn:
                if self.expected_id is not None:
                    self._check_write_mode(conn)
                where, args = self._counted_pending()
                pending = conn.execute("SELECT id FROM hosted_attempts WHERE " + where, args).fetchall()
                # The caller's own running requests are not a hold; every other pending
                # row (another process's, or unresolved) counts toward the bound. Under a
                # recovery policy the ledger's own slot owners are this process's running requests.
                if self._recovery():
                    owned = self._owned_attempts()
                    return sum(1 for row in pending if row[0] not in owned) <= MAX_CARRIED_UNKNOWN
                unresolved = sum(1 for row in pending if row[0] not in in_flight)
                return unresolved < self.max_outstanding
        except HostedAdmissionBlocked:
            return False
        finally:
            self._readiness.deadline = None

    def begin(self, request_key=None, request_digest=None, *, exempt_attempt=None):
        """exempt_attempt (recovery policies only): the stalled first attempt of the request this
        post-dispatch retry belongs to. See _begin; legacy admission ignores it."""
        if not self._recovery():
            return self._begin(request_key, request_digest)
        if request_key is None or request_digest is None:
            raise HostedAdmissionBlocked('recovery requires durable request and body binding')
        held = self._acquire_slot(wait=self.ACTIVE_ACQUIRE_WAIT_SECONDS)
        try:
            return self._begin(request_key, request_digest, held, exempt_attempt)
        except BaseException:
            self._release_slot(held)
            raise

    @staticmethod
    def _exempt_row(conn, request_key, exempt_attempt):
        """The one pending row a post-dispatch retry may leave out of its own admission count, or None.

        Accepted only when request_key is a retry key (base + RETRY_KEY_SUFFIX) and exempt_attempt
        names a pending attempt recorded under exactly that base key. Anything else is ignored, so
        admission is then decided as without the exemption. The row itself is never changed."""
        if (not isinstance(exempt_attempt, str) or request_key is None
                or not request_key.endswith(RETRY_KEY_SUFFIX) or len(request_key) <= len(RETRY_KEY_SUFFIX)):
            return None
        row = conn.execute("SELECT r.request_key FROM hosted_attempt_requests r JOIN hosted_attempts a USING(id) "
                           "WHERE r.id=? AND a.state='pending'", (exempt_attempt,)).fetchone()
        if row is None or row[0] != request_key[:-len(RETRY_KEY_SUFFIX)]:
            return None
        return exempt_attempt

    def _begin(self, request_key=None, request_digest=None, held=None, exempt_attempt=None):
        """Reserve one attempt. The bound check, the duplicate-key check and the insert are one
        BEGIN IMMEDIATE transaction, so they are atomic across every process sharing the file.
        held: the (slot, fd) this attempt will own (recovery policies only).
        exempt_attempt: under a recovery policy, a pending row excluded from the count for this
        admission only (see _exempt_row); at most one row, never modified, still pending."""
        if request_key is not None and (not isinstance(request_key, str) or not request_key or len(request_key) > 256):
            raise HostedAdmissionBlocked('invalid hosted request key')
        if request_digest is not None and (request_key is None or not isinstance(request_digest, str)
                                           or not re.fullmatch(r"[0-9a-f]{64}", request_digest)):
            raise HostedAdmissionBlocked('hosted request digest must be a sha256 hex digest bound to a request key')
        with self._serialized(write=True):
            with self.connect(write=True) as conn:
                if self.expected_id is None:
                    conn.execute('BEGIN IMMEDIATE')
                conn.execute(_REQUESTS_DDL)
                if 'request_digest' not in {r[1] for r in conn.execute('PRAGMA table_info(hosted_attempt_requests)')}:
                    # A table from the held candidate without the binding column: add it, nullable.
                    conn.execute('ALTER TABLE hosted_attempt_requests ADD COLUMN request_digest TEXT')
                if request_key is not None:
                    prior = conn.execute('SELECT request_digest FROM hosted_attempt_requests WHERE request_key=?',
                                         (request_key,)).fetchone()
                    if prior is not None:
                        # Either use refuses; a body mismatch is reported distinctly when both digests are known.
                        if prior[0] is not None and request_digest is not None and prior[0] != request_digest:
                            raise HostedRequestKeyConflict('hosted request key reused with a different body')
                        raise HostedRequestKeyConflict('hosted request key already used; never re-dispatched')
                where, args = self._counted_pending()
                # Attempts owned by a slot of this process are running here, not carried. The
                # snapshot is exact: ownership only changes inside this same write gate.
                owned = self._owned_attempts()
                # A post-dispatch retry is not blocked by its own stalled first attempt: that one
                # validated row (if counted) is left out of this admission only. It stays pending.
                exempt = None
                if self._recovery() and exempt_attempt is not None:
                    exempt = self._exempt_row(conn, request_key, exempt_attempt)
                outstanding = sum(1 for row in conn.execute("SELECT id FROM hosted_attempts WHERE " + where, args)
                                  if row[0] not in owned and row[0] != exempt)
                if self._recovery():
                    self._phases(conn)
                    # The slot locks prove no participating local writer remains active except
                    # this process's own slot owners, whose phases are left alone. They do NOT
                    # prove remote completion. Legacy and orphaned pending stay unknown.
                    others, keep = '', ()
                    if owned:
                        others, keep = ' AND id NOT IN (%s)' % ','.join('?' * len(owned)), tuple(owned)
                    conn.execute("INSERT OR IGNORE INTO hosted_attempt_phases(id,phase) "
                                 "SELECT id,'unknown' FROM hosted_attempts WHERE state='pending'" + others, keep)
                    conn.execute("UPDATE hosted_attempt_phases SET phase='unknown' WHERE id IN "
                                 "(SELECT id FROM hosted_attempts WHERE state='pending')" + others, keep)
                # Recovery: admit while at most MAX_CARRIED_UNKNOWN unowned rows count (a free slot
                # is already held). With one slot this is the earlier "carried + 1 > 3" refusal.
                if outstanding + 1 > (MAX_CARRIED_UNKNOWN + 1 if self._recovery() else self.max_outstanding):
                    raise HostedAdmissionBlocked('hosted completion unresolved; no new dispatch')
                attempt = str(uuid.uuid4())
                rowid = conn.execute("INSERT INTO hosted_attempts(id,state) VALUES (?, 'pending')", (attempt,)).lastrowid
                conn.execute('INSERT INTO hosted_attempt_requests(id, request_key, request_digest) VALUES (?, ?, ?)',
                             (attempt, request_key, request_digest))
                if self._recovery():
                    conn.execute("INSERT INTO hosted_attempt_phases(id,phase) VALUES (?, 'reserved')", (attempt,))
            # Reached only after the commit returned. Ownership is published while the write
            # gate is still held, together with the new row.
            if self.expected_id is not None:
                self._last = (attempt, 'pending')
                self._rows_seen = rowid
            if held is not None:
                with self._slots_guard:
                    self._owned[attempt] = held
            return attempt

    def complete(self, attempt, outcome=VALIDATED, *, http_status=None, usage=None):
        """Record a terminal provider exchange for this pending attempt (default: validated)."""
        if outcome not in OUTCOMES:
            raise HostedAdmissionBlocked('unsupported hosted terminal outcome')
        if http_status is not None and (isinstance(http_status, bool) or not isinstance(http_status, int)):
            raise HostedAdmissionBlocked('invalid hosted terminal status')
        prompt_tokens, completion_tokens = (None, None) if usage is None else usage
        for n in (prompt_tokens, completion_tokens):
            if n is not None and (isinstance(n, bool) or not isinstance(n, int) or n < 0):
                raise HostedAdmissionBlocked('invalid reported usage')
        with self._serialized(write=True):
            with self.connect(write=True) as conn:
                changed = conn.execute("UPDATE hosted_attempts SET state='complete' WHERE id=? AND state='pending'", (attempt,))
                if changed.rowcount != 1:
                    raise HostedAdmissionBlocked('hosted terminal accounting mismatch')
                if self._recovery():
                    if self._owner(attempt) is None:
                        raise HostedAdmissionBlocked('hosted terminal owner mismatch')
                    conn.execute("UPDATE hosted_attempt_phases SET phase='terminal' WHERE id=?", (attempt,))
                # Same transaction: the terminal state and its outcome commit together or not at all.
                conn.execute(_OUTCOMES_DDL)
                conn.execute('INSERT INTO hosted_attempt_outcomes(id, outcome, http_status, prompt_tokens, completion_tokens) '
                             'VALUES (?, ?, ?, ?, ?)', (attempt, outcome, http_status, prompt_tokens, completion_tokens))
            if self.expected_id is not None:
                self._last = (attempt, 'complete')

    def mark_sent(self, attempt):
        """Commit sent=1 for this pending attempt; call immediately before the first request byte.
        If this raises or its commit is uncertain, the caller must not send. The row stays
        pending either way and counts toward the bound; it is never treated as resolved."""
        with self._serialized(write=True):
            with self.connect(write=True) as conn:
                if self.expected_id is None:
                    conn.execute('BEGIN IMMEDIATE')
                changed = conn.execute(
                    "UPDATE hosted_attempt_requests SET sent=1 WHERE id=? AND sent=0 AND id IN "
                    "(SELECT id FROM hosted_attempts WHERE state='pending')", (attempt,))
                if changed.rowcount != 1:
                    raise HostedAdmissionBlocked('hosted sent marker mismatch; not sent')
                if self._recovery():
                    if self._owner(attempt) is None:
                        raise HostedAdmissionBlocked('hosted dispatch owner mismatch')
                    conn.execute("UPDATE hosted_attempt_phases SET phase='dispatched' WHERE id=?", (attempt,))

    def reconcile_unknown(self, *, attempt=None, request_key=None, older_than=None):
        """Operator step: record stuck pending attempts as terminal with outcome 'unknown'.

        Select exactly one of: attempt (an attempt id, as `status` lists it), request_key, or
        older_than (seconds; every pending attempt at least that old). No attempt younger than
        MIN_RECONCILE_AGE_SECONDS is ever closed, and older_than must be at least that. Each
        selected row moves pending -> complete with a hosted_attempt_reconciliations row
        (outcome 'unknown') in one transaction; its request row, sent marker and creation time
        stay as the record of possible spend. Nothing is deleted, retried or replayed, and no
        provider outcome is invented. A named attempt or key that is not pending, or too recent,
        refuses. Returns the reconciled attempt ids (possibly empty for older_than)."""
        if sum(x is not None for x in (attempt, request_key, older_than)) != 1:
            raise HostedAdmissionBlocked('reconcile needs exactly one of attempt, request key or age')
        if older_than is not None and (isinstance(older_than, bool) or not isinstance(older_than, int)
                                       or older_than < MIN_RECONCILE_AGE_SECONDS):
            raise HostedAdmissionBlocked('reconcile age must be at least %d seconds' % MIN_RECONCILE_AGE_SECONDS)
        for value in (attempt, request_key):
            if value is not None and (not isinstance(value, str) or not value or len(value) > 256):
                raise HostedAdmissionBlocked('invalid hosted reconcile selector')
        age = "datetime('now', '-%d seconds')" % (older_than or MIN_RECONCILE_AGE_SECONDS)
        with self._serialized(write=True):
            with self.connect(write=True) as conn:
                if self.expected_id is None:
                    conn.execute('BEGIN IMMEDIATE')
                if older_than is not None:
                    ids = [r[0] for r in conn.execute(
                        "SELECT id FROM hosted_attempts WHERE state='pending' AND created <= " + age
                        + " ORDER BY created, id")]
                else:
                    if attempt is not None:
                        row = conn.execute("SELECT id, created <= " + age + " FROM hosted_attempts "
                                           "WHERE id=? AND state='pending'", (attempt,)).fetchone()
                    else:
                        row = None
                        if _has_table(conn, 'hosted_attempt_requests'):
                            row = conn.execute("SELECT a.id, a.created <= " + age + " FROM hosted_attempts a "
                                               "JOIN hosted_attempt_requests r USING(id) "
                                               "WHERE r.request_key=? AND a.state='pending'", (request_key,)).fetchone()
                    if row is None:
                        raise HostedAdmissionBlocked('no pending hosted attempt matches')
                    if not row[1]:
                        raise HostedAdmissionBlocked('hosted attempt too recent to reconcile')
                    ids = [row[0]]
                owned = self._owned_attempts()
                if any(a in owned for a in ids):
                    raise HostedAdmissionBlocked('hosted attempt still owned by this process')
                if ids:
                    conn.execute(_RECONCILED_DDL)
                    phases = _has_table(conn, 'hosted_attempt_phases')
                for a in ids:
                    changed = conn.execute("UPDATE hosted_attempts SET state='complete' WHERE id=? AND state='pending'", (a,))
                    if changed.rowcount != 1:
                        raise HostedAdmissionBlocked('hosted reconcile accounting mismatch')
                    conn.execute('INSERT INTO hosted_attempt_reconciliations(id, outcome) VALUES (?, ?)',
                                 (a, RECONCILED_OUTCOME))
                    if phases:
                        conn.execute("UPDATE hosted_attempt_phases SET phase='terminal' WHERE id=?", (a,))
            if self.expected_id is not None and ids:
                self._last = (ids[-1], 'complete')
            return ids

    def pending(self):
        with self._serialized(), self.connect() as conn:
            return [r[0] for r in conn.execute("SELECT id FROM hosted_attempts WHERE state='pending' ORDER BY created, id")]

    @staticmethod
    def status(path, expected_id):
        """Read-only operator inspection: no writes, no PRAGMA changes, no write access needed.

        A non-rollback-journal ledger is reported, not opened (opening a WAL file
        read-only would need its -shm/-wal files or a writable directory).
        """
        try:
            st = os.lstat(path)
        except OSError:
            raise HostedAdmissionBlocked('hosted ledger missing') from None
        if not stat.S_ISREG(st.st_mode):
            raise HostedAdmissionBlocked('hosted ledger is not a regular file')
        mode = _file_journal_mode(path)
        if mode != WRITE_JOURNAL_MODE:
            return {'journal_mode': mode, 'supported': False}
        uri = 'file:' + urllib.parse.quote(os.path.abspath(path)) + '?mode=ro'
        try:
            conn = sqlite3.connect(uri, uri=True, timeout=5)
            try:
                if conn.execute('PRAGMA quick_check').fetchone()[0] != 'ok':
                    raise HostedAdmissionBlocked('hosted ledger failed integrity check')
                row = conn.execute('SELECT ledger_id FROM hosted_ledger_identity WHERE singleton=1').fetchone()
                if row is None or row[0] != expected_id:
                    raise HostedAdmissionBlocked('hosted ledger identity mismatch')
                pending = [r[0] for r in conn.execute(
                    "SELECT id FROM hosted_attempts WHERE state='pending' ORDER BY created, id")]
                reconciled = [r[0] for r in conn.execute(
                    'SELECT id FROM hosted_attempt_reconciliations ORDER BY recorded, id')] \
                    if _has_table(conn, 'hosted_attempt_reconciliations') else []
            finally:
                conn.close()
        except sqlite3.Error:
            raise HostedAdmissionBlocked('hosted ledger unreadable') from None
        return {'ledger_id': expected_id, 'journal_mode': mode, 'supported': True, 'pending': pending,
                'reconciled_unknown': reconciled}

    def pending_detail(self):
        """[{'id', 'sent'}] for each pending attempt. sent is False only when this code recorded
        the attempt as reserved and never sent; True, or None for a legacy row with no
        marker, means the provider may have received it. Read-only."""
        with self._serialized(), self.connect() as conn:
            pending = [r[0] for r in conn.execute(
                "SELECT id FROM hosted_attempts WHERE state='pending' ORDER BY created, id")]
            has = conn.execute("SELECT 1 FROM sqlite_master WHERE type='table' AND name='hosted_attempt_requests'").fetchone()
            sent = dict(conn.execute('SELECT id, sent FROM hosted_attempt_requests').fetchall()) if has else {}
        return [{'id': a, 'sent': (bool(sent[a]) if a in sent else None)} for a in pending]


RECONCILE_SELECTORS = {'--attempt': 'attempt', '--request-key': 'request_key',
                       '--all-pending-older-than': 'older_than'}
USAGE = ('usage: hosted_accounting.py init PATH | status PATH [LEDGER_ID] | reconcile PATH [LEDGER_ID] '
         '(--attempt ID | --request-key KEY | --all-pending-older-than SECONDS)')


def main(argv):
    """Operator CLI. `init PATH` creates a ledger (the sidecar also does this itself on first
    start); `status PATH [LEDGER_ID]` is read-only (exit 3: unsupported journal mode, reported
    not changed); `reconcile PATH [LEDGER_ID] SELECTOR VALUE` records the selected stuck pending
    attempts as terminal with outcome 'unknown' (see reconcile_unknown; exit 1 with the reason
    when refused). Without LEDGER_ID the identity stored in the file is used. Nothing is ever
    deleted."""
    if len(argv) == 2 and argv[0] == 'init':
        print(json.dumps({'ledger_id': HostedLedger.initialize(argv[1])}))
        return 0
    # Optional LEDGER_ID: insert the stored identity when it is omitted.
    if argv and argv[0] == 'status' and len(argv) == 2:
        argv = argv + [None]
    if argv and argv[0] == 'reconcile' and len(argv) == 4 and argv[2] in RECONCILE_SELECTORS:
        argv = argv[:2] + [None] + argv[2:]
    try:
        if len(argv) in (3, 5) and argv[0] in ('status', 'reconcile') and argv[2] is None:
            argv[2] = HostedLedger.stored_identity(argv[1])
    except HostedAdmissionBlocked as exc:
        print(json.dumps({'error': str(exc)}), file=sys.stderr)
        return 1
    if len(argv) == 3 and argv[0] == 'status':
        report = HostedLedger.status(argv[1], argv[2])
        print(json.dumps(report))
        return 0 if report['supported'] else 3
    if len(argv) == 5 and argv[0] == 'reconcile' and argv[3] in RECONCILE_SELECTORS:
        name, value = RECONCILE_SELECTORS[argv[3]], argv[4]
        if name == 'older_than':
            if not value.isdigit():
                print(USAGE, file=sys.stderr)
                return 2
            value = int(value)
        try:
            # The default (legacy) open validates path, identity and integrity; the policy
            # only affects admission, which reconcile does not use.
            ledger = HostedLedger.open_existing(argv[1], argv[2])
            ids = ledger.reconcile_unknown(**{name: value})
        except HostedAdmissionBlocked as exc:
            print(json.dumps({'error': str(exc)}), file=sys.stderr)
            return 1
        print(json.dumps({'reconciled': ids, 'outcome': RECONCILED_OUTCOME}))
        return 0
    print(USAGE, file=sys.stderr)
    return 2


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
