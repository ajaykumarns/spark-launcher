"""Shared admission and action planning for language and ComfyUI jobs."""
from __future__ import annotations
import copy
from contextlib import contextmanager
import json
import sqlite3
import threading
import time
import uuid
from pathlib import Path
from .catalog import SparkError, atomic_write
from .resources import read_cluster, write_cluster, resolve_workload, selected, matrix_sets, make_plan, enabled_llms
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
        self.image_gate=threading.BoundedSemaphore(1)
        self.fenced=False
        self.reloading=False
        self.last_error=None
        self.force_lock=threading.Lock()
        self.forcing_nodes=set()
        self.force_owner=None
        self.node_generation={n:0 for n in self.config['nodes']}

    def refresh(self):
        config=read_cluster(self.path)
        with self.cv:
            self.config=config;self.runtime.config=config
        return config

    def shared_comfy(self):
        return not self.config.get('policy',{}).get('comfyui_exclusive_with_llms',True)

    def overlap(self,a,b):
        if not set(a['nodes'])&set(b['nodes']):return False
        if a.get('exclusive') or b.get('exclusive'):return True
        if a['name']==b['name']:return a['name'].startswith('comfyui@')
        if self.shared_comfy():
            allowed=self.config['policy']['comfyui_shared_llms']
            if any(image['name'].startswith('comfyui@') and llm['name'] in allowed
                   for image,llm in ((a,b),(b,a))):return False
        return True

    def acquire(self,name,nodes,*,exclusive=False,request_id=None,wait=True,limit=None):
        request_id=request_id or uuid.uuid4().hex
        with self.cv:
            if self.forcing_nodes.intersection(nodes) and self.force_owner!=threading.get_ident():
                raise SparkError('A forced model operation is using the requested device. Try again after it finishes.')
            for token,lease in self.leases.items():
                if lease['request_id']==request_id:return token
            entry=next((p for p in self.pending if p['request_id']==request_id),None)
            if entry is None:
                entry={'name':name,'nodes':list(nodes),'exclusive':exclusive,'request_id':request_id}
                self.pending.append(entry)
            try:
                while True:
                    if entry.get('cancelled'):raise SparkError('A forced model operation cancelled this waiting request.')
                    existing=next((t for t,l in self.leases.items() if l['request_id']==request_id),None)
                    if existing:return existing
                    earlier=self.pending[:self.pending.index(entry)]
                    full=limit is not None and sum(l['name']==name for l in self.leases.values())>=limit
                    blocked=self.fenced or self.reloading or full or any(self.overlap(entry,l) for l in self.leases.values()) or any(self.overlap(entry,p) for p in earlier)
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
                    self.leases['adopted-'+node]={'name':'comfyui@'+node,'nodes':[node],'exclusive':not self.shared_comfy(),'request_id':current,'started_at':time.time()}
                self.cv.notify_all()

    def release(self,token):
        with self.cv:
            lease=self.leases.pop(token,None)
            self.cv.notify_all()
        if lease:
            try:self.runtime.event('released',workload=lease['name'],request_id=lease['request_id'])
            except (OSError,sqlite3.Error):pass

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
                               'config_transition':self.fenced or self.reloading}
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
            for name in enabled_llms(self.config):
                _,_,setting,_=selected(self.config,name)
                entry=after['models'][name]
                entry['proxy']=setting['backend_url']
                entry['concurrencyLimit']=setting['concurrency']
                entry.setdefault('capabilities',{})['context']=setting['context_tokens']
            # Language model residency changes only through explicit control.
            # Apply per-model overrides too; they otherwise win over globalTTL.
            after['globalTTL']=0
            for name in enabled_llms(self.config):after['models'][name]['ttl']=0
            routing={'router':{'use':'matrix','settings':{'matrix':{'sets':matrix_sets(self.config)}}}}
            if self.routing_combinations(after.get('routing',{})) != self.routing_combinations(routing):after['routing']=routing
            if before==after:return
            try:
                with self.cv:
                    self.reloading=True
                    while self.leases:self.cv.wait(0.5)
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
                with self.cv:self.reloading=False;self.cv.notify_all()

    def resolve_auto(self):
        rows=self.runtime.manager('/running').get('running',[])
        ready={r['model'] for r in rows if r['state']=='ready'}
        if 'auto' not in self.config:raise SparkError('Automatic model routing is not configured.')
        targets=self.config['auto']['targets']
        name=next((n for n in targets if n in ready),None)
        if name is None:raise SparkError('No automatic routing target is ready. Ask the human to switch the model. Do not load or unload models automatically.')
        return name

    def ready_placement(self,name,setting,nodes):
        """Use the manager's live state only for the recorded, unchanged placement."""
        path=Path(self.config['root'])/'controller'/('running-'+name+'.json')
        try:
            actual=json.loads(path.read_text())
            if set(actual.get('nodes',[]))!=set(nodes) or actual.get('units')!=setting['units']:return False
            rows=self.runtime.manager('/running').get('running',[])
            return any(isinstance(row,dict) and row.get('model')==name and row.get('state')=='ready' for row in rows)
        except (OSError,ValueError,TypeError,AttributeError,SparkError):
            return False

    def prepare_llm(self,name):
        model,placement,setting,nodes=selected(self.config,name)
        with self.transition:
            # The native manager still owns loading, FIFO drain and compatibility. An already-ready model with
            # the same recorded units needs no SSH, Docker or asset inventory on every inference request.
            if self.ready_placement(name,setting,nodes):return
            state=self.runtime.snapshot(True)['models'].get(name,{})
            if state.get('state')=='ready' and set(state.get('nodes',[]))==set(nodes):return
            snapshot=self.runtime.snapshot()
            for other,row in snapshot['models'].items():
                if other!=name and row.get('loaded') and self.config['models'][other].get('keep_loaded') and row['kind']=='llm' and set(nodes)&set(row['nodes']):
                    raise SparkError(other+' must stay loaded on the requested device.')
            # The manager's native FIFO drains language requests and applies
            # its config-generated compatibility matrix. The lifecycle hook
            # releases only idle ComfyUI caches before starting units.
            self.runtime.event('load_requested',workload=name,placement=placement)
            self.runtime.manager('/upstream/'+name+'/health',expect_json=False)
            self.runtime._cache=None

    def require_ready_llm(self,name,setting,nodes):
        if not self.ready_placement(name,setting,nodes):
            raise SparkError(name+' is not ready at the selected placement. Ask the human to switch the model. Do not load or unload models automatically.')

    def llm_request(self,requested):
        """Reserve an already-ready server. Inference never calls lifecycle APIs."""
        if not isinstance(requested,str):raise SparkError('Select a language model by name.')
        generation=dict(self.node_generation)
        with self.reconfigure:
            self.refresh()
            normalized=requested.strip().casefold().removeprefix('garuda/')
            name=self.resolve_auto() if normalized in ('spark-auto','auto') else resolve_workload(self.config,requested)[0]
            model,_,setting,nodes=selected(self.config,name)
            if model['kind']!='llm':raise SparkError('Select a language model for this endpoint.')
            if any(generation[n]!=self.node_generation[n] for n in nodes):
                raise SparkError('A forced model operation cancelled this waiting request.')
            self.require_ready_llm(name,setting,nodes)
            self.reconcile_jobs()
            token=self.acquire(name,nodes,limit=setting['concurrency'])
            try:
                # State can change while this request waits for a reservation.
                self.require_ready_llm(name,setting,nodes)
                with self.cv:
                    if self.leases[token].get('cancelled'):
                        raise SparkError('A forced model operation cancelled this request.')
                    self.leases[token]['backend_url']=setting['backend_url']
            except BaseException:self.release(token);raise
        return name,token

    def inference_origin(self,token):
        with self.cv:return self.leases[token]['backend_url']

    def register_inference_cancel(self,token,cancel):
        with self.cv:
            lease=self.leases[token]
            lease['cancel']=cancel
            cancelled=lease.get('cancelled',False)
        if cancelled:cancel()

    def _load_llm(self,requested):
        """Only explicit administrator load operations may start a server."""
        if not isinstance(requested,str):raise SparkError('Select a language model by name.')
        # Use one configuration generation for manager sync, reservation and
        # startup. A placement edit must not change nodes between these steps.
        generation=dict(self.node_generation)
        with self.reconfigure:
            self.sync_manager()
            normalized=requested.strip().casefold().removeprefix('garuda/')
            name=self.resolve_auto() if normalized in ('spark-auto','auto') else resolve_workload(self.config,requested)[0]
            model,_,setting,nodes=selected(self.config,name)
            if model['kind']!='llm':raise SparkError('Select a language model for this endpoint.')
            if any(generation[n]!=self.node_generation[n] for n in nodes):
                raise SparkError('A forced model operation cancelled this waiting request.')
            self.reconcile_jobs()
            # A ready, approved LLM may share an image job. Starting an LLM
            # still needs an exclusive lease: its startup can release caches.
            starting=self.shared_comfy() and not self.ready_placement(name,setting,nodes)
            token=self.acquire(name,nodes,exclusive=starting)
            try:
                self.prepare_llm(name)
                with self.cv:
                    if self.leases[token].get('cancelled'):
                        raise SparkError('A forced model operation cancelled this request.')
            except BaseException:self.release(token);raise
        return name,token

    @contextmanager
    def force_control(self,requested,action):
        # Enter before reconfigure: a waiting load can own that lock.
        if not self.force_lock.acquire(blocking=False):
            raise SparkError('Another forced model operation is running.')
        try:
            self.refresh()
            name,model=resolve_workload(self.config,requested)
            if model['kind']!='llm':raise SparkError('--force currently supports language models only.')
            if model.get('keep_loaded'):raise SparkError(name+' must stay loaded.')
            _,_,_,desired=selected(self.config,name)
            snapshot=self.runtime.snapshot(True)
            nodes=set(desired)|set(snapshot['models'].get(name,{}).get('nodes',[]))
            with self.cv:
                if self.fenced or self.reloading:raise SparkError('Configuration is changing. Try again after it finishes.')
                affected={n for n,row in snapshot['models'].items() if row['kind']=='llm' and row.get('loaded') and nodes.intersection(row['nodes'])}
                affected.add(name)
                affected.update(l['name'] for l in self.leases.values() if nodes.intersection(l['nodes']))
                if any(self.config['models'].get(n,{}).get('keep_loaded') for n in affected):
                    raise SparkError('A protected model uses the requested device.')
                if any(n not in self.config['models'] or self.config['models'][n]['kind']!='llm' for n in affected):
                    raise SparkError('An active image job uses the requested device. Stop that job before using --force.')
                if any(snapshot.get('comfyui',{}).get(n,{}).get('executing') or (snapshot.get('comfyui',{}).get(n,{}).get('running') and snapshot.get('comfyui',{}).get(n,{}).get('phase')!='waiting') for n in nodes):
                    raise SparkError('An active image job uses the requested device. Stop that job before using --force.')
                if action=='unload':affected={name}
                self.forcing_nodes=nodes;self.force_owner=threading.get_ident()
                for node in nodes:self.node_generation[node]+=1
                for entry in self.pending:
                    if nodes.intersection(entry['nodes']):entry['cancelled']=True
                cancellations=[]
                for lease in self.leases.values():
                    if lease['name'] in affected:
                        lease['cancelled']=True
                        if lease.get('cancel'):cancellations.append(lease['cancel'])
                self.cv.notify_all()
            self.runtime.event('force_requested',workload=name,operation=action,affected=sorted(affected))
            for cancel in cancellations:cancel()
            deadline=time.monotonic()+30
            while True:
                self.runtime.cancel_native(affected)
                busy=self.runtime.native_activity()
                with self.cv:
                    active=any(nodes.intersection(l['nodes']) for l in self.leases.values())
                if not active and not any(busy.get(n,0) for n in affected):break
                if time.monotonic()>=deadline:
                    raise SparkError('Conflicting requests did not stop within 30 seconds. No new model was started.')
                time.sleep(.1)
            if not self.reconfigure.acquire(timeout=30):
                raise SparkError('An earlier configuration operation did not stop within 30 seconds.')
            try:
                if not self.transition.acquire(timeout=30):
                    raise SparkError('An earlier model operation did not stop within 30 seconds.')
                try:
                    # Refresh actual placement after interrupted startup has finished.
                    current=self.runtime.snapshot(True)
                    for other in sorted(affected):
                        if other==name or current['models'].get(other,{}).get('loaded'):
                            self.runtime.force_stop_llm(other)
                    yield name
                finally:self.transition.release()
            finally:self.reconfigure.release()
        finally:
            with self.cv:
                self.forcing_nodes=set();self.force_owner=None;self.cv.notify_all()
            self.force_lock.release()

    def load(self,requested,*,force=False):
        if force:
            with self.force_control(requested,'load') as name:
                return {**self.load(name),'forced':True}

        self.refresh()
        name,model=resolve_workload(self.config,requested)
        if model['kind']=='llm':
            _,token=self._load_llm(name)
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

    def submit_comfy(self,graph,on=None):
        from .catalog import request_json
        from .deployment import wait_http
        self.refresh()
        on=on or self.config['models']['comfyui']['run_on']
        _,_,setting,nodes=selected(self.config,'comfyui',on)
        if len(nodes)!=1:raise SparkError('Submit each ComfyUI workflow to one device.')
        node=nodes[0]
        if not self.runtime.inspect_node(node)['comfyui']['reachable']:
            self.runtime.unit(node,'start',self.config['nodes'][node]['comfy_unit'])
            wait_http(setting['backend_url']+'/queue',timeout=180)
        result=request_json(setting['backend_url']+'/prompt',payload=graph if 'prompt' in graph else {'prompt':graph},timeout=120)
        self.runtime.event('workflow_submitted',node=node,prompt_id=result.get('prompt_id'))
        return {'node':node,**result}

    def unload(self,requested,*,force=False):
        if force:
            with self.force_control(requested,'unload') as name:
                self.runtime.event('unloaded',workload=name,reason='Explicit forced unload.')
                return {'status':'unloaded','workload':name,'forced':True}
        with self.reconfigure:
            self.refresh()
            name,model=resolve_workload(self.config,requested)
            if model.get('keep_loaded'):raise SparkError(f'{name} must stay loaded.')
            _,_,_,desired=selected(self.config,name)
            running=self.runtime.snapshot(True)['models'].get(name,{})
            nodes=tuple(sorted(set(desired)|set(running.get('nodes',[]))))
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
        shared=not actual_cpu and self.shared_comfy()
        token=self.acquire(name,[] if actual_cpu else [node],exclusive=not shared,request_id=node+':'+str(prompt_id),wait=False)
        if token is None:return {'status':'waiting','retry_after':1}
        try:
            if not actual_cpu:
                with self.transition:
                    snapshot=self.runtime.snapshot(True)
                    conflicts=[n for n,s in snapshot['models'].items() if s['kind']=='llm' and s['loaded'] and node in s['nodes']]
                    if shared:
                        policy=self.config['policy']
                        unsupported=[n for n in conflicts if n not in policy['comfyui_shared_llms']]
                        available=snapshot['nodes'][node]['memory_bytes']['MemAvailable']
                        minimum=policy['comfyui_min_available_gib']*1024**3
                        if not unsupported and available<minimum:
                            # Release only this engine's idle cache. Never stop
                            # an LLM to make room for a shared image job.
                            self.runtime.clear_comfy(node)
                            available=self.runtime.inspect_node(node)['memory_bytes']['MemAvailable']
                        if unsupported or available<minimum:
                            self.release(token)
                            return {'status':'waiting','retry_after':2,'reason':'unapproved_loaded_llm' if unsupported else 'low_available_memory',
                                    'available_bytes':available,'minimum_bytes':minimum}
                        self.runtime.event('comfy_shared_admitted',node=node,prompt_id=str(prompt_id),
                                           llms=conflicts,available_bytes=available,minimum_bytes=minimum)
                        return {'token':token,'status':'admitted','node':node,'cpu_only':False,'shared':True}
                    if conflicts:
                        self.release(token)
                        return {'status':'waiting','retry_after':2,'reason':'loaded_llm_requires_explicit_unload'}
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
        for name in enabled_llms(self.config):
            model=self.config['models'][name]
            _,on,setting,nodes=selected(self.config,name)
            data.append({'id':'garuda/'+name,'name':model.get('name',name),'context_length':setting['context_tokens'],
                         'output_tokens':model.get('output_tokens',min(16384,setting['context_tokens']-1)),'aliases':model.get('aliases',[]),
                         'inputs':model.get('inputs',['text']),'reasoning':model.get('reasoning',False),'nodes':list(nodes)})
        if self.config.get('auto'):
            targets={'garuda/'+name for name in self.config['auto']['targets']}
            routed=[row for row in data if row['id'] in targets]
            data.append({'id':'garuda/spark-auto','name':'Spark Auto','context_length':min(x['context_length'] for x in routed),
                         'aliases':['auto'],'output_tokens':min(x['output_tokens'] for x in routed),'inputs':['text']})
        audio=[]
        fields=('model','api_base','speech_url','default_voice','voices','preferred_voices',
                'languages','formats','device','api_key_required')
        for name,model in self.config['models'].items():
            if model['kind']=='audio' and isinstance(model.get('tts'),dict):
                audio.append({'id':name,'kind':'tts','name':model.get('name',name),
                              'nodes':list(selected(self.config,name)[3]),
                              **{key:model['tts'][key] for key in fields if key in model['tts']}})
        return {'data':data,'audio_services':audio,'authority':'spark01','config_version':self.config['version']}
