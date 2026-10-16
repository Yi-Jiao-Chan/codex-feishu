#!/usr/bin/env python3
"""Probe a disposable thread on the existing Codex daemon with a local model fixture."""
import base64
import hashlib
import http.server
import json
import os
import socket
import struct
import tempfile
import threading
import time
import uuid
from pathlib import Path

OUT = Path(__file__).resolve().parent
release = threading.Event()
model_called = threading.Event()
model_requests = []

class Fixture(http.server.BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'

    def log_message(self, *args):
        pass

    def do_GET(self):
        body = json.dumps({'object': 'list', 'data': []}).encode()
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers.get('Content-Length', 0))))
        serial = len(model_requests) + 1
        model_requests.append({'path': self.path, 'body': body})
        self.send_response(200)
        self.send_header('Content-Type', 'text/event-stream')
        self.send_header('Connection', 'close')
        self.end_headers()
        rid = 'bridge-fixture-response-' + str(serial)
        def event(value):
            self.wfile.write(('data: ' + json.dumps(value) + '\n\n').encode())
            self.wfile.flush()
        try:
            event({'type': 'response.created', 'response': {'id': rid}})
            if serial == 1:
                model_called.set()
                release.wait(35)
            item = {'type': 'message', 'id': 'bridge-fixture-message-' + str(serial),
                    'role': 'assistant', 'phase': 'final_answer',
                    'content': [{'type': 'output_text', 'text': 'bridge fixture reply ' + str(serial), 'annotations': []}]}
            event({'type': 'response.output_item.added', 'output_index': 0, 'item': dict(item, content=[])})
            event({'type': 'response.output_text.delta', 'item_id': item['id'], 'output_index': 0,
                   'content_index': 0, 'delta': item['content'][0]['text']})
            event({'type': 'response.output_item.done', 'output_index': 0, 'item': item})
            event({'type': 'response.completed', 'response': {'id': rid, 'status': 'completed',
                   'output': [item], 'usage': {'input_tokens': 1, 'output_tokens': 1, 'total_tokens': 2}}})
        except (BrokenPipeError, ConnectionResetError):
            pass
        self.close_connection = True

class Client:
    def __init__(self, sock_path, name):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(15)
        self.sock.connect(sock_path)
        key = base64.b64encode(os.urandom(16)).decode()
        request = ('GET / HTTP/1.1\r\nHost: localhost\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n'
                   'Sec-WebSocket-Key: ' + key + '\r\nSec-WebSocket-Version: 13\r\n\r\n')
        self.sock.sendall(request.encode())
        header = b''
        while not header.endswith(b'\r\n\r\n'):
            header += self.sock.recv(1)
        assert b'101 Switching Protocols' in header, header[:100]
        self.seq = 0
        self.events = []
        self.rpc('initialize', {'clientInfo': {'name': name, 'version': '0.1.0', 'title': 'Disposable bridge validation'},
                                'capabilities': {'experimentalApi': True}})
        self.send({'method': 'initialized'})

    def send(self, value):
        data = json.dumps(value).encode()
        mask = os.urandom(4)
        length = len(data)
        header = bytes([0x81, 0x80 | length]) if length < 126 else bytes([0x81, 0xfe]) + struct.pack('!H', length) if length < 65536 else bytes([0x81, 0xff]) + struct.pack('!Q', length)
        self.sock.sendall(header + mask + bytes(v ^ mask[i % 4] for i, v in enumerate(data)))

    def exact(self, n):
        data = b''
        while len(data) < n:
            chunk = self.sock.recv(n - len(data))
            if not chunk:
                raise EOFError('websocket disconnected')
            data += chunk
        return data

    def receive(self, timeout=15):
        self.sock.settimeout(timeout)
        while True:
            head = self.exact(2)
            length = head[1] & 127
            if length == 126:
                length = struct.unpack('!H', self.exact(2))[0]
            elif length == 127:
                length = struct.unpack('!Q', self.exact(8))[0]
            mask = self.exact(4) if head[1] & 128 else None
            data = self.exact(length)
            if mask:
                data = bytes(v ^ mask[i % 4] for i, v in enumerate(data))
            opcode = head[0] & 15
            if opcode == 8:
                raise EOFError('websocket closed')
            if opcode == 1:
                return json.loads(data)

    def rpc(self, method, params, allow_error=False):
        self.seq += 1
        rid = self.seq
        self.send({'id': rid, 'method': method, 'params': params})
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            value = self.receive(max(0.1, deadline - time.monotonic()))
            if value.get('id') == rid and ('result' in value or 'error' in value):
                if 'error' in value and not allow_error:
                    raise RuntimeError(method + ': ' + json.dumps(value['error']))
                return value
            self.events.append(value)
        raise TimeoutError(method)

    def close(self):
        self.sock.close()

def text(value):
    return [{'type': 'text', 'text': value, 'text_elements': []}]

def main():
    import subprocess
    daemon = json.loads(subprocess.check_output(['codex', 'app-server', 'daemon', 'version']))
    server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Fixture)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    provider = 'bridgefixture_' + uuid.uuid4().hex[:8]
    run_id = uuid.uuid4().hex[:12]
    client_a = Client(daemon['socketPath'], 'feishu-bridge-probe-a')
    client_b = Client(daemon['socketPath'], 'feishu-bridge-probe-b')
    cwd = tempfile.mkdtemp(prefix='codex-feishu-native-probe-')
    result = {'appServerVersion': daemon['appServerVersion'], 'localFixture': True, 'checks': {}, 'error': None}
    tid = None
    try:
        params = {'cwd': cwd, 'model': 'bridge-fixture-model', 'modelProvider': provider,
                  'approvalPolicy': 'never', 'sandbox': 'read-only', 'threadSource': 'feishu.bridge.validation',
                  'config': {'model_providers.' + provider: {'name': 'Local bridge fixture',
                            'base_url': 'http://127.0.0.1:' + str(server.server_port) + '/v1',
                            'wire_api': 'responses', 'requires_openai_auth': False,
                            'supports_websockets': False, 'request_max_retries': 0},
                             'model_reasoning_effort': 'low'}}
        started = client_a.rpc('thread/start', params)['result']['thread']
        tid = started['id']
        result['threadId'] = tid
        result['checks']['test_thread_created'] = True
        turn = client_a.rpc('turn/start', {'threadId': tid, 'input': text('bridge-probe-initial-' + run_id),
                    'clientUserMessageId': 'bridge-probe-initial-' + run_id})['result']['turn']
        result['activeTurnId'] = turn['id']
        if not model_called.wait(15):
            raise RuntimeError('local fixture did not receive a model request')
        result['checks']['active_turn_uses_local_fixture'] = True
        resumed = client_b.rpc('thread/resume', {'threadId': tid, 'excludeTurns': True})['result']['thread']
        result['checks']['two_clients_same_thread'] = resumed['id'] == tid
        def add(label):
            return client_b.rpc('thread/queue/add', {'threadId': tid, 'input': text(label + '-' + run_id),
                      'clientUserMessageId': label + '-' + run_id})['result']['queuedSubmission']
        q1 = add('bridge-probe-steer')
        q2 = add('bridge-probe-next-one')
        q3 = add('bridge-probe-next-two')
        listed = client_a.rpc('thread/queue/list', {'threadId': tid})['result']['data']
        result['checks']['queue_shared_between_clients'] = [q['id'] for q in listed] == [q1['id'], q2['id'], q3['id']]
        result['checks']['queue_does_not_steer_current_turn'] = len(model_requests) == 1
        busy = client_b.rpc('thread/queue/start', {'threadId': tid, 'queuedSubmissionId': q1['id']}, allow_error=True)
        result['queueStartWhileBusy'] = busy.get('error')
        result['checks']['queue_start_rejects_busy'] = 'error' in busy
        deleted = client_b.rpc('thread/queue/delete', {'threadId': tid, 'queuedSubmissionId': q1['id']})['result']['deleted']
        result['checks']['claimed_queued_message_for_steer'] = deleted
        steered = client_b.rpc('turn/steer', {'threadId': tid, 'expectedTurnId': turn['id'],
                   'input': q1['input'], 'clientUserMessageId': q1['clientUserMessageId']})['result']
        result['checks']['steer_targets_original_active_turn'] = steered['turnId'] == turn['id']
        wrong = client_b.rpc('turn/steer', {'threadId': tid, 'expectedTurnId': 'wrong-turn',
                   'input': text('bridge-probe-never-inject')}, allow_error=True)
        result['checks']['stale_turn_id_rejected'] = 'error' in wrong
        result['staleSteerError'] = wrong.get('error')
        release.set()
        done = []
        deadline = time.monotonic() + 25
        while time.monotonic() < deadline:
            for event in list(client_b.events):
                if event.get('method') == 'turn/completed' and event.get('params', {}).get('threadId') == tid:
                    turn_done = event['params']['turn']
                    if turn_done['id'] not in [x['id'] for x in done]:
                        done.append({'id': turn_done['id'], 'status': turn_done['status']})
            if len(done) >= 3:
                break
            try:
                client_b.events.append(client_b.receive(timeout=1))
            except socket.timeout:
                pass
        result['completedTurns'] = done
        result['checks']['native_queue_drains_after_turn'] = len(done) == 3 and all(x['status'] == 'completed' for x in done)
        result['checks']['queue_empty_after_drain'] = not client_b.rpc('thread/queue/list', {'threadId': tid})['result']['data']
        history = client_a.rpc('thread/read', {'threadId': tid, 'includeTurns': True})['result']['thread']
        users = []
        for ht in history['turns']:
            for item in ht.get('items', []):
                if item.get('type') == 'userMessage':
                    users.append({'turnId': ht['id'], 'clientId': item.get('clientId')})
        result['userMessageCorrelation'] = users
        ids = [x['clientId'] for x in users]
        result['checks']['all_inputs_persisted_once'] = all(ids.count(x) == 1 for x in ['bridge-probe-initial-' + run_id, q1['clientUserMessageId'], q2['clientUserMessageId'], q3['clientUserMessageId']])
        result['checks']['steer_persisted_in_current_turn'] = any(x['clientId'] == q1['clientUserMessageId'] and x['turnId'] == turn['id'] for x in users)
        result['checks']['queued_turn_order'] = [x['clientId'] for x in users if x['clientId'] in [q2['clientUserMessageId'],q3['clientUserMessageId']]] == [q2['clientUserMessageId'],q3['clientUserMessageId']]
        result['checks']['other_client_receives_user_events'] = any(e.get('method') in ['item/started','item/completed'] and e.get('params',{}).get('threadId') == tid and e.get('params',{}).get('item',{}).get('type') == 'userMessage' for e in client_b.events)
        result['checks']['other_client_receives_ai_events'] = any(e.get('method') in ['item/started','item/completed'] and e.get('params',{}).get('threadId') == tid and e.get('params',{}).get('item',{}).get('type') == 'agentMessage' for e in client_b.events)
        result['modelRequestCount'] = len(model_requests)
        result['checks']['steer_reaches_model_context'] = any(q1['input'][0]['text'] in json.dumps(x['body'].get('input')) for x in model_requests[1:])
    except Exception as e:
        result['error'] = type(e).__name__ + ': ' + str(e)
    finally:
        release.set()
        if tid:
            try:
                client_a.rpc('thread/archive', {'threadId': tid})
                result['testThreadArchived'] = True
            except Exception as e:
                result['archiveError'] = str(e)
        client_a.close()
        client_b.close()
        server.shutdown()
        OUT.joinpath('native-queue-probe-result.json').write_text(json.dumps(result, indent=2, ensure_ascii=False))
        print(json.dumps(result, indent=2, ensure_ascii=False), flush=True)

if __name__ == '__main__':
    main()
