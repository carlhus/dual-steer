// Package agent validates desired policy and prepares writes to the scheduler config map.
package agent

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strings"

	"go.yaml.in/yaml/v3"
)

type Config struct {
	DualSteer Policy `yaml:"dualSteer"`
}
type Policy struct {
	Enabled    *bool       `yaml:"enabled"`
	Generation uint32      `yaml:"generation"`
	Mode       string      `yaml:"mode"`
	RTTDeltaUS uint32      `yaml:"rttDeltaUs"`
	Legs       Legs        `yaml:"legs"`
	Connection *Connection `yaml:"connection"`
}
type Legs struct {
	A Leg `yaml:"A"`
	B Leg `yaml:"B"`
}
type Leg struct {
	IfName    string     `yaml:"ifname"`
	Weight    *uint32    `yaml:"weight"`
	Endpoints []Endpoint `yaml:"endpoints"`
}

// Endpoint IDs are supplied from MPTCP PM events, never inferred from ifindex.
type Endpoint struct {
	LocalID  *uint32 `yaml:"localId"`
	RemoteID *uint32 `yaml:"remoteId"`
}
type Connection struct {
	Token      *uint32 `yaml:"token"`
	NetNSInode uint64  `yaml:"netnsInode"`
}

func Decode(r io.Reader) (Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("decode config: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return cfg, fmt.Errorf("decode trailing document: %w", err)
		}
		return cfg, errors.New("exactly one YAML document is required")
	}
	return cfg, cfg.Validate()
}

func (c Config) Validate() error {
	p := c.DualSteer
	if p.Enabled == nil {
		return errors.New("dualSteer.enabled is required")
	}
	if p.Generation == 0 {
		return errors.New("dualSteer.generation must be at least 1")
	}
	if p.Mode != "load-balance" && p.Mode != "lowest-rtt" {
		return errors.New("dualSteer.mode must be load-balance or lowest-rtt")
	}
	for _, item := range []struct {
		name string
		leg  Leg
	}{{"A", p.Legs.A}, {"B", p.Legs.B}} {
		if item.leg.IfName == "" || strings.TrimSpace(item.leg.IfName) != item.leg.IfName {
			return fmt.Errorf("leg %s requires a nonempty ifname without surrounding whitespace", item.name)
		}
		if item.leg.Weight == nil || *item.leg.Weight > 100 {
			return fmt.Errorf("leg %s weight must be specified in range 0..100", item.name)
		}
	}
	if p.Legs.A.IfName == p.Legs.B.IfName {
		return errors.New("legs A and B must use different interface names")
	}
	if *p.Legs.A.Weight+*p.Legs.B.Weight != 100 {
		return errors.New("leg A and B weights must sum to 100")
	}

	if p.Connection != nil && p.Connection.Token == nil {
		return errors.New("connection.token must be explicitly supplied (zero is valid)")
	}
	seen := make(map[[2]uint32]bool)
	for _, leg := range []Leg{p.Legs.A, p.Legs.B} {
		for _, ep := range leg.Endpoints {
			if ep.LocalID == nil || ep.RemoteID == nil || *ep.LocalID > 255 || *ep.RemoteID > 255 {
				return errors.New("endpoints require localId and remoteId in 0..255")
			}
			pair := [2]uint32{*ep.LocalID, *ep.RemoteID}
			if seen[pair] {
				return fmt.Errorf("duplicate endpoint pair %v across legs", pair)
			}
			seen[pair] = true
		}
	}
	return nil
}

type InterfaceResolver func(string) (int, error)

func ResolveInterface(name string) (int, error) {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return 0, err
	}
	return iface.Index, nil
}

type Plan struct {
	Policy   Policy
	IfIndexA int
	IfIndexB int
}

func Prepare(c Config, resolve InterfaceResolver) (Plan, error) {
	if err := c.Validate(); err != nil {
		return Plan{}, err
	}
	a, err := resolve(c.DualSteer.Legs.A.IfName)
	if err != nil {
		return Plan{}, fmt.Errorf("resolve leg A interface %q: %w", c.DualSteer.Legs.A.IfName, err)
	}
	b, err := resolve(c.DualSteer.Legs.B.IfName)
	if err != nil {
		return Plan{}, fmt.Errorf("resolve leg B interface %q: %w", c.DualSteer.Legs.B.IfName, err)
	}
	if a <= 0 || b <= 0 {
		return Plan{}, errors.New("resolved interface indices must be positive")
	}
	return Plan{Policy: c.DualSteer, IfIndexA: a, IfIndexB: b}, nil
}
