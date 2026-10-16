#!/usr/bin/env python3
"""Additional native queue checks for retry, reconnect and interrupted turns."""
import importlib.util
import json
import socket
import subprocess
import tempfile
import threading
import time
import uuid

spec = importlib.util.spec_from_file_location('nativeprobe', str(__import__('pathlib').Path(__file__).resolve().with_name('native-queue-probe.py')))
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)

def main():
    d = json.loads(subprocess.check_output(['codex', 'app-server', 'daemon', 'version']))
    server = m.http.server.ThreadingHTTPServer(('127.0.0.1', 0), m.Fixture)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    provider = 'bridgerace_' + uuid.uuid4().hex[:8]
    run_id = uuid.uuid4().hex[:10]
    a, b = m.Client(d['socketPath'], 'feishu-bridge-race-a'), m.Client(d['socketPath'], 'feishu-bridge-race-b')
    clients = [a, b]
    tid = None
    r = {'appServerVersion': d['appServerVersion'], 'checks': {}, 'error': None}
    try:
        t = a.rpc('thread/start', {'cwd': tempfile.mkdtemp(prefix='codex-feishu-race-probe-'),
                'model': 'bridge-fixture-model', 'modelProvider': provider, 'sandbox': 'read-only',
                'approvalPolicy': 'never', 'threadSource': 'feishu.bridge.validation',
                'config': {'model_providers.' + provider: {'name': 'Local queue race fixture',
                  'base_url': 'http://127.0.0.1:' + str(server.server_port) + '/v1',
                  'wire_api': 'responses', 'requires_openai_auth': False,
                  'supports_websockets': False, 'request_max_retries': 0},
                  'model_reasoning_effort': 'low'}})['result']['thread']
        tid = t['id']
        r['threadId'] = tid
        r['testThreadMetadata'] = {k: t.get(k) for k in ['source','threadSource','originator','ephemeral','parentThreadId']}
        turn = a.rpc('turn/start', {'threadId': tid, 'input': m.text('race-fixture-' + run_id)})['result']['turn']
        if not m.model_called.wait(15):
            raise RuntimeError('fixture not called')
        b.rpc('thread/resume', {'threadId': tid, 'excludeTurns': True})
        r['observerReceivedThreadStarted'] = any(e.get('method') == 'thread/started' and e.get('params',{}).get('thread',{}).get('id') == tid for e in b.events)
        args = {'threadId': tid, 'input': m.text('retryable-message-' + run_id), 'clientUserMessageId': 'retryable-' + run_id}
        first = b.rpc('thread/queue/add', args)['result']['queuedSubmission']
        repeated = b.rpc('thread/queue/add', args)['result']['queuedSubmission']
        pending = a.rpc('thread/queue/list', {'threadId': tid})['result']['data']
        r['queueLengthAfterSameClientIdRetry'] = len(pending)
        r['clientIdIsIdempotencyKey'] = first['id'] == repeated['id'] and len(pending) == 1
        if first['id'] != repeated['id']:
            b.rpc('thread/queue/delete', {'threadId': tid, 'queuedSubmissionId': repeated['id']})
        claimed = b.rpc('thread/queue/delete', {'threadId': tid, 'queuedSubmissionId': first['id']})['result']['deleted']
        second_claim = b.rpc('thread/queue/delete', {'threadId': tid, 'queuedSubmissionId': first['id']})['result']['deleted']
        r['checks']['queue_delete_claims_only_once'] = claimed and not second_claim
        failure = b.rpc('turn/steer', {'threadId': tid, 'expectedTurnId': 'expired-turn',
                     'input': first['input'], 'clientUserMessageId': first['clientUserMessageId']}, allow_error=True)
        r['checks']['failed_steer_can_be_requeued_after_known_rejection'] = 'error' in failure
        restored = b.rpc('thread/queue/add', args)['result']['queuedSubmission']
        pending = a.rpc('thread/queue/list', {'threadId': tid})['result']['data']
        r['checks']['requeued_content_and_correlation_preserved'] = len(pending) == 1 and pending[0]['clientUserMessageId'] == first['clientUserMessageId'] and pending[0]['input'] == first['input']
        b.close()
        clients.remove(b)
        c = m.Client(d['socketPath'], 'feishu-bridge-race-reconnect')
        clients.append(c)
        c.rpc('thread/resume', {'threadId': tid, 'excludeTurns': True})
        after = c.rpc('thread/queue/list', {'threadId': tid})['result']['data']
        r['checks']['queue_survives_bridge_client_disconnect'] = [x['id'] for x in after] == [restored['id']]
        a.rpc('turn/interrupt', {'threadId': tid, 'turnId': turn['id']})
        deadline = time.monotonic() + 10
        interrupted = False
        while time.monotonic() < deadline and not interrupted:
            for e in c.events:
                if e.get('method') == 'turn/completed' and e.get('params',{}).get('threadId') == tid:
                    interrupted = e['params']['turn']['status'] == 'interrupted'
            if not interrupted:
                try:
                    c.events.append(c.receive(timeout=1))
                except socket.timeout:
                    pass
        r['checks']['interrupted_turn_keeps_pending_queue'] = interrupted and len(c.rpc('thread/queue/list', {'threadId': tid})['result']['data']) == 1
        a.rpc('thread/unsubscribe', {'threadId': tid})
        c.rpc('thread/unsubscribe', {'threadId': tid})
        after_unsubscribe = a.rpc('thread/queue/list', {'threadId': tid})['result']['data']
        r['checks']['pending_queue_readable_without_subscription'] = len(after_unsubscribe) == 1 and after_unsubscribe[0]['id'] == restored['id']
        r['threadStatusAfterUnsubscribe'] = a.rpc('thread/read', {'threadId': tid, 'includeTurns': False})['result']['thread']['status']
        # Explicitly clean the pending input before archiving the disposable thread.
        a.rpc('thread/queue/delete', {'threadId': tid, 'queuedSubmissionId': restored['id']})
    except Exception as e:
        r['error'] = type(e).__name__ + ': ' + str(e)
    finally:
        m.release.set()
        if tid:
            try:
                a.rpc('thread/archive', {'threadId': tid})
                r['testThreadArchived'] = True
            except Exception as e:
                r['archiveError'] = str(e)
        for client in clients:
            client.close()
        server.shutdown()
        m.OUT.joinpath('native-race-probe-result.json').write_text(json.dumps(r, indent=2, ensure_ascii=False))
        print(json.dumps(r, indent=2, ensure_ascii=False), flush=True)

if __name__ == '__main__':
    main()
