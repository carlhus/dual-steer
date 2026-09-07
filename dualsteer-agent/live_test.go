package agent

import (
	"errors"
	"maps"
	"strings"
	"testing"
)

type fakeStore struct {
	policy     map[ConnKey]MapPolicy
	paths      map[PathKey]MapPath
	events     []string
	failCommit bool
	failPathAt int
	pathCalls  int
}

func newFake() *fakeStore {
	return &fakeStore{policy: make(map[ConnKey]MapPolicy), paths: make(map[PathKey]MapPath)}
}
func (s *fakeStore) LookupPolicy(k ConnKey) (MapPolicy, error) {
	p, ok := s.policy[k]
	if !ok {
		return p, ErrNotFound
	}
	return p, nil
}
func (s *fakeStore) UpdatePolicy(k ConnKey, p MapPolicy) error {
	s.events = append(s.events, "commit")
	if s.failCommit {
		return errors.New("commit failure")
	}
	s.policy[k] = p
	return nil
}
func (s *fakeStore) DeletePolicy(k ConnKey) error {
	s.events = append(s.events, "delete-policy")
	delete(s.policy, k)
	return nil
}
func (s *fakeStore) Paths(k ConnKey) (map[PathKey]MapPath, error) {
	p := make(map[PathKey]MapPath)
	for key, v := range s.paths {
		if key.Conn == k {
			p[key] = v
		}
	}
	return p, nil
}
func (s *fakeStore) UpdatePath(k PathKey, p MapPath) error {
	s.pathCalls++
	s.events = append(s.events, "path")
	if s.pathCalls == s.failPathAt {
		return errors.New("stage failure")
	}
	s.paths[k] = p
	return nil
}
func (s *fakeStore) DeletePath(k PathKey) error {
	s.events = append(s.events, "delete-path")
	delete(s.paths, k)
	return nil
}
func livePlan(t *testing.T) Plan {
	t.Helper()
	cfg, err := Decode(strings.NewReader(validYAML))
	if err != nil {
		t.Fatal(err)
	}
	cfg.DualSteer.Connection = &Connection{Token: new(uint32(123))}
	cfg.DualSteer.Legs.A.Endpoints = []Endpoint{{new(uint32(0)), new(uint32(0))}}
	cfg.DualSteer.Legs.B.Endpoints = []Endpoint{{new(uint32(1)), new(uint32(1))}}
	plan, err := Prepare(cfg, fakeResolver)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}
func TestLiveTransaction(t *testing.T) {
	p := livePlan(t)
	key := ConnKey{NetNSInode: 42, Token: 123}
	s := newFake()
	orphan := PathKey{key, 9, 9}
	s.paths[orphan] = MapPath{2, 0}
	other := PathKey{ConnKey{NetNSInode: 43, Token: 123}, 0, 0}
	s.paths[other] = MapPath{7, 0}
	if err := ApplyLive(p, key, s); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(s.events, ","); got != "path,path,commit,delete-path" {
		t.Fatal(got)
	}
	if _, ok := s.paths[orphan]; ok {
		t.Fatal("orphan not removed")
	}
	if _, ok := s.paths[other]; !ok {
		t.Fatal("other connection changed")
	}
	if err := ApplyLive(p, key, s); err == nil || !strings.Contains(err.Error(), "stale generation") {
		t.Fatal(err)
	}
	s.events = nil
	if err := DeleteLive(key, s); err != nil {
		t.Fatal(err)
	}
	if s.events[0] != "delete-policy" || len(s.paths) != 1 {
		t.Fatalf("bad deletion: %v %v", s.events, s.paths)
	}
	if err := DeleteLive(key, s); err != nil {
		t.Fatal(err)
	}
}
func TestRollback(t *testing.T) {
	for _, commit := range []bool{false, true} {
		t.Run(map[bool]string{false: "stage", true: "commit"}[commit], func(t *testing.T) {
			p := livePlan(t)
			key := ConnKey{NetNSInode: 42, Token: 123}
			s := newFake()
			old := p.MapPolicy()
			old.Generation = 6
			s.policy[key] = old
			s.paths[PathKey{key, 0, 0}] = MapPath{6, 0}
			before := maps.Clone(s.paths)
			if commit {
				s.failCommit = true
			} else {
				s.failPathAt = 2
			}
			if err := ApplyLive(p, key, s); err == nil {
				t.Fatal("expected failure")
			}
			if s.policy[key] != old || !maps.Equal(s.paths, before) {
				t.Fatalf("failed rollback: %+v", s)
			}
		})
	}
}
func TestConnectionAndEndpoints(t *testing.T) {
	p := livePlan(t).Policy
	key, err := p.ConnectionKey()
	if err != nil {
		t.Fatal(err)
	}
	if key.NetNSInode == 0 || key.Token != 123 {
		t.Fatal(key)
	}
	p.Connection.NetNSInode = key.NetNSInode + 1
	if _, err := p.ConnectionKey(); err == nil {
		t.Fatal("accepted foreign namespace")
	}
	p.Legs.B.Endpoints = p.Legs.A.Endpoints
	if err := (Config{p}).Validate(); err == nil {
		t.Fatal("accepted duplicate endpoint IDs")
	}
}
