import contextlib
import io
import threading
import unittest
from unittest.mock import Mock,patch
from spark_launcher.image_api import generate,flux_workflow,image_models
from spark_launcher.catalog import SparkError
class ImageAPITests(unittest.TestCase):
    def setUp(self):
        self.c=Mock();self.c.image_gate=threading.BoundedSemaphore(1)
        self.c.config={'models':{'flux2-klein-4b':{'kind':'comfy_model','run_on':'spark02','settings':{'spark02':{}},'image_api':{'enabled':True}}},'nodes':{'spark02':{'comfy_url':'http://image:8188'}}}
        self.c.submit_comfy.return_value={'prompt_id':'job','node':'spark02'}
        self.body={'model':'flux2-klein-4b','prompt':'A red cube','response_format':'b64_json'}
    def test_rejects_unsupported_values_before_submitting_work(self):
        for key,value in [('n',8),('size','4096x4096'),('response_format','url'),('prompt',''),('seed',True),('quality','high'),('stream',True),('workflow',{})]:
            with self.subTest(key=key,value=value):
                with self.assertRaises(ValueError):generate(self.c,{**self.body,key:value})
        self.c.submit_comfy.assert_not_called()
    def test_returns_only_saved_image_from_requested_job(self):
        history={'job':{'status':{'completed':True},'outputs':{'13':{'images':[{'filename':'one.png','subfolder':'','type':'output'}]}}}}
        with patch('spark_launcher.image_api.request_json',return_value=history),patch('spark_launcher.image_api.urlopen',return_value=contextlib.closing(io.BytesIO(b'\x89PNG\r\n\x1a\nexample'))):
            result=generate(self.c,self.body)
        self.assertEqual(len(result['data']),1);self.assertTrue(result['data'][0]['b64_json'].startswith('iVBOR'))
        self.c.submit_comfy.assert_called_once();self.c.runtime.event.assert_called_once()
        self.assertTrue(self.c.image_gate.acquire(blocking=False))
    def test_engine_error_releases_gateway_slot(self):
        with patch('spark_launcher.image_api.request_json',return_value={'job':{'status':{'status_str':'error'}}}):
            with self.assertRaises(SparkError):generate(self.c,self.body)
        self.assertTrue(self.c.image_gate.acquire(blocking=False))
    def test_workflow_uses_installed_distilled_fp8_and_four_steps(self):
        graph=flux_workflow('test',1024,1024,42)
        self.assertEqual(graph['1']['inputs']['unet_name'],'flux-2-klein-4b-fp8.safetensors')
        self.assertEqual(graph['9']['inputs']['steps'],4);self.assertEqual(graph['10']['inputs']['batch_size'],1)
