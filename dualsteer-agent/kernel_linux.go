//go:build linux

package agent

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/cilium/ebpf"
	"golang.org/x/sys/unix"
)

type KernelMaps struct{ policy, path *ebpf.Map }

func openMap(selector, name string, keySize, valueSize uint32) (*ebpf.Map, error) {
	var m *ebpf.Map
	var err error
	if raw, ok := strings.CutPrefix(selector, "id:"); ok {
		id, parseErr := strconv.ParseUint(raw, 10, 32)
		if parseErr != nil || id == 0 {
			return nil, fmt.Errorf("invalid map ID %q", raw)
		}
		m, err = ebpf.NewMapFromID(ebpf.MapID(id))
	} else {
		if !strings.HasPrefix(selector, "/") {
			return nil, errors.New("map selector must be an absolute pinned path or id:NUMBER")
		}
		m, err = ebpf.LoadPinnedMap(selector, nil)
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", name, err)
	}
	info, err := m.Info()
	if err != nil {
		m.Close()
		return nil, err
	}
	if info.Name != name || info.Type != ebpf.Hash || info.KeySize != keySize || info.ValueSize != valueSize {
		m.Close()
		return nil, fmt.Errorf("map ABI mismatch for %s: got name=%q type=%s key=%d value=%d; expected Hash key=%d value=%d", name, info.Name, info.Type, info.KeySize, info.ValueSize, keySize, valueSize)
	}
	return m, nil
}
func OpenKernelMaps(policySelector, pathSelector string) (*KernelMaps, error) {
	policy, err := openMap(policySelector, "ds_policy_map", 16, 24)
	if err != nil {
		return nil, err
	}
	path, err := openMap(pathSelector, "ds_path_map", 24, 8)
	if err != nil {
		policy.Close()
		return nil, err
	}
	return &KernelMaps{policy, path}, nil
}
func (m *KernelMaps) Close() error { return errors.Join(m.policy.Close(), m.path.Close()) }
func mapError(err error) error {
	if errors.Is(err, ebpf.ErrKeyNotExist) {
		return ErrNotFound
	}
	return err
}
func (m *KernelMaps) LookupPolicy(k ConnKey) (MapPolicy, error) {
	var p MapPolicy
	err := m.policy.Lookup(k, &p)
	return p, mapError(err)
}
func (m *KernelMaps) UpdatePolicy(k ConnKey, p MapPolicy) error {
	return mapError(m.policy.Update(k, p, ebpf.UpdateAny))
}
func (m *KernelMaps) DeletePolicy(k ConnKey) error { return mapError(m.policy.Delete(k)) }
func (m *KernelMaps) UpdatePath(k PathKey, p MapPath) error {
	return mapError(m.path.Update(k, p, ebpf.UpdateAny))
}
func (m *KernelMaps) DeletePath(k PathKey) error { return mapError(m.path.Delete(k)) }
func (m *KernelMaps) Paths(conn ConnKey) (map[PathKey]MapPath, error) {
	result := make(map[PathKey]MapPath)
	iter := m.path.Iterate()
	var k PathKey
	var p MapPath
	for iter.Next(&k, &p) {
		if k.Conn == conn {
			result[k] = p
		}
	}
	return result, iter.Err()
}

// LockWriters serializes this agent's live operations across network namespaces.
// Other map writers must use the same lock; BPF maps have no compare-and-swap API.
func LockWriters() (func() error, error) {
	fd, err := unix.Open("/run/lock/dualsteer-agent.lock", unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, fmt.Errorf("open writer lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), "dualsteer-agent.lock")
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("another agent operation holds the writer lock: %w", err)
	}
	return file.Close, nil
}
