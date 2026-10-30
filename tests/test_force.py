"""Forced lifecycle operations cancel blockers without waiting behind their locks."""
import contextlib
import io
import json
import tempfile
import threading
import time
import unittest
from pathlib import Path
from unittest.mock import Mock, patch
from urllib.error import HTTPError

from spark_launcher.catalog import SparkError
from spark_launcher.cli import parser, run, node_action
from spark_launcher.control import remote_command, node_resources
from spark_launcher.coordinator import Coordinator
from spark_launcher.resource_runtime import Runtime
from spark_launcher.resources import write_cluster
from test_resources import configuration, FakeRuntime


class ForceTests(unittest.TestCase):
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory();self.addCleanup(self.temp.cleanup)
        self.root=Path(self.temp.name);self.config=configuration(self.root)
        self.path=self.root/'models.json';write_cluster(self.path,self.config)
        self.runtime=FakeRuntime(self.config);self.c=Coordinator(self.path,self.runtime)
        self.cancelled=[];self.stopped=[]
        self.runtime.cancel_native=lambda names:self.cancelled.append(set(names))
        def stop(name):
            self.stopped.append(name)
            self.runtime.data['models'].setdefault(name,{'kind':'llm'}).update(loaded=False,nodes=[],state='stopped')
        self.runtime.force_stop_llm=stop

    def test_force_unload_cancels_native_and_stops_only_target(self):
        result=self.c.unload('qwen',force=True)
        self.assertEqual(result,{'status':'unloaded','workload':'qwen','forced':True})
        self.assertEqual(self.cancelled,[{'qwen'}]);self.assertEqual(self.stopped,['qwen'])
        self.assertEqual(self.c.forcing_nodes,set());self.assertFalse(self.c.force_lock.locked())

    def test_force_load_stops_conflicts_then_starts_requested_model(self):
        with patch.object(self.c,'sync_manager'):
            result=self.c.load('deepseek',force=True)
        self.assertEqual(result,{'status':'ready','workload':'deepseek','forced':True})
        self.assertEqual(set(self.stopped),{'qwen','nemotron','deepseek'})
        self.assertIn(('/upstream/deepseek/health',{'expect_json':False}),self.runtime.actions)
        self.assertFalse(self.c.leases)

    def test_disjoint_model_is_not_cancelled_or_stopped(self):
        with patch.object(self.c,'sync_manager'):
            self.c.load('qwen',force=True)
        self.assertEqual(self.cancelled,[{'qwen'}]);self.assertEqual(self.stopped,['qwen'])

    def test_force_breaks_waiting_load_that_owns_reconfigure_lock(self):
        active=self.c.acquire('qwen',['spark02'])
        pending=threading.Event();errors=[]
        original=self.c.acquire
        def acquire(*args,**kwargs):
            if args[0]=='deepseek':pending.set()
            return original(*args,**kwargs)
        self.c.acquire=acquire
        def waiting_load():
            try:self.c.load('deepseek')
            except SparkError as error:errors.append(str(error))
        with patch.object(self.c,'sync_manager'):
            thread=threading.Thread(target=waiting_load,daemon=True);thread.start()
            self.assertTrue(pending.wait(1))
            def cancel(names):
                self.cancelled.append(set(names));self.c.release(active)
            self.runtime.cancel_native=cancel
            result=self.c.unload('qwen',force=True)
            thread.join(1)
        self.assertFalse(thread.is_alive());self.assertTrue(errors)
        self.assertEqual(result['status'],'unloaded');self.assertFalse(self.c.pending)
        self.assertFalse(any(path=='/upstream/deepseek/health' for path,_ in self.runtime.actions))

    def test_new_conflicting_requests_cannot_enter_during_force(self):
        errors=[];other=[]
        def cancel(names):
            def contender():
                try:self.c.acquire('qwen',['spark02'])
                except SparkError as error:errors.append(str(error))
                other.append(self.c.acquire('nemotron',['spark01']))
            thread=threading.Thread(target=contender);thread.start();thread.join(1)
            self.assertFalse(thread.is_alive())
        self.runtime.cancel_native=cancel
        self.c.unload('qwen',force=True)
        self.assertEqual(len(errors),1);self.c.release(other[0])

    def test_force_rejects_protected_models_before_cancellation(self):
        self.config['models']['qwen']['keep_loaded']=True;write_cluster(self.path,self.config)
        with self.assertRaisesRegex(SparkError,'protected'):
            self.c.load('deepseek',force=True)
        self.assertFalse(self.cancelled);self.assertFalse(self.stopped)
        self.assertFalse(self.c.force_lock.locked())
        with self.assertRaisesRegex(SparkError,'must stay loaded'):self.c.unload('qwen',force=True)

    def test_force_rejects_audio_and_active_image_work(self):
        with self.assertRaisesRegex(SparkError,'language models only'):self.c.load('parakeet',force=True)
        token=self.c.acquire('comfyui@spark02',['spark02'],exclusive=True)
        with self.assertRaisesRegex(SparkError,'image job'):self.c.load('deepseek',force=True)
        self.c.release(token);self.assertFalse(self.cancelled);self.assertFalse(self.stopped)

    def test_force_rejects_native_image_work_without_a_lease(self):
        self.runtime.data['comfyui']['spark02'].update(executing=True,phase='executing',running=1)
        with self.assertRaisesRegex(SparkError,'image job'):self.c.load('deepseek',force=True)
        self.assertFalse(self.cancelled);self.assertFalse(self.stopped)

    def test_failed_cancellation_restores_admission(self):
        self.runtime.cancel_native=Mock(side_effect=SparkError('cancel failed'))
        with self.assertRaisesRegex(SparkError,'cancel failed'):self.c.unload('qwen',force=True)
        self.assertFalse(self.c.forcing_nodes);self.assertFalse(self.c.force_lock.locked())
        token=self.c.acquire('qwen',['spark02']);self.c.release(token)
        self.assertFalse(self.stopped)

    def test_stuck_request_has_a_bounded_drain(self):
        token=self.c.acquire('qwen',['spark02'])
        with patch('spark_launcher.coordinator.time.monotonic',side_effect=[0,31]):
            with self.assertRaisesRegex(SparkError,'30 seconds'):self.c.unload('qwen',force=True)
        self.assertFalse(self.stopped);self.c.release(token)
        self.assertFalse(self.c.forcing_nodes)

    def test_normal_unload_does_not_cancel_native_requests(self):
        self.c.unload('qwen');self.assertFalse(self.cancelled);self.assertFalse(self.stopped)
        self.assertEqual(self.runtime.actions,[('/api/models/unload/qwen',{'method':'POST','expect_json':False})])

    def test_stuck_configuration_lock_is_bounded_and_restores_admission(self):
        self.c.reconfigure=Mock()
        self.c.reconfigure.acquire.return_value=False
        with self.assertRaisesRegex(SparkError,'configuration operation'):
            self.c.unload('qwen',force=True)
        self.c.reconfigure.acquire.assert_called_once_with(timeout=30)
        self.assertFalse(self.stopped);self.assertFalse(self.c.forcing_nodes)

    def test_overlapping_force_is_rejected(self):
        self.c.force_lock.acquire()
        try:
            with self.assertRaisesRegex(SparkError,'Another forced'):self.c.unload('qwen',force=True)
        finally:self.c.force_lock.release()
        self.assertFalse(self.cancelled)

    def test_flag_reaches_private_node_and_controller(self):
        config={'admin_ssh_host':'head','remote_root':'/srv/cluster'}
        command=remote_command(config,'unload','qwen',force=True)
        self.assertIn('--force',command[-1]);self.assertIn('--model qwen',command[-1])
        args=parser().parse_args(['node','unload','--root',str(self.root),'--model','qwen','--force'])
        with patch('spark_launcher.cli.node_resources',return_value={}) as call:node_action(args)
        call.assert_called_once_with(str(self.root),'unload','qwen',force=True)
        (self.root/'config').mkdir();write_cluster(self.root/'config/models.json',self.config)
        with patch('spark_launcher.control.controller_request',return_value={}) as call:
            node_resources(self.root,'unload','qwen',force=True)
        call.assert_called_once_with(self.root,'unload',{'model':'qwen','force':True})

    def test_json_force_does_not_run_a_blocking_plan_or_print_progress(self):
        with patch('spark_launcher.cli.load_config',return_value={}),patch('spark_launcher.cli.remote_rpc') as plan,patch('spark_launcher.cli.remote_result',return_value={'status':'unloaded','forced':True}) as call,contextlib.redirect_stdout(io.StringIO()) as out,contextlib.redirect_stderr(io.StringIO()) as err:
            self.assertEqual(run(['unload','qwen','--force','--json']),0)
        plan.assert_not_called();call.assert_called_once_with({},'unload','qwen',force=True)
        self.assertTrue(json.loads(out.getvalue())['forced']);self.assertEqual(err.getvalue(),'')


class NativeCancellationTests(unittest.TestCase):
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory();self.addCleanup(self.temp.cleanup)
        self.root=Path(self.temp.name);self.config=configuration(self.root)
        self.config['manager_url']='http://127.0.0.1:8000'
        self.runtime=Runtime(self.config,self.root/'models.json');self.runtime.key=lambda:'fixture-key'

    def test_cancellation_selects_model_ids_and_tolerates_a_completed_request(self):
        rows=[{'id':'1','model':'qwen'},{'id':'2','model':'nemotron'}]
        with patch.object(self.runtime,'native_requests',return_value=rows),patch('spark_launcher.resource_runtime.urlopen',side_effect=HTTPError('http://fixture',404,'finished',{},None)) as urlopen:
            self.runtime.cancel_native({'qwen'})
        request=urlopen.call_args.args[0]
        self.assertEqual(request.full_url,'http://127.0.0.1:8000/api/inflight/1/cancel')
        self.assertEqual(request.method,'POST');self.assertEqual(urlopen.call_count,1)

    def test_failed_cancel_is_reported(self):
        with patch.object(self.runtime,'native_requests',return_value=[{'id':'1','model':'qwen'}]),patch('spark_launcher.resource_runtime.urlopen',side_effect=HTTPError('http://fixture',500,'failed',{},None)):
            with self.assertRaisesRegex(SparkError,'could not cancel'):self.runtime.cancel_native({'qwen'})

    def test_force_stop_uses_recorded_placement_and_clears_manager(self):
        path=self.root/'controller/running-qwen.json';path.parent.mkdir()
        info={'nodes':['spark01'],'units':{'spark01':'old-qwen.service'}};path.write_text(json.dumps(info))
        with patch('spark_launcher.lifecycle.stop_units') as stop,patch.object(self.runtime,'manager') as manager:
            self.runtime.force_stop_llm('qwen')
        stop.assert_called_once_with(self.runtime,info)
        manager.assert_called_once_with('/api/models/unload/qwen',method='POST',expect_json=False,timeout=30)


if __name__=='__main__':unittest.main()
