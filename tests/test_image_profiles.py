import copy,json,shlex,tempfile,types,unittest
from pathlib import Path
from unittest.mock import patch
from spark_launcher.catalog import SparkError
from spark_launcher.image_profiles import activate,evaluate
from spark_launcher.cli import parser

def snapshot():
 return {'models':{'qwen3.8-flash-next':{'state':'ready','kind':'llm','loaded':True,'nodes':['spark01']}},
         'nodes':{'spark02':{'comfyui':{'running':0,'pending':0},'memory_bytes':{'MemAvailable':112*2**30}}}}

class ProfileTests(unittest.TestCase):
 def test_dedicated_rejects_language_model_on_worker(self):
  s=snapshot();s['models']['qwen3.8-flash-next']['nodes']=['spark01','spark02']
  with self.assertRaises(SparkError):evaluate({},s,'dedicated')
 def test_active_image_is_preserved(self):
  s=snapshot();s['nodes']['spark02']['comfyui']['running']=1
  with self.assertRaises(SparkError):evaluate({},s,'shared')
 def test_cli_exposes_explicit_plan(self):
  a=parser().parse_args(['image-profile','dedicated','--dry-run'])
  self.assertEqual(a.mode,'dedicated');self.assertTrue(a.dry_run)
 def transaction(self,fail_confirmation=False):
  with tempfile.TemporaryDirectory() as temp:
   root=Path(temp);(root/'config').mkdir();path=root/'config/models.json';path.write_text('{"original":true}')
   original=path.read_bytes();cfg={'controller_port':8020,'nodes':{'spark02':{'ssh':'spark02','comfy_unit':'spark-imagegen.service','comfy_url':'http://worker:8188'}},'policy':{'comfyui_min_available_gib':21.5}}
   limits={'MemoryHigh':str(14*2**30),'MemoryMax':str(16*2**30),'MemorySwapMax':'0'};old=copy.copy(limits);events=[]
   def request(url,*,payload=None,**kwargs):
    if url.endswith('/leases'):return {'active':[],'config_transition':False}
    if url.endswith('/fence'):events.append(('fence',payload['enabled']));return {}
    if url.endswith('/system_stats'):return {'system':{'ram_total':0 if fail_confirmation else int(limits['MemoryMax'])}}
    return {}
   def command(argv,**kwargs):
    words=shlex.split(argv[-1])
    self.assertNotIn('restart',words);self.assertNotIn('stop',words)
    if 'set-property' in words:
     events.append(('set',dict(limits)))
     limits.update(dict(x.split('=',1) for x in words if x.startswith(('MemoryHigh=','MemoryMax=','MemorySwapMax='))))
    return types.SimpleNamespace(stdout='\n'.join(k+'='+v for k,v in limits.items()))
   runtime=types.SimpleNamespace(snapshot=lambda *_:snapshot(),native_activity=lambda:{},
     clear_comfy=lambda _:events.append(('clear',True)),inspect_node=lambda _:snapshot()['nodes']['spark02'])
   with patch('spark_launcher.image_profiles.read_cluster',return_value=cfg),patch('spark_launcher.image_profiles.Runtime',return_value=runtime),patch('spark_launcher.image_profiles.request_json',side_effect=request),patch('spark_launcher.image_profiles.command',side_effect=command),patch('spark_launcher.image_profiles.write_cluster',side_effect=lambda p,d:p.write_text(json.dumps(d))):
    if fail_confirmation:
     with self.assertRaises(SparkError):activate(root,'dedicated')
     self.assertEqual(path.read_bytes(),original);self.assertEqual(limits,old)
    else:
     result=activate(root,'dedicated');self.assertEqual(result['status'],'applied')
     self.assertEqual(int(limits['MemoryMax']),48*2**30)
     self.assertEqual(json.loads(path.read_text())['policy']['comfyui_min_available_gib'],96)
   self.assertEqual(events[0],('fence',True));self.assertEqual(events[-1],('fence',False))
   self.assertLess(next(i for i,x in enumerate(events) if x[0]=='clear'),next(i for i,x in enumerate(events) if x[0]=='set'))
 def test_applies_only_limits_after_idle_cache_release(self):self.transaction()
 def test_failed_confirmation_restores_limits_config_and_admission(self):self.transaction(True)

if __name__=='__main__':unittest.main()
