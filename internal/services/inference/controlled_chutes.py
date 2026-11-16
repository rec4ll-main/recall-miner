"""Exact public contract; hosted controlled runtime remains unconfigured/off."""
import hashlib
import json
from pathlib import Path
from computed_probe import CONTRACT,messages_for_probe
SCHEMA_SHA='a759a08db44301a716d258f5b34509b2751a412a933e494aee8c9fb05e8e7852'

def prepare_controlled(payload,model,profile):
    # Populated only by joined offline fixtures currently. Never infer capability
    # from response success or accept a miner request's profile declaration.
    if profile is None or profile != ('chutes-json-schema-v1',model,SCHEMA_SHA):
        raise ValueError('controlled profile not admitted')
    raw=Path(__file__).with_name('probe_schema.json').read_bytes()
    if hashlib.sha256(raw).hexdigest()!=SCHEMA_SHA:raise ValueError('schema pin mismatch')
    return messages_for_probe(payload),json.loads(raw)

def validate_typed(answer):
    # Exact structural constraints of pinned schema, not its independent oracle.
    if not isinstance(answer,dict) or set(answer)!={'status','value','unit','citations'}:raise ValueError('fields')
    status,value,unit,citations=(answer[k] for k in ('status','value','unit','citations'))
    if not isinstance(citations,list) or any(not isinstance(c,str) for c in citations):raise ValueError('citations')
    array=isinstance(value,list) and len(value)<=2 and all(type(v) is int for v in value)
    if status=='unknown':
        if value is not None or unit is not None:raise ValueError('unknown values')
    elif status in ('answer','conflict'):
        if unit is not None and not isinstance(unit,str):raise ValueError('unit')
        if status=='conflict' and not array:raise ValueError('conflict array')
        if status=='answer' and not (type(value) is int or isinstance(value,str) or array):raise ValueError('answer value')
    else:raise ValueError('status')
