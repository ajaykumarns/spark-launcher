"""Inference cannot change model residency, including after admission races."""
import unittest
from unittest.mock import Mock,patch
from spark_launcher.catalog import SparkError
from spark_launcher.resources import write_cluster
import test_admission_fastpath


class ManualLoadingTests(unittest.TestCase):
    setUp=test_admission_fastpath.ReadyAdmissionTests.setUp

    def test_stopped_model_fails_before_any_start_or_manager_reload(self):
        self.runtime.data['models']['qwen'].update(loaded=False,state='stopped')
        with patch.object(self.c,'sync_manager') as sync,patch.object(self.c,'prepare_llm') as prepare:
            with self.assertRaisesRegex(SparkError,'Ask the human to switch the model'):
                self.c.llm_request('qwen')
        sync.assert_not_called();prepare.assert_not_called()
        self.assertFalse(self.c.leases);self.assertFalse(self.c.pending)
        self.assertEqual(self.runtime.actions,[('/running',{})])
        with self.assertRaises(SparkError) as error:self.c.llm_request('qwen')
        self.assertNotIn('sparky load',str(error.exception))
        self.assertIn('Do not load or unload models automatically.',str(error.exception))

    def test_ready_model_is_reserved_without_loading_or_inventory(self):
        with patch.object(self.c,'sync_manager') as sync,patch.object(self.c,'prepare_llm') as prepare,patch.object(self.runtime,'snapshot',side_effect=AssertionError('inventory')):
            name,token=self.c.llm_request('garuda/qwen')
        self.assertEqual(name,'qwen')
        self.assertEqual(self.c.inference_origin(token),'http://127.0.0.1:8001')
        sync.assert_not_called();prepare.assert_not_called()
        self.assertEqual(self.runtime.actions,[('/running',{}),('/running',{})])
        self.c.release(token)

    def test_changed_placement_does_not_reload_or_move_a_model(self):
        self.config['models']['qwen']['run_on']='both';write_cluster(self.path,self.config)
        with self.assertRaisesRegex(SparkError,'selected placement'):
            self.c.llm_request('qwen')
        self.assertEqual(self.runtime.actions,[])

    def test_missing_ledger_fails_closed(self):
        self.ledger.unlink()
        with self.assertRaisesRegex(SparkError,'Ask the human to switch the model'):
            self.c.llm_request('qwen')
        self.assertFalse(self.runtime.actions)

    def test_model_stopped_while_waiting_is_not_restarted(self):
        original=self.c.acquire
        def acquire(*args,**kwargs):
            self.runtime.data['models']['qwen'].update(loaded=False,state='stopped')
            return original(*args,**kwargs)
        with patch.object(self.c,'acquire',side_effect=acquire):
            with self.assertRaisesRegex(SparkError,'Ask the human to switch the model'):
                self.c.llm_request('qwen')
        self.assertFalse(self.c.leases)
        self.assertTrue(all(path=='/running' for path,_ in self.runtime.actions))

    def test_auto_without_ready_target_does_not_load_default(self):
        self.config['auto']={'targets':['qwen'],'default':'qwen'}
        write_cluster(self.path,self.config)
        self.runtime.data['models']['qwen'].update(loaded=False,state='stopped')
        with self.assertRaisesRegex(SparkError,'Ask the human to switch the model'):
            self.c.llm_request('garuda/spark-auto')
        self.assertEqual(self.runtime.actions,[('/running',{})])

    def test_auto_uses_ready_target_only(self):
        self.config['auto']={'targets':['deepseek','qwen'],'default':'deepseek'}
        write_cluster(self.path,self.config)
        name,token=self.c.llm_request('auto')
        self.assertEqual(name,'qwen');self.c.release(token)
        self.assertTrue(all(path=='/running' for path,_ in self.runtime.actions))

    def test_explicit_load_can_start_stopped_model(self):
        self.runtime.data['models']['qwen'].update(loaded=False,state='stopped',nodes=[])
        with patch.object(self.c,'sync_manager'):
            self.c.load('qwen')
        self.assertIn(('/upstream/qwen/health',{'expect_json':False}),self.runtime.actions)

    def test_inference_concurrency_limit_is_retained(self):
        with patch.object(self.c,'acquire',wraps=self.c.acquire) as acquire:
            _,token=self.c.llm_request('qwen')
        self.assertEqual(acquire.call_args.kwargs['limit'],4)
        self.c.release(token)
        tokens=[self.c.acquire('qwen',['spark02']) for _ in range(4)]
        self.assertIsNone(self.c.acquire('qwen',['spark02'],limit=4,wait=False,request_id='fifth'))
        for token in tokens:self.c.release(token)
        token=self.c.acquire('qwen',['spark02'],limit=4,wait=False,request_id='fifth')
        self.assertIsNotNone(token);self.c.release(token)

    def test_forced_unload_cancels_direct_inference(self):
        _,token=self.c.llm_request('qwen')
        cancel=Mock(side_effect=lambda:self.c.release(token))
        self.c.register_inference_cancel(token,cancel)
        self.runtime.cancel_native=Mock()
        self.runtime.force_stop_llm=Mock()
        self.c.unload('qwen',force=True)
        cancel.assert_called_once();self.runtime.force_stop_llm.assert_called_once_with('qwen')
        self.assertFalse(self.c.leases)

    def test_force_before_proxy_registration_cancels_before_request(self):
        _,token=self.c.llm_request('qwen')
        self.c.leases[token]['cancelled']=True
        cancel=Mock()
        self.c.register_inference_cancel(token,cancel)
        cancel.assert_called_once();self.c.release(token)

    def test_unreachable_manager_does_not_trigger_load(self):
        with patch.object(self.runtime,'manager',side_effect=SparkError('unavailable')):
            with self.assertRaisesRegex(SparkError,'Ask the human to switch the model'):
                self.c.llm_request('qwen')
        self.assertFalse(self.c.leases)
