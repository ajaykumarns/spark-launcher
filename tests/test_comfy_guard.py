import asyncio
import importlib.util
import io
import json
import os
import sys
import tempfile
import types
import unittest
from pathlib import Path
from unittest.mock import patch


class GuardTests(unittest.TestCase):
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory();self.config=Path(self.temp.name)/'config.json'
        self.config.write_text(json.dumps({'node':'spark02','controller':'http://controller','operators':['127.0.0.1']}))
        self.calls=[];self.memory_calls=[]
        owner=self
        class Executor:
            def execute(self,prompt,prompt_id,*args,**kwargs):
                owner.assertTrue(owner.module.STATE['executing'])
                if prompt_id=='failure':raise RuntimeError('engine error')
                return 'done'
        class Routes:
            def get(self,path):return lambda fn:fn
            post=get
        web=types.SimpleNamespace(middleware=lambda fn:fn,json_response=lambda data,status=200:(data,status),HTTPForbidden=PermissionError)
        memory=types.ModuleType('comfy.model_management')
        memory.throw_exception_if_processing_interrupted=lambda:None
        memory.unload_all_models=lambda:self.memory_calls.append('unload')
        memory.soft_empty_cache=lambda:self.memory_calls.append('free')
        fake_server=types.SimpleNamespace(PromptServer=types.SimpleNamespace(instance=types.SimpleNamespace(routes=Routes(),app=types.SimpleNamespace(middlewares=[]))))
        modules={'execution':types.SimpleNamespace(PromptExecutor=Executor),'comfy':types.ModuleType('comfy'),
                 'comfy.model_management':memory,'aiohttp':types.SimpleNamespace(web=web),'server':fake_server}
        self.modules=patch.dict(sys.modules,modules);self.modules.start()
        self.env=patch.dict(os.environ,{'SPARKY_COMFY_CONFIG':str(self.config)});self.env.start()
        path=Path(__file__).parents[1]/'src/spark_launcher/comfy_guard.py'
        spec=importlib.util.spec_from_file_location('guard_under_test',path);self.module=importlib.util.module_from_spec(spec);spec.loader.exec_module(self.module)
        def rpc(path,data):
            self.calls.append(path)
            return {'token':'lease'} if path=='/jobs/acquire' else {'status':'ok'}
        self.module.rpc=rpc
        self.executor=Executor()
    def tearDown(self):self.env.stop();self.modules.stop();self.temp.cleanup()
    def test_reservation_covers_the_full_execution(self):
        self.assertEqual(self.executor.execute({'1':{'class_type':'Something'}},'job'),'done')
        self.assertEqual(self.calls,['/jobs/acquire','/jobs/release'])
        self.assertFalse(self.module.STATE['executing'])
    def test_engine_failure_releases_reservation(self):
        with self.assertRaises(RuntimeError):self.executor.execute({},'failure')
        self.assertEqual(self.calls,['/jobs/acquire','/jobs/release']);self.assertEqual(self.module.STATE['phase'],'idle')
    def test_idle_cache_release_is_not_allowed_during_gpu_execution(self):
        self.module.STATE.update(phase='executing',executing=True)
        request=types.SimpleNamespace(remote='127.0.0.1')
        data,status=asyncio.run(self.module.sparky_free(request))
        self.assertEqual(status,409);self.assertEqual(self.memory_calls,[])
        self.module.STATE.update(phase='waiting',executing=False)
        data,status=asyncio.run(self.module.sparky_free(request))
        self.assertEqual(status,200);self.assertEqual(self.memory_calls,['unload','free'])
