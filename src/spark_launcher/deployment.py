"""Narrow node installation. Preserve engines, credentials and active work."""
from __future__ import annotations
import base64
from importlib.resources import files
import copy
import json
import os
import shlex
import shutil
import subprocess
import time
from pathlib import Path
from .catalog import SparkError, atomic_write, request_json
from .control import read_env
from .resources import read_cluster, write_cluster
from .resource_runtime import Runtime, command
from .gateway import mutable_provider


def install_file(source,target,mode=0o644):
    command(['sudo','-n','install','-D','-m',oct(mode)[2:],str(source),str(target)])


def install_head(stage):
    stage=Path(stage)
    candidate=read_cluster(stage/'models.json')
    root=Path(candidate['root']);folder=root/'controller';folder.mkdir(mode=0o700,exist_ok=True)
    backup=folder/'backups'/('install-'+str(time.time_ns()));backup.mkdir(mode=0o700,parents=True)
    config_path=root/'config/models.json'
    if config_path.exists():
        candidate=read_cluster(config_path)
    else:write_cluster(config_path,candidate)
    config=candidate
    runtime=Runtime(config,config_path)
    # The original containers are the preservation baseline. Do not infer
    # memory residency solely from a container's running state.
    for node,meta in config['nodes'].items():
        data=json.dumps(meta,indent=2)+'\n'
        if node=='spark01':atomic_write(folder/'node-spark01.json',data)
        else:upload_worker(config,folder/'node-spark02.json',data.encode(),0o600)
    before=runtime.snapshot(True)
    print("Device baseline saved.",flush=True)
    atomic_write(backup/'services-before.json',json.dumps(before,indent=2)+'\n')
    for node,meta in config['nodes'].items():
        data=json.dumps(meta,indent=2)+'\n'
        if node=='spark01':atomic_write(folder/'node-spark01.json',data)
        else:
            upload_worker(config,folder/'node-spark02.json',data.encode(),0o600)
        plugin_config={'node':node,'controller':('http://127.0.0.1:8020' if node=='spark01' else 'http://'+config['nodes']['spark01']['direct_ip']+':8020'),
                       'operators':['127.0.0.1','::1',meta['tailscale_ip'],config['nodes']['spark01']['tailscale_ip'],config['nodes']['spark01']['direct_ip']]}
        plugin_path=folder/('comfy-'+node+'.json')
        body=json.dumps(plugin_config)+'\n'
        guard_path=Path(meta['comfy_root'])/'custom_nodes/SparkyGuard/__init__.py'
        plugin_source=files('spark_launcher').joinpath('comfy_guard.py').read_bytes()
        if node=='spark01':
            atomic_write(plugin_path,body)
            atomic_write(guard_path,plugin_source,0o644)
        else:
            upload_worker(config,plugin_path,body.encode(),0o600)
            upload_worker(config,guard_path,plugin_source,0o644)
        dropin='[Service]\nEnvironment=SPARKY_COMFY_CONFIG='+str(plugin_path)+'\nExecStop=\nExecStop='+str(Path.home()/'.local/bin/sparky')+' node drain-comfy --file '+str(folder/('node-'+node+'.json'))+'\nTimeoutStopSec=infinity\n'
        install_unit_text(config,node,'/etc/systemd/system/'+meta['comfy_unit']+'.d/sparky.conf',dropin,stage)
    print("Node files installed.",flush=True)
    prepare_qwen_dual(config,stage)
    print("Inactive Qwen two-device recipe prepared.",flush=True)
    unit='''[Unit]
Description=Sparky device and model controller
After=network-online.target garuda-models.service
Wants=network-online.target
[Service]
Type=simple
User=%s
WorkingDirectory=%s
ExecStart=%s node controller --file %s
Restart=on-failure
RestartSec=3
UMask=0077
[Install]
WantedBy=multi-user.target
'''%(Path.home().name,root,Path.home()/'.local/bin/sparky',config_path)
    install_unit_text(config,'spark01','/etc/systemd/system/sparky-controller.service',unit,stage)
    command(['sudo','-n','systemctl','daemon-reload'])
    command(['ssh',config['nodes']['spark02']['ssh'],'sudo -n systemctl daemon-reload'])
    command(['sudo','-n','systemctl','enable','--now','sparky-controller.service'])
    wait_http('http://127.0.0.1:8020/health')
    print("Controller service is ready.",flush=True)
    # Hold new admissions while the existing ingress is moved. Requests that
    # reached the old provider URL keep running until their normal completion.
    request_json('http://127.0.0.1:8020/control/fence',payload={'enabled':True})
    completed=False
    try:
        # Install the ComfyUI guard only after queued work has finished. The
        # service ExecStop also drains, so a job arriving during this check is
        # not intentionally cancelled by the installer.
        for node,meta in config['nodes'].items():
            guard=False
            try:
                guard=request_json(meta['comfy_url']+'/sparky/state',timeout=3).get('version')==1
            except SparkError:pass
            if not guard:
                wait_comfy_idle(meta['comfy_url'])
                runtime.unit(node,'restart',meta['comfy_unit'])
                wait_http(meta['comfy_url']+'/sparky/state',timeout=180)
            print(node+" ComfyUI guard is ready.",flush=True)
        # The manager's existing hook filename remains its private integration
        # point. It now imports the Python lifecycle; the Bash implementation is
        # archived and no old user-facing command is retained.
        hook=root/'manager/cluster-control.sh'
        if hook.exists():atomic_write(backup/'cluster-control-before',hook.read_bytes())
        new_hook='#!/usr/bin/env python3\nimport sys\nfrom pathlib import Path\nsys.path.insert(0,str(Path.home()/".local/bin/sparky"))\nfrom spark_launcher.lifecycle import run_model\nrun_model('+repr(str(config_path))+',sys.argv[1],sys.argv[2])\n'
        atomic_write(hook,new_hook,0o755)
        # Adopt existing placement so a later config edit stops the actual units.
        for name,row in before['models'].items():
            if row['kind']=='llm' and row['loaded']:
                selected_setting=config['models'][name]['settings'][config['models'][name]['run_on']]
                atomic_write(folder/('running-'+name+'.json'),json.dumps({'nodes':row['nodes'],'units':selected_setting['units'],'placement':config['models'][name]['run_on']})+'\n')
        gateway_result=connect_gateway(config,backup)
        print("Gateway route is connected.",flush=True)
        # No forced cancellation: account for old requests that were already
        # running when the Bifrost URL was changed.
        runtime_activity_idle(runtime)
        completed=True
    finally:
        request_json('http://127.0.0.1:8020/control/fence',payload={'enabled':False})
    after=runtime.snapshot(True)
    protected=['parakeet-engine','parakeet-gateway']
    for name in protected:
        a=before['nodes']['spark02']['containers'][name];b=after['nodes']['spark02']['containers'][name]
        if not b['running'] or a['id']!=b['id'] or a['started_at']!=b['started_at']:
            raise SparkError('Protected Parakeet identity changed unexpectedly.')
    result={'status':'installed','config_path':str(config_path),'backup':str(backup),'parakeet_preserved':True,
            'manager_not_restarted':True,'comfy_guards_installed':True,'gateway':gateway_result}
    atomic_write(folder/'installation-result.json',json.dumps(result,indent=2)+'\n')
    return result


def upload_worker(config,target,data,mode):
    target=Path(target)
    # Paths are quoted as shell words; contents are transferred over stdin.
    script='import pathlib,os,sys,tempfile; p=pathlib.Path(sys.argv[1]); p.parent.mkdir(parents=True,exist_ok=True); data=sys.stdin.buffer.read(); f=tempfile.NamedTemporaryFile(dir=p.parent,delete=False); f.write(data); f.close(); os.chmod(f.name,int(sys.argv[2],8)); os.replace(f.name,p)'
    remote='python3 -c '+shlex.quote(script)+' '+shlex.quote(str(target))+' '+oct(mode)[2:]
    p=subprocess.run(['ssh',config['nodes']['spark02']['ssh'],remote],input=data,capture_output=True,timeout=60)
    if p.returncode:raise SparkError('Worker file installation failed.')


def install_unit_text(config,node,target,text,stage):
    local=Path(stage)/('unit-'+str(time.time_ns()))
    atomic_write(local,text)
    if node=='spark01':install_file(local,target)
    else:
        remote=Path(config['root'])/'controller/staging'/local.name
        upload_worker(config,remote,text.encode(),0o600)
        command(['ssh',config['nodes'][node]['ssh'],'sudo -n install -D -m 644 '+shlex.quote(str(remote))+' '+shlex.quote(target)])


def wait_http(url,timeout=60):
    until=time.monotonic()+timeout
    while time.monotonic()<until:
        try:return request_json(url,timeout=3)
        except (SparkError,OSError):time.sleep(1)
    raise SparkError('Service readiness timed out: '+url)


def wait_comfy_idle(url):
    quiet=0
    while quiet<3:
        data=request_json(url+'/queue',timeout=10)
        quiet=quiet+1 if not data.get('queue_running') and not data.get('queue_pending') else 0
        time.sleep(1)


def drain_comfy(node_file):
    node=json.loads(Path(node_file).read_text())
    url=node['comfy_url']
    try:request_json(url+'/sparky/drain',payload={},timeout=5)
    except SparkError:pass
    wait_comfy_idle(url)


def runtime_activity_idle(runtime):
    while any(runtime.native_activity().values()):time.sleep(.5)


def connect_gateway(config,backup):
    gateway=Path(config['root'])/'gateway'
    credentials=read_env(gateway/'gateway.env')
    auth='Basic '+base64.b64encode((credentials['BIFROST_ADMIN_USER']+':'+credentials['BIFROST_ADMIN_PASSWORD']).encode()).decode()
    def api(payload=None):
        return request_json('http://127.0.0.1:8081/api/providers/garuda',payload=mutable_provider(payload) if payload is not None else None,
                            method='PUT' if payload is not None else 'GET',headers={'Authorization':auth},timeout=30)
    before=api();after=copy.deepcopy(before)
    network=after['network_config']
    old_url=network.get('base_url')
    if not isinstance(old_url,str):raise SparkError('Expected custom provider base_url.')
    network['base_url']='http://127.0.0.1:8020'
    config_path=gateway/'data/config.json';old_config=config_path.read_bytes();document=json.loads(old_config)
    document['providers']['garuda']['network_config']['base_url']=network['base_url']
    nginx_path=gateway/'nginx.conf';old_nginx=nginx_path.read_bytes();text=old_nginx.decode()
    # Add one read-only catalog location to both existing inference listeners.
    # Other methods and all administrator endpoints keep their current rules.
    location='''    # sparky: authoritative read-only model catalog
    location = /sparky/catalog {
        limit_except GET { deny all; }
        auth_basic off;
        proxy_pass http://127.0.0.1:8020/sparky/catalog;
        proxy_set_header Host $host;
    }
'''
    if '# sparky: authoritative read-only model catalog' not in text:
        anchors=['listen 192.0.2.10:8080;','listen 127.0.0.1:8082 proxy_protocol;']
        for anchor in anchors:
            if text.count(anchor)!=1:raise SparkError('Unrecognized inference listener layout. Gateway was not changed.')
            text=text.replace(anchor,anchor+'\n'+location,1)
    atomic_write(Path(backup)/'provider-before.json',json.dumps(before,indent=2)+'\n')
    atomic_write(Path(backup)/'gateway-before.json',old_config)
    atomic_write(Path(backup)/'nginx-before.conf',old_nginx)
    try:
        api(after)
        config_path.write_text(json.dumps(document,indent=2)+'\n')
        nginx_path.write_text(text)
        command(['sudo','-n','docker','exec','garuda-inference','nginx','-t'])
        command(['sudo','-n','docker','exec','garuda-inference','nginx','-s','reload'])
        actual=api()
        check=mutable_provider(actual);prior=mutable_provider(before)
        check['network_config']=prior['network_config']
        if check!=prior or actual['network_config']!=after['network_config']:
            raise SparkError('Gateway provider fields changed outside the intended URL.')
    except BaseException:
        api(before);config_path.write_bytes(old_config);nginx_path.write_bytes(old_nginx)
        command(['sudo','-n','docker','exec','garuda-inference','nginx','-t'])
        command(['sudo','-n','docker','exec','garuda-inference','nginx','-s','reload'])
        raise
    return {'admission_enabled':True,'catalog_enabled':True,'other_provider_fields_preserved':True}


def prepare_qwen_dual(config,stage):
    """Prepare a two-rank variant without starting or replacing any engine."""
    root=Path(config['root']);extra=root/'extra'
    recipe=json.loads((extra/'nvidia-recipes/qwen3.8-flash-next.json').read_text())
    from .parameters import replace_flag
    import copy
    recipe=copy.deepcopy(recipe)
    base=recipe['serve']['command']
    base,_=replace_flag(base,'--tensor-parallel-size',2,'command')
    base,_=replace_flag(base,'--max-num-seqs',4,'command')
    base,_=replace_flag(base,'--gpu-memory-utilization',0.75,'command')
    # Keep all NVIDIA environment, quantization, attention, prefix and MTP flags.
    base+=' \\\n  --nnodes 2 --node-rank 0 --master-addr "$HEAD_IP" --master-port "$MASTER_PORT"'
    recipe['serve']['command']=base
    worker=base.replace('--node-rank 0','--node-rank 1')+' \\\n  --headless'
    recipe['serve']['additional_commands']=[{'id':'worker','command':worker}]
    source=(extra/'serve-nvidia.py').read_text()
    source=source.replace('if model.startswith("qwen") and rank != 0:\n    raise SystemExit("This Qwen recipe uses spark01 only")\n','')
    source=source.replace('(model + ".json")','(model + "-dual.json")')
    source=source.replace('env = dict(os.environ)', 'env = dict(os.environ)\nenv["B12X_COMPILE_WORKERS"] = "1"\nenv["PYTORCH_CUDA_ALLOC_CONF"] = "expandable_segments:True"')
    launcher=(extra/'start-local-nvidia.sh').read_text()
    block='if [[ "$model" == qwen3.8-flash-next && "$rank" != 0 ]]; then\n echo \'This Qwen recipe uses spark01 only\' >&2\n exit 2\nfi\n'
    if block not in launcher:raise SparkError('Unexpected NVIDIA launcher layout. Dual recipe was not installed.')
    launcher=launcher.replace(block,'')
    launcher=launcher.replace('if [[ "$model" == qwen3.8-flash-next ]]; then\n python3 "$base/extra/prepare-qwen-memory.py"\nfi\n','')
    launcher=launcher.replace('cache="$base/extra/cache/$model-fp4-local"','cache="$base/extra/cache/$model-fp4-local-dual"')
    launcher=launcher.replace('/service/serve-nvidia.py','/service/serve-qwen-dual.py')
    unit='''[Unit]
Description=Sparky Qwen two-device recipe
After=docker.service network-online.target
[Service]
Type=simple
User=%s
ExecStart=%s qwen3.8-flash-next
TimeoutStartSec=1800
TimeoutStopSec=180
KillMode=control-group
Restart=no
'''%(Path.home().name,extra/'start-qwen-dual.sh')
    payloads=[(extra/'nvidia-recipes/qwen3.8-flash-next-dual.json',(json.dumps(recipe,indent=2)+'\n').encode(),0o600),
              (extra/'serve-qwen-dual.py',source.encode(),0o644),(extra/'start-qwen-dual.sh',launcher.encode(),0o755)]
    for path,data,mode in payloads:
        atomic_write(path,data,mode);upload_worker(config,path,data,mode)
    for node in config['nodes']:install_unit_text(config,node,'/etc/systemd/system/sparky-qwen-dual.service',unit,stage)
