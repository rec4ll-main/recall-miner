"""Strict HTTP/1.1 chunked transfer framing (RFC 9112 7.1) for the generation backend.

Pure byte parsing with no I/O: ``service`` feeds it the buffered response body and
re-exports every name defined here.
"""

import re

_CHUNK_SIZE_RE = re.compile(rb"[0-9A-Fa-f]{1,16}")


_CHUNK_LINE_LIMIT = 4096


def _chunk_line(buf, i):
    """End of the CRLF-terminated line at i, or None while it is still arriving.
    A bare CR or LF, or an overlong line, is malformed however the bytes were split."""
    j = buf.find(b"\r\n", i)
    line = buf[i:j] if j >= 0 else buf[i:]
    if j < 0 and line.endswith(b"\r"):
        line = line[:-1]
    if b"\r" in line or b"\n" in line or len(line) > _CHUNK_LINE_LIMIT:
        raise RuntimeError("generation backend malformed chunked framing")
    return j if j >= 0 else None


def _chunked_progress(buf):
    """Strict incremental chunked body (RFC 9112 7.1): None while incomplete, the
    body once the zero chunk and the trailer's closing empty line are present.
    Raises as soon as the buffered bytes cannot be valid framing, including any
    data after the terminator; never waits on bytes that can only be wrong."""
    sizes, i = [], 0
    while True:
        j = _chunk_line(buf, i)
        size_text = (buf[i:] if j is None else buf[i:j]).split(b";", 1)
        if j is None:
            # A size still arriving must already be a hex prefix; once ';' has
            # arrived the size itself is complete and must be valid.
            partial = size_text[0].rstrip(b"\r")
            if (partial or len(size_text) > 1) and not _CHUNK_SIZE_RE.fullmatch(partial):
                raise RuntimeError("generation backend malformed chunk size")
            return None
        if not _CHUNK_SIZE_RE.fullmatch(size_text[0]):
            raise RuntimeError("generation backend malformed chunk size")
        size = int(size_text[0], 16)
        i = j + 2
        if size == 0:
            break
        delimiter = bytes(buf[i + size:i + size + 2])
        if not b"\r\n".startswith(delimiter):
            raise RuntimeError("generation backend truncated chunked body")
        if len(delimiter) < 2:
            return None
        sizes.append((i, size))
        i += size + 2
    while True:   # trailer fields, then the empty line that ends the message
        j = _chunk_line(buf, i)
        if j is None:
            return None
        if j == i:
            if j + 2 != len(buf):
                raise RuntimeError("generation backend data after chunked body")
            return b"".join(bytes(buf[k:k + n]) for k, n in sizes)
        i = j + 2


def _decode_chunked(raw: bytes) -> bytes:
    """Whole chunked body; incomplete framing raises (the attempt stays held)."""
    body = _chunked_progress(raw)
    if body is None:
        raise RuntimeError("generation backend truncated chunked body")
    return body
