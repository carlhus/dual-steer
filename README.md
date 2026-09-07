# DualSteer generic MPTCP legs

本專案實作論文 MPTCP/eBPF scheduler 的 Leg A／Leg B 泛化：runtime policy、WRR、SRTT 與 Go agent 的實際 map 寫入。原本的 `bpf_rr_quota`／`bpf_rtt` 保留在來源專案，本專案新增 `bpf_ds_wrr`／`bpf_ds_rtt`。

free5GC 後續整合版本固定為 **v4.2.3**，commit 見 [baseline.json](baseline.json)。本階段不包含 NAS、PCF、SMF、N3IWF 或完整 DualSteer signalling。

請先閱讀 [建置、ABI 與實驗說明](docs/dualsteer-data-plane.md)。BPF 需要論文自訂 MPTCP kernel 與附帶 bridge patch；一般 Ubuntu kernel 不足以載入。Go agent 支援 `validate`、`apply --dry-run`、實際 `apply/status/delete`，以 explicit map ID 或 bpffs pinned path 操作。

[實際驗證紀錄](docs/runtime-validation.md)：已在 QEMU 啟動修改後的 kernel，通過 verifier、WRR 熱更新、RTT 切路、斷路恢復及 map 清理測試。

```sh
make test
make agent
cd dualsteer-agent
go run ./cmd/dualsteer-agent --config config.example.yaml validate
```

核心程式依 GPL-2.0 授權，沿用來源程式的 SPDX 與作者資訊。附件研究報告是設計參考，本文的實作範圍以使用者指定的第一個 PR 為準。
