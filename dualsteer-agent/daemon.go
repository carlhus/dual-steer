package agent

import (
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"slices"
	"sync"

	cp "example.com/dual-steer/controlplane"
)

// AddressValidator verifies an operator-provided local address belongs to the
// named interface. ifindex is diagnostic/validation data, never an endpoint ID.
type AddressValidator func(cp.Leg) (int, error)

func ValidateLegAddress(leg cp.Leg) (int, error) {
	iface, err := net.InterfaceByName(leg.IfName)
	if err != nil {
		return 0, err
	}
	want, err := cp.ParseAddress(leg.LocalAddress)
	if err != nil {
		return 0, err
	}
	addresses, err := iface.Addrs()
	if err != nil {
		return 0, err
	}
	for _, addr := range addresses {
		prefix, err := netip.ParsePrefix(addr.String())
		if err == nil && prefix.Addr().Unmap() == want {
			return iface.Index, nil
		}
	}
	return 0, fmt.Errorf("address %s is not assigned to interface %s", want, leg.IfName)
}

type observedFlow struct {
	LocalAddress, DestinationAddress netip.Addr
	LocalPort, DestinationPort       uint16
	LocalID, RemoteID                uint8
}

func eventFlow(e PMEvent) observedFlow {
	return observedFlow{e.LocalAddress, e.DestinationAddress, e.LocalPort, e.DestinationPort, e.LocalID, e.RemoteID}
}

type observedConnection struct {
	initial   *PMEvent
	flows     map[observedFlow]PMEvent
	owner     string
	closed    bool
	lastError string
}
type desiredContext struct {
	assignment cp.Assignment
	deleting   bool
}

type BoundPath struct {
	LocalID  uint32 `json:"localId"`
	RemoteID uint32 `json:"remoteId"`
	Access   uint32 `json:"access"`
}
type BindingStatus struct {
	Token      uint32      `json:"token"`
	NetNSInode uint64      `json:"netnsInode"`
	Generation uint32      `json:"generation"`
	Ready      bool        `json:"ready"`
	Paths      []BoundPath `json:"paths"`
	Error      string      `json:"error,omitempty"`
}
type ContextStatus struct {
	ID         string          `json:"id"`
	Generation uint32          `json:"generation"`
	Deleting   bool            `json:"deleting"`
	Bindings   []BindingStatus `json:"bindings"`
}
type DaemonStatus struct {
	Ready               bool            `json:"ready"`
	RestartBehavior     string          `json:"restartBehavior"`
	ObservedConnections int             `json:"observedConnections"`
	Contexts            []ContextStatus `json:"contexts"`
	Error               string          `json:"error,omitempty"`
}

// Daemon serializes desired state, observations and map transactions. The
// process additionally holds LockWriters for its complete serve lifetime.
type Daemon struct {
	mu          sync.Mutex
	store       MapStore
	netns       uint64
	validate    AddressValidator
	contexts    map[string]*desiredContext
	connections map[uint32]*observedConnection
	fatal       error
}

func NewDaemon(store MapStore, netns uint64, validate AddressValidator) *Daemon {
	return &Daemon{store: store, netns: netns, validate: validate, contexts: make(map[string]*desiredContext), connections: make(map[uint32]*observedConnection)}
}

var ErrAssignmentRejected = errors.New("assignment rejected")

func rejectAssignment(err error) error { return errors.Join(ErrAssignmentRejected, err) }

func (d *Daemon) Put(a cp.Assignment) error {
	if err := a.Validate(); err != nil {
		return rejectAssignment(err)
	}
	// Canonical spelling makes IPv4-mapped IPv6 equivalent in overlap checks.
	a.Legs.A.LocalAddress = mustAddress(a.Legs.A.LocalAddress).String()
	a.Legs.B.LocalAddress = mustAddress(a.Legs.B.LocalAddress).String()
	a.Flow.DestinationAddress = mustAddress(a.Flow.DestinationAddress).String()
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.fatal != nil {
		return d.fatal
	}
	for _, leg := range []cp.Leg{a.Legs.A, a.Legs.B} {
		if _, err := d.validate(leg); err != nil {
			return rejectAssignment(err)
		}
	}
	old := d.contexts[a.ID]
	if old != nil {
		if old.deleting {
			return rejectAssignment(errors.New("context deletion pending"))
		}
		if old.assignment.ContextSpec != a.ContextSpec {
			return rejectAssignment(errors.New("context flow, DNN and legs are immutable; delete it first"))
		}
		if a.Generation < old.assignment.Generation || a.Generation == old.assignment.Generation && a != old.assignment {
			return rejectAssignment(errors.New("stale or conflicting policy generation"))
		}
	} else {
		if len(d.contexts) >= 1024 {
			return rejectAssignment(errors.New("context capacity reached"))
		}
		for _, other := range d.contexts {
			if other.assignment.Flow == a.Flow {
				return rejectAssignment(errors.New("destination flow already assigned to another context"))
			}
		}
	}
	d.contexts[a.ID] = &desiredContext{assignment: a}
	return d.reconcileLocked()
}
func mustAddress(s string) netip.Addr { a, _ := cp.ParseAddress(s); return a }

func (d *Daemon) Delete(id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if c := d.contexts[id]; c != nil {
		c.deleting = true
	}
	return d.reconcileLocked()
}

// Observe stores before reconciling, so map failure never discards an event.
// Only unrecoverable observation failures are returned to the subscriber.
func (d *Daemon) Observe(e PMEvent) error {
	if e.Type != EventEstablished && e.Type != EventSubEstablished && e.Type != EventSubClosed && e.Type != EventClosed {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.fatal != nil {
		return d.fatal
	}
	c := d.connections[e.Token]
	if e.Type == EventClosed {
		if c != nil {
			c.closed = true
		}
		_ = d.reconcileLocked()
		return nil
	}
	if c == nil {
		if e.Type == EventSubClosed {
			return nil
		}
		if len(d.connections) >= 4096 {
			return errors.New("observed connection capacity exceeded; event replay unavailable")
		}
		c = &observedConnection{flows: make(map[observedFlow]PMEvent)}
		d.connections[e.Token] = c
	}
	if c.closed {
		return errors.New("token reused before previous connection cleanup completed")
	}
	if e.Type == EventEstablished {
		if c.initial != nil && eventFlow(*c.initial) != eventFlow(e) {
			return errors.New("token reused without observed close")
		}
		c.initial = &e
	}
	if e.Type == EventSubClosed {
		delete(c.flows, eventFlow(e))
	} else {
		if len(c.flows) >= 256 {
			if _, ok := c.flows[eventFlow(e)]; !ok {
				return errors.New("subflow observation capacity exceeded")
			}
		}
		c.flows[eventFlow(e)] = e
	}
	_ = d.reconcileLocked()
	return nil
}
func (d *Daemon) Retry() error { d.mu.Lock(); defer d.mu.Unlock(); return d.reconcileLocked() }
func (d *Daemon) reconcileLocked() error {
	var failures []error
	for token, c := range d.connections {
		key := ConnKey{NetNSInode: d.netns, Token: token}
		desired := d.contexts[c.owner]
		if c.closed || desired != nil && desired.deleting {
			if c.owner != "" {
				if err := DeleteLive(key, d.store); err != nil {
					c.lastError = err.Error()
					failures = append(failures, err)
					continue
				}
			}
			if c.closed {
				delete(d.connections, token)
			} else {
				c.owner = ""
				c.lastError = ""
			}
			continue
		}
		if c.owner == "" && c.initial != nil {
			for id, candidate := range d.contexts {
				a := candidate.assignment
				e := c.initial
				if !candidate.deleting && e.DestinationAddress == mustAddress(a.Flow.DestinationAddress) && e.DestinationPort == a.Flow.DestinationPort && (e.LocalAddress == mustAddress(a.Legs.A.LocalAddress) || e.LocalAddress == mustAddress(a.Legs.B.LocalAddress)) {
					c.owner = id
					desired = candidate
					break
				}
			}
		}
		if desired == nil {
			continue
		}
		err := d.applyConnection(key, c, desired.assignment)
		c.lastError = ""
		if err != nil {
			c.lastError = err.Error()
			failures = append(failures, fmt.Errorf("token %d: %w", token, err))
		}
	}
	for id, c := range d.contexts {
		if c.deleting {
			owned := false
			for _, conn := range d.connections {
				if conn.owner == id {
					owned = true
					break
				}
			}
			if !owned {
				delete(d.contexts, id)
			}
		}
	}
	return errors.Join(failures...)
}

func (d *Daemon) plan(c *observedConnection, a cp.Assignment) (Plan, error) {
	p := Policy{Enabled: new(a.Policy.Enabled), Generation: a.Generation, Mode: a.Policy.Mode, RTTDeltaUS: a.Policy.RTTDeltaUS, Legs: Legs{A: Leg{IfName: a.Legs.A.IfName, Weight: new(a.Policy.WeightA)}, B: Leg{IfName: a.Legs.B.IfName, Weight: new(a.Policy.WeightB)}}}
	plan := Plan{Policy: p}
	var err error
	plan.IfIndexA, err = d.validate(a.Legs.A)
	if err != nil {
		return plan, err
	}
	plan.IfIndexB, err = d.validate(a.Legs.B)
	if err != nil {
		return plan, err
	}
	seen := make(map[[2]uint32]uint32)
	for _, e := range c.flows {
		var leg *Leg
		var access uint32
		var index int
		switch e.LocalAddress {
		case mustAddress(a.Legs.A.LocalAddress):
			leg = &plan.Policy.Legs.A
			index = plan.IfIndexA
		case mustAddress(a.Legs.B.LocalAddress):
			leg = &plan.Policy.Legs.B
			index = plan.IfIndexB
			access = 1
		default:
			continue
		}
		if e.IfIndex != 0 && int(e.IfIndex) != index {
			return plan, fmt.Errorf("PM interface index %d disagrees with declared leg %s (%d)", e.IfIndex, leg.IfName, index)
		}
		pair := [2]uint32{uint32(e.LocalID), uint32(e.RemoteID)}
		if previous, ok := seen[pair]; ok {
			if previous != access {
				return plan, errors.New("same endpoint pair observed on both legs")
			}
			continue
		}
		seen[pair] = access
		leg.Endpoints = append(leg.Endpoints, Endpoint{new(pair[0]), new(pair[1])})
	}
	return plan, nil
}
func (d *Daemon) applyConnection(key ConnKey, c *observedConnection, a cp.Assignment) error {
	p, err := d.plan(c, a)
	if err != nil {
		cleanup := DeleteLive(key, d.store)
		return errors.Join(err, cleanup)
	}
	// Incomplete topology uses scheduler fallback until both real legs exist.
	if len(p.Policy.Legs.A.Endpoints) == 0 || len(p.Policy.Legs.B.Endpoints) == 0 {
		return DeleteLive(key, d.store)
	}
	want, err := DesiredPaths(p.Policy, key)
	if err != nil {
		return err
	}
	have, err := d.store.LookupPolicy(key)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if err == nil && have == p.MapPolicy() {
		before, err := d.store.Paths(key)
		if err != nil {
			return err
		}
		for k, v := range want {
			if before[k] != v {
				if err := d.store.UpdatePath(k, v); err != nil {
					return err
				}
			}
		}
		for k := range before {
			if _, ok := want[k]; !ok {
				if err := d.store.DeletePath(k); err != nil && !errors.Is(err, ErrNotFound) {
					return err
				}
			}
		}
		return nil
	}
	return ApplyLive(p, key, d.store)
}
func (d *Daemon) Fail(err error) { d.mu.Lock(); defer d.mu.Unlock(); d.fatal = err }
func (d *Daemon) Cleanup() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	var failures []error
	for token, c := range d.connections {
		if c.owner != "" {
			if err := DeleteLive(ConnKey{NetNSInode: d.netns, Token: token}, d.store); err != nil {
				failures = append(failures, err)
			}
		}
	}
	return errors.Join(failures...)
}
func (d *Daemon) Status() DaemonStatus {
	d.mu.Lock()
	defer d.mu.Unlock()
	result := DaemonStatus{Ready: d.fatal == nil, RestartBehavior: "fresh-connections-only; startup refuses nonempty namespace maps", ObservedConnections: len(d.connections), Contexts: []ContextStatus{}}
	if d.fatal != nil {
		result.Error = d.fatal.Error()
	}
	for _, id := range slices.Sorted(maps.Keys(d.contexts)) {
		desired := d.contexts[id]
		context := ContextStatus{ID: id, Generation: desired.assignment.Generation, Deleting: desired.deleting, Bindings: []BindingStatus{}}
		for _, token := range slices.Sorted(maps.Keys(d.connections)) {
			c := d.connections[token]
			if c.owner != id {
				continue
			}
			b := BindingStatus{Token: token, NetNSInode: d.netns, Paths: []BoundPath{}, Error: c.lastError}
			key := ConnKey{NetNSInode: d.netns, Token: token}
			p, err := d.store.LookupPolicy(key)
			if err == nil {
				b.Generation = p.Generation
				b.Ready = p.Generation == desired.assignment.Generation && b.Error == "" && !c.closed && !desired.deleting
			} else if !errors.Is(err, ErrNotFound) {
				b.Error = err.Error()
			}
			paths, err := d.store.Paths(key)
			if err != nil {
				b.Error = err.Error()
				b.Ready = false
			}
			for k, v := range paths {
				b.Paths = append(b.Paths, BoundPath{LocalID: k.LocalID, RemoteID: k.RemoteID, Access: v.Access})
			}
			slices.SortFunc(b.Paths, func(a, b BoundPath) int {
				if a.LocalID < b.LocalID {
					return -1
				}
				if a.LocalID > b.LocalID {
					return 1
				}
				if a.RemoteID < b.RemoteID {
					return -1
				}
				if a.RemoteID > b.RemoteID {
					return 1
				}
				return 0
			})
			if b.Error != "" {
				result.Ready = false
			}
			context.Bindings = append(context.Bindings, b)
		}
		result.Contexts = append(result.Contexts, context)
	}
	return result
}
