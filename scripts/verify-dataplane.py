#!/usr/bin/env python3
"""Run finite dual-path MPTCP measurements in the isolated ds-* lab namespaces."""
import argparse
import ctypes
import json
import os
from pathlib import Path
import re
import subprocess
import struct
import sys
import time

ROOT = Path(__file__).resolve().parents[1]


def run(*cmd):
    result = subprocess.run([str(x) for x in cmd], text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    if result.returncode:
        raise RuntimeError('Command '+repr(list(map(str, cmd)))+' exited '+str(result.returncode)+':\n'+result.stdout.rstrip())
    return result.stdout


def ns(*cmd):
    return run('ip', 'netns', 'exec', 'ds-ue', *cmd)


def read(path):
    return json.loads(path.read_text())


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--baseline', action='store_true', help='measure host default scheduler only')
    p.add_argument('--bpf', action='store_true', help='exercise live policies on already loaded bpf_ds_wrr')
    p.add_argument('--policy-map')
    p.add_argument('--path-map')
    p.add_argument('--stats-map', help='id:N of ds_stats_map; required for proving custom scheduler decisions')
    p.add_argument('--reuse-lab', action='store_true', help='explicitly use existing dedicated ds-* namespaces')
    p.add_argument('--output', type=Path, default=ROOT/'build/runtime')
    p.add_argument('--phase-seconds', type=float, default=6)
    p.add_argument('--agent', type=Path, default=ROOT/'build/dualsteer-agent')
    a = p.parse_args()
    if os.geteuid() != 0:
        p.error('run with sudo (or directly as root)')
    if a.baseline and a.bpf:
        p.error('--baseline and --bpf are mutually exclusive')
    bpf = a.bpf or bool(a.policy_map or a.path_map)
    if bpf and not (a.policy_map and a.path_map):
        p.error('BPF verification needs both explicit map bindings')
    out = a.output.resolve()
    out.mkdir(parents=True, exist_ok=True)
    for name in ('sender.json', 'receiver.json', 'agent.log'):
        (out/name).unlink(missing_ok=True)
    report = dict(kernel=run('uname', '-r').strip(), scheduler='bpf_ds_wrr' if bpf else 'default',
                  measurement_note='Interface TX bytes include headers/retransmits. Byte share is not scheduler selection share.',
                  phases=[], passed=False)
    processes, files = [], []
    created, config = False, None
    def spawn(namespace, command, name):
        f = (out/name).open('w')
        files.append(f)
        child = subprocess.Popen(['ip', 'netns', 'exec', namespace, *map(str, command)], stdout=f, stderr=subprocess.STDOUT)
        processes.append(child)
        return child
    def delay(leg, ms):
        ns('tc', 'qdisc', 'replace', 'dev', 'leg-'+leg, 'root', 'netem', 'delay', str(ms)+'ms')
    def stats():
        if not a.stats_map:
            return None
        if not a.stats_map.startswith('id:'):
            raise ValueError('--stats-map expects id:N')
        # Direct native bpf syscall avoids bpftool's version-dependent BTF JSON formatting.
        machine = os.uname().machine
        nr = {'x86_64': 321, 'aarch64': 280}.get(machine)
        if nr is None:
            raise RuntimeError('stats syscall unsupported architecture: '+machine)
        libc = ctypes.CDLL(None, use_errno=True)
        def call(command, data):
            attr = ctypes.create_string_buffer(data)
            result = libc.syscall(nr, command, attr, len(data))
            if result < 0:
                code = ctypes.get_errno()
                raise OSError(code, os.strerror(code))
            return result
        fd = call(14, struct.pack('=III', int(a.stats_map[3:]), 0, 0))
        try:
            info = ctypes.create_string_buffer(40)
            call(15, struct.pack('=IIQ', fd, len(info), ctypes.addressof(info)))
            map_type, map_id, key_size, value_size = struct.unpack_from('=IIII', info.raw)
            map_name = info.raw[24:40].split(b'\0', 1)[0]
            if (map_type != 1 or map_id != int(a.stats_map[3:]) or key_size != 16
                    or value_size != 32 or map_name != b'ds_stats_map'):
                raise RuntimeError('Stats map must be HASH ds_stats_map with 16-byte keys and 32-byte values')
            conn = report['connection']
            key = ctypes.create_string_buffer(struct.pack('=QII', conn['netnsInode'], conn['token'], 0))
            value = ctypes.create_string_buffer(32)
            call(1, struct.pack('=IIQQQ', fd, 0, ctypes.addressof(key), ctypes.addressof(value), 0))
            return dict(zip(('generation', 'mode', 'selected_a', 'selected_b', 'fallback'),
                            struct.unpack('=IIQQQ', value.raw)))
        finally:
            os.close(fd)
    def snapshot():
        links = json.loads(run('ip', '-n', 'ds-ue', '-s', '-j', 'link', 'show'))
        tx = {x['ifname']: x.get('stats64', x.get('stats', {}))['tx']['bytes'] for x in links}
        ss = ns('ss', '-tin')
        payload = {'A': 0, 'B': 0}
        for block in re.split(r'(?m)(?=^ESTAB)', ss):
            first = block.split('\n', 1)[0]
            for address, leg in [('10.60.1.1:', 'A'), ('10.60.2.1:', 'B')]:
                sent = re.search(r'bytes_sent:(\d+)', block)
                if address in first and sent:
                    payload[leg] += int(sent.group(1))
        return dict(tx=tx, tcp_payload=payload, ss=ss, stats=stats(),
                    sender=read(out/'sender.json'), receiver=read(out/'receiver.json'))
    def phase(name, seconds=None, settle=2):
        time.sleep(settle)
        before = snapshot()
        time.sleep(seconds if seconds is not None else a.phase_seconds)
        after = snapshot()
        tx = {leg: after['tx']['leg-'+leg.lower()]-before['tx']['leg-'+leg.lower()] for leg in ('A', 'B')}
        payload = {leg: after['tcp_payload'][leg]-before['tcp_payload'][leg] for leg in ('A', 'B')}
        received = after['receiver']['bytes']-before['receiver']['bytes']
        result = dict(name=name, interface_tx_bytes=tx, tcp_payload_sent_delta=payload, received_payload_bytes=received,
                      byte_share_A=tx['A']/max(1, sum(tx.values())), mptcp=after['sender'])
        if after['stats'] is not None:
            result['scheduler_stats'] = after['stats']
            result['scheduler_decision_delta'] = {
                field: after['stats'][field]-before['stats'][field]
                for field in ('selected_a', 'selected_b', 'fallback')}
            decisions = result['scheduler_decision_delta']
            custom = decisions['selected_a']+decisions['selected_b']
            result['selection_share_A'] = decisions['selected_a']/custom if custom else None
        report['phases'].append(result)
        (out/(name+'-ss.txt')).write_text(after['ss'])
        print(json.dumps(result), flush=True)
        if received <= 0 or after['sender']['fallback']:
            raise RuntimeError(name+': no received payload or MPTCP fell back to TCP')
        return result
    def agent(command):
        try:
            result = ns(a.agent, '--config', config, command, '--policy-map', a.policy_map, '--path-map', a.path_map)
        except Exception as exc:
            with (out/'agent.log').open('a') as f:
                f.write(command+' FAILED\n'+str(exc)+'\n')
            raise
        with (out/'agent.log').open('a') as f:
            f.write(command+'\n'+result+'\n')
        return result
    def policy(generation, weight, mode='load-balance'):
        nonlocal config
        config = out/('policy-'+str(generation)+'.yaml')
        lines = ['dualSteer:', '  enabled: true', '  generation: '+str(generation),
                 '  mode: '+mode, '  rttDeltaUs: 1000', '  connection:',
                 '    token: '+str(report['connection']['token']),
                 '    netnsInode: '+str(report['connection']['netnsInode']), '  legs:']
        for leg, w in [('A', weight), ('B', 100-weight)]:
            local, remote = report['endpoints'][leg]
            lines += ['    '+leg+':', '      ifname: leg-'+leg.lower(), '      weight: '+str(w),
                      '      endpoints:', '        - localId: '+str(local), '          remoteId: '+str(remote)]
        config.write_text('\n'.join(lines)+'\n')
        agent('apply')
        agent('status')
    try:
        if not a.reuse_lab:
            run('bash', ROOT/'scripts/netns-lab.sh', 'up')
            created = True
        ns('sysctl', '-qw', 'net.mptcp.scheduler='+report['scheduler'])
        spawn('ds-ue', ['stdbuf', '-oL', 'ip', 'mptcp', 'monitor'], 'pm-events.log')
        time.sleep(.3)
        duration = max(180, a.phase_seconds*10+100)
        spawn('ds-peer', [sys.executable, ROOT/'scripts/traffic.py', 'receive', '--seconds', duration+10,
                          '--status', out/'receiver.json'], 'receiver.log')
        time.sleep(.3)
        spawn('ds-ue', [sys.executable, ROOT/'scripts/traffic.py', 'send', '--seconds', duration,
                        '--status', out/'sender.json'], 'sender.log')
        deadline = time.monotonic()+20
        while time.monotonic() < deadline:
            if (out/'sender.json').exists() and read(out/'sender.json')['additional_subflows'] >= 1:
                break
            time.sleep(.2)
        else:
            raise RuntimeError('Two MPTCP subflows were not established; inspect PM and workload logs')
        report['connection'] = read(out/'sender.json')
        token = report['connection']['token']
        endpoints = {}
        for line in (out/'pm-events.log').read_text().splitlines():
            fields = dict(re.findall(r'(\w+)=([^ ]+)', line))
            if 'ESTABLISHED' not in line or int(fields.get('token', '-1'), 16) != token:
                continue
            leg = {'10.60.1.1': 'A', '10.60.2.1': 'B'}.get(fields.get('daddr4'))
            if leg and 'locid' in fields and 'remid' in fields:
                endpoints[leg] = [int(fields['locid']), int(fields['remid'])]
        if set(endpoints) != {'A', 'B'}:
            raise RuntimeError('Could not discover both endpoint ID pairs from PM events')
        report['endpoints'] = endpoints
        base = phase('missing-policy' if bpf else 'default-baseline')
        if min(base['tcp_payload_sent_delta'].values()) <= 0:
            raise RuntimeError('Both legs must show traffic')
        if bpf:
            delay('a', 5)
            delay('b', 5)
            policy(1, 70)
            first = phase('wrr-70-30')
            policy(2, 20)
            second = phase('wrr-20-80')
            report['wrr_share_shift_toward_B'] = second['byte_share_A'] < first['byte_share_A']
            if a.stats_map:
                for item, gen in [(first, 1), (second, 2)]:
                    if item['scheduler_stats']['generation'] != gen or item['scheduler_stats']['mode'] != 1:
                        raise RuntimeError('WRR generation/mode not observed by scheduler')
                    if min(item['scheduler_decision_delta'][key] for key in ('selected_a', 'selected_b')) <= 0:
                        raise RuntimeError('WRR did not schedule both legs')
                if second['selection_share_A'] >= first['selection_share_A']:
                    raise RuntimeError('WRR selection counts did not shift toward B after generation update')
            # Byte ratios are observations, never asserted to equal WRR selection counts.
            delay('a', 5)
            delay('b', 40)
            policy(3, 50, 'lowest-rtt')
            rtt_a = phase('rtt-A-fast', settle=6)
        delay('a', 40)
        delay('b', 5)
        rtt_b = phase('rtt-B-fast' if bpf else 'default-RTT-flip', settle=6)
        if bpf and a.stats_map:
            if not (rtt_a['selection_share_A'] is not None and rtt_b['selection_share_A'] is not None
                    and rtt_a['selection_share_A'] > .5 and rtt_b['selection_share_A'] < .5):
                raise RuntimeError('Custom RTT selections did not favor the lower-delay leg in both phases')
        if bpf:
            agent('delete')
            deleted = phase('deleted-policy-fallback')
            if a.stats_map:
                for item in (base, deleted):
                    delta = item['scheduler_decision_delta']
                    if delta['fallback'] <= 0 or delta['selected_a'] or delta['selected_b']:
                        raise RuntimeError('Missing policy did not exclusively use default fallback')
            delay('a', 5)
            delay('b', 5)
            policy(4, 70)
        run('ip', '-n', 'ds-ue', 'link', 'set', 'leg-a', 'down')
        down = phase('leg-A-down', seconds=max(10, a.phase_seconds), settle=5)
        if down['tcp_payload_sent_delta']['B'] <= 0:
            raise RuntimeError('Surviving leg B had no TCP payload traffic')
        if bpf and a.stats_map:
            if (down['scheduler_stats']['generation'] != 4 or down['scheduler_stats']['mode'] != 1
                    or down['scheduler_decision_delta']['selected_b'] <= 0):
                raise RuntimeError('Surviving leg B was not selected by active generation 4 WRR policy')
        run('ip', '-n', 'ds-ue', 'link', 'set', 'leg-a', 'up')
        phase('link-restored', settle=4)
        report['custom_scheduler_verified'] = bool(bpf and a.stats_map)
        report['passed'] = True
    except Exception as exc:
        report['error'] = str(exc)
        print('Verification failed: '+str(exc), file=sys.stderr)
    finally:
        cleanup_errors = []
        def cleanup(label, action):
            try:
                action()
            except Exception as exc:
                cleanup_errors.append(label+': '+str(exc))
        if bpf and config:
            cleanup('delete policy', lambda: agent('delete'))
        for child in reversed(processes):
            cleanup('terminate process '+str(child.pid), child.terminate)
        for child in processes:
            def stop(child=child):
                try:
                    child.wait(timeout=4)
                except subprocess.TimeoutExpired:
                    child.kill()
                    child.wait(timeout=4)
            cleanup('wait for process '+str(child.pid), stop)
        for f in files:
            cleanup('close log '+f.name, f.close)
        if created or a.reuse_lab:
            cleanup('restore leg A', lambda: run('ip', '-n', 'ds-ue', 'link', 'set', 'leg-a', 'up'))
            cleanup('restore scheduler', lambda: ns('sysctl', '-qw', 'net.mptcp.scheduler=default'))
            cleanup('restore leg A delay', lambda: delay('a', 5))
            cleanup('restore leg B delay', lambda: delay('b', 40))
        if created:
            cleanup('remove namespaces', lambda: run('bash', ROOT/'scripts/netns-lab.sh', 'down'))
        if cleanup_errors:
            report['cleanup_errors'] = cleanup_errors
            report['passed'] = False
        try:
            (out/'report.json').write_text(json.dumps(report, indent=2)+'\n')
        except Exception as exc:
            report['passed'] = False
            report['report_write_error'] = str(exc)
            print(json.dumps(report), file=sys.stderr)
    return 0 if report['passed'] else 1


if __name__ == '__main__':
    sys.exit(main())
