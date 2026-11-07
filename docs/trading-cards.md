# 集换式卡牌（Trading Card）系统调查

本文记录 2026-11-07 对官方客户端集换式卡牌功能的逆向调查结论与模拟器现状。**尚未实现**——本文是目前唯一记录，用于后续实现时免于重新排查。

## 玩法闭环

钓到鱼有几率额外掉落该鱼种的收藏卡，结算画面弹出（专属音效）。卡收进收藏册（Explorer 面板），集齐卡组奖励奖座（房屋装饰），多余的卡可以卖、已收藏的卡卖出有确认弹窗。

## 数据结构（静态库已齐）

| 项 | item_def | 数量 | 关键属性 |
|---|---|---|---|
| 卡 | category 78，`card_<species>` | 257 | `okppoalfakf`=鱼种 key；`obcephkkfhi`=价值 5000~80000（越稀有越贵）；`obifdihmcfn`=资产类型 40/80（TradingCardAsset.GameAssetType==40） |
| 卡组 | category 82，`tcdeck_*` | 24（共 326 卡位） | `cjmjfnheidh`=卡 id 数组；`lbdaoenhcbe`=完成分；`pnaapkplkfo`=奖励奖座 `statuette_cards_*` |

卡面显示的不是物种模板，而是**那次渔获的快照**——卡牌物品内嵌鱼条目 DTO，与标本板嵌鱼共用字段名 `peipahjmida`。

## 客户端证据链

- 结算音效：`GameResultSoundsProfile.cardTradingCardReceived`（ActionSounds 425 播放）。
- 结算渲染：成绩条目 `blehdkolhfl` 携带物品数组（`onobdmohahp[]`），按运行时类型筛 `ipdpoaiekem`（卡牌），命中则入 stack（`RUIPlayerProgressStackItemTradingCard`）与 briefs（`RUIPlayerProgressBriefsTableTradingCardCell`）。客户端纯被动渲染。
- 收藏册 UI：`RUITradingCardDecksExplorerPanel` 族（Explorer 目录下 6 个类）；卡组物品 `ckfmndabaei`（DTO `imdcjllgdcn`）。
- 房屋：HouseTV/HouseTV_V2 引用卡牌展示。
- 卖卡：本地化键 `kMessage_Confirmation_TradingCardSell`、`kDlg_TradingCards_Sell_CollectedCard_Error`（已收藏卡拦截）。

## wire 类

- 卡牌物品：`dapkdcghfmg`（DTO `ipdpoaiekem`，type_id **11301**）——标准库存物 14 字段（guid/父容器/storage/定义下标/工艺/完好率/创建/到期/状态位/时效/贴纸/数量/收藏位）+ f0 `peipahjmida` 鱼快照。schema 已在 `internal/wiregen/tables.go`。
- 类目映射已登记：`internal/gameserver/items.go:121` `78: {"dapkdcghfmg", 11301}`。
- 卡组物品：`ckfmndabaei`（DTO `imdcjllgdcn`），同标准库存物形状。

## 模拟器现状

数据、wire 编码能力全部现成；**服务端零发放逻辑**——全仓无 `card_` 引用，钓获结算不产卡。客户端侧（结算弹出、收藏册、卖卡确认）预期随库存同步自动工作，未实测。

## 实现方案（2026-11-07 基于 local-live `2e65a3d` 代码调查）

### 落点选择：结算事务（settleCatchOnce），不选 14/5 入护

掉卡钩子放 `settleCatchOnce`（`internal/gameserver/catch_settlement.go:110`）的经济事务内，不放 14/5 入护：

- 结算是每尾鱼**恰好一次**的路径（回执幂等：kv 键 `catch-settlement:<uid>:<鱼guid>`，tx 内 `UPDATE kv SET v=v` 抢写锁，重放走 `decodeReceipt` 返回旧回执），天然防同一尾鱼双掉卡；14/5 入护/放生各走一次，账不好记。
- 事务内已有本次渔获的完整上下文（`*RolledFish`：Species/Grade/WeightG）和 `tx` 连接——稀有度查询用 `store.SpeciesRarityTx(ctx, tx, f.Species)`（`repos_species_rarity.go:41`，单连接池纪律：事务内禁止池化查询）。

### 发卡步骤（全部在 settleCatchOnce 事务内）

1. **判定掉率**：`rand.Float64()` 对照 tunable 分层费率——
   `card_drop_common` / `card_drop_rare` / `card_drop_super`（`s.tunableF(key, 0)`，`fishing.go:436`；默认 0 = 功能关闭，与 `fish_tv_kv` 同款开关习惯，admin 面板 kv 可调）。稀有度 0/1/2 → 三档。全档未命中则跳过后续。
2. **构造卡物品**：仿 `prepareNewItem`（`items.go:1160`）——`GetItemDefByItemID(ctx, "card_"+f.Species)` 取 def；类目映射 `78 → {"dapkdcghfmg", 11301}` 已在 `items.go:121`，`resolveWireClass` 自动选中；`applyDefAttrs` 自动铺 def 属性（价值/资产类型）。卡牌是**独立实例**：78 不在 `countableCategories`（`items.go:138`），合并发放必须 `merge=false`。
3. **嵌鱼快照（f0 peipahjmida）**：卡 wire 类字段表 f0=VAL（`tables.go:445`：`{"VAL","GUID16","OPTGUID",...}`）。复用 `s.buildPending25(f, st, level)`（`econ.go:229`）构造与结算卡同一棵 25 鱼树，然后按标本板先例 `embedMountedFish`（`craft_ops.go:1173`）同款写法：
   `fields[0] = proto.Field{Op: "VAL", Val: tree}` → `FieldsToJSON` 写回 `it.FieldsJSON`。
   树内鱼 guid 用 `uuid5OIDHex(cardGUID + "/card_fish")` 派生，避免与鱼护内原鱼同 guid（同 `mountedFishTreeTx` 的做法）。建议新 helper `prepareNewCardTx(ctx, tx, uid, f)` 承载 2~3 步（需要 tx 查稀有度）。
4. **入库**：`grantPreparedItemTx(ctx, tx, card, false)`（`economy_tx.go:282`）入 `backpack`。
5. **回执扩展**：`catchReceipt` 加 `CardGUID string \`json:",omitempty"\``（`catch_settlement.go:22`）随回执 JSON 落 kv——重放结算时 `result(true)` 带出卡 guid，14/4 handler 仍能重推库存同步而不会二次发卡。
6. **库存推送**：14/4 响应（801）发出后，`fishingEndSettle` 尾部（`progress_ops.go:404` 起的补饵推送同段）调
   `ctx.Sess.Push4_22ItemGUIDs([]string{cardGUID})`（`profilebuild.go:450`，只推指定物品树）。
   **不要用 Push4_21**——那是移除信号（`profilebuild.go:408`）。已知卡牌 wire 编码可走 `ItemToTree` 通用路径，无需专门适配。

### 卡组判定与奖座（第二期）

- 清单来源：`ListItemDefsByCategory(ctx, 82)`（`repos2.go:465`），AttrsJSON 解析数组属性 `cjmjfnheidh`（`defAttrs` 同款 json.Unmarshal，`items.go:951`）得到卡 def 清单。
- 判定时机：发卡事务内追加检查（只在掉卡时触发，成本可忽略）——按所属卡组查
  `SELECT COUNT(DISTINCT item_id) FROM item WHERE uid=? AND item_id IN (…)` 对照清单长度。
- 奖座发放：集齐 → `prepareNewItem` + `grantPreparedItemTx` 发 `statuette_cards_*` 进 `house` 容器（`misc_fix.go:805` 惯例）；发前查重（背包/房屋已有该 statuette 或 kv 记账）防重复发。

### 测试与迁移

- 无 schema 变更、无迁移；tunables 走 kv 表。
- 测试层：gameserver handler 测试（handlerBox invoke 惯例）——tunable 调到 1.0 后跑 14/4，断言：item 表新增 `card_<species>` 行（wire_class=dapkdcghfmg）、FieldsJSON f0 内嵌 25 树、kv 回执含 CardGUID；**用同一 call_id 重放 14/4 断言不双发卡**；tunable=0 断言不掉卡。

## 待确认点

- 官方掉率不公开，社区资料也缺；分层几率是自定数值。
- 结算画面如何把卡归到"本次钓获"（成绩条目物品数组的填充来源是深状态链 `llgjfoldbcc`/`emifcdomieb`，未追到底）。已核实 813 结算对象 `ndiejnnioni` 只有 f0~f9 十个字段、无物品数组（`tables.go:689`）——弹卡显示走的是客户端从背包状态自取，实现后需实测确认"结算响应后推 4/22 新卡"是否足够触发弹出与音效。
