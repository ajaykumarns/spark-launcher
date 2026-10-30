"""Exercise the real HTTP parser and proxy without model or device actions."""
import http.client
import json
import socket
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from unittest.mock import Mock

from spark_launcher.controller_server import Handler, Server
from spark_launcher.catalog import SparkError


class ControllerHTTPTests(unittest.TestCase):
    def setUp(self):
        self.coordinator=Mock()
        self.coordinator.plan.return_value={'status':'planned'}
        self.coordinator.load.return_value={'status':'loaded'}
        self.coordinator.unload.return_value={'status':'unloaded'}
        self.server=Server(('127.0.0.1',0),Handler)
        self.server.coordinator=self.coordinator
        self.thread=threading.Thread(target=self.server.serve_forever,kwargs={'poll_interval':.01},daemon=True)
        self.thread.start()
        self.addCleanup(self.cleanup)

    def cleanup(self):
        self.server.shutdown();self.server.server_close();self.thread.join(2)

    def request(self,method,path,body=None,headers=None):
        client=http.client.HTTPConnection('127.0.0.1',self.server.server_port,timeout=2)
        try:
            client.request(method,path,body=body,headers=headers or {})
            response=client.getresponse()
            return response.status,dict(response.getheaders()),json.loads(response.read())
        finally:client.close()

    def raw(self,data):
        with socket.create_connection(self.server.server_address,timeout=2) as client:
            client.sendall(data);client.shutdown(socket.SHUT_WR)
            result=bytearray()
            while True:
                block=client.recv(65536)
                if not block:return bytes(result)
                result.extend(block)

    def test_get_cannot_load_a_model_even_with_a_json_body(self):
        status,headers,_=self.request('GET','/control/load',json.dumps({'model':'qwen'}))
        self.assertEqual(status,405);self.assertEqual(headers['Connection'],'close')
        self.coordinator.load.assert_not_called()

    def test_post_cannot_read_control_config(self):
        self.assertEqual(self.request('POST','/control/config','{}')[0],405)
        self.coordinator.refresh.assert_not_called()

    def test_inference_for_stopped_model_returns_error_without_load(self):
        self.coordinator.llm_request.side_effect=SparkError('qwen is not ready. Ask the human to switch the model. Do not load or unload models automatically.')
        status,_,body=self.request('POST','/v1/chat/completions',json.dumps({'model':'qwen'}))
        self.assertEqual(status,409);self.assertIn('Ask the human to switch the model',body['error'])
        self.assertNotIn('sparky load',body['error'])
        self.coordinator.load.assert_not_called();self.coordinator.inference_origin.assert_not_called()

    def test_image_discovery_does_not_advertise_language_models(self):
        self.coordinator.config={'models':{'chat':{'kind':'llm'},'flux2-klein-4b':{'kind':'comfy_model','image_api':{'enabled':True}}}}
        status,_,data=self.request('GET','/v1/image-models')
        self.assertEqual(status,200)
        self.assertEqual([m['id'] for m in data['data']],['flux2-klein-4b'])
        self.coordinator.runtime.manager.assert_not_called()

    def test_non_object_json_does_not_cause_an_internal_error(self):
        self.assertEqual(self.request('POST','/control/load','[]')[0],400)
        self.coordinator.load.assert_not_called()

    def test_ambiguous_lengths_are_rejected_without_reusing_connection(self):
        cases=(b'Content-Length: 2\r\nContent-Length: 2\r\n',
               b'Content-Length: 2\r\nTransfer-Encoding: chunked\r\n')
        for headers in cases:
            with self.subTest(headers=headers):
                response=self.raw(b'POST /control/plan HTTP/1.1\r\nHost: localhost\r\n'+headers+b'\r\n{}')
                self.assertTrue(response.startswith(b'HTTP/1.1 400'))
                self.assertIn(b'Connection: close',response)
        self.coordinator.plan.assert_not_called()

    def test_truncated_or_invalid_chunks_never_reach_control_action(self):
        for body in (b'0\r\n',b'2\r\n{}XX0\r\n\r\n',b'-1\r\n',b'100\r\nshort'):
            with self.subTest(body=body):
                response=self.raw(b'POST /control/plan HTTP/1.1\r\nHost: localhost\r\nTransfer-Encoding: chunked\r\n\r\n'+body)
                self.assertTrue(response.startswith(b'HTTP/1.1 400'))
        self.coordinator.plan.assert_not_called()

    def test_valid_chunked_json_is_still_supported(self):
        body=b'{"model":"qwen"}'
        chunks=(format(len(body),'x').encode()+b'\r\n'+body+b'\r\n0\r\nX-Trailer: test\r\n\r\n')
        response=self.raw(b'POST /control/plan HTTP/1.1\r\nHost: localhost\r\nTransfer-Encoding: chunked\r\n\r\n'+chunks)
        self.assertTrue(response.startswith(b'HTTP/1.1 200'))
        self.coordinator.plan.assert_called_once_with('qwen','load',None)

    def test_force_is_forwarded_only_as_an_explicit_boolean(self):
        for operation in ('load','unload'):
            self.assertEqual(self.request('POST','/control/'+operation,json.dumps({'model':'qwen','force':True}))[0],200)
            getattr(self.coordinator,operation).assert_called_once_with('qwen',force=True)
            getattr(self.coordinator,operation).reset_mock()
            for value in ('false',1,None):
                self.assertEqual(self.request('POST','/control/'+operation,json.dumps({'model':'qwen','force':value}))[0],400)
            getattr(self.coordinator,operation).assert_not_called()

    def test_fence_requires_a_boolean(self):
        self.assertEqual(self.request('POST','/control/fence','{"enabled":"false"}')[0],400)

    def test_unused_native_responses_and_embedding_routes_are_absent(self):
        for path in ('/v1/responses','/v1/embeddings'):
            self.assertEqual(self.request('POST',path,'{"model":"qwen"}')[0],404)
        self.coordinator.llm_request.assert_not_called()

    def test_proxy_rewrites_model_streams_and_removes_connection_headers(self):
        received={};payload=b'data: {"text":"OK"}\n\ndata: [DONE]\n\n'
        class Upstream(BaseHTTPRequestHandler):
            def log_message(self,*_):pass
            def do_POST(self):
                received.update(body=json.loads(self.rfile.read(int(self.headers['Content-Length']))),headers=self.headers)
                self.send_response(200)
                self.send_header('Content-Type','text/event-stream')
                self.send_header('Content-Length',str(len(payload)))
                self.send_header('Connection','X-Hide')
                self.send_header('X-Hide','private-hop-value')
                self.end_headers();self.wfile.write(payload);self.wfile.flush()
        upstream=ThreadingHTTPServer(('127.0.0.1',0),Upstream)
        thread=threading.Thread(target=upstream.serve_forever,kwargs={'poll_interval':.01},daemon=True);thread.start()
        self.coordinator.config={'manager_url':'http://127.0.0.1:1'}
        self.coordinator.inference_origin.return_value=f'http://127.0.0.1:{upstream.server_port}'
        self.coordinator.llm_request.return_value=('qwen','lease')
        client=http.client.HTTPConnection(*self.server.server_address,timeout=2)
        try:
            client.request('POST','/v1/chat/completions',json.dumps({'model':'garuda/qwen','stream':True}),{'Connection':'X-Drop','X-Drop':'private-hop-value','X-Client-ID':'test'})
            response=client.getresponse()
            self.assertEqual(response.status,200);self.assertIsNone(response.getheader('X-Hide'))
            self.assertEqual(response.read(),payload)
        finally:
            client.close();upstream.shutdown();upstream.server_close();thread.join(2)
        self.assertEqual(received['body']['model'],'qwen')
        self.assertIsNone(received['headers'].get('X-Drop'))
        self.assertEqual(received['headers']['X-Client-ID'],'test')
        self.coordinator.release.assert_called_once_with('lease')

    def assert_proxy_cancels_blocked_upstream(self,send_headers,stream,force=False):
        entered=threading.Event();cancelled=threading.Event();released=threading.Event()
        class Upstream(BaseHTTPRequestHandler):
            def log_message(self,*_):pass
            def do_POST(self):
                self.rfile.read(int(self.headers['Content-Length']))
                if send_headers:
                    self.send_response(200)
                    self.send_header('Content-Type','text/event-stream')
                    self.send_header('Connection','close')
                    self.end_headers();self.wfile.flush()
                entered.set()
                self.connection.settimeout(2)
                try:
                    if self.connection.recv(1)==b'':cancelled.set()
                except OSError:pass
        upstream=ThreadingHTTPServer(('127.0.0.1',0),Upstream)
        thread=threading.Thread(target=upstream.serve_forever,kwargs={'poll_interval':.01},daemon=True)
        thread.start()
        self.coordinator.config={'manager_url':'http://127.0.0.1:1'}
        self.coordinator.inference_origin.return_value=f'http://127.0.0.1:{upstream.server_port}'
        self.coordinator.llm_request.return_value=('qwen','lease')
        self.coordinator.release.side_effect=lambda token:released.set()
        cancel_callbacks=[]
        self.coordinator.register_inference_cancel.side_effect=lambda token,cancel:cancel_callbacks.append(cancel)
        client=http.client.HTTPConnection(*self.server.server_address,timeout=2)
        try:
            client.request('POST','/v1/chat/completions',json.dumps({'model':'qwen','stream':stream}))
            self.assertTrue(entered.wait(2),'upstream did not receive the request')
            if force:cancel_callbacks[0]()
            else:client.sock.shutdown(socket.SHUT_RDWR);client.close()
            self.assertTrue(cancelled.wait(1),'proxy did not close the blocked upstream')
            self.assertTrue(released.wait(1),'controller did not release the request lease')
            self.coordinator.release.assert_called_once_with('lease')
        finally:
            client.close();upstream.shutdown();upstream.server_close();thread.join(2)

    def test_cancel_stream_before_upstream_headers(self):
        self.assert_proxy_cancels_blocked_upstream(False,True)

    def test_cancel_stream_during_prefill_after_upstream_headers(self):
        self.assert_proxy_cancels_blocked_upstream(True,True)

    def test_cancel_nonstream_request_before_upstream_headers(self):
        self.assert_proxy_cancels_blocked_upstream(False,False)

    def test_forced_unload_cancels_blocked_direct_server(self):
        self.assert_proxy_cancels_blocked_upstream(False,True,force=True)


if __name__=='__main__':unittest.main()
