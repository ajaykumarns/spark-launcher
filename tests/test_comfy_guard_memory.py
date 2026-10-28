import asyncio,importlib.util,json,os,tempfile,types,sys,unittest
from pathlib import Path
from unittest.mock import patch
class GuardTests(unittest.TestCase):
 def setUp(self):
  self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup);self.root=Path(self.tmp.name)
  c=self.root/'config.json';c.write_text(json.dumps({'node':'spark02','controller':'http://127.0.0.1:8020','operators':['127.0.0.1']}))
  class Routes:
   def get(self,*_):return lambda f:f
   def post(self,*_):return lambda f:f
  class Executor:
   def execute(self,*a,**k):return 0
  web=types.SimpleNamespace(json_response=lambda d:d,middleware=lambda f:f,HTTPForbidden=PermissionError)
  mods={'execution':types.SimpleNamespace(PromptExecutor=Executor),'comfy':types.ModuleType('comfy'),
   'comfy.model_management':types.SimpleNamespace(current_loaded_models=[]),'aiohttp':types.SimpleNamespace(web=web),
   'server':types.SimpleNamespace(PromptServer=types.SimpleNamespace(instance=types.SimpleNamespace(routes=Routes(),app=types.SimpleNamespace(middlewares=[]))))}
  self.p1=patch.dict(sys.modules,mods);self.p2=patch.dict(os.environ,SPARKY_COMFY_CONFIG=str(c));self.p1.start();self.p2.start();self.addCleanup(self.p1.stop);self.addCleanup(self.p2.stop)
  spec=importlib.util.spec_from_file_location('guard_under_test',Path(__file__).resolve().parents[1]/'src/spark_launcher/comfy_guard.py');self.mod=importlib.util.module_from_spec(spec);spec.loader.exec_module(self.mod)
 def test_real_host_field_uses_memavailable_bytes(self):
  p=self.root/'meminfo';p.write_text('MemTotal: 100000 kB\nMemAvailable: 80000 kB\nMemFree: 100 kB\n')
  self.assertEqual(self.mod.host_memory_snapshot(p),{'MemTotal':102400000,'MemAvailable':81920000})
 def test_missing_or_bad_measurement_is_unknown(self):
  self.assertIsNone(self.mod.host_memory_snapshot(self.root/'absent'))
  p=self.root/'meminfo';p.write_text('MemAvailable: invalid kB\n');self.assertIsNone(self.mod.host_memory_snapshot(p))
 def test_profile_contains_only_memory_fields(self):
  d=self.mod.memory_profile(['main.py','--reserve-vram','12','--cache-none','--disable-pinned-memory','--disable-smart-memory','--api-key','SECRET'])
  self.assertEqual(d['reserve_vram_gib'],12);self.assertTrue(d['disable_smart_memory']);self.assertNotIn('SECRET',json.dumps(d))
 def test_state_preserves_job_fields_and_exposes_host_field(self):
  self.mod.STATE.update(phase='executing',prompt_id='our-job',executing=True)
  with patch.object(self.mod,'host_memory_snapshot',return_value={'MemTotal':100,'MemAvailable':70}):d=asyncio.run(self.mod.sparky_state(None))
  self.assertEqual(d['prompt_id'],'our-job');self.assertEqual(d['version'],2);self.assertEqual(d['host_memory_bytes']['MemAvailable'],70)
if __name__=='__main__':unittest.main()
