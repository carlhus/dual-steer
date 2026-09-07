// Package controlplane defines the experimental policy interface, not a 3GPP SBI.
package controlplane

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

const Prefix = "/research/dualsteer/v1"

type Policy struct {
	Enabled    bool   `json:"enabled"`
	Mode       string `json:"mode"`
	WeightA    uint32 `json:"weightA"`
	WeightB    uint32 `json:"weightB"`
	RTTDeltaUS uint32 `json:"rttDeltaUs"`
}

func (p Policy) Validate() error {
	if p.Mode != "load-balance" && p.Mode != "lowest-rtt" {
		return fmt.Errorf("unsupported mode %q", p.Mode)
	}
	if p.WeightA > 100 || p.WeightB > 100 || p.WeightA+p.WeightB != 100 {
		return errors.New("weights must be in 0..100 and total 100")
	}
	return nil
}

type Leg struct {
	IfName       string `json:"ifname"`
	LocalAddress string `json:"localAddress"`
}
type Legs struct {
	A Leg `json:"A"`
	B Leg `json:"B"`
}
type Flow struct {
	DestinationAddress string `json:"destinationAddress"`
	DestinationPort    uint16 `json:"destinationPort"`
}
type ContextSpec struct {
	ID   string `json:"id"`
	DNN  string `json:"dnn"`
	Legs Legs   `json:"legs"`
	Flow Flow   `json:"flow"`
}

// ValidateName keeps context IDs and DNNs safe as single HTTP path segments.
func ValidateName(s string) error {
	if len(s) == 0 || len(s) > 128 || s == "." || s == ".." {
		return errors.New("name must contain 1..128 letters, digits, dots, underscores or hyphens")
	}
	for _, c := range s {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._-", c) {
			continue
		}
		return errors.New("name contains an unsupported character")
	}
	return nil
}

func ParseAddress(s string) (netip.Addr, error) {
	a, err := netip.ParseAddr(s)
	if err != nil || a.Zone() != "" || a.IsUnspecified() || a.IsMulticast() {
		return netip.Addr{}, fmt.Errorf("invalid unicast address %q", s)
	}
	return a.Unmap(), nil
}

func (s ContextSpec) Validate() error {
	if err := ValidateName(s.ID); err != nil {
		return fmt.Errorf("id: %w", err)
	}
	if err := ValidateName(s.DNN); err != nil {
		return fmt.Errorf("dnn: %w", err)
	}
	for _, leg := range []Leg{s.Legs.A, s.Legs.B} {
		if leg.IfName == "" || len(leg.IfName) > 15 || strings.ContainsAny(leg.IfName, " /\t\r\n") {
			return fmt.Errorf("invalid interface name %q", leg.IfName)
		}
		if _, err := ParseAddress(leg.LocalAddress); err != nil {
			return err
		}
	}
	a, _ := ParseAddress(s.Legs.A.LocalAddress)
	b, _ := ParseAddress(s.Legs.B.LocalAddress)
	if a == b || s.Legs.A.IfName == s.Legs.B.IfName {
		return errors.New("legs require different interfaces and local addresses")
	}
	if _, err := ParseAddress(s.Flow.DestinationAddress); err != nil {
		return err
	}
	if s.Flow.DestinationPort == 0 {
		return errors.New("destinationPort must be nonzero")
	}
	return nil
}

// Assignment is flattened JSON: id/dnn/legs/flow plus policy and generation.
// Generation belongs to the SMF context; Policy is the operator decision from PCF.
type Assignment struct {
	ContextSpec
	Policy     Policy `json:"policy"`
	Generation uint32 `json:"generation"`
}

func (a Assignment) Validate() error {
	if err := a.ContextSpec.Validate(); err != nil {
		return err
	}
	if err := a.Policy.Validate(); err != nil {
		return err
	}
	if a.Generation == 0 {
		return errors.New("generation must be nonzero")
	}
	return nil
}
