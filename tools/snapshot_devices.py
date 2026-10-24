"""Read-only service proof. Never read credentials or model weights."""
import json
import subprocess
import sys
from pathlib import Path
import shlex
from spark_launcher.catalog import load_config
from spark_launcher.control import safe_host

SCRIPT = '''import hashlib,json,pathlib,subprocess,sys
root=pathlib.Path(sys.argv[1])
files={}
for name in ['manager/config.yaml','manager/cluster-control.sh','extra/serve-nvidia.py','extra/start-local-nvidia.sh','config/models.json']:
 p=root/name
 if p.exists(): files[name]=hashlib.sha256(p.read_bytes()).hexdigest()
names=subprocess.check_output(['sudo','-n','docker','ps','-q'],text=True).split()
containers=[]
if names:
 for item in json.loads(subprocess.check_output(['sudo','-n','docker','inspect',*names],text=True)):
  containers.append({'name':item['Name'].lstrip('/'),'id':item['Id'],'started_at':item['State']['StartedAt'],'status':item['State']['Status']})
units={}
for name in ['garuda-models.service','sparky-controller.service','spark-imagegen.service']:
 raw=subprocess.run(['systemctl','show',name,'-p','MainPID','-p','ActiveState','-p','ActiveEnterTimestampMonotonic'],text=True,capture_output=True).stdout
 units[name]=dict(line.split('=',1) for line in raw.splitlines() if '=' in line)
print(json.dumps({'files':files,'containers':sorted(containers,key=lambda x:x['name']),'units':units}))
'''
config=load_config()
head=safe_host(config.get('admin_ssh_host'))
worker=safe_host(config.get('worker_ssh_host'))
command='python3 - '+shlex.quote(config['remote_root'])
results={}
for name, remote in [('spark01',command),('spark02','ssh '+shlex.quote(worker)+' '+shlex.quote(command))]:
    result=subprocess.run(['ssh',head,remote],input=SCRIPT,text=True,capture_output=True,timeout=30,check=True)
    results[name]=json.loads(result.stdout)
Path(sys.argv[1]).write_text(json.dumps(results,indent=2)+'\n')
print('Saved safe service snapshot:',sys.argv[1])
