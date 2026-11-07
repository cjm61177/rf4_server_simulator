# 集换式卡牌：钓获掉卡、卡组奖座与管理端调试

Status: done

## Problem Statement

客户端有完整的集换式卡牌功能（结算弹卡、收藏册、卡组奖座、卖卡），静态数据齐备（257 张卡 / 24 个卡组 / 24 个奖座），但服务端从不发放卡——玩家钓一辈子鱼也见不到一张卡，卡牌玩法整体不可用。管理端也没有观察集卡进度、快速凑齐卡组测奖座的手段。

调查依据见 `docs/trading-cards.md`（客户端证据链、wire 类、实现方案）。

## Solution

钓获结算时按物种稀有度分层概率掉落该鱼种的收藏卡（含本次渔获快照，入背包，结算后同步到客户端），掉率默认 0（功能关闭），面板 tunables 可调。集齐某卡组后自动发放对应奖座进房屋。管理端玩家详情新增卡牌小节：查看各卡组进度、一键发整组卡（测奖座）、单张发卡复用现有发物品入口（gameserver 侧对 `card_*` 自动补快照）。

## User Stories

1. 作为玩家，我钓到鱼时有几率收到该鱼种的收藏卡（结算画面弹出、收藏册可见），以便玩收集玩法。
2. 作为玩家，我集齐一个卡组后收到奖座（房屋装饰）。
3. 边界：鱼种没有对应卡 def（`card_<species>` 不存在）时静默跳过，不掉卡也不报错。
4. 边界：同一尾鱼重复结算（回执重放）绝不二次发卡；发过的奖座绝不重复发。
5. 边界：掉卡功能默认关闭（tunable=0），不影响现有结算链路。
6. 作为管理员，我在面板看到玩家各卡组进度（x/y、缺哪些），并能一键发整组卡验证奖座。

## Implementation Decisions

- 掉卡钩子放 `gameserver` 的 `settleCatchOnce` 经济事务内（回执幂等键 `catch-settlement:<uid>:<鱼guid>` 已有，卡 guid 记入回执 JSON，重放时仍可重推库存但不重发）。
- 卡物品构造：复用 `prepareNewItem`（wire 类映射 `78 → dapkdcghfmg/11301` 已在 items.go），f0 `peipahjmida` 嵌 `buildKeepnet25` 鱼快照（同标本板 `embedMountedFish` 手法，鱼 guid 用 uuid5 派生防冲突）。入库 `grantPreparedItemTx(merge=false)`——卡是不可堆叠独立实例。
- 掉率：tunables `card_drop_common` / `card_drop_rare` / `card_drop_super`（默认 0），按 `species_rarity`（`SpeciesRarityTx`，事务内单连接纪律）分层。
- 卡组判定：发卡后检查含该卡的卡组（`ListItemDefsByCategory(82)`，`cjmjfnheidh` 为 item_id 字符串数组），`COUNT(DISTINCT item_id)` 对照清单；集齐发 `statuette_cards_*`（category 72）进 `backpack` 容器，完成度复查与奖座查重在同一个短事务内（幂等）。
- 推送：14/4 响应后 `Push4_22ItemGUIDs([卡guid])`（**不用 Push4_21**——那是移除信号）。
- 管理端：`grant_item` 队列 op 对 `card_` 前缀走发卡路径（合成鱼快照）；新队列 op `grant_deck`；admin 新只读端点 `GET /api/cards?uid=` 与 `POST /api/cards/grant_deck`；面板玩家详情加卡牌小节。
- 无表结构变更、无迁移；tunables 走 kv 表。
- 兼容：不新增/修改任何命令响应形状（复用 4/22 物品同步与现有结算响应）；旧档无卡不受影响；回滚 = tunable 置 0。

## Testing Decisions

- 单元/存储层（gameserver handler 测试，真实 SQLite）：
  - 掉率分层映射（稀有度 0/1/2 → 对应 tunable，缺档回 0）。
  - `prepareNewCard` 产出的卡行 wire_class=`dapkdcghfmg`、FieldsJSON f0 内嵌 25 快照且鱼 guid 与鱼护不同。
  - tunable=1.0 时 14/4 结算发卡：item 表新增行、回执 JSON 含 CardGUID；**同一 call_id 重放 14/4 不双发**（回执路径）。
  - tunable=0 不发卡。
  - 集齐卡组发奖座一次；重复触发不重复发。
  - `grant_item` 发 `card_x` 产出含快照的卡；`grant_deck` 发齐整组并触发奖座。
- 期望值来源：卡/卡组 def 的静态库字面量（card_g_carp #15106 等）；wire 编码用现有契约校验兜底。
- 回滚后：掉卡/奖座测试会红（行为消失），其余不红。

## Out of Scope

- 卖卡/收藏册的专用服务端逻辑（走现有物品卖钱通道，未实测，留待实机验证）。
- 卡面美术资源与资产类型（obifdihmcfn 40/80）的服务端处理。
- 掉率数值的官服对齐（官方不公开，分层值为本地玩法数值）。
- 客户端结算弹卡显示链路的实测确认（813 无物品数组，弹卡依赖客户端自取背包；实现后实机验证）。
