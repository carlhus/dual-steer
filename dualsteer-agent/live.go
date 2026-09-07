package agent

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func CurrentNetNSInode() (uint64, error) {
	info, err := os.Stat("/proc/self/ns/net")
	if err != nil {
		return 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errors.New("network namespace stat unavailable")
	}
	return stat.Ino, nil
}
func (p Policy) ConnectionKey() (ConnKey, error) {
	if p.Connection == nil || p.Connection.Token == nil {
		return ConnKey{}, errors.New("live operations require dualSteer.connection.token from MPTCP PM events")
	}
	inode, err := CurrentNetNSInode()
	if err != nil {
		return ConnKey{}, err
	}
	if p.Connection.NetNSInode != 0 && p.Connection.NetNSInode != inode {
		return ConnKey{}, fmt.Errorf("connection netnsInode %d differs from current namespace %d; execute agent in the socket network namespace", p.Connection.NetNSInode, inode)
	}
	return ConnKey{NetNSInode: inode, Token: *p.Connection.Token}, nil
}
func DesiredPaths(p Policy, key ConnKey) (map[PathKey]MapPath, error) {
	if len(p.Legs.A.Endpoints) == 0 || len(p.Legs.B.Endpoints) == 0 {
		return nil, errors.New("live apply requires explicit endpoints for each leg; interface indices are not endpoint IDs")
	}
	result := make(map[PathKey]MapPath)
	for access, leg := range []Leg{p.Legs.A, p.Legs.B} {
		for _, ep := range leg.Endpoints {
			if ep.LocalID == nil || ep.RemoteID == nil {
				return nil, errors.New("endpoint localId and remoteId required")
			}
			result[PathKey{key, *ep.LocalID, *ep.RemoteID}] = MapPath{p.Generation, uint32(access)}
		}
	}
	return result, nil
}

// ApplyLive stages generation-bound paths then commits the policy. Callers must
// serialize all writers for these maps; the CLI takes a process-shared flock.
// Replacing path entries can temporarily force the scheduler's default fallback.
func ApplyLive(p Plan, key ConnKey, s MapStore) error {
	if err := (Config{p.Policy}).Validate(); err != nil {
		return err
	}
	if key.NetNSInode == 0 || key.Reserved != 0 {
		return errors.New("invalid connection key")
	}
	desired, err := DesiredPaths(p.Policy, key)
	if err != nil {
		return err
	}
	old, err := s.LookupPolicy(key)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("read existing policy: %w", err)
	}
	if err == nil && p.Policy.Generation <= old.Generation {
		return fmt.Errorf("stale generation %d: must exceed live generation %d", p.Policy.Generation, old.Generation)
	}
	before, err := s.Paths(key)
	if err != nil {
		return err
	}
	staged := make([]PathKey, 0, len(desired))
	rollback := func(cause error) error {
		var failures []error
		for _, k := range staged {
			var err error
			if v, ok := before[k]; ok {
				err = s.UpdatePath(k, v)
			} else {
				err = s.DeletePath(k)
			}
			if err != nil && !errors.Is(err, ErrNotFound) {
				failures = append(failures, fmt.Errorf("rollback path %v: %w", k, err))
			}
		}
		return errors.Join(append([]error{cause}, failures...)...)
	}
	for k, v := range desired {
		if err := s.UpdatePath(k, v); err != nil {
			return rollback(fmt.Errorf("stage path: %w", err))
		}
		staged = append(staged, k)
	}
	if err := s.UpdatePolicy(key, p.MapPolicy()); err != nil {
		return rollback(fmt.Errorf("commit policy: %w", err))
	}
	var cleanup []error
	for k := range before {
		if _, ok := desired[k]; !ok {
			if err := s.DeletePath(k); err != nil && !errors.Is(err, ErrNotFound) {
				cleanup = append(cleanup, err)
			}
		}
	}
	if err := errors.Join(cleanup...); err != nil {
		return fmt.Errorf("policy committed but stale path cleanup failed (retry delete or advance generation): %w", err)
	}
	return nil
}

// DeleteLive removes policy first so the scheduler falls back before paths vanish.
// It is idempotent and also removes orphaned paths after a partial operation.
func DeleteLive(key ConnKey, s MapStore) error {
	if err := s.DeletePolicy(key); err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("delete policy: %w", err)
	}
	paths, err := s.Paths(key)
	if err != nil {
		return err
	}
	var failures []error
	for k := range paths {
		if err := s.DeletePath(k); err != nil && !errors.Is(err, ErrNotFound) {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
