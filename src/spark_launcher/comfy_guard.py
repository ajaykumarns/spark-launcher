"""ComfyUI custom node: acquire a device before GPU work; never cancel a job."""
import functools
import gc
import weakref
import json
import os
import threading
import time
from pathlib import Path
from urllib.request import Request, urlopen

import execution
import comfy.model_management as memory
from aiohttp import web
from server import PromptServer

CONFIG=json.loads(Path(os.environ['SPARKY_COMFY_CONFIG']).read_text())
NODE=CONFIG['node']
LOCK=threading.RLock()
EXECUTORS=weakref.WeakSet()
STATE={'phase':'idle','prompt_id':None,'executing':False,'version':1}


def rpc(path,data):
    request=Request(CONFIG['controller']+path,data=json.dumps({'node':NODE,**data}).encode(),headers={'Content-Type':'application/json'})
    with urlopen(request,timeout=120) as response:return json.load(response)


def patch_executor(cls):
    original_init=cls.__init__
    @functools.wraps(original_init)
    def tracked_init(self,*args,**kwargs):
        original_init(self,*args,**kwargs)
        EXECUTORS.add(self)
    cls.__init__=tracked_init
    original=cls.execute
    if getattr(original,'_sparky_guard',False):return
    @functools.wraps(original)
    def guarded(self,prompt,prompt_id,*args,**kwargs):
        cpu_only=bool(prompt) and all(n.get('class_type')=='SparkyProbe' for n in prompt.values())
        token=None
        with LOCK:
            STATE.update(phase='waiting',prompt_id=str(prompt_id),executing=False)
        try:
            while token is None:
                memory.throw_exception_if_processing_interrupted()
                try:
                    result=rpc('/jobs/acquire',{'prompt_id':str(prompt_id),'cpu_only':cpu_only})
                    token=result.get('token')
                except OSError:
                    # A control-link failure must not start an unreserved GPU job.
                    time.sleep(2)
                if token is None:time.sleep(1)
            with LOCK:STATE.update(phase='executing',executing=True)
            return original(self,prompt,prompt_id,*args,**kwargs)
        finally:
            with LOCK:STATE.update(phase='idle',prompt_id=None,executing=False)
            for attempt in range(3):
                try:
                    if token:rpc('/jobs/release',{'token':token})
                    else:rpc('/jobs/cancel',{'prompt_id':str(prompt_id)})
                    break
                except OSError:time.sleep(1)
    guarded._sparky_guard=True
    cls.execute=guarded


patch_executor(execution.PromptExecutor)
routes=PromptServer.instance.routes


@routes.get('/sparky/state')
async def sparky_state(request):
    with LOCK:return web.json_response({**STATE,'resident_models':len(getattr(memory,'current_loaded_models',[]))})


def local_operator(request):
    return request.remote in CONFIG['operators']


@routes.post('/sparky/free')
async def sparky_free(request):
    if not local_operator(request):raise web.HTTPForbidden()
    # Same lock protects entry into GPU execution. Freeing cached weights cannot
    # race a job between its admission reply and its first GPU operation.
    with LOCK:
        if STATE['executing']:return web.json_response({'status':'busy'},status=409)
        memory.unload_all_models()
        for executor in list(EXECUTORS):
            if hasattr(executor,"reset"):executor.reset()
        gc.collect()
        memory.soft_empty_cache()
        return web.json_response({'status':'released_idle_cache'})


@routes.post('/sparky/drain')
async def sparky_drain(request):
    if not local_operator(request):raise web.HTTPForbidden()
    STATE['draining']=True
    return web.json_response({'status':'draining'})


@web.middleware
async def drain_gate(request,handler):
    if STATE.get('draining') and request.method=='POST' and request.path.rstrip('/') in ('/prompt','/api/prompt'):
        return web.json_response({'error':'ComfyUI is draining for a service update. Retry after it is ready.'},status=503)
    return await handler(request)


PromptServer.instance.app.middlewares.append(drain_gate)


class SparkyProbe:
    """A CPU-only integration probe. It never loads weights or uses CUDA."""
    @classmethod
    def INPUT_TYPES(cls):return {'required':{}}
    RETURN_TYPES=('STRING',)
    FUNCTION='check'
    CATEGORY='Sparky'
    OUTPUT_NODE=True
    def check(self):return {'ui':{'text':['SPARKY_GUARD_OK']},'result':('SPARKY_GUARD_OK',)}


NODE_CLASS_MAPPINGS={'SparkyProbe':SparkyProbe}
NODE_DISPLAY_NAME_MAPPINGS={'SparkyProbe':'Sparky control probe (CPU only)'}
