# dualsteer-agent

Go 1.26 Linux controller for generic two-leg MPTCP policies. It uses
[cilium/ebpf v0.20.0](https://github.com/cilium/ebpf/tree/v0.20.0) to read and write
existing kernel maps. No free5GC configuration or binaries are modified. Loading
and attaching the scheduler remains the external loader's responsibility.

```sh
cd dualsteer-agent
go build -o /tmp/dualsteer-agent ./cmd/dualsteer-agent
/tmp/dualsteer-agent --config config.example.yaml validate
/tmp/dualsteer-agent --config config.example.yaml apply --dry-run
```

Omitting the command defaults to `validate`. `validate` checks the strict YAML
schema and policy without resolving interfaces. `apply --dry-run` resolves actual
interface indices and prints the six ABI policy fields; when connection binding
is present it also prints the actual namespace inode, token, and endpoint pairs.
It never opens or updates BPF maps. Sample interface names are placeholders;
missing interfaces produce an error identifying the leg and name.

For live operations, start from `config.live.example.yaml`, set its token and
endpoint pairs from this connection's MPTCP PM events, and run inside the socket's
network namespace. Endpoint IDs are integers in 0..255; zero is valid for the
initial endpoint. Each leg accepts multiple pairs, with no duplicate pair across
legs. **Interface indices are diagnostic only and never become endpoint IDs.**
The optional `connection.netnsInode` is checked against `/proc/self/ns/net`;
a mismatch is an error. The agent uses the current inode when it is omitted.

```sh
# First load/attach the BPF scheduler externally and pin its maps.
# Run these commands as root or with the appropriate BPF capabilities.
/tmp/dualsteer-agent --config live.yaml apply \
  --policy-map /sys/fs/bpf/dualsteer/ds_policy_map \
  --path-map /sys/fs/bpf/dualsteer/ds_path_map
/tmp/dualsteer-agent --config live.yaml status \
  --policy-map /sys/fs/bpf/dualsteer/ds_policy_map \
  --path-map /sys/fs/bpf/dualsteer/ds_path_map
/tmp/dualsteer-agent --config live.yaml delete \
  --policy-map /sys/fs/bpf/dualsteer/ds_policy_map \
  --path-map /sys/fs/bpf/dualsteer/ds_path_map
```

Either map selector also accepts `id:123`, with the actual ID from `bpftool map
show`. Map names must be `ds_policy_map` and `ds_path_map`, both Hash maps, with
key/value sizes 16/24 and 24/8 respectively. ABI mismatch fails before writes.
The maps must come from the same scheduler instance; names and sizes alone do
not prove attachment or ownership. `status` reads actual map values and reports
whether each endpoint generation matches the live policy. Map values alone do
not prove that traffic uses the scheduler. Status and delete do not require the
configured interfaces to still exist.

`apply` requires a generation greater than the existing policy's generation.
It snapshots old paths, stages the new generation's endpoint bindings, then
atomically replaces the whole policy value. Old unused paths are removed after
commit. A staging or commit failure attempts to restore the original paths;
rollback failures are returned explicitly. A cleanup failure after commit reports
that the policy was committed. The CLI takes `/run/lock/dualsteer-agent.lock` for
live operations, including status, to serialize agent processes across network
namespaces. Other writers must follow the same lock; BPF map APIs provide no
compare-and-swap transaction. The lock must be writable by the operator.

The generation check makes partially staged bindings fall back to the default
scheduler; there can be a brief fallback window during updates. A process killed
mid-stage can leave orphan or mismatched paths. Retry the uncommitted generation,
or use `delete` to clean up. `delete` removes policy first and then every path
for that exact namespace/token; it is idempotent. Run it when a connection closes,
before token or namespace inode reuse. There is no automatic PM event watcher in
this controller. Keep generations increasing for a connection; do not wrap the
uint32 generation counter or reuse a deleted connection's generation for that
same still-live connection. `enabled: false` applies a new generation requesting
default scheduler fallback and leaves the attachment present.

The YAML decoder rejects unknown fields, duplicate keys, and multiple documents.
`enabled`, positive `generation`, supported `mode`, both interface names, and
weights are required. Weights are integers in 0..100 and total 100, including
`lowest-rtt` mode. `rttDeltaUs` is a nonnegative uint32 in microseconds; omission
means zero. Connection/endpoint fields are optional for validation and basic
dry-runs, but live apply requires a token and explicit endpoints for both legs.

```sh
go test ./...
go vet ./...
# Actual kernel map tests: private temporary maps, no scheduler attachment.
go test -c -o /tmp/dualsteer-agent-tests
sudo env DUALSTEER_KERNEL_TEST=1 /tmp/dualsteer-agent-tests -test.run '^TestKernelMaps$' -test.v
```

Kernel tests require bpffs mounted at `/sys/fs/bpf`. They exercise real map ID and
pin opening, ABI bytes, apply/update/status/delete, stale generations, and wrong
map rejection; their maps and pins are removed after execution. Ordinary tests
exercise transaction failure rollback with a fake `MapStore` and do not require
root. These map tests are separate from MPTCP scheduler data-plane verification.
