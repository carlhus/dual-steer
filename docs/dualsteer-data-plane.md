# Generic Leg A／Leg B data plane

## 範圍與來源

本 PR 在獨立 `dual-steer` 專案實作兩條 IP leg 的 MPTCP 排程。兩條 leg 均可來自 3GPP，但排程器不推測 access technology。free5GC **v4.2.3** 為後續整合基準，固定 commit `3b34a08e93a9b334f0f4005d3a3a9f79b66d59b9`；本 PR 沒有改動或啟動 free5GC。

優先依據：

- 使用者提供的 `112-atsss-paper.pdf`，第 3.2.1 節（印刷頁 23–24）的 weighted round robin，以及第 3.2.2 節（24–25）的 `tcp_sock.srtt_us`。
- [carlhus/atsss](https://github.com/carlhus/atsss)，本機 `ATSSS-UE/mptcp_net-next` 與 `ATSSS-UPF/mptcp_net-next` 的實際程式碼。
- [free5GC v4.2.3 release](https://github.com/free5gc/free5gc/releases/tag/v4.2.3)。本機 `/home/ubuntu/free5gc` 是該 tag 之後的 checkout，不能當成精確 v4.2.3。

論文記載 kernel 6.5，但供應的 source tree Makefile 為 **6.6-rc2**。建置 ABI 以 source tree 為準，不能只憑 kernel 版本字串判斷相容。研究報告中的後續控制平面、容器與 NAS 計畫不在此 PR 範圍。

## 已找到的 custom kernel modifications

以下路徑相對於論文的 `mptcp_net-next`，UE 與 UPF 均有對應實作：

| 檔案 | 自訂部分與泛化影響 |
| --- | --- |
| `net/mptcp/protocol.h` | `mptcp_sock.weight_3gpp`、subflow quota/weight/SRTT 欄位與 scheduler ABI；有 local token、local/remote endpoint ID |
| `net/mptcp/ctrl.c` | namespace 級 weight 與 access IPv4 sysctl，不足以表達不同 connection 的 policy |
| `net/mptcp/sched.c` | UL／DL 地址分類、自訂 quota/SRTT helpers、weight 初始化；custom scheduler error 不會觸發 default |
| `net/mptcp/bpf.c` | MPTCP struct_ops verifier、可呼叫 kfunc 白名單 |
| `net/mptcp/protocol.c` | scheduler 初始化與傳送流程 |
| `tools/testing/selftests/bpf/bpf_tcp_helpers.h` | 手工縮減的 CO-RE 結構與 kfunc 宣告 |
| `tools/testing/selftests/bpf/progs/mptcp_bpf_rr_quota.c` | 固定地址、compile-time DL 選擇、per-subflow quota；新 policy 無法可靠刷新舊分類 |
| `tools/testing/selftests/bpf/progs/mptcp_bpf_rtt.c` | 讀取 `srtt_us >> 3` 選最小正值 |

原 quota helper 的 `mptcp_subflow_set_weight` 參數宣告為 bool，也無法正確傳遞一般權重。新 scheduler 的 quota/state 使用自身 map，不使用這組 access-specific 欄位或 helpers。

這不是 vanilla upstream MPTCP 可直接編譯載入的程式：依賴該 fork 的 `mptcp_sched_ops`、`mptcp_sched_data`、context-by-position 與 scheduling kfunc。額外 bridge patch 暴露原 kernel default 邏輯，避免用 `return -1` 假裝 fallback。它是 **必要的 MPTCP kernel patch**，不屬於 free5GC 修改。

## 架構與 ABI

```text
YAML + 本端 MPTCP PM events → Go agent apply / status / delete
                                               ↓
                    ds_path_map + ds_policy_map (per connection)
                                               ↓
              bpf_ds_wrr / bpf_ds_rtt → Leg A or Leg B subflow
                       │ missing/disabled policy or reinjection
                       └── kernel default scheduler bridge
```

`include/dualsteer_policy.h` 是 native-endian ABI 的來源：

- connection key：namespace inode (`stat /proc/PID/ns/net`) + **本端** MPTCP token + zero reserved field。不是 netns cookie；UE 與 peer 的 token 不同。
- path key：connection key + local endpoint ID + remote endpoint ID。ID 0 有效。ifindex 僅供 agent 診斷，不等於 endpoint ID，也不能從 `sk_bound_dev_if` 猜測所有經過某介面的連線。
- policy：generation、enabled、mode、A/B weight、RTT delta（微秒）。weight 各在 0–100、總和 100，generation 不為 0。
- path value：generation 與 leg identity。Controller 必須先建立相同 generation 的 path mapping，再以單次 map replacement 發布 policy；每次修改都前進 generation。更新途中不相符的 mapping 不參與自訂選路。
- scheduler state 為 socket storage；連線關閉後由 kernel 清除。`ds_stats_map` 提供同一 connection 累計的 A/B 選路與 fallback 次數，供實際測試辨識 policy 是否生效。外部 controller 必須以 `delete` 清除已關閉 connection 的 policy/path entries，避免 token／namespace inode 重用時套用舊 policy。目前沒有常駐 PM event daemon，自動生命週期管理仍需外部呼叫。

WRR 權重描述 **排程選取次數**，不保證 iperf byte ratio 等於 70/30；subflow 的 congestion window、RTT、實際 burst 大小會影響吞吐比例。同 leg 多個 subflow 不額外增加該 leg 權重。RTT mode 忽略零值測量，只有改善超過 `rttDeltaUs` 才切換；generation 更新會重置歷史。

policy 缺失、停用、不合法、沒有可選路徑或 reinjection 時使用真實 kernel default。Fallback 不受自訂 weight 限制，可能使用 weight 0 或尚未分類的可用路徑，以保留連線傳送能力。這也表示「零 SRTT 不被自訂 RTT 演算法選為最佳」不等於「default 永遠不准使用尚未量測的路徑」。

兩個新增 scheduler 名稱共用 map 與排程邏輯，mode 由 policy 決定；因此切換 mode 不需重編或改 namespace sysctl。原本 `bpf_rr_quota`、`bpf_rtt` 完全保留在來源 tree。

## 建置與 Go agent

從本專案根目錄執行：

```sh
make test
cd dualsteer-agent
go build -o ../build/dualsteer-agent ./cmd/dualsteer-agent
../build/dualsteer-agent --config config.example.yaml validate
```

預期最後一行為 `configuration valid (syntax and policy only; interfaces and kernel state not checked)`。Go 使用 1.26，修改前已執行 Modern Go Guidelines CLI。

在含有 `leg-a`／`leg-b` 介面的 namespace 內：

```sh
sudo ip netns exec ds-ue ./build/dualsteer-agent \
  --config dualsteer-agent/config.example.yaml apply --dry-run
```

預期顯示實際 ifindex、70/30 權重與 `planned ds_policy: generation=1 enabled=1 mode=1 weight_a=70 weight_b=30 rtt_delta_us=1000`，明確標註不修改 kernel。缺少介面時回報是哪個 leg 及 ifname。

### 實際 map 操作

複製 `dualsteer-agent/config.live.example.yaml`，用本端 PM events 的 token 與 endpoint ID pairs 填入 `connection` 及各 leg 的 `endpoints`。`netnsInode` 可省略，agent 會讀取目前 namespace；若指定，必須與目前 namespace 相同。map selector 接受 `id:NUMBER` 或絕對 bpffs pinned path。

```sh
sudo ip netns exec ds-ue ./build/dualsteer-agent \
  --config /path/to/live.yaml apply \
  --policy-map id:POLICY_MAP_ID --path-map id:PATH_MAP_ID
sudo ip netns exec ds-ue ./build/dualsteer-agent \
  --config /path/to/live.yaml status \
  --policy-map id:POLICY_MAP_ID --path-map id:PATH_MAP_ID
sudo ip netns exec ds-ue ./build/dualsteer-agent \
  --config /path/to/live.yaml delete \
  --policy-map id:POLICY_MAP_ID --path-map id:PATH_MAP_ID
```

上述 `POLICY_MAP_ID`／`PATH_MAP_ID` 須替換成此次 loader 的實際 ID。`apply` 先驗證 map 名稱、Hash 類型、key/value 大小，再 staging paths、commit policy；generation 必須大於目前 live generation。提交前失敗會嘗試回復 path entries。提交後清理舊 path 失敗會回報「已提交但清理失敗」，不假裝整筆更新未生效。

`status` 讀取實際 policy 與 path entries，顯示 generation 是否相符；它不把 map 內容當成 scheduler attachment 或流量證據。`delete` 先移除 policy 再清除該 connection 的 paths，重複執行有效。CLI 使用 `/run/lock/dualsteer-agent.lock` 序列化操作；其他 writer 也須遵循相同鎖，BPF map 不提供此流程需要的跨 map transaction。

## BPF 建置與載入（目標 lab host）

需要支援 BPF target 的 clang、C compiler、Python 3，以及論文 kernel tree。Makefile 只讀取外部 kernel headers，生成檔案放在本專案 `build/`。

```sh
make bpf KERNEL_SRC=/path/to/paper/mptcp_net-next CLANG=clang
make check-kernel-patch KERNEL_SRC=/path/to/paper/mptcp_net-next
```

目前 Makefile 的 BPF target 是 x86；其他架構需要調整 target define 與 include 路徑。這是編譯 CO-RE object，尚未執行 verifier。

在獨立的 kernel 工作副本套用 `bpf/mptcp-default-kfunc.patch`，啟用該 fork 的 `CONFIG_MPTCP`、`CONFIG_BPF_SYSCALL`、`CONFIG_BPF_JIT`、`CONFIG_DEBUG_INFO_BTF`，依該 tree 的建置流程重建並啟動 kernel。請保留原 kernel 作為開機選項。patch 必須同時作用於承載 UE／peer scheduler 的 kernel；單主機 namespace lab 共用同一個 kernel。

啟動相符 kernel 後，先用 bpftool 檢查 BTF 中存在 `mptcp_sched_ops` 與 `bpf_mptcp_sched_default`，再執行：

```sh
sudo bpftool -d struct_ops register build/dualsteer.bpf.o
sudo bpftool struct_ops show
sudo bpftool map show
# 建立下節 namespace 後，對新連線選擇 scheduler
sudo ip netns exec ds-ue sysctl -w net.mptcp.scheduler=bpf_ds_wrr
sudo ip netns exec ds-peer sysctl -w net.mptcp.scheduler=bpf_ds_rtt
```

object 同時註冊兩個名稱；不要重複載入來切 mode。未提供 policy 時應走 default。記錄這次載入的 `ds_policy_map`、`ds_path_map` 與 struct_ops map IDs；不要在多個 instance 間只靠相同 map name 更新。卸載前先將 namespace scheduler 設回 `default`、關閉引用 scheduler 的連線，再以 `bpftool struct_ops unregister id ID` 逐一卸載此次註冊的兩個 struct_ops maps。

建議使用上節 agent 進行更新。需要排查 raw ABI 時，也可在 lab 手動使用 `bpftool map update`；下例只產生指令、不執行，且不具備 agent 的鎖定、validation 與 rollback。先將環境變數填成 **該次實際觀測值**：`DS_NETNS_INODE`、`DS_TOKEN`、`DS_PATH_MAP_ID`、`DS_POLICY_MAP_ID`、`DS_A_LOCAL_ID`、`DS_A_REMOTE_ID`、`DS_B_LOCAL_ID`、`DS_B_REMOTE_ID`，另設非零 `DS_GENERATION`。netns inode 可用 `stat -Lc %i /run/netns/ds-ue` 取得，其餘 ID 由本端 PM events 與本次載入的 map 清單取得。以下範例必須在目標機執行，使用該機 native endian。

```python
import os
import struct

def value(name):
    return int(os.environ["DS_" + name], 0)

def emit(map_id, key, val):
    print("sudo bpftool map update id", map_id,
          "key hex", key.hex(" "), "value hex", val.hex(" "))

conn = struct.pack("=QII", value("NETNS_INODE"), value("TOKEN"), 0)
generation = value("GENERATION")
assert 0 < generation < 2**32
for leg, name in enumerate(("A", "B")):
    key = conn + struct.pack("=II", value(name + "_LOCAL_ID"),
                            value(name + "_REMOTE_ID"))
    emit(value("PATH_MAP_ID"), key, struct.pack("=II", generation, leg))
# Publish policy LAST: mode 1=WRR, 2=RTT, weights A/B, delta in microseconds.
emit(value("POLICY_MAP_ID"), conn,
     struct.pack("=IIIIII", generation, 1, 1, 70, 30, 1000))
```

檢查生成的三條指令後依序執行。要切成 RTT，把最後一列 mode 改為 2，遞增 generation，再重新產生、更新 mapping 和 policy。更新期間既有 path entry 被新 generation 覆蓋，可能暫時進入 default；此版不提供跨多個 map 的 transaction。UE 與 peer 必須各自建立 key/mapping，不能共用同一 token。

## Namespace 實驗拓樸

```text
ds-ue                                         ds-peer (MPTCP endpoint)
  leg-a 10.60.1.2 ─────── veth ─────── 10.60.1.1 leg-a
  leg-b 10.60.2.2 ─────── veth ─────── 10.60.2.1 leg-b
```

這是純 IP data-plane lab，沒有模擬 gNB、PDU session 或宣稱完成 3GPP DualSteer。

```sh
sudo bash scripts/netns-lab.sh up
sudo ip -n ds-ue mptcp endpoint show
sudo ip -n ds-peer mptcp endpoint show
sudo ip netns exec ds-ue ping -c 2 10.60.1.1
sudo ip netns exec ds-ue ping -c 2 10.60.2.1
```

腳本建立 peer 的 `id 2 signal` endpoint。先從 UE 連到 `10.60.1.1`，peer 宣告 Leg B 地址後，kernel path manager 可建立第二條 subflow；需用支援 MPTCP 的測試程式，普通 TCP iperf 不足以證明。

可在兩個 terminal 用 Python 3 明確建立 MPTCP socket：

```sh
# Terminal 1: receiver
sudo ip netns exec ds-peer python3 -c '
import socket
s = socket.socket(socket.AF_INET, socket.SOCK_STREAM, socket.IPPROTO_MPTCP)
s.bind(("0.0.0.0", 5001)); s.listen(1)
c, _ = s.accept()
while c.recv(65536): pass
'

# Terminal 2: sender, keep running while examining/altering paths
sudo ip netns exec ds-ue python3 -c '
import socket, time
s = socket.socket(socket.AF_INET, socket.SOCK_STREAM, socket.IPPROTO_MPTCP)
s.connect(("10.60.1.1", 5001))
end = time.monotonic() + 120
while time.monotonic() < end: s.sendall(bytes(65536))
'
```

第三個 terminal 檢查 `sudo ip netns exec ds-ue ss -Mnti` 與 `ss -nti`；必須觀察到兩條 subflow 才能測雙路政策。可使用 `ip mptcp monitor` 蒐集本端 token 與 endpoint IDs，勿假設初始 subflow ID 一定等於配置的 interface/endpoint index。

反轉延遲與測試斷路：

```sh
sudo ip netns exec ds-ue tc qdisc replace dev leg-a root netem delay 40ms
sudo ip netns exec ds-ue tc qdisc replace dev leg-b root netem delay 5ms
sudo ip -n ds-ue link set leg-a down
# Recover before subsequent tests
sudo ip -n ds-ue link set leg-a up
```

載入及 map binding 完成後，lowest-rtt 應在平滑 RTT 改善超過 delta 後切換；斷路收斂受 TCP loss detection／MPTCP reinjection 影響，不保證瞬時切換。停止測試程式後可執行 `sudo bash scripts/netns-lab.sh down`。

## 驗證邊界

2026-09-07 已完成實際 kernel map、QEMU patched-kernel 載入及自訂 scheduler 雙路流量測試，詳見 [實測數據與產物雜湊](runtime-validation.md)。

原生 C 測試執行與 BPF 共用的選路函式，驗證權重、失效路徑、零 RTT、hysteresis 與 generation 更新。Go 測試使用 fake writer 驗證 policy 與錯誤傳遞，並檢查 ABI 欄位順序。這些不等於 kernel verifier 或實際雙路流量測試。

### 可重現的自動驗證

主機維持 `6.8.0-138-generic`，使用免密 sudo 完成實際 kernel map 與 namespace 基準測試。論文 kernel 在獨立 QEMU guest 啟動，不安裝主機 kernel，也不重新啟動主機。

Ubuntu 建置套件：

```sh
sudo apt-get install --no-install-recommends build-essential clang \
  flex bison libelf-dev libssl-dev dwarves libbpf-dev zlib1g-dev \
  qemu-system-x86 busybox-static cpio bc python3 iproute2 file
```

另需可執行的 bpftool ELF binary（Ubuntu `linux-tools-$(uname -r)` 提供）。`qemu-initramfs.sh` 會避開依 host kernel 版本 dispatch 的 `/usr/sbin/bpftool` wrapper，可用 `BPFTOOL_BIN` 指定真正 binary。

```sh
# Unit tests / actual host kernel maps (does not attach scheduler)
make test agent bpf
make kernel-map-test

# Actual host default-scheduler dual-path baseline; cleans its namespaces
sudo python3 scripts/verify-dataplane.py --baseline --output build/runtime-host

# Build isolated source from the supplied kernel repository's Git HEAD
make qemu-kernel
# Build initramfs, boot guest, load BPF, exercise live policy and traffic
make qemu-test
```

QEMU 優先使用可用的 KVM，沒有 `/dev/kvm` 時使用 TCG。guest 沒有外部 NIC；veth、netem 與 MPTCP 流量都位於 guest namespaces。9p 只分享本專案到 `/work`，供讀取程式與寫回驗證結果。預設 VM timeout 300 秒，可設定 `QEMU_TIMEOUT`；主機 kernel 原始目錄與 free5GC 都不改動。

自動 workload 用 `IPPROTO_MPTCP` 持續傳輸，從 `MPTCP_INFO` 讀本端 token，並從 PM events 取得真正 local/remote endpoint ID pairs。驗證內容包括：

- 兩條已建立 subflow，各路 TCP payload 增長，未降級成普通 TCP。
- 實際 `apply` 將 WRR 70/30 更新為 20/80，scheduler counters 必須顯示兩路被選取，且 A 選取比例降低。
- mode 更新為 lowest-rtt，反轉 netem 延遲後，自訂選路偏好必須由 A 轉向 B。
- 未設定及刪除 policy 時，只有 default fallback counters 增長。
- 再次套用 WRR 後關閉 Leg A，Leg B 的 payload 與自訂選路次數繼續增加。
- 清除 policy/path entries 與測試 namespace；清理失敗也會讓測試失敗。

結果檔案：`build/kernel-map-test.log`（手動重導向時）、`build/runtime-host/report.json`、`build/runtime-bpf/report.json`、`build/guest-results/verification.log`、`build/qemu/console.log`。`make qemu-test` 必須看到 guest success marker 才回傳成功；QEMU 正常關機本身不算測試通過。

`report.json` 中 `passed` 是流量與清理結果，`custom_scheduler_verified` 只有在供應 stats map、並通過自訂選路 assertions 時才為 true。veth/QEMU 結果是功能驗證，不是實體雙 3GPP access、free5GC integration 或效能 benchmark。
