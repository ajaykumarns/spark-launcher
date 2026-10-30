"""Explicit OCR service management over administrator SSH, independent of LLMs."""
from __future__ import annotations
import contextlib
import fcntl
import json
import os
from pathlib import Path
import re
import secrets
import shlex
import subprocess
import tempfile
import tomllib
from urllib.parse import urlsplit
from .catalog import SparkError, request_json
from .control import run_remote_json, safe_host

ACTIONS=('install','status','start','stop')


def remote_ocr(config,action,*,repo=None,ref=None,rebuild=False):
    root=config.get('remote_root','')
    if not Path(root).is_absolute():raise SparkError('Set an absolute remote_root in the client configuration.')
    payload={'action':action,'repo':repo,'ref':ref,'rebuild':rebuild,'gateway':config['gateway']}
    command=['ssh','-o','ConnectTimeout=10',safe_host(config.get('admin_ssh_host')),
             '~/.local/bin/sparky --json node ocr '+shlex.join(['--root',root,'--payload','-'])]
    return run_remote_json(command,payload=json.dumps(payload),timeout=7230 if action=='install' else 180)


def format_ocr(result):
    lines=['OCR service: '+result['state'], 'Installed: '+('yes' if result['installed'] else 'no')]
    if result.get('image'):lines.append('Image: '+result['image'])
    if result.get('running'):lines.append('API ready: '+('yes' if result.get('api_ready') else 'no'))
    if result.get('revision'):lines.append('Source revision: '+result['revision'])
    if result.get('log'):lines.append('Installation log on Spark01: '+result['log'])
    return '\n'.join(lines)


class OCRService:
    def __init__(self,root):
        self.root=Path(root)
        if not self.root.is_absolute():raise SparkError('The cluster root must be an absolute path.')
        self.home=self.root/'ocr';self.metadata=self.home/'sparky-install.json';self.log=None

    def record(self):
        try:return json.loads(self.metadata.read_text())
        except FileNotFoundError:return {}

    def source(self):return Path(self.record().get('source',str(self.home/'source')))

    def env_file(self,source=None):
        stable=self.home/'service.env'
        return stable if stable.exists() else (source or self.source())/'deploy/.env'

    def docker(self,*args,timeout=120,check=True):
        return self.run(['sudo','-n','docker',*args],timeout=timeout,check=check)

    def run(self,args,*,timeout=7200,check=True,env=None):
        if self.log:
            with self.log.open('a') as log:
                result=subprocess.run(args,stdout=log,stderr=subprocess.STDOUT,timeout=timeout,env=env,text=True)
        else:result=subprocess.run(args,capture_output=True,text=True,timeout=timeout,env=env)
        if check and result.returncode:
            suffix=' Check the private installation log: '+str(self.log) if self.log else ''
            raise SparkError('OCR '+Path(args[0]).name+' command failed.'+suffix)
        return result

    def inspect(self):
        # Never return Docker's environment, service tokens, or health log text.
        result=subprocess.run(['sudo','-n','docker','inspect','--format',
            '{"state":{{json .State}},"image":{{json .Config.Image}},"project":{{json (index .Config.Labels "com.docker.compose.project")}}}',
            'spark-ocr'],capture_output=True,text=True,timeout=30)
        if result.returncode:
            if 'no such' in (result.stderr or '').lower():return None
            raise SparkError('Cannot inspect OCR. Check Docker and administrator access on Spark01.')
        return json.loads(result.stdout)

    def status(self):
        info=self.inspect();source=self.source();record=self.record()
        installed=(source/'deploy/compose.yaml').is_file() and self.env_file(source).is_file()
        state=info['state'] if info else {}
        running=bool(state.get('Running'))
        ready=False
        if running:
            try:ready=request_json('http://127.0.0.1:8097/health',timeout=3).get('service')=='spark-ocr'
            except (SparkError,OSError,ValueError):pass
        return {'service':'ocr','host':'spark01','installed':installed,
                'state':state.get('Status','stopped' if installed else 'not_installed'),
                'running':running,'health':state.get('Health',{}).get('Status','unknown'),
                'api_ready':ready,'image':info.get('image') if info else None,
                'revision':record.get('revision'),'source':str(source)}

    def project(self):
        info=self.inspect();project=(info or {}).get('project') or self.record().get('project','spark-ocr')
        if not re.fullmatch(r'[a-z0-9][a-z0-9_-]*',project):raise SparkError('Invalid OCR Compose project name.')
        return project

    def compose(self,source,*args,project=None):
        env=dict(os.environ);env['OCR_SERVICE_ENV_FILE']=str(self.env_file(source))
        self.run(['sudo','-n','env','OCR_SERVICE_ENV_FILE='+env['OCR_SERVICE_ENV_FILE'],
            'docker','compose','--project-name',project or self.project(),
            '--env-file',str(self.env_file(source)),'-f',str(source/'deploy/compose.yaml'),*args],env=env,timeout=180)

    def start(self,source=None,project=None):
        source=source or self.source()
        if not (source/'deploy/compose.yaml').is_file() or not self.env_file(source).is_file():
            raise SparkError('OCR is not installed. Run sparky ocr install --repo URL first.')
        self.compose(source,'up','-d','--no-build','--no-deps','--wait','--wait-timeout','90','ocr',project=project)
        result=self.status()
        if not result['running'] or not result['api_ready']:raise SparkError('OCR did not become ready.')
        return result

    def stop(self):
        info=self.inspect()
        if info and info['state'].get('Running'):self.docker('stop','--time','30','spark-ocr',timeout=60)
        result=self.status()
        if result['running']:raise SparkError('OCR is still running.')
        return result

    def write(self,path,text):
        path.parent.mkdir(parents=True,exist_ok=True)
        fd,name=tempfile.mkstemp(dir=path.parent)
        try:
            with os.fdopen(fd,'w') as f:f.write(text)
            os.chmod(name,0o600);os.replace(name,path)
        finally:Path(name).unlink(missing_ok=True)

    @contextlib.contextmanager
    def lock(self):
        self.home.mkdir(parents=True,exist_ok=True)
        descriptor=os.open(self.home/'sparky.lock',os.O_CREAT|os.O_RDWR,0o600)
        try:
            try:fcntl.flock(descriptor,fcntl.LOCK_EX|fcntl.LOCK_NB)
            except BlockingIOError:raise SparkError('Another OCR control command is running.') from None
            yield
        finally:os.close(descriptor)

    def install(self,payload):
        previous=self.record();repo=payload.get('repo') or previous.get('repo')
        ref=payload.get('ref') or previous.get('ref') or 'main'
        validate_repository(repo,ref)
        before=self.status();old_source=self.source();project=self.project()
        stage=self.home/'releases'/secrets.token_hex(8);stage.mkdir(parents=True,mode=0o700)
        source=stage/'source';self.log=stage/'install.log';self.write(self.log,'OCR installation\n')
        self.run(['git','clone','--quiet','--no-checkout','--',repo,str(source)],timeout=300)
        self.run(['git','-C',str(source),'checkout','--quiet','--detach',ref],timeout=300)
        revision=subprocess.check_output(['git','-C',str(source),'rev-parse','HEAD'],text=True,timeout=30).strip()
        spec=tomllib.loads((source/'pyproject.toml').read_text())['project']
        if spec.get('name')!='spark-ocr' or not re.fullmatch(r'\d+\.\d+\.\d+',spec.get('version','')):
            raise SparkError('The repository is not a supported spark-ocr release.')
        if not (source/'scripts/ensure_bifrost.py').is_file():
            raise SparkError('Update the OCR repository revision. It lacks the Sparky registration script.')
        stable=self.home/'service.env';models=self.home/'models'
        if not stable.exists():
            old=self.env_file(old_source)
            if old.is_file():self.write(stable,old.read_text())
            else:
                gateway=payload.get('gateway','');url=urlsplit(gateway)
                if url.scheme!='https' or not url.hostname or url.username or url.password or url.path not in ('','/') or url.query or url.fragment or any(c in gateway for c in '\r\n'):
                    raise SparkError('Set an HTTPS gateway origin before installing OCR.')
                self.write(stable,'SPARK_OCR_TOKEN='+secrets.token_urlsafe(32)+'\nOCR_BIFROST_URL='+gateway.rstrip('/')+'\nOCR_MODELS_HOST_DIR='+str(models)+'\n')
        # Preserve a custom model cache from the existing private environment.
        from .control import read_env
        settings=read_env(stable);models=Path(settings['OCR_MODELS_HOST_DIR'])
        if not models.is_absolute():raise SparkError('OCR_MODELS_HOST_DIR must be absolute.')
        models.mkdir(parents=True,exist_ok=True)
        version=spec['version'];image='spark-ocr:'+version
        cached=self.docker('image','inspect',image,check=False,timeout=30).returncode==0
        if payload.get('rebuild') or not cached:
            self.docker('build','-f',str(source/'deploy/Dockerfile.indra'),'-t','spark-ocr-indra:'+version,str(source),timeout=7200)
            self.docker('build','-f',str(source/'deploy/Dockerfile'),'-t',image,'--build-arg','INDRA_IMAGE=spark-ocr-indra:'+version,str(source),timeout=7200)
        self.docker('run','--rm','--user',str(os.getuid())+':'+str(os.getgid()),
            '-e','HOME=/tmp','-e','HF_HUB_DISABLE_XET=1','-v',str(models)+':/models',image,'bootstrap',timeout=1800)
        # Source is staged separately. No existing source tree or credentials are overwritten.
        record={'repo':repo,'ref':ref,'revision':revision,'source':str(source),'project':project}
        saved=self.metadata.read_text() if self.metadata.exists() else None
        try:
            self.write(self.metadata,json.dumps(record,indent=2)+'\n')
            self.start(source,project)
            self.run(['python3',str(source/'scripts/ensure_bifrost.py'),'--gateway-root',str(self.root/'gateway'),
                      '--service-env',str(stable),'--provider-template',str(source/'config/bifrost.example.json')],timeout=180)
        except Exception:
            if saved is None:self.metadata.unlink(missing_ok=True)
            else:self.write(self.metadata,saved)
            if before['running']:
                try:self.start(old_source,project)
                except Exception:raise SparkError('OCR installation failed and recovery needs attention. Check '+str(self.log)) from None
            else:
                with contextlib.suppress(Exception):self.stop()
            raise
        return {**self.status(),'action':'install','image_reused':cached and not payload.get('rebuild'),
                'bifrost_registered':True,'log':str(self.log)}


def validate_repository(repo,ref):
    if not isinstance(repo,str) or any(c in repo for c in '\r\n'):
        raise SparkError('For the first install, give --repo with the private OCR repository URL.')
    url=urlsplit(repo)
    https=url.scheme=='https' and url.hostname and not (url.username or url.password or url.query or url.fragment)
    ssh=bool(re.fullmatch(r'git@[A-Za-z0-9_.-]+:[A-Za-z0-9_./-]+',repo))
    if not (https or ssh):raise SparkError('Use an HTTPS or git@host:path repository URL without credentials.')
    if not isinstance(ref,str) or not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9._/-]{0,199}',ref):
        raise SparkError('Use a branch, tag, or commit for --ref.')


def manage(root,payload):
    action=payload.get('action')
    if action not in ACTIONS:raise SparkError('Choose OCR install, status, start, or stop.')
    service=OCRService(root)
    if action=='status':return service.status()
    with service.lock():
        if action=='install':return service.install(payload)
        return {**getattr(service,action)(),'action':action}
