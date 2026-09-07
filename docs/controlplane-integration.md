# PCF → SMF → agent 研究介面（PR2）

本 PR 在隔離的 free5GC **v4.2.3** PCF／SMF 原始碼中加入 opt-in 研究模式，將 PCF policy 經 SMF 送至 agent，再由真實 MPTCP PM events 自動綁定連線。原有 `/home/ubuntu/free5gc` 工作目錄不作 reset、checkout 或修改。

## 固定版本與重現

精確 core／PCF／SMF commit 見 [pins.json](../integration/free5gc/pins.json)。隔離目錄為 `build/free5gc-v4.2.3`；版本控制保存各 NF 的 patch，而非忽略目錄中的未追蹤變更。

```sh
# 可使用 GitHub，或 --source /path/to/existing/free5gc 從本機唯讀複製。
python3 scripts/free5gc-research.py prepare
make controlplane-build
make controlplane-test
# 需先按 runtime-validation.md 準備測試 kernel 與 QEMU 相依工具。
make qemu-controlplane-test
```

既有隔離目錄只在 HEAD 與 patch 都符合時沿用；腳本拒絕重置不符合的工作副本。`export` 將隔離 NF 改動匯出為 patch，`verify` 以乾淨的固定 commit 驗證 patch 可套用。

## 資料流

1. PCF 依 DNN 提供 `enabled`、`mode`、`weightA`／`weightB`、`rttDeltaUs`。`mode` 支援 `load-balance`、`lowest-rtt`。
2. SMF context 保存 DNN、Leg A／B 的介面與本機 IP、目的位址與埠。PCF 內容變更時 SMF 自動增加 generation；傳送失敗沿用該 generation 重試，並每 2 秒重送已接受的 assignment。正常停止時會在有限時間內釋放 agent contexts，失敗則回報錯誤。
3. Agent 使用 Unix socket 接收 assignment。它在所在 netns 訂閱 `mptcp_pm_events`，依目的位址／埠與本機 Leg 位址找到連線，再由事件中的 local／remote endpoint IDs 建立 map key。ifindex 僅用於介面驗證。
4. 同一連線的新 generation 透過既有 map transaction 生效。連線 CLOSED 或 context DELETE 會清理 policy／path maps；PM 訊息遺失會停止服務並清理，避免以不完整觀測繼續運作。

API 共用前綴 `/research/dualsteer/v1`：

| 元件 | 方法與路徑 | 內容 |
| --- | --- | --- |
| PCF | `GET/PUT /policies/{dnn}` | 純 policy JSON |
| SMF | `POST /contexts` | `id`、`dnn`、`legs`、`flow` |
| SMF | `GET/DELETE /contexts/{id}` | 查詢狀態／釋放 context |
| Agent | `PUT/GET/DELETE /contexts/{id}` | SMF 自動呼叫；PUT 為扁平 assignment JSON |
| Agent | `GET /status` | 觀測到的 token、endpoint IDs、實際綁定 generation |

PCF 配置範例：

```json
{"listen":"127.0.0.1:18081","policies":{"internet":{"enabled":true,"mode":"load-balance","weightA":70,"weightB":30,"rttDeltaUs":1000}}}
```

SMF 配置範例：

```json
{"listen":"127.0.0.1:18082","pcf_url":"http://127.0.0.1:18081","agent_socket":"/run/dualsteer-agent.sock","poll_ms":200}
```

以實際 NF binary 啟動研究模式：

```sh
build/pcf-research --dualsteer-research --dualsteer-config /path/to/pcf.json
build/smf-research --dualsteer-research --dualsteer-config /path/to/smf.json
```

Agent 必須在 MPTCP sender 所在 netns、建立連線之前啟動。BPF object 需先載入，啟動參數指定其 maps；以下 map IDs 由載入程式查詢，不是 token：

```sh
ip netns exec ds-ue build/dualsteer-agent serve \
  --listen unix:/run/dualsteer-agent.sock \
  --policy-map id:POLICY_MAP_ID --path-map id:PATH_MAP_ID
```

對 SMF 建立 context 的 operator JSON 不含 token、endpoint ID 或 generation：

```json
{"id":"lab","dnn":"internet","legs":{"A":{"ifname":"leg-a","localAddress":"10.60.1.2"},"B":{"ifname":"leg-b","localAddress":"10.60.2.2"}},"flow":{"destinationAddress":"10.60.1.1","destinationPort":5001}}
```

更新 PCF 的 policy 為 `weightA:20, weightB:80` 即觸發自動傳播，無需呼叫 SMF refresh 或操作 maps。SMF 的 `appliedGeneration` 表示 agent 已接受；是否取得兩條 path 並完成 map 綁定，須看 agent binding 的 `ready` 與 `generation`。

## 範圍與生命週期限制

這是編入固定版本 NF binary 的研究分支，未啟用時保留正常啟動路徑。研究模式不啟動 NRF／PFCP／標準 SBI，也不宣稱完成 NAS、PDU session、標準 Npcf policy association 或 DualSteer signalling。Leg 關聯由研究 context 提供，不是從正式 PDU session 自動推導。

PCF／SMF listener 限 loopback，agent Unix socket 權限為本機使用；未提供遠端認證、持久化或高可用性。SMF generation 僅限程序生命週期。Agent 必須先於新連線訂閱事件，不支援重啟後還原既有連線；啟動拒絕目前 netns 的非空 maps，避免默默刪掉既有設定。研究程序重啟須重新建立 context 與連線；異常強制終止後須先處理殘留 maps。目的位址／埠在單一 agent 中只允許一個 context，避免歧義。

本次驗收使用 QEMU 測試核心、兩條 netns links 與單一長連線。權重比較以 scheduler 的 A／B 選路次數為準；TCP 擁塞、封包大小及重傳可能使位元組比例不同。測試操作員只建立 context、更新 PCF policy；讀取 token 與 maps 僅用於比對證據。

## 本次實測結果

2026-09-07 在 `6.6.0-rc2-dualsteer-test` QEMU guest 完成驗收，使用真正的 patched PCF／SMF cmd binaries、netlink subscriber 與 BPF maps。

| 項目 | 結果 |
| --- | --- |
| PCF 70/30 → SMF generation 1 → agent → BPF | A 13,140 次／B 5,631 次，A 比例 70.0016% |
| PCF PUT 20/80 → SMF generation 2 → 同一 binding | A 3,824 次／B 15,299 次，A 比例 19.9969% |
| 連線連續性 | 同一 token `1632774984`、同一 sender，兩階段均維持 MPTCP 雙 subflow |
| 量測期間 scheduler fallback | 兩階段皆 0 |
| 事件自動識別 | A endpoint `(0,0)`、B endpoint `(1,2)`，操作請求不含 token／endpoint IDs |
| PM CLOSED 自動清理 | policy／path／stats maps entries 全部為 0 |
| SMF DELETE | SMF 與 agent 查詢均回 404，程序正常停止 |

完整 [驗收報告](validation/controlplane-report.json) 保留 operator 請求、各段控制面／map／MPTCP 觀測；[artifact manifest](validation/controlplane-manifest.json) 保存固定版本與測試 binary／patch hashes。BPF object SHA256 為 `b37aaeb49b4ce0b67324dcd92e7c42d1231a1e8036a44c9f120d6a0849ea2115`，與 PR1 相同。

另已通過 shared contract、agent、PCF／SMF 研究套件單元測試與 `go vet`，agent 與 NF 研究套件 `-race`，以及既有 C 選路測試。乾淨隔離副本建立與 patch 套用已實測；原 free5GC、PCF、SMF 的 HEAD、工作目錄狀態及 diff hash 均與修改前一致。未執行完整 free5GC UE 註冊／PDU session 或正式 SBI 互通測試；獨立 privileged Go PM 測試預設跳過，真實 PM 訂閱與自動綁定由上述 guest 驗收覆蓋。
