# PCF／SMF 研究政策串接與 MPTCP agent 自動綁定

原先政策熱更新需要人工提供 MPTCP token／endpoint IDs 並操作 agent。此 PR 固定隔離 free5GC v4.2.3 PCF／SMF，新增 opt-in 研究模式：PCF 提供模式及權重，SMF 保存 Leg A／B 關聯、generation 與 context 生命週期，agent 透過 Unix HTTP 接受 assignment，從 kernel PM events 自動綁定與清理。

原有 dirty free5GC 目錄保持不變。NF patch、版本固定檔、隔離建立腳本與實際 QEMU 驗收報告均納入版本控制。這次研究介面尚未接入正式 SBI／PDU session 流程。

驗證：實際 kernel 中同一 MPTCP 連線由 PCF 70/30 自動切換至 20/80，A 選路量測為 70.0016% → 19.9969%，全程不輸入 token 或手動更新 maps。PM close 清空 maps，SMF DELETE 移除兩端 context，程序正常停止。單元測試、race tests、vet、既有 C tests 與乾淨 patch 套用通過。細節見 [控制面串接文件](controlplane-integration.md)。
