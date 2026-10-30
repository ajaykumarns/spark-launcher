import copy
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch,Mock
from spark_launcher.catalog import SparkError
from spark_launcher.coordinator import Coordinator
from spark_launcher.resources import validate_cluster,write_cluster
from test_resources import configuration,FakeRuntime
GIB=1024**3
class SharedImageTests(unittest.TestCase):
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory();self.addCleanup(self.temp.cleanup)
        self.config=configuration(Path(self.temp.name));self.config['nodes']['spark02']['comfy_url']='http://image:8188'
        self.config['policy'].update(comfyui_exclusive_with_llms=False,comfyui_shared_llms=['qwen'],comfyui_min_available_gib=18)
        self.path=Path(self.temp.name)/'models.json';write_cluster(self.path,self.config)
        self.rt=FakeRuntime(self.config);self.rt.data['nodes']['spark02']={'memory_bytes':{'MemAvailable':23*GIB}}
        self.rt.clear_comfy=Mock();self.rt.inspect_node=Mock(return_value={'memory_bytes':{'MemAvailable':12*GIB}})
        self.coord=Coordinator(self.path,self.rt);self.graph={'1':{'class_type':'UNETLoader','inputs':{}}}
        self.queue=patch('spark_launcher.catalog.request_json',side_effect=self.reply);self.queue.start();self.addCleanup(self.queue.stop)
    def reply(self,url,**kw):
        if url.endswith('/queue'):return {'queue_running':[[0,'image-job',self.graph]]}
        return {'phase':'idle'}
    def admit(self):return self.coord.comfy_acquire('spark02','image-job')
    def test_eight_llm_requests_and_image_can_hold_leases_together(self):
        for _ in range(8):self.coord.acquire('qwen',['spark01','spark02'])
        self.rt.native_activity=lambda:{'qwen':8}
        self.assertTrue(self.admit()['shared']);self.assertEqual(len(self.coord.leases),9);self.assertEqual(self.rt.actions,[])
        self.assertIsNotNone(self.coord.acquire('qwen',['spark02'],wait=False))
        self.assertIsNone(self.coord.acquire('comfyui@spark02',['spark02'],wait=False))
    def test_low_memory_waits_without_unloading_llm(self):
        self.rt.data['nodes']['spark02']['memory_bytes']['MemAvailable']=12*GIB
        result=self.admit();self.assertEqual(result['reason'],'low_available_memory');self.assertNotIn('token',result)
        self.assertFalse(self.coord.leases);self.assertEqual(self.rt.actions,[]);self.rt.clear_comfy.assert_called_once_with('spark02')
    def test_idle_image_cache_can_be_released_to_admit_next_job(self):
        self.rt.data['nodes']['spark02']['memory_bytes']['MemAvailable']=12*GIB
        self.rt.inspect_node.return_value={'memory_bytes':{'MemAvailable':23*GIB}}
        self.assertIn('token',self.admit());self.assertEqual(self.rt.actions,[])
    def test_unapproved_loaded_llm_is_never_unloaded_for_shared_image(self):
        self.rt.data['models']['nemotron']['nodes']=['spark02']
        self.assertEqual(self.admit()['reason'],'unapproved_loaded_llm');self.assertEqual(self.rt.actions,[])
    def test_explicit_unload_and_model_start_still_wait_for_image(self):
        self.admit();self.assertIsNone(self.coord.acquire('qwen',['spark02'],exclusive=True,wait=False))
        self.assertIsNone(self.coord.acquire('nemotron',['spark02'],wait=False))
    def test_exclusive_image_job_requires_explicit_llm_unload(self):
        self.coord.config['policy']['comfyui_exclusive_with_llms']=True
        self.assertEqual(self.admit()['reason'],'loaded_llm_requires_explicit_unload')
        self.assertEqual(self.rt.actions,[]);self.assertFalse(self.coord.leases)
    def test_cpu_flag_cannot_bypass_gpu_admission(self):
        with self.assertRaises(SparkError):self.coord.comfy_acquire('spark02','image-job',cpu_only=True)
        self.assertFalse(self.coord.leases)
    def test_controller_restart_adopts_shared_image_and_allows_llm(self):
        self.queue.stop()
        with patch('spark_launcher.catalog.request_json',return_value={'executing':True,'prompt_id':'image-job'}):self.coord.reconcile_jobs()
        self.assertIsNotNone(self.coord.acquire('qwen',['spark02'],wait=False))
        self.assertIsNone(self.coord.acquire('qwen',['spark02'],exclusive=True,wait=False))
    def test_shared_policy_requires_allowlist_and_positive_finite_memory(self):
        for key,value in [('comfyui_shared_llms',[]),('comfyui_shared_llms',['missing']),('comfyui_min_available_gib',0),('comfyui_min_available_gib',float('nan')),('comfyui_min_available_gib',True)]:
            with self.subTest(key=key,value=value):
                c=copy.deepcopy(self.config);c['policy'][key]=value
                with self.assertRaises(SparkError):validate_cluster(c)
