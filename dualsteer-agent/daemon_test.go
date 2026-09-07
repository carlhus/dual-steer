package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	cp "example.com/dual-steer/controlplane"
)

func testAssignment() cp.Assignment {
	return cp.Assignment{ContextSpec: cp.ContextSpec{ID: "lab", DNN: "internet", Legs: cp.Legs{A: cp.Leg{IfName: "a0", LocalAddress: "192.0.2.1"}, B: cp.Leg{IfName: "b0", LocalAddress: "198.51.100.1"}}, Flow: cp.Flow{DestinationAddress: "192.0.2.2", DestinationPort: 5201}}, Policy: cp.Policy{Enabled: true, Mode: "load-balance", WeightA: 70, WeightB: 30}, Generation: 1}
}
func testValidator(leg cp.Leg) (int, error) {
	switch leg.IfName {
	case "a0":
		return 7, nil
	case "b0":
		return 99, nil
	default:
		return 0, errors.New("unknown interface")
	}
}
func testEvents() (PMEvent, PMEvent) {
	a := PMEvent{Type: EventEstablished, Token: 123, LocalID: 0, RemoteID: 0, LocalAddress: netip.MustParseAddr("192.0.2.1"), DestinationAddress: netip.MustParseAddr("192.0.2.2"), LocalPort: 40001, DestinationPort: 5201, IfIndex: 7}
	b := a
	b.Type = EventSubEstablished
	b.LocalAddress = netip.MustParseAddr("198.51.100.1")
	b.LocalID = 31
	b.RemoteID = 9
	b.IfIndex = 99
	b.LocalPort = 40002
	return a, b
}
func requireObserve(t *testing.T, d *Daemon, events ...PMEvent) {
	t.Helper()
	for _, e := range events {
		if err := d.Observe(e); err != nil {
			t.Fatal(err)
		}
	}
}
func requirePut(t *testing.T, d *Daemon, a cp.Assignment) {
	t.Helper()
	if err := d.Put(a); err != nil {
		t.Fatal(err)
	}
}

func TestDaemonAutomaticBindingOrdersAndHotUpdate(t *testing.T) {
	for _, order := range []string{"policy-first", "events-first", "subflow-first"} {
		t.Run(order, func(t *testing.T) {
			s := newFake()
			d := NewDaemon(s, 42, testValidator)
			a, b := testEvents()
			assignment := testAssignment()
			switch order {
			case "policy-first":
				requirePut(t, d, assignment)
				requireObserve(t, d, a)
				if len(s.policy) != 0 {
					t.Fatal("committed incomplete topology")
				}
				requireObserve(t, d, b)
			case "events-first":
				requireObserve(t, d, a, b)
				requirePut(t, d, assignment)
			case "subflow-first":
				requireObserve(t, d, b)
				requirePut(t, d, assignment)
				if len(s.policy) != 0 {
					t.Fatal("bound without initial connection selector")
				}
				requireObserve(t, d, a)
			}
			key := ConnKey{NetNSInode: 42, Token: 123}
			before := s.policy[key]
			if before.WeightA != 70 || before.WeightB != 30 || before.Generation != 1 {
				t.Fatal(before)
			}
			if s.paths[PathKey{key, 31, 9}].Access != 1 {
				t.Fatal("kernel endpoint IDs not used")
			}
			if _, ok := s.paths[PathKey{key, 99, 9}]; ok {
				t.Fatal("ifindex became endpoint ID")
			}
			status := d.Status()
			if !status.Ready || !status.Contexts[0].Bindings[0].Ready {
				t.Fatal(status)
			}
			assignment.Generation = 2
			assignment.Policy.WeightA = 20
			assignment.Policy.WeightB = 80
			requirePut(t, d, assignment)
			if len(s.policy) != 1 || s.policy[key].WeightA != 20 || s.policy[key].WeightB != 80 || s.policy[key].Generation != 2 {
				t.Fatal(s.policy)
			}
			commits := len(s.events)
			requirePut(t, d, assignment)
			if len(s.events) != commits {
				t.Fatal("idempotent retry rewrote maps")
			}
			requireObserve(t, d, PMEvent{Type: EventClosed, Token: 123})
			if len(s.policy) != 0 || len(s.paths) != 0 || len(d.Status().Contexts[0].Bindings) != 0 {
				t.Fatal("close failed to clean binding")
			}
		})
	}
}

func TestDaemonRetryPreservesObservations(t *testing.T) {
	s := newFake()
	s.failCommit = true
	d := NewDaemon(s, 42, testValidator)
	requirePut(t, d, testAssignment())
	a, b := testEvents()
	requireObserve(t, d, a, b)
	if d.Status().Ready {
		t.Fatal("map failure reported ready")
	}
	if len(d.connections[123].flows) != 2 {
		t.Fatal("lost event on map failure")
	}
	s.failCommit = false
	if err := d.Retry(); err != nil {
		t.Fatal(err)
	}
	if !d.Status().Ready || !d.Status().Contexts[0].Bindings[0].Ready {
		t.Fatal(d.Status())
	}
	b.Type = EventSubClosed
	requireObserve(t, d, b)
	if len(s.policy) != 0 || len(s.paths) != 0 {
		t.Fatal("lost leg did not select fallback")
	}
	b.Type = EventSubEstablished
	requireObserve(t, d, b)
	if len(s.policy) != 1 {
		t.Fatal("restored subflow did not restore same generation")
	}
	if err := d.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if len(s.policy)+len(s.paths) != 0 {
		t.Fatal("shutdown cleanup failed")
	}
}

type deleteFailureStore struct {
	*fakeStore
	fail bool
}

func (s *deleteFailureStore) DeletePolicy(k ConnKey) error {
	if s.fail {
		return errors.New("delete unavailable")
	}
	return s.fakeStore.DeletePolicy(k)
}
func TestDaemonCloseRetriesCleanup(t *testing.T) {
	s := &deleteFailureStore{fakeStore: newFake()}
	d := NewDaemon(s, 42, testValidator)
	requirePut(t, d, testAssignment())
	a, b := testEvents()
	requireObserve(t, d, a, b)
	s.fail = true
	requireObserve(t, d, PMEvent{Type: EventClosed, Token: 123})
	status := d.Status()
	if status.Ready || len(status.Contexts[0].Bindings) != 1 {
		t.Fatal(status)
	}
	s.fail = false
	if err := d.Retry(); err != nil {
		t.Fatal(err)
	}
	if !d.Status().Ready || len(d.Status().Contexts[0].Bindings) != 0 || len(s.policy)+len(s.paths) != 0 {
		t.Fatal(d.Status())
	}
}
func TestDaemonRejectsWrongFlowAndInterface(t *testing.T) {
	s := newFake()
	d := NewDaemon(s, 42, testValidator)
	requirePut(t, d, testAssignment())
	a, b := testEvents()
	a.DestinationPort = 443
	requireObserve(t, d, a, b)
	if len(s.policy) != 0 || len(d.Status().Contexts[0].Bindings) != 0 {
		t.Fatal("matched wrong destination")
	}
	requireObserve(t, d, PMEvent{Type: EventClosed, Token: 123})
	a, b = testEvents()
	b.IfIndex = 7
	requireObserve(t, d, a, b)
	if len(s.policy) != 0 || d.Status().Ready {
		t.Fatal("accepted wrong interface on leg B")
	}
}
func TestDaemonGenerationAndContextOwnership(t *testing.T) {
	d := NewDaemon(newFake(), 42, testValidator)
	a := testAssignment()
	requirePut(t, d, a)
	conflict := a
	conflict.ID = "other"
	if err := d.Put(conflict); !errors.Is(err, ErrAssignmentRejected) {
		t.Fatal(err)
	}
	conflict = a
	conflict.Policy.WeightA = 40
	conflict.Policy.WeightB = 60
	if err := d.Put(conflict); !errors.Is(err, ErrAssignmentRejected) {
		t.Fatal(err)
	}
	a.Generation = 2
	requirePut(t, d, a)
	a.Generation = 1
	if err := d.Put(a); !errors.Is(err, ErrAssignmentRejected) {
		t.Fatal(err)
	}
	if err := d.Delete("lab"); err != nil {
		t.Fatal(err)
	}
	if len(d.Status().Contexts) != 0 {
		t.Fatal("context not deleted")
	}
}
func TestDaemonHTTPAdmissionAndStatus(t *testing.T) {
	d := NewDaemon(newFake(), 42, testValidator)
	handler := d.Handler()
	a := testAssignment()
	body, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path string, body []byte) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(method, cp.Prefix+path, bytes.NewReader(body)))
		return w
	}
	if got := request(http.MethodPut, "/contexts/wrong", body); got.Code != 400 {
		t.Fatal(got.Code)
	}
	if got := request(http.MethodPut, "/contexts/lab", append(body, []byte(" {}")...)); got.Code != 400 {
		t.Fatal(got.Code)
	}
	if got := request(http.MethodPut, "/contexts/lab", body); got.Code != 202 {
		t.Fatal(got.Code, got.Body.String())
	}
	got := request(http.MethodGet, "/contexts/lab", nil)
	var status ContextStatus
	if err := json.Unmarshal(got.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if got.Code != 200 || status.ID != "lab" || status.Generation != 1 || status.Bindings == nil || len(status.Bindings) != 0 {
		t.Fatal(got.Body.String())
	}
	if got := request(http.MethodDelete, "/contexts/lab", nil); got.Code != 200 {
		t.Fatal(got.Code)
	}
	if got := request(http.MethodGet, "/contexts/lab", nil); got.Code != 404 {
		t.Fatal(got.Code)
	}
}
