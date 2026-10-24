"""Config-driven service lifecycle called by the existing llama-swap manager."""
from __future__ import annotations
import fcntl
import json
import os
import signal
import time
from pathlib import Path
from .catalog import SparkError, atomic_write
from .resources import read_cluster, selected
from .resource_runtime import Runtime, command


def run_model(config_path, action, name):
    config = read_cluster(config_path)
    runtime = Runtime(config, config_path)
    state = Path(config['root'])/'manager'
    model, placement, setting, nodes = selected(config, name)
    if model['kind'] != 'llm':raise SparkError('This lifecycle entry is for language models.')
    actual_path=Path(config['root'])/'controller'/('running-'+name+'.json')
    def actual():
        try:return json.loads(actual_path.read_text())
        except FileNotFoundError:return {'nodes':list(nodes),'units':setting['units']}
    def stop():
        # Always stop actual placement. The desired config may already have changed.
        info=actual()
        (state/('stopping.'+name)).touch()
        for node in info['nodes']:
            runtime.unit(node,'stop',info['units'][node])
    if action=='stop':
        # Native FIFO only calls this after request drain. External explicit
        # unloads go through the controller, which also drains and fences calls.
        stop();return
    if action!='run':raise SparkError('Use run or stop.')
    locks=[]
    started_here=False
    try:
        for node in sorted(nodes):
            file=(state/('owner.'+node+'.lock')).open('a')
            fcntl.flock(file,fcntl.LOCK_EX)
            locks.append(file)
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
                    port=':'+str(rendezvous['port'])
                    while time.monotonic()<deadline:
                        if port in command(['ss','-ltn'],check=False).stdout:break
                        time.sleep(1)
                    else:raise SparkError('The recipe rendezvous did not start.')
            fcntl.flock(operation,fcntl.LOCK_UN)
        finished=False
        def finish(*_):
            nonlocal finished
            finished=True
        signal.signal(signal.SIGTERM,finish);signal.signal(signal.SIGINT,finish)
        while not finished and not (state/('stopping.'+name)).exists():
            for node in nodes:
                args=['systemctl','is-active','--quiet',setting['units'][node]]
                if node=='spark02':
                    import shlex
                    args=['ssh',config['nodes'][node]['ssh'],shlex.join(args)]
                if command(args,check=False).returncode:
                    raise SparkError('A model node stopped. The remaining nodes will be released.')
            time.sleep(3)
        stop()
    except BaseException:
        # A failed startup may have started one node. Clean up only this model.
        # Never restore old desired config or stop protected audio.
        if started_here:stop()
        raise
    finally:
        for file in reversed(locks):file.close()
