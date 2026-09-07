#!/usr/bin/env python3
"""Finite, persistent IPPROTO_MPTCP workload, with atomic live JSON status."""
import argparse
import json
import os
import select
import socket
import struct
import time
from pathlib import Path


def status(sock, count, started, state):
    # Linux uapi mptcp.h: u8 subflows at 0, padding then u32 flags/token at 8/12.
    raw = sock.getsockopt(284, 1, 256)
    flags, token = struct.unpack_from('=II', raw, 8)
    out = dict(state=state, bytes=count, elapsed=time.monotonic()-started,
               token=token, flags=flags, fallback=bool(flags & 1),
               additional_subflows=raw[0], netnsInode=os.stat('/proc/self/ns/net').st_ino)
    for name, offset in [('bytes_retrans', 48), ('bytes_sent', 56),
                         ('bytes_received', 64), ('bytes_acked', 72)]:
        if len(raw) >= offset+8:
            out[name] = struct.unpack_from('=Q', raw, offset)[0]
    return out


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('role', choices=['send', 'receive'])
    p.add_argument('--address', default='10.60.1.1')
    p.add_argument('--port', type=int, default=5001)
    p.add_argument('--seconds', type=float, default=120)
    p.add_argument('--status', type=Path, required=True)
    a = p.parse_args()
    sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM, getattr(socket, 'IPPROTO_MPTCP', 262))
    sock.settimeout(20)
    if a.role == 'receive':
        sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        sock.bind(('0.0.0.0', a.port))
        sock.listen(1)
        listener = sock
        sock, _ = listener.accept()
    else:
        sock.connect((a.address, a.port))
    sock.setblocking(False)
    started = time.monotonic()
    count, last = 0, 0
    payload = bytes(65536)
    def publish(state):
        value = status(sock, count, started, state)
        temp = a.status.with_suffix('.tmp')
        temp.write_text(json.dumps(value)+'\n')
        temp.replace(a.status)
    try:
        while time.monotonic()-started < a.seconds:
            now = time.monotonic()
            if now-last >= .2:
                publish('running')
                last = now
            readable, writable, _ = select.select([sock] if a.role == 'receive' else [],
                                                  [sock] if a.role == 'send' else [], [], .1)
            try:
                if writable:
                    count += sock.send(payload)
                if readable:
                    data = sock.recv(65536)
                    if not data:
                        break
                    count += len(data)
            except BlockingIOError:
                pass
        publish('done')
    finally:
        sock.close()


if __name__ == '__main__':
    main()
