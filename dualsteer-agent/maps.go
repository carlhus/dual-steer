package agent

import "errors"

// ConnKey mirrors struct ds_conn_key. Reserved must remain zero. The token is
// scoped to the socket's network namespace; it is not an interface identifier.
type ConnKey struct {
	NetNSInode uint64
	Token      uint32
	Reserved   uint32
}

// MapPolicy mirrors the 24-byte native-endian struct ds_policy in
// ../include/dualsteer_policy.h. Interface indices are intentionally absent.
type MapPolicy struct {
	Generation uint32
	Enabled    uint32
	Mode       uint32
	WeightA    uint32
	WeightB    uint32
	RTTDeltaUS uint32
}

// MapStore abstracts real kernel maps for transaction and failure tests.
type MapStore interface {
	LookupPolicy(ConnKey) (MapPolicy, error)
	UpdatePolicy(ConnKey, MapPolicy) error
	DeletePolicy(ConnKey) error
	Paths(ConnKey) (map[PathKey]MapPath, error)
	UpdatePath(PathKey, MapPath) error
	DeletePath(PathKey) error
}

var ErrNotFound = errors.New("map entry not found")

type PathKey struct {
	Conn     ConnKey
	LocalID  uint32
	RemoteID uint32
}
type MapPath struct {
	Generation uint32
	Access     uint32
}

func (p Plan) MapPolicy() MapPolicy {
	enabled := uint32(0)
	if *p.Policy.Enabled {
		enabled = 1
	}
	mode := uint32(1)
	if p.Policy.Mode == "lowest-rtt" {
		mode = 2
	}
	return MapPolicy{p.Policy.Generation, enabled, mode, *p.Policy.Legs.A.Weight, *p.Policy.Legs.B.Weight, p.Policy.RTTDeltaUS}
}
