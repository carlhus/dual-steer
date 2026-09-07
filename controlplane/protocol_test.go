package controlplane

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func validAssignment() Assignment {
	return Assignment{ContextSpec: ContextSpec{ID: "lab", DNN: "internet", Legs: Legs{A: Leg{"leg-a", "10.60.1.2"}, B: Leg{"leg-b", "10.60.2.2"}}, Flow: Flow{"10.60.1.1", 5001}}, Policy: Policy{true, "load-balance", 70, 30, 1000}, Generation: 1}
}

func TestInvalidAssignments(t *testing.T) {
	for name, mutate := range map[string]func(*Assignment){
		"weight overflow":          func(a *Assignment) { a.Policy.WeightA = math.MaxUint32; a.Policy.WeightB = 101 },
		"bad sum":                  func(a *Assignment) { a.Policy.WeightB = 80 },
		"unknown mode":             func(a *Assignment) { a.Policy.Mode = "fallback" },
		"zero generation":          func(a *Assignment) { a.Generation = 0 },
		"path traversal":           func(a *Assignment) { a.ID = "../lab" },
		"same address mapped IPv6": func(a *Assignment) { a.Legs.B.LocalAddress = "::ffff:10.60.1.2" },
		"same interface":           func(a *Assignment) { a.Legs.B.IfName = "leg-a" },
		"multicast":                func(a *Assignment) { a.Flow.DestinationAddress = "224.0.0.1" },
		"zero port":                func(a *Assignment) { a.Flow.DestinationPort = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			a := validAssignment()
			mutate(&a)
			if a.Validate() == nil {
				t.Fatal("invalid assignment accepted")
			}
		})
	}
}

func TestAssignmentWireContract(t *testing.T) {
	a := validAssignment()
	if err := a.Validate(); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"id":"lab"`, `"generation":1`, `"A":{"ifname":"leg-a"`, `"weightA":70`} {
		if !strings.Contains(string(data), field) {
			t.Fatalf("missing field %s in %s", field, data)
		}
	}
	var decoded Assignment
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded != a {
		t.Fatalf("round trip differs: %+v", decoded)
	}
}
