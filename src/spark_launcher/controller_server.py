"""Small HTTP admission service. It keeps the existing model engines and manager."""
from __future__ import annotations
import http.client
import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlsplit
from .catalog import SparkError, request_json
from .coordinator import Coordinator

MAX_BODY=128*1024*1024
HOP={'connection','keep-alive','proxy-authenticate','proxy-authorization','te','trailer','transfer-encoding','upgrade','host','content-length','accept-encoding'}


class Handler(BaseHTTPRequestHandler):
    protocol_version='HTTP/1.1'
    server_version='Sparky/0.2'

    def log_message(self,*_):
        pass  # Never log prompts, authorization headers, or user-provided URLs.

    @property
    def coordinator(self):return self.server.coordinator

    def body(self):
        if self.headers.get('Transfer-Encoding','').lower()=='chunked':
            data=bytearray()
            while True:
                size=int(self.rfile.readline(128).split(b';',1)[0],16)
                if size==0:
                    while self.rfile.readline(8192).strip():pass
                    break
                if len(data)+size>MAX_BODY:raise SparkError('Request exceeds the configured transport size.')
                data.extend(self.rfile.read(size));self.rfile.read(2)
            return bytes(data)
        size=int(self.headers.get('Content-Length',0))
        if size<0 or size>MAX_BODY:raise SparkError('Request exceeds the configured transport size.')
        data=self.rfile.read(size)
        if len(data)!=size:raise SparkError('Incomplete request body.')
        return data

    def reply(self,data,status=200):
        body=json.dumps(data).encode()
        self.send_response(status)
        self.send_header('Content-Type','application/json')
        self.send_header('Content-Length',str(len(body)))
        self.end_headers();self.wfile.write(body)

    def allowed_control(self):
        return self.client_address[0] in ('127.0.0.1','::1')

    def dispatch(self):
        path=urlsplit(self.path).path
        c=self.coordinator
        if self.command=='GET' and path=='/health':return self.reply({'status':'ok'})
        if self.command=='GET' and path in ('/v1/models','/sparky/catalog'):
            catalog=c.catalog()
            if path=='/v1/models':
                wire=c.runtime.manager('/v1/models')
                extra={m['id'].removeprefix('garuda/'):m for m in catalog['data']}
                for row in wire.get('data',[]):
                    key=row['id'].removeprefix('garuda/')
                    if key in extra:row.update({k:v for k,v in extra[key].items() if k!='id'})
                return self.reply(wire)
            return self.reply(catalog)
        if path.startswith('/control/'):
            if not self.allowed_control():return self.reply({'error':'Use administrator SSH for control operations.'},403)
            if path=='/control/fence' and self.command=='POST':
                enabled=bool(json.loads(self.body()).get('enabled'))
                with c.cv:c.fenced=enabled;c.cv.notify_all()
                return self.reply({'fenced':enabled})
            if self.command=='GET':
                if path=='/control/status':return self.reply(c.snapshot(True))
                if path=='/control/config':return self.reply({'path':str(c.path),'config':c.refresh()})
                if path=='/control/leases':return self.reply(c.snapshot()['admission'])
            data=json.loads(self.body() or b'{}')
            if path=='/control/plan':return self.reply(c.plan(data['model'],data.get('action','load'),data.get('on')))
            if path=='/control/load':return self.reply(c.load(data['model']))
            if path=='/control/unload':return self.reply(c.unload(data['model']))
            if path=='/control/run':
                from .resources import selected
                c.refresh()
                on=data.get('on') or c.config['models']['comfyui']['run_on']
                _,_,setting,nodes=selected(c.config,'comfyui',on)
                if len(nodes)!=1:raise SparkError('Submit each ComfyUI workflow to one device.')
                graph=data['workflow']
                payload=graph if 'prompt' in graph else {'prompt':graph}
                result=request_json(setting['backend_url']+'/prompt',payload=payload,timeout=120)
                c.runtime.event('workflow_submitted',node=nodes[0],prompt_id=result.get('prompt_id'))
                return self.reply({'node':nodes[0],**result})
            if path=='/control/placement':return self.reply(c.set_placement(data['model'],data['on']))
        if path in ('/jobs/acquire','/jobs/release','/jobs/cancel') and self.command=='POST':
            data=json.loads(self.body())
            node=data.get('node')
            if node not in c.config['nodes']:raise SparkError('Unknown job device.')
            expected={'spark01':{'127.0.0.1','::1',c.config['nodes']['spark01']['direct_ip']},
                      'spark02':{c.config['nodes']['spark02']['direct_ip']}}
            if self.client_address[0] not in expected[node]:return self.reply({'error':'Job source does not match its device.'},403)
            if path=='/jobs/acquire':return self.reply(c.comfy_acquire(node,str(data['prompt_id']),data.get('cpu_only',False)))
            if path=='/jobs/cancel':
                request_id=node+':'+str(data['prompt_id'])
                with c.cv:
                    c.pending[:]=[p for p in c.pending if p['request_id']!=request_id]
                    c.cv.notify_all()
                return self.reply({'status':'cancelled'})
            token=data['token']
            with c.cv:
                lease=c.leases.get(token)
                if lease and lease['name'] not in ('comfyui@'+node,'cpu-probe@'+node):raise SparkError('Lease belongs to another workload.')
            c.release(token);return self.reply({'status':'released'})
        if self.command=='POST' and path in ('/v1/chat/completions','/v1/completions','/v1/responses','/v1/embeddings'):
            if not self.allowed_control():return self.reply({'error':'Use the existing gateway for inference.'},403)
            body=json.loads(self.body())
            if not isinstance(body.get('model'),str):return self.reply({'error':'model is required'},400)
            name,token=c.llm_request(body['model'])
            try:
                body['model']=name
                return self.proxy(c.config['manager_url'],path,json.dumps(body).encode())
            finally:c.release(token)
        self.reply({'error':'Unknown endpoint'},404)

    def proxy(self,origin,path,body):
        target=urlsplit(origin)
        connection=http.client.HTTPConnection(target.hostname,target.port,timeout=7200)
        headers={k:v for k,v in self.headers.items() if k.lower() not in HOP}
        headers['Content-Length']=str(len(body))
        started=False
        try:
            connection.request(self.command,path,body=body,headers=headers)
            response=connection.getresponse()
            self.send_response(response.status)
            for key,value in response.getheaders():
                if key.lower() not in HOP:self.send_header(key,value)
            # Close-delimited streaming avoids buffering long reasoning replies.
            self.send_header('Connection','close');self.end_headers();started=True
            while True:
                chunk=response.read1(64*1024)
                if not chunk:break
                self.wfile.write(chunk);self.wfile.flush()
        except (OSError,http.client.HTTPException):
            if not started:self.reply({'error':'Model upstream is unavailable.'},502)
        finally:
            self.close_connection=True
            connection.close()

    def handle_request(self):
        try:self.dispatch()
        except (BrokenPipeError,ConnectionResetError):self.close_connection=True
        except (SparkError,KeyError,ValueError) as error:
            self.reply({'error':str(error)},409 if isinstance(error,SparkError) else 400)
        except Exception:
            # Raw upstream errors and command output may include private data.
            self.coordinator.last_error='A device operation failed. See the local service journal.'
            self.reply({'error':self.coordinator.last_error},503)
    do_GET=handle_request
    do_POST=handle_request


class Server(ThreadingHTTPServer):
    daemon_threads=True
    allow_reuse_address=True


def serve(config_path):
    coordinator=Coordinator(config_path)
    port=coordinator.config.get('controller_port',8020)
    addresses=['127.0.0.1',coordinator.config['nodes']['spark01']['direct_ip']]
    servers=[]
    try:
        for address in addresses:
            server=Server((address,port),Handler);server.coordinator=coordinator
            threading.Thread(target=server.serve_forever,daemon=True).start();servers.append(server)
        def reconcile():
            import time
            while True:
                try:coordinator.reconcile_jobs()
                except Exception:pass
                time.sleep(5)
        threading.Thread(target=reconcile,daemon=True).start()
        coordinator.runtime.event('controller_started',config_path=str(config_path))
        threading.Event().wait()
    finally:
        for server in servers:server.shutdown();server.server_close()
