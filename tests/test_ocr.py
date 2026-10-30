import contextlib
import io
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import Mock,patch
from spark_launcher.catalog import SparkError
from spark_launcher.cli import parser,run,node_action
from spark_launcher.ocr import OCRService,manage,remote_ocr,validate_repository


class OCRTests(unittest.TestCase):
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory();self.addCleanup(self.temp.cleanup)
        self.root=Path(self.temp.name);self.service=OCRService(self.root)
        self.config={'gateway':'https://spark.example','remote_root':str(self.root),'admin_ssh_host':'head-admin'}

    def installed(self):
        source=self.root/'ocr/source';(source/'deploy').mkdir(parents=True)
        (source/'deploy/compose.yaml').write_text('services: {}')
        (source/'deploy/.env').write_text('SPARK_OCR_TOKEN=private\n')
        return source

    def test_nested_json_flags_and_install_options(self):
        for words in [['--json','ocr','status'],['ocr','--json','status'],['ocr','status','--json']]:
            self.assertTrue(parser().parse_args(words).json)
        args=parser().parse_args(['ocr','install','--repo','git@git.example:user/ocr.git','--ref','deadbeef','--rebuild'])
        self.assertEqual(args.ocr_action,'install');self.assertTrue(args.rebuild)

    def test_cli_routes_only_to_ocr_ssh_protocol(self):
        result={'installed':True,'state':'exited'}
        with patch('spark_launcher.cli.load_config',return_value=self.config),patch('spark_launcher.ocr.remote_ocr',return_value=result) as remote,contextlib.redirect_stdout(io.StringIO()) as output:
            self.assertEqual(run(['ocr','stop','--json']),0)
        self.assertEqual(json.loads(output.getvalue()),result)
        self.assertEqual(remote.call_args.args[1],'stop')

    def test_remote_payload_uses_stdin_and_quotes_root(self):
        config={**self.config,'remote_root':str(self.root/"space; literal")}
        with patch('spark_launcher.ocr.run_remote_json',return_value={}) as remote:
            remote_ocr(config,'install',repo='git@git.example:user/ocr.git',ref='main')
        command=remote.call_args.args[0];payload=json.loads(remote.call_args.kwargs['payload'])
        self.assertEqual(command[:4],['ssh','-o','ConnectTimeout=10','head-admin'])
        self.assertIn("'"+config['remote_root']+"'",command[4])
        self.assertNotIn('git@git.example',command[4]);self.assertEqual(payload['action'],'install')

    def test_bad_repository_urls_and_refs_are_rejected(self):
        for repo,ref in [('https://user:secret@git.example/ocr','main'),('-bad','main'),('https://git.example/ocr','--bad'),('https://git.example/ocr\nX=bad','main')]:
            with self.assertRaises(SparkError):validate_repository(repo,ref)

    def test_status_does_not_start_or_download_anything(self):
        with patch.object(self.service,'inspect',return_value=None),patch.object(self.service,'run') as execute:
            self.assertEqual(self.service.status()['state'],'not_installed')
            execute.assert_not_called();self.assertFalse(self.service.home.exists())

    def test_docker_failure_is_not_reported_as_stopped(self):
        with patch('spark_launcher.ocr.subprocess.run',return_value=subprocess.CompletedProcess([],1,'','permission denied')):
            with self.assertRaises(SparkError):self.service.inspect()

    def test_status_does_not_expose_health_logs_or_credentials(self):
        self.installed()
        info={'state':{'Status':'running','Running':True,'Health':{'Status':'healthy','Log':[{'Output':'private-token'}]}},'image':'spark-ocr:0.2.0'}
        with patch.object(self.service,'inspect',return_value=info),patch('spark_launcher.ocr.request_json',return_value={'service':'spark-ocr'}):
            result=self.service.status()
        self.assertTrue(result['api_ready']);self.assertNotIn('private',json.dumps(result))

    def test_stop_targets_only_the_ocr_container(self):
        with patch.object(self.service,'inspect',return_value={'state':{'Running':True}}),patch.object(self.service,'docker') as docker,patch.object(self.service,'status',return_value={'running':False}):
            self.service.stop()
        self.assertEqual(docker.call_args.args,('stop','--time','30','spark-ocr'))

    def test_start_cannot_build_or_start_dependencies(self):
        source=self.installed()
        with patch.object(self.service,'compose') as compose,patch.object(self.service,'status',return_value={'running':True,'api_ready':True}):self.service.start()
        self.assertEqual(compose.call_args.args[0],source)
        self.assertIn('--no-build',compose.call_args.args);self.assertIn('--no-deps',compose.call_args.args)
        self.assertEqual(compose.call_args.args[-1],'ocr')

    def test_compose_keeps_the_existing_project_and_private_env(self):
        source=self.installed()
        with patch.object(self.service,'project',return_value='deploy'),patch.object(self.service,'run') as execute:
            self.service.compose(source,'stop','ocr')
        args=execute.call_args.args[0]
        self.assertEqual(args[args.index('--project-name')+1],'deploy')
        self.assertIn('OCR_SERVICE_ENV_FILE='+str(source/'deploy/.env'),args)
        self.assertNotIn('private',json.dumps(args))

    def test_control_lock_prevents_parallel_install_and_stop(self):
        with self.service.lock():
            with self.assertRaises(SparkError):
                with OCRService(self.root).lock():pass

    def test_failed_download_keeps_existing_install_and_credentials(self):
        source=self.installed();self.service.write(self.service.metadata,json.dumps({'source':str(source),'repo':'https://git.example/ocr','ref':'main'}))
        before=self.service.metadata.read_bytes();credentials=(source/'deploy/.env').read_bytes()
        with patch.object(self.service,'status',return_value={'running':True}),patch.object(self.service,'project',return_value='deploy'),patch.object(self.service,'run',side_effect=SparkError('download failed')):
            with self.assertRaises(SparkError):self.service.install({})
        self.assertEqual(self.service.metadata.read_bytes(),before)
        self.assertEqual((source/'deploy/.env').read_bytes(),credentials)

class InstallTests(unittest.TestCase):
    setUp=OCRTests.setUp
    installed=OCRTests.installed
    def prepare_install(self):
        source=self.installed()
        (source/'deploy/.env').write_text('SPARK_OCR_TOKEN=keep-this-token\nOCR_BIFROST_URL=https://spark.example\nOCR_MODELS_HOST_DIR='+str(self.root/'retained-models')+'\n')
        record={'source':str(source),'repo':'https://git.example/ocr','ref':'main','project':'deploy'}
        self.service.write(self.service.metadata,json.dumps(record))
        before=self.service.metadata.read_bytes()
        def command(args,**kwargs):
            if args[:2]==['git','clone']:
                dest=Path(args[-1]);(dest/'scripts').mkdir(parents=True)
                (dest/'scripts/ensure_bifrost.py').write_text('# registration')
                (dest/'pyproject.toml').write_text('[project]\nname="spark-ocr"\nversion="0.2.0"\n')
            return subprocess.CompletedProcess(args,0,'','')
        return source,before,command

    def test_install_reuses_images_and_preserves_credentials_and_cache(self):
        source,before,command=self.prepare_install()
        with patch.object(self.service,'status',return_value={'running':True,'installed':True,'state':'running'}),patch.object(self.service,'project',return_value='deploy'),patch.object(self.service,'run',side_effect=command),patch.object(self.service,'docker',return_value=subprocess.CompletedProcess([],0)) as docker,patch.object(self.service,'start'),patch('spark_launcher.ocr.subprocess.check_output',return_value='a'*40):
            result=self.service.install({})
        self.assertTrue(result['image_reused']);self.assertTrue(result['bifrost_registered'])
        self.assertIn('keep-this-token',(self.service.home/'service.env').read_text())
        self.assertNotIn('keep-this-token',json.dumps(result))
        calls=[call.args for call in docker.call_args_list]
        self.assertFalse(any(args[0]=='build' for args in calls))
        bootstrap=next(args for args in calls if args[0]=='run')
        self.assertIn(str(self.root/'retained-models')+':/models',bootstrap)
        self.assertTrue((source/'deploy/.env').exists())

    def test_failed_activation_restores_the_old_source_record(self):
        source,before,command=self.prepare_install()
        with patch.object(self.service,'status',return_value={'running':True}),patch.object(self.service,'project',return_value='deploy'),patch.object(self.service,'run',side_effect=command),patch.object(self.service,'docker',return_value=subprocess.CompletedProcess([],0)),patch.object(self.service,'start',side_effect=[SparkError('not ready'),{'running':True}]) as start,patch('spark_launcher.ocr.subprocess.check_output',return_value='a'*40):
            with self.assertRaises(SparkError):self.service.install({})
        self.assertEqual(self.service.metadata.read_bytes(),before)
        self.assertEqual(start.call_args_list[-1].args,(source,'deploy'))

if __name__=='__main__':unittest.main()
