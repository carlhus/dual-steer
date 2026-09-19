# 實際 kernel 與雙路驗證紀錄

驗證日期：2026-09-07 UTC。結果：**通過**，`passed=true`、`custom_scheduler_verified=true`。

主機 `6.8.0-138-generic` 上的實際 kernel map 整合測試、Go tests/vet、原生 C 選路測試均通過。另在 QEMU TCG 啟動 `6.6.0-rc2-dualsteer-test`，實際通過 BPF verifier 並註冊 `bpf_ds_wrr`／`bpf_ds_rtt`。主機 kernel 與 free5GC 均未修改。

## 同一連線的實測結果

測試使用 `ds-ue`／`ds-peer` 的兩對 veth。PM events 觀察到 Leg A endpoint pair `(0,0)`、Leg B `(1,2)`；兩條 subflow 在同一 MPTCP 連線中運作，沒有降級成普通 TCP。

| 階段 | 自訂 A 選路次數 | 自訂 B 選路次數 | Default 呼叫次數 | 自訂 A 比例 | 接收 payload bytes |
| --- | ---: | ---: | ---: | ---: | ---: |
| `missing-policy` | 0 | 0 | 8,861 | — | 314,586,248 |
| `wrr-70-30` | 15,510 | 6,647 | 0 | 70.00045% | 1,439,956,992 |
| `wrr-20-80` | 4,499 | 17,996 | 0 | 20.00000% | 1,432,879,136 |
| `rtt-A-fast` | 20,031 | 0 | 0 | 100.00000% | 1,315,635,200 |
| `rtt-B-fast` | 0 | 1,216 | 0 | 0.00000% | 15,904,128 |
| `deleted-policy-fallback` | 0 | 0 | 8,971 | — | 284,195,760 |
| `leg-A-down` | 0 | 4,394 | 0 | 0.00000% | 30,355,824 |
| `link-restored` | 16,331 | 6,999 | 0 | 70.00000% | 1,517,969,664 |

WRR 從 generation 1 的 70/30 更新到 generation 2 的 20/80，沒有重載 BPF 或重建連線。generation 3 改成 lowest-rtt；反轉 5ms/40ms netem 後，量測區間內自訂選路由全 A 轉成全 B。刪除 policy 後觀察到真實 default fallback；generation 4 再套用 WRR，才執行斷線與恢復測試。

這些數字是各量測區間的增量。介面重設與切路後有收斂等待，並非即時切換延遲保證；WRR 比例指排程決策，不承諾任意環境的吞吐比例。QEMU/veth 結果是功能驗證，不是實體 3GPP 或 free5GC 效能評測。

測試結束後，實際讀回 `policy=[]`、`paths=[]`、`stats=[]`，guest 正常關機並輸出 `DUALSTEER_GUEST_PASS` 與 `DUALSTEER_GUEST_EXIT=0`。

## 修正了實際載入才會發現的問題

- 避免 LLVM 將候選位置計算最佳化成 verifier 不允許的 pointer OR。
- 使用固定 Leg A/B 索引，避免舊 verifier 在 helper 呼叫後遺失動態索引界線。
- 將每次呼叫的候選 scratch 放入 socket storage，避免八條 subflow 的 verifier 狀態組合超過上限；保留八條 subflow 支援。
- 保留原始 token key 供 release 清理 stats；該 kernel 在 scheduler release 前可能已清除 socket token。
- 測試 kernel 啟用 `CONFIG_FILE_LOCKING`，支援 agent 的實際 flock 序列化。

## 重現與原始證據

```sh
make test agent bpf
make kernel-map-test
make qemu-kernel
make qemu-test
```

套件與環境需求見 [data-plane 操作文件](dualsteer-data-plane.md)。此次 guest 驗證由 `scripts/qemu-test.sh` 執行。

- [完整流量報告](../build/runtime-bpf/report.json)
- [Agent apply/status/delete 紀錄](../build/runtime-bpf/agent.log)
- [PM events](../build/runtime-bpf/pm-events.log)
- [Guest kernel 設定](../build/guest-results/kernel.config)
- [註冊的 struct_ops](../build/guest-results/struct-ops.json)
- [清理後 map 內容](../build/guest-results/maps-after-cleanup.json)
- [建置 manifest](../build/guest-results/manifest.json)
- [Guest console](../build/qemu/console.log)
- [主機 default scheduler 基準](../build/runtime-host/report.json)

`build/` 證據是本次本地產物，`make clean` 會移除，可用上列指令重新產生。

已驗證 kernel source commit：`48826ae77cc9a10907e990f1077caedc90ea4cd3`。

已驗證產物 SHA-256：

```text
BPF object  b37aaeb49b4ce0b67324dcd92e7c42d1231a1e8036a44c9f120d6a0849ea2115
Go agent    b7bd41f69fda694fb0232b5da39596cf086ba2750375c5de930d8ffa2e4c8d4c
```

以上為 PR1 的驗證範圍，當時由呼叫端供應 connection／endpoint binding。後續 PR2 已完成自動 PM event watcher 與 free5GC v4.2.3 PCF／SMF 研究介面串接，見 [控制面串接與驗收](controlplane-integration.md)。實體雙 3GPP access、正常 PDU session 整合與正式控制面程序仍未完成；最新狀態見 [進度總覽](progress-and-next-steps.md)。
