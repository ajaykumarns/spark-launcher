import copy
import json
import tempfile
import threading
import time
import unittest
from pathlib import Path
from unittest.mock import patch
from spark_launcher.catalog import SparkError
from spark_launcher.resources import validate_cluster,selected,matrix_sets,make_plan,write_cluster
from spark_launcher.coordinator import Coordinator


def configuration(root):
    def model(name,on):
        settings={}
        for where,nodes in [('spark01',['spark01']),('spark02',['spark02']),('both',['spark01','spark02'])]:
            settings[where]={'units':{n:name+'.service' for n in nodes},'backend_url':'http://127.0.0.1:8001','context_tokens':123456,'concurrency':4}
        return {'kind':'llm','name':name,'run_on':on,'settings':settings,'aliases':[]}
    return {'version':1,'root':str(root),'controller_port':8020,'nodes':{'spark01':{},'spark02':{}},'policy':{'interrupt_active_jobs':False,'protected_models':['parakeet']},
            'models':{'qwen':model('qwen','spark02'),'nemotron':model('nemotron','spark01'),'deepseek':model('deepseek','both'),
                      'parakeet':{'kind':'audio','name':'Parakeet','run_on':'spark02','keep_loaded':True,'settings':{'spark02':{'containers':['parakeet-engine','parakeet-gateway']}}}}}


class FakeRuntime:
    def __init__(self,c):
        self.config=c;self.events=[];self.actions=[];self._cache=None
        self.data={'nodes':{},'models':{'qwen':{'kind':'llm','loaded':True,'nodes':['spark02'],'state':'ready','active_requests':0},
                                     'nemotron':{'kind':'llm','loaded':True,'nodes':['spark01'],'state':'ready','active_requests':0}},
                   'comfyui':{'spark01':{'running':0,'pending':0,'phase':'idle'},'spark02':{'running':0,'pending':0,'phase':'idle'}}}
    def snapshot(self,fresh=False):return copy.deepcopy(self.data)
    def event(self,action,**fields):self.events.append((action,fields))
    def native_activity(self):return {}
    def manager(self,path,**kwargs):
        self.actions.append((path,kwargs))
        if path=='/running':return {'running':[{'model':n,'state':s['state']} for n,s in self.data['models'].items() if s['loaded']]}
        return {}


class ResourceTests(unittest.TestCase):
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory();self.root=Path(self.temp.name)
        self.c=configuration(self.root);self.path=self.root/'models.json';write_cluster(self.path,self.c)
        self.runtime=FakeRuntime(self.c);self.coord=Coordinator(self.path,self.runtime)
    def tearDown(self):self.temp.cleanup()

    def test_config_drives_compatibility_without_model_name_rules(self):
        values=set(matrix_sets(self.c).values())
        self.assertIn('nemotron & qwen',values);self.assertIn('deepseek',values)
        self.c['models']['qwen']['run_on']='spark01'
        values=set(matrix_sets(self.c).values())
        self.assertEqual(values,{'nemotron','qwen','deepseek'})
        self.c['models']['qwen']['run_on']='both'
        self.assertEqual(selected(self.c,'qwen')[3],('spark01','spark02'))

    def test_config_rejects_unsafe_or_missing_definitions(self):
        bad=copy.deepcopy(self.c);bad['models']['parakeet']['keep_loaded']=False
        with self.assertRaises(SparkError):validate_cluster(bad)
        bad=copy.deepcopy(self.c);bad['policy']['interrupt_active_jobs']=True
        with self.assertRaises(SparkError):validate_cluster(bad)
        bad=copy.deepcopy(self.c);bad['models']['qwen']['settings']['both']['units'].pop('spark02')
        with self.assertRaises(SparkError):validate_cluster(bad)

    def test_plan_uses_actual_placement_and_does_not_mutate(self):
        before=copy.deepcopy(self.runtime.data)
        plan=make_plan(self.c,'deepseek',self.runtime.data)
        self.assertEqual({x['workload'] for x in plan['steps'] if x['action']=='unload'},{'qwen','nemotron'})
        self.assertEqual(plan['protected'],['parakeet']);self.assertEqual(self.runtime.data,before)
        self.runtime.data['models']['qwen']['active_requests']=4
        self.assertIn('qwen',make_plan(self.c,'deepseek',self.runtime.data)['wait_for'])
        self.assertEqual(self.runtime.actions,[])

    def test_keep_loaded_blocks_even_explicit_unload(self):
        with self.assertRaises(SparkError):self.coord.unload('parakeet')
        self.assertEqual(self.runtime.actions,[])

    def test_four_parallel_requests_finish_before_device_is_reassigned(self):
        tokens=[self.coord.acquire('qwen',['spark02']) for _ in range(4)]
        waiting={}
        def contender():waiting['token']=self.coord.acquire('comfyui@spark02',['spark02'],exclusive=True)
        thread=threading.Thread(target=contender);thread.start()
        time.sleep(.05);self.assertNotIn('token',waiting)
        for token in tokens[:-1]:self.coord.release(token)
        time.sleep(.05);self.assertNotIn('token',waiting)
        self.coord.release(tokens[-1]);thread.join(2)
        self.assertIn('token',waiting);self.coord.release(waiting['token'])

    def test_disjoint_device_stays_available(self):
        comfy=self.coord.acquire('comfyui@spark01',['spark01'],exclusive=True)
        qwen=self.coord.acquire('qwen',['spark02'])
        self.assertEqual(len(self.coord.leases),2)
        self.coord.release(qwen);self.coord.release(comfy)

    def test_waiting_job_is_not_starved_by_more_requests(self):
        active=self.coord.acquire('qwen',['spark02'])
        self.assertIsNone(self.coord.acquire('comfyui@spark02',['spark02'],exclusive=True,request_id='job',wait=False))
        self.assertIsNone(self.coord.acquire('qwen',['spark02'],request_id='new-agent',wait=False))
        self.coord.release(active)
        job=self.coord.acquire('comfyui@spark02',['spark02'],exclusive=True,request_id='job',wait=False)
        self.assertIsNotNone(job)
        self.coord.release(job)
        agent=self.coord.acquire('qwen',['spark02'],request_id='new-agent',wait=False)
        self.assertIsNotNone(agent);self.coord.release(agent)

    def test_job_retry_does_not_duplicate_reservation(self):
        first=self.coord.acquire('comfyui@spark02',['spark02'],exclusive=True,request_id='same-job')
        again=self.coord.acquire('comfyui@spark02',['spark02'],exclusive=True,request_id='same-job')
        self.assertEqual(first,again);self.assertEqual(len(self.coord.leases),1);self.coord.release(first)

    def test_placement_edit_does_not_restart_anything(self):
        result=self.coord.set_placement('qwen','both')
        self.assertEqual(result['run_on'],'both');self.assertEqual(self.runtime.actions,[])
        self.assertEqual(json.loads(self.path.read_text())['models']['qwen']['run_on'],'both')

    def test_initial_install_does_not_reload_manager(self):
        import yaml
        p=self.root/'manager';p.mkdir()
        models={n:{'proxy':m['settings'][m['run_on']]['backend_url'],'concurrencyLimit':4,'capabilities':{'context':123456}}
                for n,m in self.c['models'].items() if m['kind']=='llm'}
        baseline={'globalTTL':10800,'models':models,'apiKeys':['do-not-touch'],
                  'routing':{'router':{'use':'matrix','settings':{'matrix':{'sets':{'pair':'nemotron & qwen','single':'deepseek'}}}}}}
        (p/'config.yaml').write_text(yaml.safe_dump(baseline));original=(p/'config.yaml').read_bytes()
        with patch('spark_launcher.coordinator.command') as calls:self.coord.sync_manager()
        calls.assert_not_called();self.assertEqual((p/'config.yaml').read_bytes(),original)

    def test_unknown_native_activity_prevents_unload(self):
        self.runtime.native_activity=lambda: (_ for _ in ()).throw(SparkError('unreachable'))
        with self.assertRaises(SparkError):self.coord.unload('qwen')
        self.assertEqual(self.runtime.actions,[])

    def test_releasing_comfy_cache_refuses_an_executing_job(self):
        from spark_launcher.resource_runtime import Runtime
        self.c['nodes']['spark01']['comfy_url']='http://127.0.0.1:8188'
        runtime=Runtime(self.c,self.path)
        with patch('spark_launcher.resource_runtime.request_json',return_value={'phase':'executing'}) as network:
            with self.assertRaises(SparkError):runtime.clear_comfy('spark01')
        self.assertEqual(network.call_count,1)


if __name__=='__main__':unittest.main()
