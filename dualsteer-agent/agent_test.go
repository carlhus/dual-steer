package agent

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validYAML = `dualSteer:
  enabled: true
  generation: 7
  mode: load-balance
  rttDeltaUs: 1000
  legs:
    A: {ifname: leg-a, weight: 60}
    B: {ifname: leg-b, weight: 40}
`

func TestDecode(t *testing.T) {
	cases := []struct {
		name, input string
		valid       bool
	}{
		{"valid", validYAML, true},
		{"rtt", strings.Replace(validYAML, "load-balance", "lowest-rtt", 1), true},
		{"disabled", strings.Replace(validYAML, "enabled: true", "enabled: false", 1), true},
		{"zero weight", strings.ReplaceAll(strings.ReplaceAll(validYAML, "weight: 60", "weight: 0"), "weight: 40", "weight: 100"), true},
		{"bad sum", strings.Replace(validYAML, "weight: 60", "weight: 61", 1), false},
		{"bad mode", strings.Replace(validYAML, "load-balance", "random", 1), false},
		{"negative weight", strings.Replace(validYAML, "weight: 60", "weight: -1", 1), false},
		{"oversize weight", strings.Replace(validYAML, "weight: 60", "weight: 4294967296", 1), false},
		{"missing weight", strings.Replace(validYAML, ", weight: 60", "", 1), false},
		{"missing enabled", strings.Replace(validYAML, "  enabled: true\n", "", 1), false},
		{"missing generation", strings.Replace(validYAML, "  generation: 7\n", "", 1), false},
		{"duplicate interface", strings.Replace(validYAML, "leg-b", "leg-a", 1), false},
		{"unknown field", validYAML + "  typo: 1\n", false},
		{"nested unknown", strings.Replace(validYAML, "ifname: leg-a", "ifname: leg-a, extra: 1", 1), false},
		{"duplicate key", validYAML + "  enabled: false\n", false},
		{"extra document", validYAML + "---\n{}\n", false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Decode(strings.NewReader(tt.input))
			if (err == nil) != tt.valid {
				t.Fatalf("valid=%v err=%v", tt.valid, err)
			}
		})
	}
}
func fakeResolver(name string) (int, error) {
	switch name {
	case "leg-a":
		return 11, nil
	case "leg-b":
		return 12, nil
	}
	return 0, errors.New("no such interface")
}

func TestApplyAndABI(t *testing.T) {
	cfg, err := Decode(strings.NewReader(validYAML))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Prepare(cfg, fakeResolver)
	if err != nil {
		t.Fatal(err)
	}
	key := ConnKey{NetNSInode: 42, Token: 123}
	want := MapPolicy{Generation: 7, Enabled: 1, Mode: 1, WeightA: 60, WeightB: 40, RTTDeltaUS: 1000}
	if plan.MapPolicy() != want {
		t.Fatal(plan.MapPolicy())
	}
	if binary.Size(key) != 16 || binary.Size(want) != 24 {
		t.Fatal("ABI size mismatch")
	}
	var data bytes.Buffer
	if err := binary.Write(&data, binary.NativeEndian, want); err != nil {
		t.Fatal(err)
	}
	for i, want := range []uint32{7, 1, 1, 60, 40, 1000} {
		if got := binary.NativeEndian.Uint32(data.Bytes()[i*4:]); got != want {
			t.Fatalf("word %d=%d want %d", i, got, want)
		}
	}

}
func TestCLI(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(validYAML), 0600); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{"validate", "apply"} {
		t.Run(cmd, func(t *testing.T) {
			args := []string{cmd, "--config", path}
			if cmd == "apply" {
				args = append(args, "--dry-run")
			}
			var output bytes.Buffer
			if err := Run(args, &output, fakeResolver); err != nil {
				t.Fatal(err)
			}
			if cmd != "validate" && !strings.Contains(output.String(), "ifindex=11") {
				t.Fatal(output.String())
			}
			if cmd == "apply" && !strings.Contains(output.String(), "planned ds_policy: generation=7 enabled=1 mode=1 weight_a=60 weight_b=40 rtt_delta_us=1000") {
				t.Fatal(output.String())
			}
			if cmd == "status" && !strings.Contains(output.String(), "live kernel status unavailable") {
				t.Fatal(output.String())
			}
		})
	}
	var output bytes.Buffer
	if err := Run([]string{"--config", path}, &output, fakeResolver); err != nil {
		t.Fatalf("default validate: %v", err)
	}
	output.Reset()
	if err := Run([]string{"--config", path, "apply"}, &output, fakeResolver); err == nil || !strings.Contains(err.Error(), "require --policy-map") {
		t.Fatalf("real apply should fail: %v", err)
	}
	err := Run([]string{"--config", path, "apply", "--dry-run"}, &output, func(name string) (int, error) { return 0, errors.New("device missing") })
	if err == nil || !strings.Contains(err.Error(), `leg A interface "leg-a"`) {
		t.Fatalf("missing interface error: %v", err)
	}
}
