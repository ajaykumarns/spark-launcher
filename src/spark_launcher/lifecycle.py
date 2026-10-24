"""Config-driven service lifecycle called by the existing llama-swap manager."""
from __future__ import annotations
import fcntl
import json
import signal
import time
from pathlib import Path
from .catalog import SparkError, atomic_write
from .resources import read_cluster, selected
from .resource_runtime import Runtime, command


class StopRequested(BaseException):
    """Stop a partially started model without leaving a rank behind."""


def stop_units(runtime, info):
    failures=[]
    for node in info['nodes']:
        try:runtime.unit(node,'stop',info['units'][node])
        except Exception:failures.append(node)
    if failures:raise SparkError('Could not confirm model shutdown on: '+', '.join(failures))


def run_model(config_path, action, name):
    config = read_cluster(config_path)
    runtime = Runtime(config, config_path)
    state = Path(config['root'])/'manager'
    if name not in config['models']:raise SparkError('Unknown lifecycle model.')
    model=config['models'][name]
    if model['kind'] != 'llm':raise SparkError('This lifecycle entry is for language models.')
    actual_path=Path(config['root'])/'controller'/('running-'+name+'.json')
    def actual():
        try:return json.loads(actual_path.read_text())
        except FileNotFoundError:
            _,_,setting,nodes=selected(config,name)
            return {'nodes':list(nodes),'units':setting['units']}
    def stop():
        # Always stop actual placement. The desired config may already have changed.
        info=actual()
        (state/('stopping.'+name)).touch()
        stop_units(runtime,info)
    if action=='stop':
        # Native FIFO only calls this after request drain. External explicit
        # unloads go through the controller, which also drains and fences calls.
        stop();return
    if action!='run':raise SparkError('Use run or stop.')
    model,placement,setting,nodes=selected(config,name)
    locks=[]
    started_here=False
    starting=True
    finished=False
    def finish(*_):
        nonlocal finished
        finished=True
        if starting:raise StopRequested()
    previous={sig:signal.signal(sig,finish) for sig in (signal.SIGTERM,signal.SIGINT)}
    try:
        for node in sorted(nodes):
            file=(state/('owner.'+node+'.lock')).open('a')
            locks.append(file)
            fcntl.flock(file,fcntl.LOCK_EX)
        operation=(state/'operation.lock').open('a')
        with operation:
            fcntl.flock(operation,fcntl.LOCK_EX)
            # Never rely on desired placement to decide whether an old process
            # can share a node. Inspect the actual units before starting.
            snapshot=runtime.snapshot(fresh=True)
            existing=snapshot['models'].get(name,{})
            if existing.get('loaded'):
                raise SparkError('The requested model still has a running service. It was not stopped automatically.')
            for other,row in snapshot['models'].items():
                if other!=name and row['kind']=='llm' and row['loaded'] and set(nodes)&set(row['nodes']):
                    raise SparkError(f'{other} still occupies a required device.')
            for node in nodes:
                comfy=snapshot['comfyui'][node]
                if comfy.get('executing') or (comfy['running'] and comfy.get('phase')!='waiting'):
                    raise SparkError('ComfyUI still has active GPU work. No job was stopped.')
                runtime.clear_comfy(node)
            # Re-check actual available host memory after releasing idle caches.
            snapshot=runtime.snapshot(fresh=True)
            for node,minimum in setting.get('minimum_available_gib',{}).items():
                available=snapshot['nodes'][node]['memory_bytes']['MemAvailable']/1024**3
                if available < minimum:
                    raise SparkError(f'{node} has {available:.1f} GiB available; this recipe needs {minimum} GiB at startup. Parakeet was kept loaded.')
            # Compile selected numeric values from Spark 1 into per-node
            # recipe files. These files are outputs, not independent settings.
            for node in nodes:
                spec={'parameters':{'context_tokens':setting['context_tokens'],**setting.get('parameters',{})},'bindings':setting.get('parameter_bindings',{}).get(node,[])}
                if not spec['parameters']:continue
                if node=='spark01':
                    from .parameters import render_parameters
                    render_parameters(spec)
                else:
                    import subprocess,shlex
                    payload=json.dumps(spec)
                    result=subprocess.run(['ssh',config['nodes'][node]['ssh'],'~/.local/bin/sparky node render --payload -'],input=payload,text=True,capture_output=True,timeout=30)
                    if result.returncode:raise SparkError('Worker recipe rendering failed. No model was started.')
            atomic_write(actual_path,json.dumps({'nodes':list(nodes),'units':setting['units'],'placement':placement})+'\n')
            (state/('stopping.'+name)).unlink(missing_ok=True)
            started_here=True
            for node in setting.get('startup_order',list(reversed(nodes))):
                runtime.unit(node,'start',setting['units'][node])
                rendezvous=setting.get('rendezvous',{})
                if rendezvous.get('node')==node:
                    deadline=time.monotonic()+100
                    args=['ss','-H','-ltn','sport = :'+str(rendezvous['port'])]
                    if node=='spark02':
                        import shlex
                        args=['ssh',config['nodes'][node]['ssh'],shlex.join(args)]
                    while time.monotonic()<deadline:
                        if command(args,check=False).stdout.strip():break
                        time.sleep(1)
                    else:raise SparkError('The recipe rendezvous did not start.')
            fcntl.flock(operation,fcntl.LOCK_UN)
        starting=False
        while not finished and not (state/('stopping.'+name)).exists():
            for node in nodes:
                args=['systemctl','is-active','--quiet',setting['units'][node]]
                if node=='spark02':
                    import shlex
                    args=['ssh',config['nodes'][node]['ssh'],shlex.join(args)]
                if command(args,check=False).returncode:
                    raise SparkError('A model node stopped. The remaining nodes will be released.')
            time.sleep(3)
    except StopRequested:
        pass
    finally:
        # Signals are handled before any rank starts. Try every rank even if
        # one node cannot be reached, then release all ownership locks.
        starting=False
        try:
            if started_here:stop()
        finally:
            for file in reversed(locks):file.close()
            for sig,handler in previous.items():signal.signal(sig,handler)
