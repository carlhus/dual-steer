//go:build linux

package agent

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/rlimit"
)

// Opt in explicitly; these tests create private temporary BPF maps and remove
// them afterwards. They verify real syscalls, not scheduler traffic behavior.
func TestKernelMaps(t *testing.T) {
	if os.Getenv("DUALSTEER_KERNEL_TEST") != "1" {
		t.Skip("set DUALSTEER_KERNEL_TEST=1 and run as root/CAP_BPF")
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatal(err)
	}
	create := func(name string, key, value uint32) *ebpf.Map {
		t.Helper()
		m, err := ebpf.NewMap(&ebpf.MapSpec{Name: name, Type: ebpf.Hash, KeySize: key, ValueSize: value, MaxEntries: 16})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { m.Close() })
		return m
	}
	policy := create("ds_policy_map", 16, 24)
	path := create("ds_path_map", 24, 8)
	selector := func(m *ebpf.Map) string {
		t.Helper()
		info, err := m.Info()
		if err != nil {
			t.Fatal(err)
		}
		id, ok := info.ID()
		if !ok {
			t.Fatal("no map ID")
		}
		return fmt.Sprintf("id:%d", id)
	}
	s, err := OpenKernelMaps(selector(policy), selector(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	p := livePlan(t)
	key, err := p.Policy.ConnectionKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplyLive(p, key, s); err != nil {
		t.Fatal(err)
	}
	var raw [24]byte
	if err := policy.Lookup(key, &raw); err != nil {
		t.Fatal(err)
	}
	for i, want := range []uint32{7, 1, 1, 60, 40, 1000} {
		if got := binary.NativeEndian.Uint32(raw[i*4:]); got != want {
			t.Fatalf("kernel ABI word %d=%d want %d", i, got, want)
		}
	}
	paths, err := s.Paths(key)
	if err != nil || len(paths) != 2 {
		t.Fatalf("paths=%v err=%v", paths, err)
	}
	if err := ApplyLive(p, key, s); err == nil {
		t.Fatal("accepted stale generation")
	}
	p.Policy.Generation = 8
	p.Policy.Mode = "lowest-rtt"
	if err := ApplyLive(p, key, s); err != nil {
		t.Fatal(err)
	}
	got, err := s.LookupPolicy(key)
	if err != nil || got.Mode != 2 || got.Generation != 8 {
		t.Fatalf("readback %+v %v", got, err)
	}
	wrong := create("wrong_name", 16, 24)
	if m, err := OpenKernelMaps(selector(wrong), selector(path)); err == nil {
		m.Close()
		t.Fatal("accepted wrong name")
	}
	wrongSize := create("ds_policy_map", 16, 20)
	if m, err := OpenKernelMaps(selector(wrongSize), selector(path)); err == nil {
		m.Close()
		t.Fatal("accepted wrong value size")
	}
	if err := DeleteLive(key, s); err != nil {
		t.Fatal(err)
	}
	// Exercise CLI pinned-path opening, real writes/reads/deletes, and namespace guard.
	pinDir, err := os.MkdirTemp("/sys/fs/bpf", "dualsteer-agent-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(pinDir) })
	policyPin, pathPin := filepath.Join(pinDir, "policy"), filepath.Join(pinDir, "paths")
	if err := policy.Pin(policyPin); err != nil {
		t.Fatal(err)
	}
	if err := path.Pin(pathPin); err != nil {
		t.Fatal(err)
	}
	config := strings.Replace(validYAML, "  legs:", "  connection: {token: 123}\n  legs:", 1)
	config = strings.Replace(config, "weight: 60}", "weight: 60, endpoints: [{localId: 0, remoteId: 0}]}", 1)
	config = strings.Replace(config, "weight: 40}", "weight: 40, endpoints: [{localId: 1, remoteId: 1}]}", 1)
	configPath := filepath.Join(t.TempDir(), "live.yaml")
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	run := func(command string) string {
		t.Helper()
		output.Reset()
		if err := Run([]string{command, "--config", configPath, "--policy-map", policyPin, "--path-map", pathPin}, &output, fakeResolver); err != nil {
			t.Fatal(err)
		}
		return output.String()
	}
	if got := run("apply"); !strings.Contains(got, "applied kernel policy") {
		t.Fatal(got)
	}
	if got := run("status"); !strings.Contains(got, "generation=7") || !strings.Contains(got, "localId=1 remoteId=1 generation=7 access=1 matchesPolicy=true") {
		t.Fatal(got)
	}
	run("delete")
	if got := run("status"); !strings.Contains(got, "kernel policy absent") || !strings.Contains(got, "orphan paths=0") {
		t.Fatal(got)
	}
	t.Log("real kernel map IDs, pins, ABI bytes, apply/update/status/delete, stale generation, and ABI rejection passed")
}
