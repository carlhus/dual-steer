//go:build linux

package agent

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	EventEstablished    = 2
	EventClosed         = 3
	EventSubEstablished = 10
	EventSubClosed      = 11
)

// PMEvent follows include/uapi/linux/mptcp_pm.h. IDs come from the kernel,
// including the initial subflow IDs, and are never derived from ifindex.
type PMEvent struct {
	Type                             uint8
	Token                            uint32
	LocalID, RemoteID                uint8
	LocalAddress, DestinationAddress netip.Addr
	LocalPort, DestinationPort       uint16
	IfIndex                          int32
}

func netlinkAttrs(data []byte) (map[uint16][]byte, error) {
	result := make(map[uint16][]byte)
	for len(data) > 0 {
		if len(data) < 4 {
			return nil, errors.New("truncated netlink attribute")
		}
		n := int(binary.NativeEndian.Uint16(data))
		if n < 4 || n > len(data) || (n+3)&^3 > len(data) {
			return nil, errors.New("invalid netlink attribute length")
		}
		typ := binary.NativeEndian.Uint16(data[2:]) & 0x3fff
		if _, found := result[typ]; found {
			return nil, fmt.Errorf("duplicate netlink attribute %d", typ)
		}
		result[typ] = data[4:n]
		data = data[(n+3)&^3:]
	}
	return result, nil
}

// DecodePMEvent decodes one generic-netlink payload. Addresses and ports have
// network byte order; family, token and interface index have native byte order.
func DecodePMEvent(data []byte) (PMEvent, error) {
	var e PMEvent
	if len(data) < 4 {
		return e, errors.New("truncated generic netlink header")
	}
	e.Type = data[0]
	if e.Type != EventEstablished && e.Type != EventClosed && e.Type != EventSubEstablished && e.Type != EventSubClosed {
		return e, nil
	}
	attrs, err := netlinkAttrs(data[4:])
	if err != nil {
		return e, err
	}
	require := func(id uint16, n int) ([]byte, error) {
		v := attrs[id]
		if len(v) != n {
			return nil, fmt.Errorf("attribute %d requires %d bytes, got %d", id, n, len(v))
		}
		return v, nil
	}
	v, err := require(1, 4)
	if err != nil {
		return e, err
	}
	e.Token = binary.NativeEndian.Uint32(v)
	if e.Type == EventClosed {
		return e, nil
	}
	v, err = require(2, 2)
	if err != nil {
		return e, err
	}
	family := binary.NativeEndian.Uint16(v)
	var local, dest uint16
	var size int
	switch family {
	case unix.AF_INET:
		local, dest, size = 5, 7, 4
	case unix.AF_INET6:
		local, dest, size = 6, 8, 16
	default:
		return e, fmt.Errorf("unsupported PM address family %d", family)
	}
	v, err = require(local, size)
	if err != nil {
		return e, err
	}
	e.LocalAddress, _ = netip.AddrFromSlice(v)
	e.LocalAddress = e.LocalAddress.Unmap()
	v, err = require(dest, size)
	if err != nil {
		return e, err
	}
	e.DestinationAddress, _ = netip.AddrFromSlice(v)
	e.DestinationAddress = e.DestinationAddress.Unmap()
	v, err = require(3, 1)
	if err != nil {
		return e, err
	}
	e.LocalID = v[0]
	v, err = require(4, 1)
	if err != nil {
		return e, err
	}
	e.RemoteID = v[0]
	v, err = require(9, 2)
	if err != nil {
		return e, err
	}
	e.LocalPort = binary.BigEndian.Uint16(v)
	v, err = require(10, 2)
	if err != nil {
		return e, err
	}
	e.DestinationPort = binary.BigEndian.Uint16(v)
	if v, ok := attrs[15]; ok {
		if len(v) != 4 {
			return e, errors.New("bad PM interface index")
		}
		e.IfIndex = int32(binary.NativeEndian.Uint32(v))
	}
	return e, nil
}

func encodeAttr(typ uint16, data []byte) []byte {
	b := make([]byte, (len(data)+7)&^3)
	binary.NativeEndian.PutUint16(b, uint16(len(data)+4))
	binary.NativeEndian.PutUint16(b[2:], typ)
	copy(b[4:], data)
	return b
}

type PMSubscription struct {
	fd     int
	family uint16
}

// SubscribePM resolves the actual family/group IDs and joins the event group
// synchronously. Successful return is the startup observation boundary.
func SubscribePM() (*PMSubscription, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_GENERIC)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*PMSubscription, error) { unix.Close(fd); return nil, err }
	if err = unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return fail(err)
	}
	if err = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, 4<<20); err != nil {
		return fail(err)
	}
	if err = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Sec: 3}); err != nil {
		return fail(err)
	}
	payload := append([]byte{3, 2, 0, 0}, encodeAttr(2, []byte("mptcp_pm\x00"))...)
	req := make([]byte, 16+len(payload))
	binary.NativeEndian.PutUint32(req, uint32(len(req)))
	binary.NativeEndian.PutUint16(req[4:], unix.GENL_ID_CTRL)
	binary.NativeEndian.PutUint16(req[6:], unix.NLM_F_REQUEST)
	binary.NativeEndian.PutUint32(req[8:], 1)
	copy(req[16:], payload)
	if err = unix.Sendto(fd, req, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return fail(err)
	}
	buf := make([]byte, 64<<10)
	n, _, err := unix.Recvfrom(fd, buf, 0)
	if err != nil {
		return fail(err)
	}
	msgs, err := syscall.ParseNetlinkMessage(buf[:n])
	if err != nil {
		return fail(err)
	}
	var family uint16
	var group uint32
	for _, m := range msgs {
		if m.Header.Type == unix.NLMSG_ERROR {
			return fail(netlinkError(m.Data))
		}
		if m.Header.Type != unix.GENL_ID_CTRL || len(m.Data) < 4 {
			continue
		}
		attrs, err := netlinkAttrs(m.Data[4:])
		if err != nil {
			return fail(err)
		}
		if len(attrs[1]) == 2 {
			family = binary.NativeEndian.Uint16(attrs[1])
		}
		groups, err := netlinkAttrs(attrs[7])
		if err != nil {
			return fail(err)
		}
		for _, g := range groups {
			a, err := netlinkAttrs(g)
			if err != nil {
				return fail(err)
			}
			if strings.TrimRight(string(a[1]), "\x00") == "mptcp_pm_events" && len(a[2]) == 4 {
				group = binary.NativeEndian.Uint32(a[2])
			}
		}
	}
	if family == 0 || group == 0 {
		return fail(errors.New("mptcp_pm event family/group unavailable"))
	}
	if err = unix.SetsockoptInt(fd, unix.SOL_NETLINK, unix.NETLINK_ADD_MEMBERSHIP, int(group)); err != nil {
		return fail(err)
	}
	if err = unix.SetNonblock(fd, true); err != nil {
		return fail(err)
	}
	return &PMSubscription{fd: fd, family: family}, nil
}
func netlinkError(data []byte) error {
	if len(data) < 4 {
		return errors.New("truncated netlink error")
	}
	code := int32(binary.NativeEndian.Uint32(data))
	if code == 0 {
		return errors.New("unexpected netlink acknowledgement")
	}
	return syscall.Errno(-code)
}
func (s *PMSubscription) Close() error { return unix.Close(s.fd) }

// Run treats ENOBUFS, truncation and malformed observed events as fatal. The
// caller must fail closed because this API cannot replay missed connections.
func (s *PMSubscription) Run(ctx context.Context, handle func(PMEvent) error) error {
	buf := make([]byte, 1<<20)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		fds := []unix.PollFd{{Fd: int32(s.fd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, 250)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if n == 0 {
			continue
		}
		n, _, flags, from, err := unix.Recvmsg(s.fd, buf, nil, 0)
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("PM event stream lost: %w", err)
		}
		if flags&unix.MSG_TRUNC != 0 {
			return errors.New("PM event datagram truncated")
		}
		peer, ok := from.(*unix.SockaddrNetlink)
		if !ok || peer.Pid != 0 {
			return errors.New("PM event not from kernel")
		}
		msgs, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil {
			return err
		}
		for _, m := range msgs {
			if m.Header.Type == unix.NLMSG_OVERRUN {
				return errors.New("PM event stream overrun")
			}
			if m.Header.Type == unix.NLMSG_ERROR {
				return netlinkError(m.Data)
			}
			if m.Header.Type != s.family {
				continue
			}
			event, err := DecodePMEvent(m.Data)
			if err != nil {
				return err
			}
			if err = handle(event); err != nil {
				return err
			}
		}
	}
}
