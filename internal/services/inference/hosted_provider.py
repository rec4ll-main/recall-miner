"""Optional Chutes request/response contract; no discovery, retries or logging."""
from dataclasses import dataclass, field
import math
import os
import re
import stat


class HostedProviderError(RuntimeError):
    """Safe to return without exposing credentials, prompts or provider bodies."""


SECRET_MAX_BYTES = 16384
_SECRET_OPEN_FLAGS = os.O_RDONLY | os.O_NONBLOCK | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOCTTY", 0)


def read_secret_file(path):
    """Read an operator-provisioned credential file; callers mask every error.

    Only a regular file of at most SECRET_MAX_BYTES that no group/other can
    write. O_NONBLOCK keeps a FIFO from blocking startup; fstat on the opened
    descriptor (not the path) decides. A symlink is followed only when the
    runtime UID cannot write the directory holding the link. A read-only mount
    stays an operator obligation, reported by secret_file_facts().
    """
    if os.path.islink(path) and os.access(os.path.dirname(os.path.abspath(path)), os.W_OK):
        raise ValueError("credential symlink in a runtime-writable directory")
    fd = os.open(path, _SECRET_OPEN_FLAGS)
    try:
        st = os.fstat(fd)
        if not stat.S_ISREG(st.st_mode):
            raise ValueError("credential is not a regular file")
        if st.st_mode & (stat.S_IWGRP | stat.S_IWOTH):
            raise ValueError("credential writable by group/other")
        if st.st_size > SECRET_MAX_BYTES:
            raise ValueError("oversize")
        raw = os.read(fd, SECRET_MAX_BYTES + 1)
    finally:
        os.close(fd)
    if len(raw) > SECRET_MAX_BYTES:
        raise ValueError("oversize")
    return raw.decode("utf-8").rstrip("\r\n")


def secret_file_facts(path):
    """Non-secret facts for an operator preflight; never reads the content."""
    fd = os.open(path, _SECRET_OPEN_FLAGS)
    try:
        st = os.fstat(fd)
        read_only_mount = bool(os.fstatvfs(fd).f_flag & os.ST_RDONLY)
    finally:
        os.close(fd)
    return {"regular": stat.S_ISREG(st.st_mode), "mode": oct(stat.S_IMODE(st.st_mode)),
            "uid": st.st_uid, "gid": st.st_gid, "runtime_uid": os.geteuid(),
            "read_only_mount": read_only_mount, "symlink": os.path.islink(path),
            "size_ok": st.st_size <= SECRET_MAX_BYTES}


@dataclass(frozen=True)
class ChutesConfig:
    enabled: bool = False
    model: str = ""
    api_key: str = field(default="", repr=False)
    schema_mode: str = "none"
    base_url: str = "https://llm.chutes.ai/v1"

    @classmethod
    def from_env(cls, env):
        enabled=env.get("GEN_CHUTES_ENABLED", "").lower() == "true"
        key=env.get("CHUTES_API_KEY", "")
        if enabled and env.get("CHUTES_API_KEY_FILE"):
            if key:raise HostedProviderError("choose one Chutes credential source")
            try:
                key=read_secret_file(env['CHUTES_API_KEY_FILE'])
            except Exception:
                raise HostedProviderError("Chutes credential file unavailable") from None
        return cls(enabled,
                   env.get("CHUTES_MODEL", "Qwen/Qwen3.8-27B-TEE"), key,
                   env.get("CHUTES_SCHEMA_MODE", "none"))

    def request(self, messages, max_tokens, temperature, *, response_schema=None,
                require_complete=False):
        if not self.enabled:
            raise HostedProviderError("hosted provider disabled")
        # Concrete model only: routing aliases/pools would change the evaluated model.
        if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", self.model):
            raise HostedProviderError("explicit concrete hosted model ID required")
        if not self.api_key or any(c.isspace() for c in self.api_key):
            raise HostedProviderError("hosted credential missing or invalid")
        if self.base_url != "https://llm.chutes.ai/v1":
            raise HostedProviderError("unapproved hosted endpoint")
        if self.schema_mode not in ("none", "json_schema"):
            raise HostedProviderError("unsupported hosted schema capability")
        if require_complete:
            # Complete/schema semantics are provider-neutral; alternate hosted
            # profile verification/admission is not wired into the controller.
            raise HostedProviderError("controlled hosted capability not verified")
        if response_schema is not None and self.schema_mode != "json_schema":
            raise HostedProviderError("hosted JSON schema capability not configured")
        if not isinstance(messages, list) or not messages:
            raise HostedProviderError("invalid hosted messages")
        normalized = []
        for message in messages:
            if (not isinstance(message, dict) or message.get("role") not in ("system", "user", "assistant")
                    or not isinstance(message.get("content"), str)):
                raise HostedProviderError("invalid hosted message")
            normalized.append({"role": message["role"], "content": message["content"]})
        if max_tokens is not None and (isinstance(max_tokens, bool) or not isinstance(max_tokens, int) or max_tokens <= 0):
            raise HostedProviderError("invalid requested token budget")
        temp = 0.0 if temperature is None else temperature
        if isinstance(temp, bool) or not isinstance(temp, (int, float)) or not math.isfinite(temp) or not 0 <= temp <= 2:
            raise HostedProviderError("invalid hosted temperature")
        payload = {"model": self.model, "messages": normalized,
                   "temperature": temp, "stream": False}
        if max_tokens is not None:
            payload["max_tokens"] = max_tokens
        if response_schema is not None:
            if not isinstance(response_schema, dict) or not (response_schema.get("type") == "object" or isinstance(response_schema.get("anyOf"),list)):
                raise HostedProviderError("invalid requested JSON schema")
            payload["response_format"] = {"type": "json_schema", "json_schema": {
                "name": "recall_response", "strict": True, "schema": response_schema}}
        return payload, {"Authorization": "Bearer " + self.api_key}


# Finish reasons accepted as proof that generation ended: "stop" is the pinned
# contract's completion; "length" is the observed Chutes token-limit terminal.
# Any other or unknown value is not terminal proof and the attempt stays held.
TERMINAL_FINISH_REASONS = frozenset(("stop", "length"))


def provider_terminal_usage(out):
    """(prompt, completion) tokens when the envelope shows a finished generation, else None.

    Every choice has a recognized terminal finish_reason and usage is reported,
    so the provider's work for this request ended. Says nothing about
    acceptance, model or cost.
    """
    if not isinstance(out, dict):
        return None
    choices, usage = out.get("choices"), out.get("usage")
    if not isinstance(choices, list) or not choices or not all(
            isinstance(c, dict) and c.get("finish_reason") in TERMINAL_FINISH_REASONS for c in choices):
        return None
    if not isinstance(usage, dict):
        return None
    counts = (usage.get("prompt_tokens"), usage.get("completion_tokens"))
    if any(isinstance(n, bool) or not isinstance(n, int) or n < 0 for n in counts):
        return None
    return counts


def completed_hosted_text(out, expected_model):
    if not isinstance(out, dict) or out.get("model") != expected_model:
        raise HostedProviderError("hosted model identity missing or mismatched")
    choices = out.get("choices")
    if not isinstance(choices, list) or len(choices) != 1 or not isinstance(choices[0], dict):
        raise HostedProviderError("invalid hosted choices")
    choice = choices[0]
    message = choice.get("message")
    if choice.get("finish_reason") != "stop" or not isinstance(message, dict):
        raise HostedProviderError("hosted generation incomplete")
    if message.get("tool_calls") or message.get("refusal"):
        raise HostedProviderError("unsupported hosted completion")
    text = message.get("content")
    # Never substitute reasoning_content for the final answer; embedded traces
    # are rejected, not heuristically stripped into an apparently valid answer.
    if not isinstance(text, str) or not text.strip() or re.search(r"</?(?:think|analysis)\b", text, re.I):
        raise HostedProviderError("hosted final content missing or invalid")
    usage = out.get("usage")
    if not isinstance(usage, dict):
        raise HostedProviderError("hosted usage missing")
    counts = [usage.get(k) for k in ("prompt_tokens", "completion_tokens")]
    if any(isinstance(n, bool) or not isinstance(n, int) or n < 0 for n in counts):
        raise HostedProviderError("invalid hosted usage")
    return text.strip(), *counts
