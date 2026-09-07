//go:build linux

package agent

import (
	"context"
	"encoding/binary"
	"errors"
	"net/netip"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func native16(v uint16) []byte  { b := make([]byte, 2); binary.NativeEndian.PutUint16(b, v); return b }
func native32(v uint32) []byte  { b := make([]byte, 4); binary.NativeEndian.PutUint32(b, v); return b }
func network16(v uint16) []byte { b := make([]byte, 2); binary.BigEndian.PutUint16(b, v); return b }
func eventPayload(e PMEvent) []byte {
	b := []byte{e.Type, 1, 0, 0}
	b = append(b, encodeAttr(1, native32(e.Token))...)
	if e.Type == EventClosed {
		return b
	}
	family := uint16(unix.AF_INET)
	local, dest := uint16(5), uint16(7)
	if e.LocalAddress.Is6() {
		family = unix.AF_INET6
		local, dest = 6, 8
	}
	for _, a := range []struct {
		typ  uint16
		data []byte
	}{{2, native16(family)}, {3, []byte{e.LocalID}}, {4, []byte{e.RemoteID}}, {local, e.LocalAddress.AsSlice()}, {dest, e.DestinationAddress.AsSlice()}, {9, network16(e.LocalPort)}, {10, network16(e.DestinationPort)}, {15, native32(uint32(e.IfIndex))}} {
		b = append(b, encodeAttr(a.typ, a.data)...)
	}
	return b
}
func TestPMDecoderNativeAndNetworkByteOrder(t *testing.T) {
	a, b := testEvents()
	a.Token = 0x12345678
	b.Token = 0xffffffff
	v6 := b
	v6.LocalAddress = netip.MustParseAddr("2001:db8::1")
	v6.DestinationAddress = netip.MustParseAddr("2001:db8::2")
	for _, want := range []PMEvent{a, b, v6, {Type: EventClosed, Token: 0}} {
		got, err := DecodePMEvent(eventPayload(want))
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("got %+v want %+v", got, want)
		}
	}
}
func TestPMDecoderRejectsMalformedRelevantEvents(t *testing.T) {
	a, _ := testEvents()
	valid := eventPayload(a)
	missingID := append([]byte{EventEstablished, 1, 0, 0}, encodeAttr(1, native32(123))...)
	duplicate := append(append([]byte{}, valid...), encodeAttr(1, native32(123))...)
	wrongToken := append([]byte{EventClosed, 1, 0, 0}, encodeAttr(1, []byte{1})...)
	for _, data := range [][]byte{{}, valid[:len(valid)-1], missingID, duplicate, wrongToken} {
		if _, err := DecodePMEvent(data); err == nil {
			t.Fatalf("accepted malformed payload %x", data)
		}
	}
	if _, err := DecodePMEvent([]byte{15, 1, 0, 0}); err != nil {
		t.Fatal("irrelevant listener event should not need token", err)
	}
}
func TestPMSubscriptionKernel(t *testing.T) {
	if os.Getenv("DUALSTEER_TEST_PM") != "1" {
		t.Skip("set DUALSTEER_TEST_PM=1 in an MPTCP-enabled namespace with CAP_NET_ADMIN")
	}
	sub, err := SubscribePM()
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	err = sub.Run(ctx, func(PMEvent) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}
