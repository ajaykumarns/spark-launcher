"""Read real device state; keep private manager credentials on Spark 1."""
from __future__ import annotations
import copy
import json
import shlex
import socket
import subprocess
import time
from pathlib import Path
from urllib.request import Request, urlopen
from .catalog import SparkError, request_json
from .control import read_env, safe_host
from .resources import selected, model_units

GIB = 1024 ** 3


def command(args, timeout=30, check=True, input=None):
    result = subprocess.run(args, capture_output=True, text=True, timeout=timeout, input=input)
    if check and result.returncode:
        raise SparkError(f"Device operation failed: {args[0]} (exit {result.returncode}).")
    return result


def probe(node):
    mem = {}
    for line in Path('/proc/meminfo').read_text().splitlines():
        key, _, rest = line.partition(':')
        if key in ('MemTotal', 'MemAvailable', 'SwapFree'):
            mem[key] = int(rest.split()[0]) * 1024
    units = {}
    names = sorted({u for values in node.get('units', {}).values() for u in (values if isinstance(values,list) else [values])})
    if node.get('comfy_unit'):names=sorted(set(names)|{node['comfy_unit']})
    if names:
        raw = command(['systemctl', 'show', *names, '-p', 'Id', '-p', 'ActiveState', '-p', 'SubState'], check=False).stdout
        for block in raw.strip().split('\n\n'):
            fields = dict(line.split('=', 1) for line in block.splitlines() if '=' in line)
            if fields.get('Id'):
                units[fields['Id']] = fields.get('ActiveState')
    containers = {}
    ids=command(['sudo','-n','docker','ps','-aq']).stdout.split()
    if ids:
        template='{"name":{{json .Name}},"running":{{.State.Running}},"id":{{json .Id}},"started_at":{{json .State.StartedAt}},"oom_killed":{{.State.OOMKilled}}}'
        result=command(['sudo','-n','docker','inspect','--format',template,*ids])
        try:
            for line in result.stdout.splitlines():
                item=json.loads(line);name=item.pop('name').lstrip('/')
                containers[name]=item
        except (ValueError,KeyError,AttributeError):
            raise SparkError('Could not read the container state. No device action was taken.') from None
    assets = []
    for root in node.get('asset_roots', []):
        path = Path(root)
        if not path.exists():continue
        if 'ComfyUI' in root:
            for file in path.rglob('*'):
                if file.is_file() and file.suffix in ('.safetensors', '.gguf', '.ckpt', '.pt', '.pth'):
                    assets.append({'root': root, 'path': str(file.relative_to(path)), 'bytes': file.stat().st_size})
        else:
            assets.extend({'root': root, 'path': p.name, 'directory': p.is_dir()} for p in path.iterdir())
    comfy = {'reachable': False, 'running': 0, 'pending': 0, 'guarded': False}
    if node.get('comfy_url'):
        try:
            queue = request_json(node['comfy_url'] + '/queue', timeout=3)
            comfy.update(reachable=True, running=len(queue.get('queue_running', [])), pending=len(queue.get('queue_pending', [])),
                         prompt_ids=[str(row[1]) for row in queue.get('queue_running', []) if len(row)>1])
            try:
                state = request_json(node['comfy_url'] + '/sparky/state', timeout=3)
                comfy.update(state, guarded=True)
            except SparkError:
                pass
        except (SparkError, ValueError):
            pass
    return {'hostname': socket.gethostname(), 'memory_bytes': mem, 'units': units,
            'containers': containers, 'assets': assets, 'comfyui': comfy}


class Runtime:
    def __init__(self, config, config_path):
        self.config = config
        self.path = Path(config_path)
        self.root = Path(config['root'])
        self._cache = None
        self._cache_at = 0

    def key(self):
        key = read_env(self.root/'vllm/api.env').get('VLLM_API_KEY')
        if not key:raise SparkError('Private manager credential is missing.')
        return key

    def manager(self, path, method='GET', expect_json=True):
        return request_json(self.config['manager_url'] + path, method=method,
                            headers={'Authorization':'Bearer '+self.key()}, timeout=7200, expect_json=expect_json)

    def native_activity(self):
        request = Request(self.config['manager_url']+'/api/events', headers={'Authorization':'Bearer '+self.key()})
        counts = {}
        # Initial SSE snapshot includes model-dispatched requests, including
        # old requests admitted before this controller was installed.
        with urlopen(request, timeout=8) as response:
            for _ in range(200):
                line = response.readline(4*1024*1024)
                if not line:break
                if not line.startswith(b'data:'):continue
                envelope = json.loads(line[5:])
                if envelope.get('type') != 'inflight':continue
                data = envelope.get('data')
                data = json.loads(data) if isinstance(data, str) else data
                if data.get('operation') != 'snapshot':continue
                for entry in data.get('requests', []):
                    name=entry.get('model') or entry.get('model_id')
                    if name:counts[name] = counts.get(name, 0)+1
                return counts
        raise SparkError('Cannot confirm active manager requests. No model was stopped.')

    def inspect_node(self, name):
        node = copy.deepcopy(self.config['nodes'][name])
        # Service ownership follows the model definitions. Do not trust a
        # second service list or a worker projection from an earlier install.
        node['units']={model_name:model_units(model,name)
                       for model_name,model in self.config['models'].items() if model['kind']=='llm'}
        if name == 'spark01':return probe(node)
        result = command(['ssh','-o','BatchMode=yes','-o','ConnectTimeout=8',safe_host(node['ssh']),
                          '~/.local/bin/sparky node probe --file -'], timeout=35,input=json.dumps(node))
        return json.loads(result.stdout)

    def snapshot(self, fresh=False):
        if not fresh and self._cache is not None and time.monotonic()-self._cache_at < 2:
            return copy.deepcopy(self._cache)
        from concurrent.futures import ThreadPoolExecutor
        with ThreadPoolExecutor(max_workers=2) as pool:
            nodes = dict(zip(self.config['nodes'],pool.map(self.inspect_node,self.config['nodes'])))
        running = self.manager('/running').get('running', [])
        states = {r['model']:r['state'] for r in running}
        models={}
        for name, model in self.config['models'].items():
            loaded_nodes=[]
            if model['kind']=='llm':
                for n, data in nodes.items():
                    values=model_units(model,n)
                    if any(data['units'].get(u) in ('active','activating') for u in values):loaded_nodes.append(n)
                loaded=bool(loaded_nodes)
            elif model['kind']=='audio':
                setting=model['settings'][model['run_on']]
                places=(model['run_on'],)
                loaded=all(nodes[n]['containers'].get(c,{}).get('running') for n in places for c in setting.get('containers',[]))
                loaded_nodes=list(places) if loaded else []
            elif model['kind']=='comfyui':
                loaded_nodes=[n for n,d in nodes.items() if d['comfyui']['reachable']]
                loaded=bool(loaded_nodes)
            else:
                loaded=False
            models[name]={'kind':model['kind'],'loaded':loaded,'nodes':loaded_nodes,
                          'desired':model['run_on'],'state':states.get(name,'running' if loaded else 'stopped'),
                          'keep_loaded':bool(model.get('keep_loaded')),'active_requests':0}
        result={'nodes':nodes,'models':models,'comfyui':{n:d['comfyui'] for n,d in nodes.items()}}
        self._cache=copy.deepcopy(result);self._cache_at=time.monotonic()
        return result

    def clear_comfy(self,node):
        url=self.config['nodes'][node]['comfy_url']
        state=request_json(url+'/sparky/state',timeout=5)
        if state.get('phase')=='executing':raise SparkError('ComfyUI is executing a job. Cache was kept.')
        # The job guard serializes cache release with entry into GPU execution.
        request_json(url+'/sparky/free',payload={},timeout=60)

    def unit(self,node,action,unit):
        args=['sudo','-n','systemctl',action,unit]
        if node=='spark02':args=['ssh','-o','BatchMode=yes',safe_host(self.config['nodes'][node]['ssh']),shlex.join(args)]
        command(args,timeout=180)

    def ensure_audio(self,name):
        _,_,setting,nodes=selected(self.config,name)
        for node in nodes:
            # docker start preserves IDs and does nothing destructive to an
            # already-running container. Never restart or stop protected audio.
            args=['sudo','-n','docker','start',*setting['containers']]
            if node=='spark02':args=['ssh','-o','BatchMode=yes',safe_host(self.config['nodes'][node]['ssh']),shlex.join(args)]
            command(args,timeout=60)

    def event(self,action,**fields):
        import sqlite3
        path=self.root/'controller/events.sqlite3'
        with sqlite3.connect(path,timeout=15) as db:
            db.execute('PRAGMA journal_mode=WAL')
            db.execute('CREATE TABLE IF NOT EXISTS decisions (id INTEGER PRIMARY KEY, time TEXT DEFAULT CURRENT_TIMESTAMP, action TEXT, detail TEXT)')
            db.execute('INSERT INTO decisions(action,detail) VALUES(?,?)',(action,json.dumps(fields)))
