# DualSteer 目前進度與下一步

整理日期：2026-09-16。最近一次完整功能驗收：2026-09-07。

## 目前結論

**已完成兩條純 IP path 上的 MPTCP/eBPF steering，以及 PCF → SMF → agent 的研究用政策串接。** 在實際測試 kernel 中，PCF 將權重由 70/30 更新成 20/80，SMF 自動更新 generation，agent 從 PM events 取得 token／endpoint IDs 並更新 maps，同一條 MPTCP 連線維持運作。

**尚未完成兩條真正 3GPP PDU legs、正式 session 生命週期整合或 Rel-19 NAS/SBI 對齊。** 目前結果足以證明研究控制鏈可驅動資料面，不能視為完整 5GC DualSteer 已完成。

## 已完成的工作

| 範圍 | 完成內容 | 驗證邊界 |
| --- | --- | --- |
| PR1：通用資料面 | Leg A／B 抽象、WRR、lowest-rtt、generation 熱更新、default fallback | 實際自訂 kernel，QEMU／veth 雙路 |
| PR1：agent map 操作 | 真正的 apply／status／delete、policy／path maps 更新與清理 | 主機 kernel map 測試與 guest 流量測試 |
| PR1：故障情境 | RTT 優劣反轉、A 斷路後走 B、恢復後重新分流 | 資料面測試，非完整控制面故障矩陣 |
| PR2：隔離 free5GC | 固定 v4.2.3 core／PCF／SMF、獨立工作副本、可重現 patches | 原有 dirty free5GC 目錄在 PR2 驗收時確認未變 |
| PR2：PCF | 依 DNN 提供模式、權重與 RTT delta，支援研究 API 更新 | opt-in 研究模式，未走正式 policy association |
| PR2：SMF | Leg 關聯、generation、polling、重送、context 刪除與正常停止清理 | context 由研究 API 建立，尚未由 PDU session 事件驅動 |
| PR2：agent daemon | 訂閱真實 PM events，自動取得 token／endpoint IDs、綁定與清理 | 需先於連線啟動，尚不支援重啟接管既有連線 |
| PR2：端到端驗收 | PCF 70/30 → SMF → agent → BPF，再自動更新成 20/80 | 同一條 MPTCP 連線，不人工填識別碼或更新 maps |

### 已保存的版本

- Repository：`/home/ubuntu/dual-steer`
- 本機分支：`feat/pcf-smf-agent-policy`
- PR1 commit：`3b94538` — generic DualSteer MPTCP data plane。
- PR2 commit：`9a6fa35` — PCF／SMF 研究政策與 PM-driven agent。
- 隔離 free5GC：`/home/ubuntu/dual-steer/build/free5gc-v4.2.3`。
- 精確 core／NF commits：[pins.json](../integration/free5gc/pins.json)。
- 目前沒有 Git remote；PR1／PR2 指本機實作批次，尚無遠端 PR。

## 驗收證據

### PR1：資料面

WRR 70/30 與 20/80、RTT 反轉切路、刪除 policy 後 fallback、Leg A 斷路／恢復、結束後 maps 清理均已通過。BPF verifier 與 scheduler 註冊也已實際執行。

詳見 [kernel 與雙路驗證紀錄](runtime-validation.md)。環境為 `6.6.0-rc2-dualsteer-test`，不是一般 Ubuntu kernel 即可直接替代；主機 kernel 沒有被替換。

### PR2：PCF 到資料面的完整研究鏈

| 階段 | SMF generation | A 選路次數 | B 選路次數 | A 比例 | 量測期間 fallback |
| --- | ---: | ---: | ---: | ---: | ---: |
| PCF 70/30 | 1 | 13,140 | 5,631 | 70.0016% | 0 |
| PCF 更新 20/80 | 2 | 3,824 | 15,299 | 19.9969% | 0 |

兩階段為同一條 MPTCP 連線，皆有兩條 subflows。PM CLOSED 後 policy／path／stats maps 全部清空；SMF DELETE 後，SMF 與 agent context 查詢均回 404，程序正常停止。

以上比例是 **scheduler 選路次數**，不等於吞吐量或位元組比例，也不是實體雙 3GPP 效能數據。

- [控制面串接說明](controlplane-integration.md)
- [已納入版本控制的驗收報告](validation/controlplane-report.json)
- [版本與產物 hash manifest](validation/controlplane-manifest.json)

## 仍未完成的部分

1. **真實 session 關聯**：SMF 尚未以實際 UE／PDU Session ID、UE IP 與 session 建立／釋放事件維護 DualSteer context；目前由 operator 提供 Leg 介面、位址與 flow selector。
2. **雙 3GPP 路徑**：尚未驗證經 gNB／UPF 的兩條 PDU legs，現有驗收使用 veth 純 IP paths。
3. **正式控制面程序**：研究模式不啟動正常 NRF／PFCP／標準 SBI 流程，尚未整合正式 Npcf policy association、NAS 或 Rel-19 訊息與程序。
4. **共同資料端點與雙向流量**：尚未驗證 UPF／DN 或 MPTCP proxy／anchor 佈局下的完整上、下行行為。
5. **恢復與規模**：context 與 generation 尚未持久化；agent 重啟只支援新連線，異常退出可能留下 maps。同一 agent 的相同目的位址／埠目前只允許一個 context。
6. **完整品質與故障驗證**：資料面已做 RTT／link-down 測試，但尚未完成 PCF → SMF → agent 全鏈的模式切換、重啟恢復、多 context 隔離、抖動／loss／hold-down 狀態機與效能評測。

## 下一步建議：PR3 接入真正的 SMF session 生命週期

**先讓 DualSteer context 由真正的 PDU session 建立、更新與釋放事件驅動，將研究鏈接入正常 free5GC 運作路徑。** 目前政策與資料面已可連動，最主要的缺口是 context 與實際接入／session 之間的關係。

原始研究報告提出 PR3 做 Rel-19 NAS/SBI alignment。依現有程式與驗收邊界，建議先補 session 整合前置條件；Rel-19 對齊保留為後續工作，屆時另行核對適用規範版本與程序。本文件沒有進行新的標準合規研究。

### 建議限定範圍

1. **建立正常 5GC 基線**：延續隔離 v4.2.3 副本，盤點並補齊正常啟動需要的元件、UE／gNB 測試端及配置；先驗證一條普通 PDU session 可建立、傳輸與釋放，保留 logs。
2. **新增 session adapter**：在正常 SMF session 流程中建立與釋放 DualSteer 關聯，保存 correlation ID、UE／session 識別、DNN 與分配的 IP。明確定義如何將兩個 lab sessions 關聯為一組，及如何將 UE 端介面資訊交給 agent；不能假設 SMF 自動知道 Linux ifname。
3. **接回既有政策链**：重用 PCF 政策模型與 agent assignment，先保留明確標示的研究傳遞介面。正常模式需可選擇啟用；未啟用時普通 session 行為保持不變。

### PR3 驗收標準

- 正常 free5GC 流程完成 session 建立與釋放，具備控制面紀錄與實際傳輸證據。
- 符合條件的 session 事件自動建立／更新關聯，不再由 operator 直接呼叫研究 `POST /contexts` 代替 session 建立。
- 分清「一條 leg 就緒」與「兩條 legs 都就緒」，只有符合實際拓撲時才啟用雙路政策；另一條 leg 未就緒時不產生虛假的 ready 狀態。
- session 釋放會撤銷對應 binding；僅剩一條 leg 或全部釋放的行為有明確規則與測試，且不影響不相關 context。
- 既有 PR2 的同一 MPTCP 連線 70/30 → 20/80 驗收仍通過。

**PR3 的單一 session 基線不等於雙 3GPP 驗收完成。** 若要在同一 PR 宣稱真實雙路完成，還必須提供下列下一階段的全部證據，不能以 veth 結果代替。

### 接續順序

| 優先序 | 工作 | 完成條件 |
| --- | --- | --- |
| 1 | 正常 SMF session adapter（上述 PR3） | session 事件驅動關聯與清理 |
| 2 | 兩條真正 3GPP legs + 共同 DN／MPTCP 端點 | 兩條已建立的 PDU paths 承載同一 MPTCP 連線；上、下行可用；PCF 熱更新仍成立 |
| 3 | 全鏈故障與恢復 | RTT 模式、單 leg 釋放、程序重啟、多 context 隔離均有可重現結果 |
| 4 | Rel-19 NAS／SBI 對齊 | 固定規範版本，逐項列出實作映射、差異與程序驗證 |
| 5 | 效能與 CI | 固定實驗矩陣、重複試驗、吞吐／延遲／切換中斷時間及 artifacts |

## 重現入口與本次整理的檢查範圍

在 repository 根目錄執行；kernel 與工具相依需求請先依既有文件準備：

```sh
# 單元測試與既有資料面測試
make test

# 建立／檢查隔離副本並測試 NF 研究套件
python3 scripts/free5gc-research.py prepare
make controlplane-test

# 在已有測試 kernel 的環境中重跑研究鏈驗收
make qemu-controlplane-test
```

`build/` 是本機產物，`make clean` 會移除其中的隔離副本與驗收資料；NF patches 與 PR2 報告另有納入版本控制。

本次整理核對了 Git 狀態、既有文件、固定版本、已保存的 JSON 報告及文件連結；沒有重新執行 Go／C 測試、QEMU 或完整 5GC 驗收。上文「已通過」均指 2026-09-07 保存的實測結果。本文只整理進度與提出下一步，尚未實作 PR3。
