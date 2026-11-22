"""Compose rendering of the adapter's generation settings, and the runtime refusal of them.

GEN-001 legacy test ruling: the repository docker-compose.yml still serializes the
legacy GEN_OPENAI_* fields. The ComposeGeneration tests below check that rendering
only; they are not runtime support. With GEN_OPENAI_URL or any non-Chutes
connector set, the adapter still starts and serves embedding/rerank, but refuses
generation and never dispatches to that URL (LegacyConnectorRefusal), so an old
URL never activates an alternate connector. The root CPU proxy does not advertise
generate. Chutes generation lives only behind the opt-in `chutes` profile
(ChutesProfile below): without it the default stack is unchanged.
"""
import json
import os
from pathlib import Path
import subprocess
import shutil
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[3]
HERE = Path(__file__).resolve().parent

class ComposeGeneration(unittest.TestCase):
 """Rendering only: these fields reach the container, where the adapter refuses generation."""
 def render(self, *profiles, **values):
  if not shutil.which('docker') or subprocess.run(['docker','compose','version'],capture_output=True,timeout=5).returncode:
   if os.environ.get('CI'):self.fail('docker compose required in CI')
   self.skipTest('docker compose unavailable')
  with tempfile.TemporaryDirectory() as home:
   env={'PATH':os.environ['PATH'],'HOME':home,'MINER_INTERNAL_API_KEY':'synthetic-render-only',**values}
   r=subprocess.run(['docker','compose','--env-file','/dev/null','-f',str(ROOT/'docker-compose.yml'),*[a for p in profiles for a in ('--profile',p)],'config','--format','json'],env=env,capture_output=True,text=True,timeout=20)
   self.assertEqual(r.returncode,0,r.stderr)
   return json.loads(r.stdout)['services']
 def test_local_backend_stays_default(self):
  services=self.render();env=services['miner-inference-service']['environment']
  self.assertEqual(env['GEN_OPENAI_URL'],'')
  self.assertEqual(env['GEN_OPENAI_MODEL'],env['GEN_MODEL'])
  self.assertEqual(env['GEN_OPENAI_TIMEOUT_SECONDS'],'60')
  # The CPU inference service keeps its own admission settings; it never gets the Chutes one.
  self.assertEqual((env['GEN_MAX_INFLIGHT'],env['GEN_MAX_QUEUE'],env['GEN_ACQUIRE_TIMEOUT_SECONDS']),('1','2','25'))
  self.assertNotIn('CHUTES_MAX_CONCURRENT',env)
 def test_miner_generation_deadline_default(self):
  self.assertEqual(self.render()['miner']['environment']['GENERATION_BACKEND_TIMEOUT_SECONDS'],'58')
  self.assertEqual(self.render(GENERATION_BACKEND_TIMEOUT_SECONDS='40')['miner']['environment']['GENERATION_BACKEND_TIMEOUT_SECONDS'],'40')
 def test_remote_backend_and_advertised_model_agree(self):
  services=self.render(GEN_OPENAI_URL='http://synthetic-backend.invalid:8000/v1',MINER_GEN_MODEL='synthetic-served-model',GEN_OPENAI_TIMEOUT_SECONDS='17')
  env=services['miner-inference-service']['environment'];proxy=services['cpu-model-proxy']['environment']
  self.assertEqual(env['GEN_OPENAI_URL'],'http://synthetic-backend.invalid:8000/v1')
  self.assertEqual(env['GEN_OPENAI_MODEL'],'synthetic-served-model')
  self.assertEqual(env['GEN_OPENAI_TIMEOUT_SECONDS'],'17')
  self.assertNotIn(env['GEN_OPENAI_MODEL'],proxy['MODELS'].split(','))
  self.assertEqual(proxy['UPSTREAM_URL'],'http://miner-inference-service:9200')
 def test_cpu_proxy_does_not_advertise_generate(self):
  proxy=self.render()['cpu-model-proxy']['environment']
  self.assertEqual(proxy['CAPABILITIES'].split(','),['embedding','rerank'])
  self.assertEqual(len(proxy['MODELS'].split(',')),2)
 def test_full_endpoint_is_passed_unchanged(self):
  url='http://synthetic-backend.invalid:8000/v1/chat/completions'
  env=self.render(GEN_OPENAI_URL=url)['miner-inference-service']['environment']
  self.assertEqual(env['GEN_OPENAI_URL'],url)

 def test_default_render_leaves_chutes_disabled(self):
  env=self.render()['miner-inference-service']['environment']
  self.assertNotEqual(str(env.get('GEN_CHUTES_ENABLED','')).lower(),'true')

CHUTES=('chutes-generation','chutes-genproxy')
CHUTES_ENV={'CHUTES_MODEL':'synthetic-org/synthetic-model','CHUTES_KEY_DIR':'/synthetic/key-dir',
            'CHUTES_STATE_DIR':'/synthetic/state-dir','CHUTES_LEDGER_ID':'00000000-0000-4000-8000-000000000001'}

class ChutesProfile(unittest.TestCase):
 """The opt-in `chutes` profile: rendering only, never up/pull/build."""
 render=ComposeGeneration.render
 def test_absent_without_profile(self):
  for values in ({},CHUTES_ENV):
   with self.subTest(values=bool(values)):
    services=self.render(**values)
    for name in CHUTES:self.assertNotIn(name,services)
    self.assertNotIn('generate',services['cpu-model-proxy']['environment']['CAPABILITIES'].split(','))
 def test_miner_model_only_when_set(self):
  self.assertIsNone(self.render()['miner']['environment'].get('CHUTES_MODEL'))
  self.assertEqual(self.render(**CHUTES_ENV)['miner']['environment']['CHUTES_MODEL'],CHUTES_ENV['CHUTES_MODEL'])
 def test_profile_services_are_hardened(self):
  services=self.render('chutes',**CHUTES_ENV)
  for name in CHUTES:
   with self.subTest(service=name):
    svc=services[name]
    self.assertNotIn('ports',svc);self.assertNotIn('expose',svc)
    self.assertIs(svc['read_only'],True)
    self.assertEqual(svc['cap_drop'],['ALL'])
    self.assertIn('no-new-privileges:true',svc['security_opt'])
    self.assertEqual(svc['user'],'1001:1001')
    self.assertEqual(svc['restart'],'unless-stopped')
    self.assertIn('healthcheck',svc)
  self.assertTrue(any(str(t).startswith('/tmp') for t in services['chutes-generation']['tmpfs']))
 def test_key_and_state_mounts(self):
  gen=self.render('chutes',**CHUTES_ENV)['chutes-generation'];env=gen['environment']
  mounts={v['target']:v for v in gen['volumes']}
  create=lambda m:m.get('bind',{}).get('create_host_path',False)  # omitted by Compose when false
  key,state=mounts['/run/chutes'],mounts['/state']
  self.assertEqual((key['type'],key['source'],key.get('read_only'),create(key)),('bind','/synthetic/key-dir',True,False))
  # CHUTES_STATE_DIR set: that host directory replaces the default volume.
  self.assertEqual((state['type'],state['source'],bool(state.get('read_only'))),('bind','/synthetic/state-dir',False))
  self.assertEqual(env['CHUTES_API_KEY_FILE'],'/run/chutes/api-key')
  self.assertEqual(env['CHUTES_LEDGER_PATH'],'/state/hosted.sqlite')
 def test_ledger_uses_named_volume_by_default(self):
  for values in ({},{'CHUTES_STATE_DIR':''}):
   with self.subTest(values=values):
    gen=self.render('chutes',CHUTES_KEY_DIR='/synthetic/key-dir',**values)['chutes-generation']
    state={v['target']:v for v in gen['volumes']}['/state']
    self.assertEqual((state['type'],state['source'],bool(state.get('read_only'))),('volume','chutes-state',False))
 def test_unset_key_dir_never_mounts_the_project(self):
  gen=self.render('chutes')['chutes-generation']
  key={v['target']:v for v in gen['volumes']}['/run/chutes']
  self.assertTrue(key['source'].startswith('/nonexistent/'),key['source'])
  self.assertFalse(key.get('bind',{}).get('create_host_path',False))
 def test_generation_settings_and_routing(self):
  services=self.render('chutes',**CHUTES_ENV)
  gen,proxy,cpu=(services[n]['environment'] for n in ('chutes-generation','chutes-genproxy','cpu-model-proxy'))
  self.assertEqual((gen['GENERATION_CONNECTOR'],gen['GEN_CHUTES_ENABLED']),('chutes','true'))
  self.assertEqual(gen['CHUTES_MODEL'],CHUTES_ENV['CHUTES_MODEL'])
  self.assertEqual(gen['CHUTES_LEDGER_ID'],CHUTES_ENV['CHUTES_LEDGER_ID'])
  self.assertEqual(self.render('chutes',CHUTES_LEDGER_ID='pending-init')['chutes-generation']['environment']['CHUTES_LEDGER_ID'],'pending-init')
  # The one concurrency setting; the settings it replaced and the hosted timeout are not forwarded.
  self.assertEqual(gen['CHUTES_MAX_CONCURRENT'],'4')
  self.assertEqual(self.render('chutes',CHUTES_MAX_CONCURRENT='9',**CHUTES_ENV)['chutes-generation']['environment']['CHUTES_MAX_CONCURRENT'],'9')
  for unset in ('HOSTED_MAX_OUTSTANDING','HOSTED_MAX_CONCURRENT','HOSTED_RECOVERY_POLICY','HOSTED_UNKNOWN_AGE_OUT_SECONDS',
                'GEN_MAX_INFLIGHT','GEN_MAX_QUEUE','GEN_OPENAI_TIMEOUT_SECONDS','GEN_OPENAI_URL'):self.assertNotIn(unset,gen)
  self.assertEqual(services['miner']['environment']['GENERATION_BACKEND_TIMEOUT_SECONDS'],'58')
  self.assertEqual((proxy['CAPABILITIES'],proxy['MODELS']),('generate',CHUTES_ENV['CHUTES_MODEL']))
  self.assertEqual((proxy['SERVICE_NAME'],json.loads(proxy['METADATA'])),('chutes',{'generation_connector':'chutes'}))
  self.assertEqual(proxy['UPSTREAM_URL'],'http://chutes-generation:'+gen['LISTEN_ADDR'].lstrip(':'))
  self.assertEqual(proxy['UPSTREAM_HEALTH_PATH'],'/health/generation')
  self.assertEqual(services['miner']['environment']['CHUTES_MODEL'],CHUTES_ENV['CHUTES_MODEL'])
  self.assertEqual(cpu['CAPABILITIES'].split(','),['embedding','rerank'])
  self.assertNotIn(CHUTES_ENV['CHUTES_MODEL'],cpu['MODELS'].split(','))
 def test_no_key_value_in_any_environment(self):
  for name,svc in self.render('chutes',**CHUTES_ENV).items():
   self.assertNotIn('CHUTES_API_KEY',svc.get('environment') or {},name)

class LegacyConnectorRefusal(unittest.TestCase):
 """Startup in a clean process: no Compose or Docker needed. A legacy URL or another
 connector refuses generation only; the process starts and embedding/rerank keep serving."""
 STUB=('import sys,types\n'
       'sys.modules.setdefault("sentence_transformers",types.SimpleNamespace(CrossEncoder=object,SentenceTransformer=object))\n'
       'sys.modules.setdefault("transformers",types.SimpleNamespace(AutoModelForCausalLM=object,AutoTokenizer=object))\n'
       'import service\n')
 # Serves /embed, /rerank and /generate in-process with fake models. The HTTP(S)
 # connection classes and socket.create_connection are wrapped by a recorder that
 # counts every non-loopback destination, so any attempt to reach the legacy URL shows.
 PROBE=STUB+(
  'import json,socket,threading,urllib.error,urllib.request\n'
  'from http.server import ThreadingHTTPServer\n'
  'calls=[]\n'
  'def outbound(host):\n'
  ' if not str(host).startswith("127.0.0.1"):calls.append(str(host))\n'
  'class RecHTTP(service.http.client.HTTPConnection):\n'
  ' def __init__(self,host,*a,**k):outbound(host);super().__init__(host,*a,**k)\n'
  'class RecHTTPS(service.http.client.HTTPSConnection):\n'
  ' def __init__(self,host,*a,**k):outbound(host);super().__init__(host,*a,**k)\n'
  'service.http.client.HTTPConnection=RecHTTP;service.http.client.HTTPSConnection=RecHTTPS\n'
  '_orig=socket.create_connection\n'
  'def _cc(addr,*a,**k):\n'
  ' outbound(addr[0]);return _orig(addr,*a,**k)\n'
  'socket.create_connection=_cc\n'
  'class Vecs(list):\n'
  ' def tolist(self):return list(self)\n'
  'class Emb:\n'
  ' def encode(self,texts,**k):return Vecs([[1.0,0.0] for _ in texts])\n'
  'class Rr:\n'
  ' def predict(self,pairs,**k):return [float(i) for i,_ in enumerate(pairs)]\n'
  'service.embedder=Emb();service.reranker=Rr()\n'
  'srv=ThreadingHTTPServer(("127.0.0.1",0),service.Handler);port=srv.server_address[1]\n'
  'threading.Thread(target=srv.serve_forever,daemon=True).start()\n'
  'def post(path,body):\n'
  ' req=urllib.request.Request("http://127.0.0.1:%d%s"%(port,path),data=json.dumps(body).encode(),headers={"Content-Type":"application/json"})\n'
  ' try:\n'
  '  with urllib.request.urlopen(req,timeout=10) as r:return r.status,json.loads(r.read())\n'
  ' except urllib.error.HTTPError as e:return e.code,json.loads(e.read())\n'
  'def get(path):\n'
  ' try:\n'
  '  with urllib.request.urlopen("http://127.0.0.1:%d%s"%(port,path),timeout=10) as r:return r.status,json.loads(r.read())\n'
  ' except urllib.error.HTTPError as e:return e.code,json.loads(e.read())\n'
  'out={"enabled":service.CHUTES_CONFIG.enabled,"url":service.GEN_OPENAI_URL,\n'
  ' "embed":post("/embed",{"inputs":["a","b"]}),"rerank":post("/rerank",{"query":"q","texts":["x","y"]}),\n'
  ' "generate":post("/generate",{"query":"q","contexts":[{"id":"1","text":"t"}]}),\n'
  ' "readiness":get("/health/generation")}\n'
  'srv.shutdown()\n'
  'try:service.generate_via_openai([{"role":"user","content":"q"}],1,0)\n'
  'except Exception as e:out["direct"]=type(e).__name__\n'
  'out["calls"]=calls\n'
  'print(json.dumps(out))\n')
 LEGACY=({'GEN_OPENAI_URL':'http://synthetic-backend.invalid:8000/v1'},
         {'GEN_OPENAI_URL':'http://synthetic-backend.invalid:8000/v1/chat/completions'},
         {'GENERATION_CONNECTOR':'openai'},
         # Chutes enabled alongside a legacy URL: still refused, and the credential is not read.
         {'GEN_OPENAI_URL':'http://synthetic-backend.invalid:8000/v1','GEN_CHUTES_ENABLED':'true',
          'CHUTES_API_KEY_FILE':'/run/synthetic-chutes/absent'})
 def start(self, code=None, **values):
  with tempfile.TemporaryDirectory() as home:
   env={'PATH':os.environ.get('PATH',''),'HOME':home,**values}
   if os.environ.get('PYTHONPATH'):env['PYTHONPATH']=os.environ['PYTHONPATH']
   return subprocess.run([sys.executable,'-c',code or self.STUB],cwd=str(HERE),env=env,capture_output=True,text=True,timeout=60)
 def test_clean_environment_starts(self):
  r=self.start();self.assertEqual(r.returncode,0,r.stderr)
  self.assertNotIn('generation_refused',r.stderr)
 def test_legacy_url_and_other_connectors_start_with_generation_refused(self):
  for values in self.LEGACY:
   with self.subTest(values=values):
    r=self.start(**values)
    self.assertEqual(r.returncode,0,r.stderr)
    self.assertIn('generation_refused',r.stderr)
    self.assertIn('only the Chutes generation connector is supported',r.stderr)
    self.assertNotIn('synthetic-backend',r.stderr+r.stdout)
 def test_legacy_url_is_never_dispatched_and_embeddings_serve(self):
  for values in self.LEGACY:
   with self.subTest(values=values):
    r=self.start(self.PROBE,**values)
    self.assertEqual(r.returncode,0,r.stderr)
    out=json.loads(r.stdout.strip().splitlines()[-1])
    self.assertFalse(out['enabled'])
    self.assertEqual(out['url'],'')
    self.assertEqual(out['embed'],[200,[[1.0,0.0],[1.0,0.0]]])
    self.assertEqual(out['rerank'][0],200)
    self.assertEqual(out['generate'],[503,{'error':'generation_disabled'}])
    self.assertEqual(out['readiness'][0],503)
    self.assertEqual(out['readiness'][1]['reason'],'generation disabled')
    self.assertEqual(out['direct'],'HostedProviderError')
    self.assertEqual(out['calls'],[])

if __name__=='__main__':unittest.main()
