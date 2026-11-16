"""Provider-neutral profile comparison. Pure check, never enables a runtime lane.

Verification references must come from trusted controller configuration, not a
miner/provider claim. Matching metadata is necessary, not capability evidence.
"""
from dataclasses import dataclass


@dataclass(frozen=True)
class StructuredCapability:
    provider: str
    model: str
    schema_sha256: str
    wire_mode: str
    verification_reference: str
    complete_envelope: bool


def negotiate(capability, *, provider, model, schema_sha256):
    if not isinstance(capability, StructuredCapability):
        raise ValueError('verified structured capability missing')
    if (capability.provider != provider or capability.model != model
            or capability.schema_sha256 != schema_sha256
            or not capability.verification_reference or not capability.complete_envelope):
        raise ValueError('structured capability binding mismatch')
    if capability.wire_mode not in ('openai_json_schema', 'vllm_xgrammar_json_schema'):
        raise ValueError('structured capability cannot satisfy exact schema')
    return capability.wire_mode
