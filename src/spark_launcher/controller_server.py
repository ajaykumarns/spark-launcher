"""Small HTTP admission service. It keeps the existing model engines and manager."""
from __future__ import annotations
import http.client
import json
import select
import socket
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlsplit
from .catalog import SparkError, request_json
from .coordinator import Coordinator

MAX_BODY=128*1024*1024
HOP={'connection','keep-alive','proxy-authenticate','proxy-authorization','proxy-connection','te','trailer','transfer-encoding','upgrade','host','content-length','accept-encoding'}


def hop_headers(headers):
    return HOP|{part.strip().lower() for value in headers.get_all('Connection',[]) for part in value.split(',')}


class Handler(BaseHTTPRequestHandler):
    protocol_version='HTTP/1.1'
    server_version='Sparky/0.2'

    def log_message(self,*_):
        pass  # Never log prompts, authorization headers, or user-provided URLs.

    @property
    def coordinator(self):return self.server.coordinator

    def body(self):
        lengths=self.headers.get_all('Content-Length',[])
        encodings=self.headers.get_all('Transfer-Encoding',[])
        if len(lengths)>1 or len(encodings)>1 or (lengths and encodings):
            raise ValueError('Ambiguous request body framing.')
        previous=self.connection.gettimeout()
        self.connection.settimeout(60)
        try:
            if encodings:
                if encodings[0].strip().lower()!='chunked':raise ValueError('Unsupported transfer encoding.')
                data=bytearray()
                while True:
                    line=self.rfile.readline(128)
                    token=line.split(b';',1)[0].strip()
                    if not line.endswith(b'\r\n') or not token or any(c not in b'0123456789abcdefABCDEF' for c in token):
                        raise ValueError('Invalid chunk size.')
                    size=int(token,16)
                    if size==0:
                        total=0
                        while True:
                            trailer=self.rfile.readline(8193);total+=len(trailer)
                            if not trailer.endswith(b'\r\n') or total>8192:raise ValueError('Invalid chunk trailer.')
                            if trailer==b'\r\n':return bytes(data)
                    if len(data)+size>MAX_BODY:raise ValueError('Request exceeds the configured transport size.')
                    chunk=self.rfile.read(size)
                    if len(chunk)!=size or self.rfile.read(2)!=b'\r\n':raise ValueError('Incomplete chunk.')
                    data.extend(chunk)
            length=lengths[0] if lengths else '0'
            if not length.isdecimal():raise ValueError('Invalid Content-Length.')
            size=int(length)
            if size>MAX_BODY:raise ValueError('Request exceeds the configured transport size.')
            data=self.rfile.read(size)
            if len(data)!=size:raise ValueError('Incomplete request body.')
            return data
        finally:self.connection.settimeout(previous)

    def json_body(self):
        data=json.loads(self.body() or b'{}')
        if not isinstance(data,dict):raise ValueError('The request body must be a JSON object.')
        return data

    def reply(self,data,status=200):
        body=json.dumps(data).encode()
        self.send_response(status)
        self.send_header('Content-Type','application/json')
        self.send_header('Content-Length',str(len(body)))
        self.send_header('Connection','close')
        self.close_connection=True
        self.end_headers();self.wfile.write(body)

    def allowed_control(self):
        return self.client_address[0] in ('127.0.0.1','::1')

    def dispatch(self):
        path=urlsplit(self.path).path
        c=self.coordinator
        if self.command=='GET' and path=='/health':return self.reply({'status':'ok'})
        if self.command=='GET' and path=='/v1/image-models':
            from .image_api import image_models
            c.refresh()
            return self.reply({'object':'list','data':image_models(c.config)})
        if self.command=='GET' and path in ('/v1/models','/sparky/catalog'):
            catalog=c.catalog()
            if path=='/v1/models':
                wire=c.runtime.manager('/v1/models')
                extra={m['id'].removeprefix('garuda/'):m for m in catalog['data']}
                for row in wire.get('data',[]):
                    key=row['id'].removeprefix('garuda/')
                    if key in extra:row.update({k:v for k,v in extra[key].items() if k!='id'})
                from .image_api import image_models
                wire.setdefault('data',[]).extend(image_models(c.config))
                return self.reply(wire)
            return self.reply(catalog)
        if self.command=='POST' and path=='/v1/images/generations':
            from .image_api import generate
            return self.reply(generate(c,self.json_body()))
        if path.startswith('/control/'):
            if not self.allowed_control():return self.reply({'error':'Use administrator SSH for control operations.'},403)
            reads={'/control/status','/control/config','/control/leases'}
            writes={'/control/fence','/control/plan','/control/load','/control/unload','/control/run','/control/placement'}
            if path not in reads|writes:return self.reply({'error':'Unknown endpoint'},404)
            if (path in reads and self.command!='GET') or (path in writes and self.command!='POST'):
                return self.reply({'error':'Method not allowed.'},405)
            if path=='/control/fence' and self.command=='POST':
                enabled=self.json_body().get('enabled')
                if type(enabled) is not bool:raise ValueError('enabled must be true or false.')
                with c.cv:c.fenced=enabled;c.cv.notify_all()
                return self.reply({'fenced':enabled})
            if self.command=='GET':
                if path=='/control/status':return self.reply(c.snapshot(True))
                if path=='/control/config':return self.reply({'path':str(c.path),'config':c.refresh()})
                if path=='/control/leases':return self.reply(c.snapshot()['admission'])
            data=self.json_body()
            if path=='/control/plan':return self.reply(c.plan(data['model'],data.get('action','load'),data.get('on')))
            if path in ('/control/load','/control/unload'):
                force=data.get('force',False)
                if type(force) is not bool:raise ValueError('force must be true or false.')
                operation=c.load if path=='/control/load' else c.unload
                return self.reply(operation(data['model'],**({'force':True} if force else {})))
            if path=='/control/run':
                return self.reply(c.submit_comfy(data['workflow'],data.get('on')))
            if path=='/control/placement':return self.reply(c.set_placement(data['model'],data['on']))
        if path in ('/jobs/acquire','/jobs/release','/jobs/cancel') and self.command=='POST':
            data=self.json_body()
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
        # Bifrost translates Responses into Chat Completions. No embedding
        # runtime or native Responses server is installed behind this proxy.
        if self.command=='POST' and path in ('/v1/chat/completions','/v1/completions'):
            if not self.allowed_control():return self.reply({'error':'Use the existing gateway for inference.'},403)
            body=self.json_body()
            if not isinstance(body.get('model'),str):return self.reply({'error':'model is required'},400)
            name,token=c.llm_request(body['model'])
            try:
                body['model']=name
                # Send inference directly to the reserved server. Even if it
                # stops after admission, the request cannot make llama-swap
                # start it again or evict another model.
                return self.proxy(c.inference_origin(token),path,json.dumps(body).encode(),token=token)
            finally:c.release(token)
        self.reply({'error':'Unknown endpoint'},404)

    def proxy(self,origin,path,body,token=None):
        target=urlsplit(origin)
        connection=http.client.HTTPConnection(target.hostname,target.port,timeout=7200)
        headers={k:v for k,v in self.headers.items() if k.lower() not in hop_headers(self.headers)}
        headers['Content-Length']=str(len(body))
        started=False
        finished=threading.Event()
        disconnected=threading.Event()
        upstream_socket=[]

        def cancel():
            disconnected.set()
            transport=upstream_socket[0] if upstream_socket else connection.sock
            if transport is not None:
                try:transport.shutdown(socket.SHUT_RDWR)
                except OSError:pass

        if token is not None:self.coordinator.register_inference_cancel(token,cancel)

        def watch_client():
            # A blocked upstream read must not hide a downstream cancellation.
            # Keep the socket reference: getresponse() can detach it from the
            # HTTPConnection when the upstream sends Connection: close.
            while not finished.is_set():
                try:
                    ready,_,_=select.select([self.connection],[],[],.1)
                    if not ready:continue
                    if self.connection.recv(1,socket.MSG_PEEK):
                        finished.wait(.1)
                        continue
                except (OSError,ValueError):
                    if finished.is_set():return
                cancel()
                return

        watcher=threading.Thread(target=watch_client,daemon=True)
        watcher.start()
        try:
            if disconnected.is_set():return
            connection.request(self.command,path,body=body,headers=headers)
            if connection.sock is not None:upstream_socket.append(connection.sock)
            if disconnected.is_set():return
            response=connection.getresponse()
            self.send_response(response.status)
            for key,value in response.getheaders():
                if key.lower() not in hop_headers(response.headers):self.send_header(key,value)
            # Close-delimited streaming avoids buffering long reasoning replies.
            self.send_header('Connection','close');self.end_headers();started=True
            while True:
                chunk=response.read1(64*1024)
                if not chunk:break
                self.wfile.write(chunk);self.wfile.flush()
        except (OSError,http.client.HTTPException):
            if not started and not disconnected.is_set():self.reply({'error':'Model upstream is unavailable.'},502)
        finally:
            finished.set()
            self.close_connection=True
            connection.close()
            watcher.join(.5)

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
