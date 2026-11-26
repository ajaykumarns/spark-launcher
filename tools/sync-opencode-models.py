#!/usr/bin/env python3
"""Update owned OpenCode providers from Sparky's public catalog."""
import argparse
import json
import shutil
from datetime import datetime, timezone
from pathlib import Path

p = argparse.ArgumentParser(description=__doc__)
p.add_argument('--catalog', required=True)
p.add_argument('--config', required=True)
p.add_argument('--default-model')
p.add_argument('--gateway', required=True)
args = p.parse_args()
catalog = json.load(open(args.catalog))
path = Path(args.config)
config = json.loads(path.read_text())
providers = config.setdefault('provider', {})
for key in ['garuda', 'spark']:
    if key not in providers:
        continue
    provider = providers[key]
    old = provider.get('models', {})
    models = {}
    for row in catalog['data']:
        if row.get('api_kind') == 'decision' or not row['id'].startswith('garuda/'):
            continue
        model_key = row['id'].removeprefix('garuda/')
        legacy = row.get('legacy_ids', [])
        # This old local name refers to the same Qwen 27B model.
        if model_key == 'qwen3.8-27b-nvfp4-dflash2' and 'qwen3.8-27b' in old:
            legacy = [*legacy, 'qwen3.8-27b']
        previous = old.get(model_key, next((old[x] for x in legacy if x in old), {}))
        model = dict(previous)
        model['id'] = row.get('request_model', row['id'])
        model['name'] = row['name']
        model['limit'] = {'context': row['context_length'], 'output': row['output_tokens']}
        model['toolcall'] = True
        model['reasoning'] = row.get('reasoning', False)
        model['modalities'] = {'input': row.get('inputs', ['text']), 'output': ['text']}
        models[model_key] = model
        for alias in legacy:
            models[alias] = dict(model)
    provider['options']['baseURL'] = args.gateway.rstrip('/') + '/v1'
    provider['models'] = models
if args.default_model:
    config['model'] = args.default_model
    config['small_model'] = args.default_model
backup = path.with_name(path.name + '.before-stable-ids-' + datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ'))
shutil.copy2(path, backup)
path.write_text(json.dumps(config, indent=2) + '\n')
print('Updated', path, 'Backup:', backup)
