import unittest,time
from unittest.mock import patch
from test_hosted_admission_bound import ServiceSentMarkerTests,Base
class SendFence(ServiceSentMarkerTests):
 def test_cancel_during_committed_sent_marker_must_not_dispatch(self):
  original=self.chutes.ledger.mark_sent
  canceled=[False]
  def commit_then_cancel(attempt):
   original(attempt);canceled[0]=True
  request=self.service.http.client.HTTPConnection.request
  calls=[]
  def record_request(conn,*a,**kw):
   calls.append(1)
   result=request(conn,*a,**kw)
   limit=time.monotonic()+1
   while not self.posts and time.monotonic()<limit:time.sleep(.001)
   return result
  with patch.object(self.chutes.ledger,'mark_sent',commit_then_cancel),patch.object(self.service.http.client.HTTPConnection,'request',record_request):
   with self.assertRaises(self.service.GenerationCanceled):
    self.service.generate_via_openai([{'role':'user','content':'synthetic'}],None,0,should_abort=lambda:canceled[0])
  print('cancel-during-marker',{'requests':len(calls),'posts':len(self.posts),'pending':self.chutes.ledger.pending_detail()},flush=True)
  self.assertEqual(calls,[], 'request dispatched after cancellation became true during marker commit')
class OptionalKey(Base):
 def test_unkeyed_distinct_attempts_bypass_dedup(self):
  ledger=self.open(bound=2);a=ledger.begin();ledger.mark_sent(a)
  b=ledger.begin();ledger.mark_sent(b)
  self.assertNotEqual(a,b);self.assertEqual(len(ledger.pending()),2)
 def test_default_one_unresolved_still_blocks(self):
  from hosted_accounting import HostedAdmissionBlocked
  ledger=self.open();a=ledger.begin();ledger.mark_sent(a)
  with self.assertRaises(HostedAdmissionBlocked):ledger.begin()
  self.assertFalse(ledger.available())
if __name__=='__main__':
 suite=unittest.TestSuite([SendFence('test_cancel_during_committed_sent_marker_must_not_dispatch'),OptionalKey('test_unkeyed_distinct_attempts_bypass_dedup'),OptionalKey('test_default_one_unresolved_still_blocks')])
 raise SystemExit(not unittest.TextTestRunner(verbosity=2).run(suite).wasSuccessful())
