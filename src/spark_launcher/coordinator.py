"""Shared admission and action planning for language and ComfyUI jobs."""
from __future__ import annotations
import copy
import hashlib
import json
import threading
import time
import uuid
from pathlib import Path
from .catalog import SparkError, atomic_write
from .resources import read_cluster, write_cluster, resolve_workload, selected, matrix_sets, make_plan
from .resource_runtime import Runtime, command


class Coordinator:
    def __init__(self,path,runtime=None):
        self.path=Path(path)
        self.config=read_cluster(path)
        self.runtime=runtime or Runtime(self.config,path)
        self.cv=threading.Condition(threading.RLock())
        self.transition=threading.RLock()
        self.reconfigure=threading.RLock()
        self.leases={}
        self.pending=[]
        self.fenced=False
        self.last_error=None

    def refresh(self):
        config=read_cluster(self.path)
        with self.cv:
            self.config=config;self.runtime.config=config
        return config

    @staticmethod
    def overlap(a,b):
        return bool(set(a['nodes'])&set(b['nodes'])) and (a.get('exclusive') or b.get('exclusive') or a['name']!=b['name'])

    def acquire(self,name,nodes,*,exclusive=False,request_id=None,wait=True):
        request_id=request_id or uuid.uuid4().hex
        with self.cv:
            for token,lease in self.leases.items():
                if lease['request_id']==request_id:return token
            entry=next((p for p in self.pending if p['request_id']==request_id),None)
            if entry is None:
                entry={'name':name,'nodes':list(nodes),'exclusive':exclusive,'request_id':request_id}
                self.pending.append(entry)
            try:
                while True:
                    existing=next((t for t,l in self.leases.items() if l['request_id']==request_id),None)
                    if existing:return existing
                    earlier=self.pending[:self.pending.index(entry)]
                    blocked=self.fenced or any(self.overlap(entry,l) for l in self.leases.values()) or any(self.overlap(entry,p) for p in earlier)
                    if not blocked:
                        token=uuid.uuid4().hex
                        self.runtime.event('admitted',workload=name,nodes=list(nodes),request_id=request_id)
                        self.leases[token]={**entry,'started_at':time.time()}
                        self.pending.remove(entry)
                        return token
                    if not wait:return None
                    self.cv.wait(0.5)
            except BaseException:
                if entry in self.pending:self.pending.remove(entry)
                self.cv.notify_all()
                raise

    def reconcile_jobs(self):
        from .catalog import request_json
        for node,meta in self.config['nodes'].items():
            if not meta.get('comfy_url'):continue
            try:state=request_json(meta['comfy_url']+'/sparky/state',timeout=3)
            except (SparkError,OSError):continue  # Unknown means keep reservations.
            prompt_id=state.get('prompt_id')
            current=node+':'+str(prompt_id) if prompt_id is not None else None
            with self.cv:
                for token,lease in list(self.leases.items()):
                    if lease['name'] in ('comfyui@'+node,'cpu-probe@'+node) and lease['request_id']!=current:
                        self.leases.pop(token,None)
                self.pending[:]=[p for p in self.pending if not(p['name'] in ('comfyui@'+node,'cpu-probe@'+node) and p['request_id']!=current)]
                if state.get('executing') and not any(l['request_id']==current for l in self.leases.values()):
                    self.leases['adopted-'+node]={'name':'comfyui@'+node,'nodes':[node],'exclusive':True,'request_id':current,'started_at':time.time()}
                self.cv.notify_all()

    def release(self,token):
        with self.cv:
            lease=self.leases.pop(token,None)
            self.cv.notify_all()
        if lease:
            try:self.runtime.event('released',workload=lease['name'],request_id=lease['request_id'])
            except OSError:pass

    def snapshot(self,fresh=False):
        data=self.runtime.snapshot(fresh)
        try:
            native=self.runtime.native_activity()
            data['activity_known']=True
        except (SparkError,OSError):
            native={};data['activity_known']=False
        with self.cv:
            for name,state in data['models'].items():
                state['active_requests']=max(sum(l['name']==name for l in self.leases.values()),native.get(name,0))
            data['admission']={'active':[{'workload':l['name'],'nodes':l['nodes'],'started_at':l['started_at']} for l in self.leases.values()],
                               'waiting':[{'workload':p['name'],'nodes':p['nodes']} for p in self.pending],
                               'config_transition':self.fenced}
        data['config_path']=str(self.path)
        return data

    def plan(self,requested,action='load',on=None):
        self.refresh()
        name,_=resolve_workload(self.config,requested)
        return make_plan(self.config,name,self.snapshot(True),action,on)

    def wait_native_idle(self,names=None):
        # Native in-flight tracking covers requests that predate installation
        # and private administrator requests that did not use this proxy.
        while True:
            busy=self.runtime.native_activity()
            if not any(count and (names is None or name in names) for name,count in busy.items()):return
            time.sleep(0.5)

    @staticmethod
    def routing_combinations(routing):
        sets=routing.get('router',{}).get('settings',{}).get('matrix',{}).get('sets',{})
        return {tuple(sorted(x.strip() for x in alternative.split('&')))
                for value in sets.values() for alternative in value.split('|')}

    def sync_manager(self):
        """Apply placement only when it changes. Never reload on installation."""
        import yaml
        with self.reconfigure:
            self.refresh()
            path=Path(self.config['root'])/'manager/config.yaml'
            before=yaml.safe_load(path.read_text())
            after=copy.deepcopy(before)
            for name,model in self.config['models'].items():
                if model['kind']!='llm':continue
                _,_,setting,_=selected(self.config,name)
                entry=after['models'][name]
                entry['proxy']=setting['backend_url']
                entry['concurrencyLimit']=setting['concurrency']
                entry.setdefault('capabilities',{})['context']=setting['context_tokens']
            routing={'router':{'use':'matrix','settings':{'matrix':{'sets':matrix_sets(self.config)}}}}
            if self.routing_combinations(after.get('routing',{})) != self.routing_combinations(routing):after['routing']=routing
            if before==after:return
            with self.cv:
                self.fenced=True
                while self.leases:self.cv.wait(0.5)
            try:
                self.wait_native_idle()
                # Keep real ComfyUI GPU work running through a manager reload.
                # Only language model lifecycle is changed here.
                backup=Path(self.config['root'])/'controller/backups'/('manager-'+str(time.time_ns())+'.yaml')
                atomic_write(backup,path.read_bytes())
                candidate=Path(self.config['root'])/'controller/manager-candidate.yaml'
                atomic_write(candidate,yaml.safe_dump(after,sort_keys=False))
                result=command([str(Path.home()/'.local/bin/llama-swap'),'--config',str(candidate),'--validate'],check=False)
                if result.returncode:raise SparkError('Generated manager config failed validation. No live config was changed.')
                atomic_write(path,candidate.read_bytes())
                try:
                    command(['sudo','-n','systemctl','kill','--kill-whom=main','--signal=HUP','garuda-models.service'])
                except BaseException:
                    atomic_write(path,backup.read_bytes())
                    raise
                time.sleep(1)
                self.runtime._cache=None
                self.runtime.event('manager_config_applied',backup=str(backup),reason='Selected placement or runtime settings changed.')
            finally:
                with self.cv:self.fenced=False;self.cv.notify_all()

    def resolve_auto(self):
        rows=self.runtime.manager('/running').get('running',[])
        ready={r['model'] for r in rows if r['state']=='ready'}
        targets=self.config['auto']['targets']
        return next((n for n in targets if n in ready),self.config['auto']['default'])

    def prepare_llm(self,name):
        model,placement,setting,nodes=selected(self.config,name)
        with self.transition:
            state=self.runtime.snapshot(True)['models'].get(name,{})
            if state.get('state')=='ready' and set(state.get('nodes',[]))==set(nodes):return
            # The manager's native FIFO drains language requests and applies
            # its config-generated compatibility matrix. The lifecycle hook
            # releases only idle ComfyUI caches before starting units.
            self.runtime.event('load_requested',workload=name,placement=placement)
            self.runtime.manager('/upstream/'+name+'/health',expect_json=False)
            self.runtime._cache=None

    def llm_request(self,requested):
        self.sync_manager()
        name=self.resolve_auto() if requested.removeprefix('garuda/')=='spark-auto' else resolve_workload(self.config,requested)[0]
        model,_,_,nodes=selected(self.config,name)
        if model['kind']!='llm':raise SparkError('Select a language model for this endpoint.')
        self.reconcile_jobs()
        token=self.acquire(name,nodes)
        try:self.prepare_llm(name)
        except BaseException:self.release(token);raise
        return name,token

    def load(self,requested):
        self.refresh()
        name,model=resolve_workload(self.config,requested)
        if model['kind']=='llm':
            _,token=self.llm_request(name)
            self.release(token)
        elif model['kind']=='audio':
            selected(self.config,name)
            self.runtime.ensure_audio(name)
        else:
            _,_,_,nodes=selected(self.config,name)
            for node in nodes:
                state=self.runtime.inspect_node(node)['comfyui']
                if not state['reachable']:self.runtime.unit(node,'start',self.config['nodes'][node]['comfy_unit'])
            return {'status':'ready','workload':name,'note':'ComfyUI is available. Weights load when a workflow runs.'}
        return {'status':'ready','workload':name}

    def unload(self,requested):
        self.refresh()
        name,model=resolve_workload(self.config,requested)
        if model.get('keep_loaded'):raise SparkError(f'{name} must stay loaded.')
        _,_,_,nodes=selected(self.config,name)
        token=self.acquire(name,nodes,exclusive=True)
        try:
            if model['kind']=='llm':
                self.wait_native_idle({name})
                self.runtime.manager('/api/models/unload/'+name,method='POST',expect_json=False)
            elif model['kind'] in ('comfyui','comfy_model'):
                for node in nodes:
                    while True:
                        state=self.runtime.inspect_node(node)['comfyui']
                        if not state.get('executing') and (not state['running'] or state.get('phase')=='waiting'):break
                        time.sleep(1)
                    self.runtime.clear_comfy(node)
            else:raise SparkError('No safe unload adapter is configured for this audio service.')
            self.runtime._cache=None
            self.runtime.event('unloaded',workload=name,reason='Explicit request after active work finished.')
            return {'status':'unloaded','workload':name}
        finally:self.release(token)

    def comfy_acquire(self,node,prompt_id,cpu_only=False):
        if node not in self.config['nodes']:raise SparkError('Unknown device.')
        # Confirm the job exists at the real ComfyUI engine. Never trust a
        # caller's assertion that a GPU workflow is CPU-only.
        from .catalog import request_json
        queue=request_json(self.config['nodes'][node]['comfy_url']+'/queue',timeout=5)
        item=next((row for row in queue.get('queue_running',[]) if str(row[1])==str(prompt_id)),None)
        if item is None:raise SparkError('The ComfyUI job is not running in its engine queue.')
        graph=item[2]
        actual_cpu=bool(graph) and all(v.get('class_type')=='SparkyProbe' for v in graph.values())
        if cpu_only and not actual_cpu:raise SparkError('This workflow requires a GPU reservation.')
        name=('cpu-probe@' if actual_cpu else 'comfyui@')+node
        token=self.acquire(name,[] if actual_cpu else [node],exclusive=True,request_id=node+':'+str(prompt_id),wait=False)
        if token is None:return {'status':'waiting','retry_after':1}
        try:
            if not actual_cpu:
                with self.transition:
                    snapshot=self.runtime.snapshot(True)
                    conflicts=[n for n,s in snapshot['models'].items() if s['kind']=='llm' and s['loaded'] and node in s['nodes']]
                    self.wait_native_idle(set(conflicts))
                    for other in conflicts:
                        self.runtime.manager('/api/models/unload/'+other,method='POST',expect_json=False)
                        self.runtime.event('unloaded',workload=other,reason='ComfyUI workflow needs the device.',node=node)
                    # Audio remains loaded; re-check free memory after LLMs stop.
                    self.runtime._cache=None
            return {'token':token,'status':'admitted','node':node,'cpu_only':actual_cpu}
        except BaseException:self.release(token);raise

    def set_placement(self,requested,on):
        with self.reconfigure:
            self.refresh()
            name,_=resolve_workload(self.config,requested)
            selected(self.config,name,on)
            updated=copy.deepcopy(self.config);updated['models'][name]['run_on']=on
            write_cluster(self.path,updated)
            self.refresh()
            self.runtime.event('placement_changed',workload=name,placement=on)
            return {'workload':name,'run_on':on,'takes_effect':'on the next load request'}

    def catalog(self):
        self.refresh()
        data=[]
        for name,model in self.config['models'].items():
            if model['kind']!='llm':continue
            _,on,setting,nodes=selected(self.config,name)
            data.append({'id':'garuda/'+name,'name':model['name'],'context_length':setting['context_tokens'],
                         'output_tokens':model.get('output_tokens',16384),'aliases':model.get('aliases',[]),
                         'inputs':model.get('inputs',['text']),'reasoning':model.get('reasoning',False),'nodes':list(nodes)})
        data.append({'id':'garuda/spark-auto','name':'Spark Auto','context_length':min(x['context_length'] for x in data),
                     'aliases':['auto'],'output_tokens':16384,'inputs':['text']})
        return {'data':data,'authority':'spark01','config_version':self.config['version']}
