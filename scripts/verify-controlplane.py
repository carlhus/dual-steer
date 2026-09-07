#!/usr/bin/env python3
"""Verify PCF -> SMF -> PM-driven agent -> BPF on one persistent MPTCP flow."""
import argparse
import ctypes
import errno
import http.client
import json
import os
from pathlib import Path
import signal
import socket
import struct
import subprocess
import sys
import time
from urllib.parse import urlsplit


ROOT = Path(__file__).resolve().parents[1]
PREFIX = '/research/dualsteer/v1'


def run(*command):
    result = subprocess.run(list(map(str, command)), text=True,
                            stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    if result.returncode:
        raise RuntimeError(f'{list(map(str, command))!r} exited {result.returncode}:\n{result.stdout}')
    return result.stdout


def read_json(path):
    return json.loads(path.read_text())


class UnixHTTPConnection(http.client.HTTPConnection):
    def __init__(self, path):
        super().__init__('localhost', timeout=3)
        self.path = path

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self.path)


def request(base, method, path, body=None):
    if base.startswith('unix:'):
        connection = UnixHTTPConnection(base[5:])
    else:
        url = urlsplit(base)
        if url.scheme != 'http' or url.hostname not in ('127.0.0.1', 'localhost'):
            raise ValueError('Test control-plane HTTP endpoints must use local loopback')
        connection = http.client.HTTPConnection(url.hostname, url.port, timeout=3)
    try:
        payload = None if body is None else json.dumps(body).encode()
        headers = {} if body is None else {'Content-Type': 'application/json'}
        connection.request(method, path, payload, headers)
        response = connection.getresponse()
        raw = response.read()
        if not raw:
            content = None
        else:
            try:
                content = json.loads(raw)
            except json.JSONDecodeError:
                content = raw.decode(errors='replace')
        return response.status, content
    finally:
        connection.close()


class ObserverMap:
    """Read-only BPF observer; this driver never updates or deletes map entries."""
    def __init__(self, reference, name, key_size, value_size):
        if not reference.startswith('id:') or not reference[3:].isdigit():
            raise ValueError('BPF map references must use id:N')
        self.nr = {'x86_64': 321, 'aarch64': 280}[os.uname().machine]
        self.libc = ctypes.CDLL(None, use_errno=True)
        self.fd = self.call(14, struct.pack('=III', int(reference[3:]), 0, 0))
        self.key_size, self.value_size = key_size, value_size
        try:
            info = ctypes.create_string_buffer(40)
            self.call(15, struct.pack('=IIQ', self.fd, len(info), ctypes.addressof(info)))
            actual = struct.unpack_from('=IIII', info.raw)
            actual_name = info.raw[24:40].split(b'\0', 1)[0].decode()
            if actual != (1, int(reference[3:]), key_size, value_size) or actual_name != name:
                raise RuntimeError(f'Unexpected BPF map ABI: {actual_name} {actual}')
        except Exception:
            self.close()
            raise

    def call(self, command, payload):
        attr = ctypes.create_string_buffer(payload)
        result = self.libc.syscall(self.nr, command, attr, len(payload))
        if result < 0:
            code = ctypes.get_errno()
            raise OSError(code, os.strerror(code))
        return result

    def lookup(self, key):
        if len(key) != self.key_size:
            raise ValueError('Incorrect observer key size')
        key_buffer = ctypes.create_string_buffer(key)
        value = ctypes.create_string_buffer(self.value_size)
        try:
            self.call(1, struct.pack('=IIQQQ', self.fd, 0,
                                    ctypes.addressof(key_buffer), ctypes.addressof(value), 0))
        except OSError as exc:
            if exc.errno == errno.ENOENT:
                return None
            raise
        return value.raw

    def entries(self):
        entries, previous = [], None
        # The isolated test has one connection and two paths. A larger result
        # indicates unexpected residual state, so bound observation explicitly.
        for _ in range(100):
            key = None if previous is None else ctypes.create_string_buffer(previous)
            following = ctypes.create_string_buffer(self.key_size)
            try:
                self.call(4, struct.pack('=IIQQ', self.fd, 0,
                                        0 if key is None else ctypes.addressof(key),
                                        ctypes.addressof(following)))
            except OSError as exc:
                if exc.errno == errno.ENOENT:
                    return entries
                raise
            previous = following.raw
            value = self.lookup(previous)
            if value is not None:
                entries.append((previous, value))
        raise RuntimeError('Unexpectedly many or continuously changing entries in test maps')

    def close(self):
        if self.fd is not None:
            os.close(self.fd)
            self.fd = None


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--policy-map', required=True)
    parser.add_argument('--path-map', required=True)
    parser.add_argument('--stats-map', required=True)
    parser.add_argument('--pcf', type=Path, default=ROOT/'build/pcf-research')
    parser.add_argument('--smf', type=Path, default=ROOT/'build/smf-research')
    parser.add_argument('--agent', type=Path, default=ROOT/'build/dualsteer-agent')
    parser.add_argument('--output', type=Path, default=ROOT/'build/runtime-controlplane')
    parser.add_argument('--phase-seconds', type=float, default=6)
    args = parser.parse_args()
    if os.geteuid() != 0 or 'dualsteer.guest=1' not in Path('/proc/cmdline').read_text().split():
        parser.error('run only as root inside the disposable DualSteer guest')
    if args.phase_seconds <= 0 or args.phase_seconds > 30:
        parser.error('--phase-seconds must be within (0, 30]')
    out = args.output.resolve()
    out.mkdir(parents=True, exist_ok=True)
    for name in ('sender.json', 'receiver.json', 'report.json'):
        (out/name).unlink(missing_ok=True)
    report = {
        'passed': False, 'kernel': run('uname', '-r').strip(), 'scheduler': 'bpf_ds_wrr',
        'interface': 'research extension; not a standardized 3GPP SBI',
        'map_access': 'read-only observer; all policy/path writes originate from the PM-driven agent',
        'manual_connection_identifiers_supplied': False, 'phases': [], 'control_requests': [],
    }
    pcf_url, smf_url = 'http://127.0.0.1:18081', 'http://127.0.0.1:18082'
    agent_socket = Path('/run/dualsteer-agent.sock')
    agent_url = 'unix:'+str(agent_socket)
    children, logs, maps = {}, [], {}
    created_lab = context_created = False
    context_path = PREFIX+'/contexts/lab'

    def spawn(name, command, namespace=None):
        if namespace:
            command = ['ip', 'netns', 'exec', namespace, *command]
        command = list(map(str, command))
        log = (out/(name+'.log')).open('w')
        logs.append(log)
        child = subprocess.Popen(command, stdout=log, stderr=subprocess.STDOUT)
        children[name] = child
        report.setdefault('processes', {})[name] = {'command': command, 'pid': child.pid}
        return child

    def daemons_alive():
        for name in ('agent', 'pcf', 'smf'):
            child = children.get(name)
            if child is not None and child.poll() is not None:
                tail = (out/(name+'.log')).read_text()[-3000:]
                raise RuntimeError(f'{name} exited {child.returncode}:\n{tail}')

    def wait(label, check, timeout=25):
        deadline, last = time.monotonic()+timeout, None
        while time.monotonic() < deadline:
            daemons_alive()
            try:
                last = check()
                if last:
                    return last
            except (OSError, http.client.HTTPException, json.JSONDecodeError) as exc:
                last = str(exc)
            time.sleep(.2)
        raise RuntimeError(f'Timed out waiting for {label}; last observation: {last!r}')

    def get_context(base):
        status, body = request(base, 'GET', context_path)
        return body if status == 200 else None

    def write_request(base, method, path, body=None, accepted=(200,)):
        status, response = request(base, method, path, body)
        report['control_requests'].append({'component': base, 'method': method, 'path': path,
                                           'body': body, 'status': status, 'response': response})
        if status not in accepted:
            raise RuntimeError(f'{method} {base}{path} returned {status}: {response!r}')
        return status, response

    def connection_key():
        observed = report['connection']
        return struct.pack('=QII', observed['netnsInode'], observed['token'], 0)

    def read_policy():
        raw = maps['policy'].lookup(connection_key())
        if raw is None:
            return None
        return dict(zip(('generation', 'enabled', 'mode', 'weight_a', 'weight_b', 'rtt_delta_us'),
                        struct.unpack('=IIIIII', raw)))

    def read_stats():
        raw = maps['stats'].lookup(connection_key())
        if raw is None:
            raise RuntimeError('No scheduler statistics for the observed connection')
        return dict(zip(('generation', 'mode', 'selected_a', 'selected_b', 'fallback'),
                        struct.unpack('=IIQQQ', raw)))

    def read_paths():
        paths = []
        for key, value in maps['paths'].entries():
            if key[:16] != connection_key():
                continue
            local_id, remote_id = struct.unpack_from('=II', key, 16)
            generation, access = struct.unpack('=II', value)
            paths.append({'localId': local_id, 'remoteId': remote_id,
                          'generation': generation, 'access': access})
        return sorted(paths, key=lambda path: (path['access'], path['localId'], path['remoteId']))

    def active_binding(generation):
        state = get_context(agent_url)
        if not state or state.get('generation') != generation:
            return None
        ready = [binding for binding in state.get('bindings', [])
                 if binding.get('ready') and binding.get('generation') == generation]
        if len(ready) != 1:
            return None
        binding = ready[0]
        observed = report['connection']
        if binding.get('token') != observed['token'] or binding.get('netnsInode') != observed['netnsInode']:
            raise RuntimeError('Agent bound a different connection from the persistent test stream')
        if {path.get('access') for path in binding.get('paths', [])} != {0, 1}:
            return None
        return state

    def smf_applied(generation, weight):
        state = get_context(smf_url)
        if (state and state.get('generation') == generation and
                state.get('appliedGeneration') == generation and not state.get('deleting') and
                state.get('policy', {}).get('weightA') == weight and
                state.get('policy', {}).get('weightB') == 100-weight):
            return state
        return None

    def bpf_applied(generation, weight):
        policy = read_policy()
        paths = read_paths()
        if (policy and policy['generation'] == generation and policy['enabled'] == 1 and
                policy['mode'] == 1 and policy['weight_a'] == weight and
                policy['weight_b'] == 100-weight and len(paths) == 2 and
                {path['access'] for path in paths} == {0, 1} and
                all(path['generation'] == generation for path in paths)):
            return {'policy': policy, 'paths': paths}
        return None

    def phase(generation, weight):
        smf_state = wait(f'SMF generation {generation}', lambda: smf_applied(generation, weight))
        agent_state = wait(f'automatic binding generation {generation}', lambda: active_binding(generation))
        bpf_state = wait(f'BPF generation {generation}', lambda: bpf_applied(generation, weight))
        time.sleep(2)
        before = {'stats': read_stats(), 'receiver': read_json(out/'receiver.json')}
        time.sleep(args.phase_seconds)
        after = {'stats': read_stats(), 'receiver': read_json(out/'receiver.json'),
                 'sender': read_json(out/'sender.json')}
        daemons_alive()
        observed = report['connection']
        sender = after['sender']
        if (sender['token'] != observed['token'] or sender['netnsInode'] != observed['netnsInode'] or
                sender['fallback'] or sender['additional_subflows'] < 1 or sender['state'] != 'running'):
            raise RuntimeError('Persistent two-subflow MPTCP connection changed or stopped')
        delta = {name: after['stats'][name]-before['stats'][name]
                 for name in ('selected_a', 'selected_b', 'fallback')}
        total = delta['selected_a']+delta['selected_b']
        received = after['receiver']['bytes']-before['receiver']['bytes']
        share = delta['selected_a']/total if total else None
        result = {'generation': generation, 'requested_weight_a': weight, 'smf': smf_state,
                  'agent': agent_state, 'bpf': bpf_state, 'scheduler_stats': after['stats'],
                  'scheduler_decision_delta': delta, 'selection_share_a': share,
                  'received_payload_bytes': received, 'mptcp': sender}
        report['phases'].append(result)
        print(json.dumps(result), flush=True)
        if (min(delta['selected_a'], delta['selected_b']) <= 0 or received <= 0 or
                share is None or abs(share-weight/100) > .05 or
                after['stats']['generation'] != generation or after['stats']['mode'] != 1):
            raise RuntimeError(f'Generation {generation} did not produce the requested WRR behavior')
        return result

    def stop(name, daemon=False):
        child = children.get(name)
        if child is None:
            return
        if child.poll() is None:
            child.send_signal(signal.SIGTERM)
        try:
            code = child.wait(timeout=10)
        except subprocess.TimeoutExpired:
            child.kill()
            child.wait(timeout=3)
            raise RuntimeError(f'{name} required forced termination')
        report['processes'][name]['exitCode'] = code
        if daemon and code != 0:
            raise RuntimeError(f'{name} did not shut down cleanly: exit {code}')

    def empty_maps():
        counts = {name: len(observer.entries()) for name, observer in maps.items()}
        return {'counts': counts} if not any(counts.values()) else None

    try:
        maps['policy'] = ObserverMap(args.policy_map, 'ds_policy_map', 16, 24)
        maps['paths'] = ObserverMap(args.path_map, 'ds_path_map', 24, 8)
        maps['stats'] = ObserverMap(args.stats_map, 'ds_stats_map', 16, 32)
        if not empty_maps():
            raise RuntimeError('Control-plane acceptance requires empty initial policy/path/stats maps')
        report['software_pins'] = read_json(ROOT/'integration/free5gc/pins.json')
        run('ip', 'link', 'set', 'lo', 'up')
        run('bash', ROOT/'scripts/netns-lab.sh', 'up')
        created_lab = True
        run('ip', 'netns', 'exec', 'ds-ue', 'sysctl', '-qw', 'net.mptcp.scheduler=bpf_ds_wrr')
        for leg in ('a', 'b'):
            run('ip', 'netns', 'exec', 'ds-ue', 'tc', 'qdisc', 'replace', 'dev', 'leg-'+leg,
                'root', 'netem', 'delay', '5ms')
        if agent_socket.exists():
            raise RuntimeError('Refusing to reuse an existing agent Unix socket')
        spawn('agent', [args.agent, 'serve', '--listen', agent_url,
                        '--policy-map', args.policy_map, '--path-map', args.path_map], 'ds-ue')
        def agent_ready():
            status, body = request(agent_url, 'GET', PREFIX+'/status')
            return body if status == 200 and isinstance(body, dict) and body.get('ready') else None
        report['agent_ready_before_traffic'] = wait('PM agent readiness', agent_ready)
        initial = {'enabled': True, 'mode': 'load-balance', 'weightA': 70,
                   'weightB': 30, 'rttDeltaUs': 1000}
        pcf_config = {'listen': '127.0.0.1:18081', 'policies': {'internet': initial}}
        smf_config = {'listen': '127.0.0.1:18082', 'pcf_url': pcf_url,
                      'agent_socket': str(agent_socket), 'poll_ms': 200}
        for component, config, binary in [('pcf', pcf_config, args.pcf), ('smf', smf_config, args.smf)]:
            config_path = out/(component+'.json')
            config_path.write_text(json.dumps(config, indent=2)+'\n')
            spawn(component, [binary, '--dualsteer-research', '--dualsteer-config', config_path])
            base = pcf_url if component == 'pcf' else smf_url
            wait(component+' health', lambda base=base: request(base, 'GET', '/healthz')[0] == 200)
        status, seeded = request(pcf_url, 'GET', PREFIX+'/policies/internet')
        if status != 200 or seeded != initial:
            raise RuntimeError(f'PCF did not load initial operator configuration: {status} {seeded!r}')
        report['initial_pcf_policy'] = seeded
        spec = {'id': 'lab', 'dnn': 'internet',
                'legs': {'A': {'ifname': 'leg-a', 'localAddress': '10.60.1.2'},
                         'B': {'ifname': 'leg-b', 'localAddress': '10.60.2.2'}},
                'flow': {'destinationAddress': '10.60.1.1', 'destinationPort': 5001}}
        write_request(smf_url, 'POST', PREFIX+'/contexts', spec, accepted=(200, 201, 202))
        context_created = True
        report['smf_context_before_traffic'] = wait('initial SMF delivery', lambda: smf_applied(1, 70))
        pending = wait('agent pending context', lambda: get_context(agent_url))
        if pending.get('generation') != 1 or pending.get('bindings'):
            raise RuntimeError('Agent context was not pending before the stream existed')
        report['agent_pending_before_traffic'] = pending
        duration = max(180, args.phase_seconds*4+120)
        spawn('receiver', [sys.executable, ROOT/'scripts/traffic.py', 'receive', '--seconds', duration+10,
                           '--status', out/'receiver.json'], 'ds-peer')
        time.sleep(.3)
        spawn('sender', [sys.executable, ROOT/'scripts/traffic.py', 'send', '--seconds', duration,
                         '--status', out/'sender.json'], 'ds-ue')
        def established():
            observed = read_json(out/'sender.json')
            return observed if observed['additional_subflows'] >= 1 and not observed['fallback'] else None
        report['connection'] = wait('two MPTCP subflows', established)
        first = phase(1, 70)
        updated = dict(initial, weightA=20, weightB=80)
        # The sole live update is the operator-facing PCF PUT. SMF polling and
        # the agent's PM binding must carry it to the existing connection.
        write_request(pcf_url, 'PUT', PREFIX+'/policies/internet', updated)
        second = phase(2, 20)
        if second['selection_share_a'] >= first['selection_share_a']:
            raise RuntimeError('PCF update did not shift the existing flow toward leg B')
        report['same_connection_verified'] = True
        stop('sender')
        stop('receiver')
        def closed_binding():
            state = get_context(agent_url)
            return state if state and not state.get('bindings') else None
        report['agent_after_pm_close'] = wait('automatic PM-close unbinding', closed_binding)
        report['maps_after_pm_close'] = wait('automatic PM-close map cleanup', empty_maps)
        write_request(smf_url, 'DELETE', context_path, accepted=(204, 202))
        def context_removed():
            smf_code = request(smf_url, 'GET', context_path)[0]
            agent_code = request(agent_url, 'GET', context_path)[0]
            return {'smf_status': smf_code, 'agent_status': agent_code} if smf_code == agent_code == 404 else None
        report['context_delete'] = wait('SMF and agent context removal', context_removed)
        context_created = False
        report['controlplane_verified'] = True
        report['passed'] = True
    except Exception as exc:
        report['error'] = str(exc)
        print('Control-plane verification failed: '+str(exc), file=sys.stderr, flush=True)
    finally:
        cleanup_errors = []
        def cleanup(label, action):
            try:
                action()
            except Exception as exc:
                cleanup_errors.append(label+': '+str(exc))
        if context_created and children.get('smf') and children['smf'].poll() is None:
            cleanup('delete SMF context', lambda: write_request(smf_url, 'DELETE', context_path, accepted=(202, 204, 404)))
        for name in ('sender', 'receiver'):
            cleanup('stop '+name, lambda name=name: stop(name))
        for name in ('smf', 'pcf', 'agent'):
            cleanup('shutdown '+name, lambda name=name: stop(name, daemon=True))
        if 'agent' in children and agent_socket.exists():
            cleanup_errors.append('agent Unix socket remained after shutdown')
        if created_lab:
            cleanup('remove namespaces', lambda: run('bash', ROOT/'scripts/netns-lab.sh', 'down'))
        for observer in maps.values():
            cleanup('close observer map', observer.close)
        for log in logs:
            cleanup('close log', log.close)
        report['clean_shutdown'] = not cleanup_errors
        if cleanup_errors:
            report['cleanup_errors'] = cleanup_errors
            report['passed'] = False
        (out/'report.json').write_text(json.dumps(report, indent=2)+'\n')
    return 0 if report['passed'] else 1


if __name__ == '__main__':
    sys.exit(main())
