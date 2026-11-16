import contextlib,unittest
from test_hosted_admission_bound import Base,ServiceSentMarkerTests
from hosted_accounting import HostedAdmissionBlocked
class LostAck(Base):
 def arm(self,ledger):
  original=ledger.connect
  @contextlib.contextmanager
  def commit_then_raise(*a,**kw):
   with original(*a,**kw) as conn:yield conn
   if kw.get('write'):raise OSError('synthetic acknowledgment lost after durable commit')
  ledger.connect=commit_then_raise
  return original
 def test_reservation_lost_ack_keeps_key_and_hold(self):
  led=self.open();self.arm(led)
  with self.assertRaises(OSError):led.begin('fixed')
  fresh=self.open();self.assertEqual(len(fresh.pending()),1)
  self.assertEqual(fresh.pending_detail()[0]['sent'],False)
  with self.assertRaises(HostedAdmissionBlocked):fresh.begin('fixed')
 def test_sent_lost_ack_remains_possibly_sent(self):
  led=self.open();a=led.begin('fixed');self.arm(led)
  with self.assertRaises(OSError):led.mark_sent(a)
  fresh=self.open();self.assertEqual(fresh.pending_detail(),[{'id':a,'sent':True}])
 def test_terminal_lost_ack_keeps_durable_terminal_and_key(self):
  led=self.open();a=led.begin('fixed');led.mark_sent(a);self.arm(led)
  with self.assertRaises(OSError):led.complete(a)
  fresh=self.open();self.assertEqual(fresh.pending(),[])
  with self.assertRaises(HostedAdmissionBlocked):fresh.begin('fixed')
class ServiceLostAck(ServiceSentMarkerTests):
 def test_mark_commits_then_raises_no_post(self):
  original=self.chutes.ledger.mark_sent
  def lost(a):original(a);raise OSError('synthetic acknowledgment lost')
  self.chutes.ledger.mark_sent=lost
  from hosted_provider import HostedProviderError
  with self.assertRaises(HostedProviderError):self.service.generate_via_openai([{'role':'user','content':'synthetic'}],None,0)
  self.assertEqual(self.posts,[]);self.assertEqual(self.chutes.ledger.pending_detail()[0]['sent'],True)
if __name__=='__main__':
 suite=unittest.TestSuite([unittest.defaultTestLoader.loadTestsFromTestCase(LostAck),ServiceLostAck('test_mark_commits_then_raises_no_post')])
 raise SystemExit(not unittest.TextTestRunner(verbosity=2).run(suite).wasSuccessful())
