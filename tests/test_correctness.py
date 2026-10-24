"""Failure-path tests for configuration, transactions and device ownership."""
import copy
import json
import os
import signal
import sqlite3
import subprocess
import sys
import tempfile
import threading
import unittest
from pathlib import Path
from unittest.mock import Mock, patch

from spark_launcher.adapters import build_plan, child_env, check_passthrough
from spark_launcher.catalog import SparkError, atomic_write, fetch_catalog, make_catalog, validate_config
from spark_launcher.coordinator import Coordinator
from spark_launcher.lifecycle import run_model, stop_units
from spark_launcher.locking import file_lock
from spark_launcher.parameters import render_parameters
from spark_launcher.resources import validate_cluster, write_cluster, matrix_sets
from spark_launcher.resource_runtime import Runtime
from test_resources import configuration, FakeRuntime


class CorrectnessTests(unittest.TestCase):
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root=Path(self.temp.name)
        self.config=configuration(self.root)
        self.path=self.root/'models.json'
        write_cluster(self.path,self.config)
        self.runtime=FakeRuntime(self.config)
        self.coordinator=Coordinator(self.path,self.runtime)

    def test_process_exit_releases_lock_even_with_a_stale_file(self):
        path=self.root/'process.lock'
        path.touch()
        script=('from spark_launcher.locking import file_lock; import os,sys; '
                'lock=file_lock(sys.argv[1],"busy"); lock.__enter__(); os._exit(0)')
        subprocess.run([sys.executable,'-c',script,str(path)],check=True,timeout=5)
        with file_lock(path,'busy'):
            with self.assertRaisesRegex(SparkError,'busy'):
                with file_lock(path,'busy'):self.fail('Two owners acquired one lock')
        self.assertTrue(path.exists())
        with file_lock(path,'busy'):pass

    def test_worker_override_is_updated_with_base_python_arguments(self):
        path=self.root/'worker.py'
        path.write_text('args=["--max-model-len", "123"]\nargs[args.index("--max-model-len") + 1] = "64"\n')
        render_parameters({'parameters':{'context':456},'bindings':[{'path':str(path),'format':'python','fields':{'context':'--max-model-len'}}]})
        scope={};exec(path.read_text(),scope)
        self.assertEqual(scope['args'],['--max-model-len','456'])

    def test_each_bound_file_must_match_before_any_file_is_written(self):
        first=self.root/'recipe.sh';first.write_text('--context 123\n')
        second=self.root/'worker.sh';second.write_text('--different 123\n')
        bindings=[{'path':str(p),'format':'command','fields':{'context':'--context'}} for p in (first,second)]
        with self.assertRaises(SparkError):render_parameters({'parameters':{'context':456},'bindings':bindings})
        self.assertEqual(first.read_text(),'--context 123\n')
        self.assertEqual(second.read_text(),'--different 123\n')

    def test_recipe_write_failure_restores_files_already_written(self):
        first=self.root/'first.sh';second=self.root/'second.sh'
        for p in (first,second):p.write_text('--context 123\n')
        bindings=[{'path':str(p),'format':'command','fields':{'context':'--context'}} for p in (first,second)]
        failed=False
        def write(path,data,mode=0o600):
            nonlocal failed
            if Path(path)==second and not failed:
                failed=True;raise OSError('simulated full disk')
            return atomic_write(path,data,mode)
        with patch('spark_launcher.parameters.atomic_write',side_effect=write):
            with self.assertRaises(OSError):render_parameters({'parameters':{'context':456},'bindings':bindings})
        for p in (first,second):self.assertEqual(p.read_text(),'--context 123\n')

    def test_invalid_configurations_fail_before_device_operations(self):
        cases=[]
        def case(change):
            c=copy.deepcopy(self.config);change(c);cases.append(c)
        case(lambda c:c['models'].update({'../escape':c['models'].pop('qwen')}))
        case(lambda c:c['models']['qwen']['settings']['spark02'].update(units={'spark02':'--no-block'}))
        case(lambda c:c['models']['qwen']['settings']['spark02'].update(enabled='false'))
        case(lambda c:c['models']['qwen'].update(run_on=[]))
        case(lambda c:c['models']['qwen']['settings']['spark02'].update(parameters={'context_tokens':1}))
        case(lambda c:c['models']['qwen']['settings']['both'].update(startup_order=['spark01','spark01']))
        case(lambda c:c['models']['qwen']['settings']['spark02'].update(minimum_available_gib={'spark01':1}))
        case(lambda c:c['models']['qwen'].update(aliases=['NEMOTRON']))
        case(lambda c:c.update(auto={'targets':['missing'],'default':'missing'}))
        case(lambda c:c['models']['qwen'].update(output_tokens=True))
        for c in cases:
            with self.subTest(config=c):
                with self.assertRaises(SparkError):validate_cluster(c)
        self.assertEqual(self.runtime.actions,[])

    def test_disabled_model_does_not_break_catalog_or_compatibility(self):
        self.config['models']['nemotron']['settings']['spark01']['enabled']=False
        write_cluster(self.path,self.config)
        ids={row['id'] for row in self.coordinator.catalog()['data']}
        self.assertNotIn('garuda/nemotron',ids)
        self.assertIn('garuda/qwen',ids)
        self.assertTrue(all('nemotron' not in group for group in matrix_sets(self.config).values()))

    def test_auto_context_uses_only_its_actual_targets(self):
        self.config['models']['nemotron']['settings']['spark01']['context_tokens']=30000
        self.config['auto']={'targets':['qwen'],'default':'qwen'}
        write_cluster(self.path,self.config)
        rows=self.coordinator.catalog()['data']
        auto=next(row for row in rows if row['id']=='garuda/spark-auto')
        self.assertEqual(auto['context_length'],123456)
        client=validate_config({'version':1,'gateway':'https://example.com'})
        catalog=make_catalog(client,{'data':rows})
        self.assertEqual(next(m for m in catalog['models'] if m['id']=='garuda/spark-auto')['context_tokens'],123456)

    def test_many_models_need_only_pairwise_device_checks(self):
        template=self.config['models']['qwen']
        self.config['models']={}
        for index in range(40):
            model=copy.deepcopy(template);model['run_on']='spark01' if index<20 else 'spark02'
            self.config['models']['model'+str(index)]=model
        self.assertEqual(len(matrix_sets(self.config)),400)

    def test_runtime_uses_model_units_instead_of_stale_node_service_lists(self):
        runtime=Runtime(self.config,self.path)
        self.config['nodes']['spark01']['units']={'qwen':['stale.service']}
        self.config['nodes']['spark02']['ssh']='worker'
        def probe(meta):
            self.assertEqual(meta['units']['qwen'],['qwen.service'])
            return {'units':{'nemotron.service':'active'},'comfyui':{'reachable':False},'containers':{}}
        worker={'units':{'qwen.service':'active'},'comfyui':{'reachable':False},'containers':{}}
        def command(args,**kwargs):
            meta=json.loads(kwargs['input'])
            self.assertEqual(meta['units']['qwen'],['qwen.service'])
            self.assertIn('--file -',args[-1])
            return subprocess.CompletedProcess(args,0,json.dumps(worker))
        with patch('spark_launcher.resource_runtime.probe',side_effect=probe),patch('spark_launcher.resource_runtime.command',side_effect=command),patch.object(runtime,'manager',return_value={'running':[]}):
            data=runtime.snapshot(True)
        self.assertEqual(data['models']['qwen']['nodes'],['spark02'])
        self.assertTrue(data['models']['qwen']['loaded'])

    def test_unload_reserves_actual_device_after_placement_edit(self):
        self.config['models']['qwen']['run_on']='spark01'
        write_cluster(self.path,self.config)
        with patch.object(self.coordinator,'acquire',wraps=self.coordinator.acquire) as acquire:
            self.coordinator.unload('qwen')
        self.assertEqual(set(acquire.call_args.args[1]),{'spark01','spark02'})

    def manager_config(self):
        import yaml
        folder=self.root/'manager';folder.mkdir()
        data={'globalTTL':10800,'models':{name:{'proxy':m['settings'][m['run_on']]['backend_url'],'concurrencyLimit':4,'capabilities':{'context':123456}} for name,m in self.config['models'].items() if m['kind']=='llm'}}
        path=folder/'config.yaml';path.write_text(yaml.safe_dump(data))
        self.config['policy']['idle_ttl_seconds']=123
        write_cluster(self.path,self.config)
        return path

    def test_manager_update_applies_idle_policy_and_preserves_existing_fence(self):
        import yaml
        path=self.manager_config();self.coordinator.fenced=True
        with patch('spark_launcher.coordinator.command',return_value=subprocess.CompletedProcess([],0)),patch('spark_launcher.coordinator.time.sleep'):
            self.coordinator.sync_manager()
        self.assertEqual(yaml.safe_load(path.read_text())['globalTTL'],123)
        self.assertTrue(self.coordinator.fenced)

    def test_fence_set_during_reload_is_not_cleared_by_reload(self):
        self.manager_config()
        def command(*args,**kwargs):
            self.coordinator.fenced=True
            return subprocess.CompletedProcess([],0)
        with patch('spark_launcher.coordinator.command',side_effect=command),patch('spark_launcher.coordinator.time.sleep'):
            self.coordinator.sync_manager()
        self.assertTrue(self.coordinator.fenced);self.assertFalse(self.coordinator.reloading)

    def test_interrupted_lease_drain_restores_fence_without_touching_config(self):
        path=self.manager_config();before=path.read_bytes()
        token=self.coordinator.acquire('qwen',['spark02'])
        with patch.object(self.coordinator.cv,'wait',side_effect=OSError('interrupted')):
            with self.assertRaises(OSError):self.coordinator.sync_manager()
        self.assertFalse(self.coordinator.fenced)
        self.assertEqual(path.read_bytes(),before)
        self.coordinator.release(token)

    def test_protected_llm_blocks_implicit_eviction(self):
        self.config['models']['qwen']['keep_loaded']=True
        write_cluster(self.path,self.config);self.coordinator.refresh()
        with self.assertRaisesRegex(SparkError,'qwen must stay loaded'):
            self.coordinator.prepare_llm('deepseek')
        self.assertEqual(self.runtime.actions,[])

    def test_gateway_rollback_restores_files_even_when_provider_restore_fails(self):
        from spark_launcher.deployment import connect_gateway
        gateway=self.root/'gateway';(gateway/'data').mkdir(parents=True)
        (gateway/'gateway.env').write_text('BIFROST_ADMIN_USER=test\nBIFROST_ADMIN_PASSWORD=fixture-only\n')
        self.config['nodes']['spark01']['tailscale_ip']='192.0.2.10'
        original='listen 192.0.2.10:8080;\nlisten 127.0.0.1:8082 proxy_protocol;\n'
        nginx=gateway/'nginx.conf';nginx.write_text(original)
        before={'network_config':{'base_url':'http://127.0.0.1:8000'}}
        static=gateway/'data/config.json';static.write_text(json.dumps({'providers':{'garuda':before}}));old_static=static.read_bytes()
        backup=self.root/'backup'
        with patch('spark_launcher.deployment.request_json',side_effect=[before,{},SparkError('provider unavailable')]),patch('spark_launcher.deployment.command',side_effect=[SparkError('nginx check failed'),None,None]):
            with self.assertRaisesRegex(SparkError,'Rollback needs attention'):connect_gateway(self.config,backup)
        self.assertEqual(nginx.read_text(),original);self.assertEqual(static.read_bytes(),old_static)

    def test_placement_cannot_change_between_reservation_and_startup(self):
        entered=threading.Event();finish=threading.Event();changed=threading.Event();errors=[];result=[]
        def prepare(name):
            entered.set()
            if not finish.wait(2):raise RuntimeError('test startup timeout')
        def request():
            try:result.append(self.coordinator.llm_request('qwen'))
            except BaseException as error:errors.append(error)
        def edit():
            self.coordinator.set_placement('qwen','both');changed.set()
        with patch.object(self.coordinator,'sync_manager'),patch.object(self.coordinator,'reconcile_jobs'),patch.object(self.coordinator,'prepare_llm',side_effect=prepare):
            a=threading.Thread(target=request);a.start()
            self.assertTrue(entered.wait(1))
            b=threading.Thread(target=edit);b.start()
            try:self.assertFalse(changed.wait(.05))
            finally:finish.set();a.join(2);b.join(2)
        self.assertFalse(a.is_alive());self.assertFalse(b.is_alive());self.assertEqual(errors,[])
        self.assertEqual(self.coordinator.leases[result[0][1]]['nodes'],['spark02'])
        self.coordinator.release(result[0][1])

    def test_audit_failure_does_not_write_another_http_response_after_release(self):
        token=self.coordinator.acquire('qwen',['spark02'])
        self.runtime.event=Mock(side_effect=sqlite3.OperationalError('database is locked'))
        self.coordinator.release(token)
        self.assertNotIn(token,self.coordinator.leases)

    def test_shutdown_attempts_every_rank_when_one_node_fails(self):
        runtime=Mock();runtime.unit.side_effect=[SparkError('offline'),None]
        with self.assertRaises(SparkError):stop_units(runtime,{'nodes':['spark01','spark02'],'units':{'spark01':'head.service','spark02':'worker.service'}})
        self.assertEqual(runtime.unit.call_count,2)

    def test_stop_uses_actual_placement_even_if_new_recipe_is_disabled(self):
        (self.root/'manager').mkdir();(self.root/'controller').mkdir()
        self.config['models']['qwen']['settings']['spark02']['enabled']=False
        write_cluster(self.path,self.config)
        (self.root/'controller/running-qwen.json').write_text(json.dumps({'nodes':['spark01'],'units':{'spark01':'old.service'}}))
        runtime=Mock()
        with patch('spark_launcher.lifecycle.Runtime',return_value=runtime):run_model(self.path,'stop','qwen')
        runtime.unit.assert_called_once_with('spark01','stop','old.service')

    def test_sigterm_during_startup_cleans_up_partial_distributed_model(self):
        (self.root/'manager').mkdir()
        self.config['models']['qwen']['run_on']='both'
        self.config['nodes']['spark02']['ssh']='worker'
        write_cluster(self.path,self.config)
        runtime=Mock()
        runtime.snapshot.return_value={'models':{},'comfyui':{n:{'running':0} for n in ('spark01','spark02')}}
        calls=[]
        def unit(node,action,name):
            calls.append((node,action,name))
            if action=='start':signal.getsignal(signal.SIGTERM)(signal.SIGTERM,None)
        runtime.unit.side_effect=unit
        previous=signal.getsignal(signal.SIGTERM)
        with patch('spark_launcher.lifecycle.Runtime',return_value=runtime),patch('spark_launcher.parameters.render_parameters'),patch('subprocess.run',return_value=subprocess.CompletedProcess([],0)):
            run_model(self.path,'run','qwen')
        self.assertEqual(sum(action=='start' for _,action,_ in calls),1)
        self.assertEqual({node for node,action,_ in calls if action=='stop'},{'spark01','spark02'})
        self.assertIs(signal.getsignal(signal.SIGTERM),previous)

    def test_unused_client_model_settings_are_rejected(self):
        with self.assertRaisesRegex(SparkError,'unused client models'):
            validate_config({'version':1,'gateway':'https://example.com','models':{'qwen':{'context':1}}})

    def test_corrupt_catalog_shapes_fail_with_a_safe_error(self):
        c=validate_config({'version':1,'gateway':'https://example.com'})
        for wire in ([],{'data':[None]},{'data':[{'id':'garuda/test','context_length':20000,'aliases':'test'}]},{'data':[{'id':'garuda/test','context_length':20000,'output_tokens':'100'}]}):
            with self.subTest(wire=wire):
                with self.assertRaises(SparkError):make_catalog(c,wire)
        with patch.dict(os.environ,{'SPARKY_STATE_DIR':str(self.root)}):
            (self.root/'catalog.json').write_text('[]')
            with self.assertRaisesRegex(SparkError,'No usable cached'):fetch_catalog(c,offline=True)

    def test_opencode_keeps_existing_process_mcp_and_other_providers(self):
        c=validate_config({'version':1,'gateway':'https://example.com','binaries':{'opencode':'/usr/bin/true'}})
        model={'id':'garuda/test','context_tokens':20000}
        plan=build_plan(c,{'models':[model]},model,'opencode',self.root,'test')
        original={'provider':{'other':{'name':'Keep'}},'mcp':{'local':{'command':['tool']}},'enabled_providers':['other']}
        custom=self.root/'opencode.json'
        custom.write_text('{"mcp":{"custom":{"enabled":false,"command":["tool"],"type":"local"}}}')
        with patch.dict(os.environ,{'OPENCODE_CONFIG_CONTENT':json.dumps(original),'OPENCODE_CONFIG':str(custom)}):
            env=child_env(plan)
            child=json.loads(env['OPENCODE_CONFIG_CONTENT'])
        self.assertEqual(env['OPENCODE_CONFIG'],str(custom))
        self.assertEqual(child['mcp'],original['mcp']);self.assertEqual(child['provider']['other'],original['provider']['other'])
        self.assertEqual(child['enabled_providers'],['other','spark'])

    def test_prompt_delimiter_and_compact_model_flags(self):
        check_passthrough('codex',['exec','--','--model'])
        for args in (['-mcloud'],['-c','review_model="cloud"']):
            with self.assertRaises(SparkError):check_passthrough('codex',args)


if __name__=='__main__':unittest.main()
