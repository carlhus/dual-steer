module example.com/dual-steer/dualsteer-agent

go 1.26.2

require (
	example.com/dual-steer/controlplane v0.0.0
	github.com/cilium/ebpf v0.20.0
	go.yaml.in/yaml/v3 v3.0.4
	golang.org/x/sys v0.37.0
)

replace example.com/dual-steer/controlplane => ../controlplane
