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

## 实现草案（未排期）

1. 钓获入护（14/5）或结算（14/4）时按几率掉卡：插一条 `card_<species>` 物品（wire `dapkdcghfmg`，f0 内嵌本次 25 鱼快照）进背包/储藏，走现有库存同步。
2. 掉率为本地玩法数值，做成 tunable；可按物种稀有度分层（普通/稀有/超稀有递增）。
3. 卡组收集判定与奖座发放是服务端职责，需读玩家卡库存对照 `cjmjfnheidh` 清单。

## 待确认点

- 官方掉率不公开，社区资料也缺；分层几率是自定数值。
- 结算画面如何把卡归到"本次钓获"（成绩条目物品数组的填充来源是深状态链 `llgjfoldbcc`/`emifcdomieb`，未追到底）；实现时需实测确认"结算时背包里恰好有新卡"是否足够触发弹出。
