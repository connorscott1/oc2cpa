#!/usr/bin/env python3
"""Exercise a native plugin in a disposable v8.0.44 host, using loopback only."""
import argparse
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import io
import json
import os
from pathlib import Path
import platform
import shutil
import socket
import subprocess
import tarfile
import tempfile
import threading
import time
import urllib.error
import urllib.request

ROOT=Path(__file__).resolve().parents[1]
HOSTS={
 ('Linux','x86_64'):('CLIProxyAPI_8.0.44_linux_amd64.tar.gz','3957c34fe4a95a37f5997ab63d78dca431fa1521997d7c2ab3bdfe169ad89307'),
 ('Darwin','arm64'):('CLIProxyAPI_8.0.44_darwin_aarch64.tar.gz','50b75e0bcec8576b93561f2ab2941ff08af26fc9273cf686e9c2b337716bbc46'),
}

def free_port():
    with socket.socket() as s:
        s.bind(('127.0.0.1',0))
        return s.getsockname()[1]

def main():
    parser=argparse.ArgumentParser()
    parser.add_argument('--host',type=Path)
    parser.add_argument('--download',action='store_true')
    args=parser.parse_args()
    manifests=list((ROOT/'dist').rglob('*.build.json'))
    if len(manifests)!=1:
        raise SystemExit('Expected one local build; use a clean dist directory.')
    manifest=json.loads(manifests[0].read_text())
    library=manifests[0].parent/manifest['library']
    assert hashlib.sha256(library.read_bytes()).hexdigest()==manifest['library_sha256']
    calls=[]
    class Mock(BaseHTTPRequestHandler):
        def log_message(self,*args): pass
        def do_GET(self):
            raw=json.dumps({'data':[{'id':'review-test-free'}]}).encode()
            self.send_response(200);self.send_header('Content-Length',str(len(raw)));self.end_headers();self.wfile.write(raw)
        def do_POST(self):
            body=json.loads(self.rfile.read(int(self.headers['Content-Length'])))
            assert self.headers.get('Authorization')=='Bearer public'
            assert self.headers.get('x-opencode-session','').startswith('ses_')
            assert any(t['function']['name']=='read' for t in body['tools'])
            calls.append(body)
            frames=[{'id':'mock','choices':[{'index':0,'delta':{'role':'assistant','content':'mock-only-ok'},'finish_reason':None}]},
                    {'choices':[{'index':0,'delta':{},'finish_reason':'stop'}],'usage':{'prompt_tokens':2,'completion_tokens':3,'total_tokens':5}}]
            raw=''.join('data: '+json.dumps(f)+'\n\n' for f in frames)+'data: [DONE]\n\n'
            self.send_response(200);self.send_header('Content-Type','text/event-stream');self.send_header('Content-Length',str(len(raw)));self.end_headers();self.wfile.write(raw.encode())
    mock=ThreadingHTTPServer(('127.0.0.1',0),Mock)
    thread=threading.Thread(target=mock.serve_forever,daemon=True);thread.start()
    try:
      with tempfile.TemporaryDirectory(prefix='oc2cpa-smoke-') as temp:
        base=Path(temp)
        host=args.host.resolve() if args.host else None
        if host is None:
            if not args.download: raise SystemExit('Supply --host or --download.')
            name,expected=HOSTS[(platform.system(),platform.machine())]
            data=urllib.request.urlopen('https://github.com/router-for-me/CLIProxyAPI/releases/download/v8.0.44/'+name,timeout=60).read()
            assert hashlib.sha256(data).hexdigest()==expected,'Host archive checksum mismatch'
            with tarfile.open(fileobj=io.BytesIO(data),mode='r:gz') as archive:
                host=base/'cli-proxy-api'
                host.write_bytes(archive.extractfile('cli-proxy-api').read())
            host.chmod(0o700)
        auth=base/'auth';auth.mkdir()
        plugins=base/'plugins';plugins.mkdir()
        shutil.copy2(library,plugins/library.name)
        credential=auth/'oc2cpa.json'
        original=json.dumps({'type':'oc2cpa','label':'preserve-this-label','proxy_url':'','note':'preserve-this-note'}).encode()
        credential.write_bytes(original)
        port=free_port()
        config=base/'config.yaml'
        cfg={
          'config-version':8,
          'server':{'host':'127.0.0.1','port':port,'discovery':{'enabled':False},'response-compression':{'enabled':False}},
          'management':{'allow-remote':False,'secret-key':'local-smoke-management','disable-control-panel':True,'disable-auto-update-panel':True},
          'access':{'api-keys':['local-smoke-client']},
          'oauth':{'auth-dir':str(auth)},
          'plugins':{'enabled':True,'dir':str(plugins),'configs':{'oc2cpa':{'enabled':True,'upstream_base_url':f'http://127.0.0.1:{mock.server_port}','dynamic_models':False}}},
          'logging':{'to-file':False},
        }
        config.write_text(json.dumps(cfg)) # JSON is valid YAML.
        env={k:v for k,v in os.environ.items() if k in ('PATH','SYSTEMROOT','WINDIR','TMPDIR','TMP','TEMP','LANG')}
        log=base/'host.log'
        with log.open('wb') as output:
          proc=subprocess.Popen([str(host),'--config',str(config),'--local-model'],cwd=base,env=env,stdout=output,stderr=subprocess.STDOUT)
          try:
            def request(path,payload=None,key='local-smoke-client'):
                req=urllib.request.Request(f'http://127.0.0.1:{port}'+path,data=None if payload is None else json.dumps(payload).encode(),headers={'Authorization':'Bearer '+key,'Content-Type':'application/json'})
                with urllib.request.urlopen(req,timeout=15) as response:return response.read()
            deadline=time.monotonic()+30
            while True:
                if proc.poll() is not None: raise AssertionError('Host exited: '+log.read_text())
                try:
                    catalog=json.loads(request('/v1/models'))
                    if any(m['id']=='big-pickle' for m in catalog.get('data',[])):break
                except (OSError,ValueError): pass
                if time.monotonic()>deadline:raise AssertionError('Host/plugin did not become ready: '+log.read_text())
                time.sleep(.1)
            assert credential.read_bytes()==original,'Registration overwrote the existing credential'
            for stream in (False,True):
                data=request('/v1/chat/completions',{'model':'big-pickle','stream':stream,'messages':[{'role':'user','content':'local mock test'}]})
                assert b'mock-only-ok' in data, data
                if not stream:assert json.loads(data)['choices'][0]['message']['content']=='mock-only-ok'
            try:
                request('/v0/management/plugins/oc2cpa/usage/summary',key='wrong-test-key')
                raise AssertionError('Unauthenticated management request succeeded')
            except urllib.error.HTTPError as error: assert error.code in (401,403)
            summary=json.loads(request('/v0/management/plugins/oc2cpa/usage/summary',key='local-smoke-management'))
            assert summary['provider']=='oc2cpa'
            # The host may rewrite the plaintext management key to a hash on startup.
            # Append a comment to its current YAML so the watcher reconfigures safely.
            config.write_text(config.read_text()+'\n# smoke reload\n')
            time.sleep(1)
            request('/v1/models')
            assert credential.read_bytes()==original,'Reload overwrote the existing credential'
            assert len(calls)==2
            print(json.dumps({'host':'v8.0.44','host_source':'1f917e77c4562623aa49e9804d999524151a2f96','plugin_source':manifest['source_commit'],'checks':['native load','static catalog','credential preservation','aggregate mock inference','streaming mock inference','management authentication','management summary','reload preservation'],'external_inference':False},indent=2))
          finally:
            proc.terminate()
            try:proc.wait(timeout=5)
            except subprocess.TimeoutExpired:proc.kill();proc.wait()
    finally:
        mock.shutdown();mock.server_close()
if __name__=='__main__': main()
