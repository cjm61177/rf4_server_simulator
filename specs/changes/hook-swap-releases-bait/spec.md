# 换钩必退饵

Status: done

<!-- 状态取值：draft | ready | in-progress | done | cancelled
     完成时原地改这里，不要归档、不要搬家、不要另开第二份。 -->

## Problem Statement

手杆玩家在钓组弹窗里换钩后，挂在旧钩上的饵仍留在竿上并被自动转挂到新钩
（判鱼链路 `baitHookAssignments` 把失效关联重绑到唯一的新钩）。玩家视角是
「换了钩，饵跟着跑」：与官服语义不符，且新钩与旧饵尺寸可能不匹配
（竿 def 自带允许钩号白名单，饵也带尺寸档，客户端在白名单数据齐全时会
把超范围钩灰掉；本地服务端从未下发该数组，客户端什么都让装）。
同族问题在 2026-11-07 钓具链分析里已有先例（H2 断钩不清饵 → 「换钩后永远没口」）。

## Solution

钩离开钓组的每条路径上，挂在它上面的鱼饵（品类 27，不含拟饵族）一并退回：
拆分子实例整条删除并把份数退回来源堆，普通饵退回背包；同时清掉该饵的
`bait_hook` 关联与 `rebait:<槽位>` 待补挂记录，防止抛竿自动补挂把饵又挂回去。
800 快照照常携带退回素材（f1 更新段 / f2 移除段 + 回包后来源堆强制刷新），
客户端无需任何改动即可看到饵回到背包。

## User Stories

1. 作为手杆玩家，我在钓组里把钩 A 换成钩 B，希望 A 上的饵自动回到背包，以便按新钩尺寸重新选饵。
2. 作为手杆玩家，我卸下钩（17/3）时，钩上的饵同样回背包，不会残留一个无钩的饵槽。
3. 边界：单钩双饵组合两个饵都退；拆分挂载的饵按份数退回来源堆（蛆×15 退 15）；
   全拆（17/4）行为不变（饵本来就作为独立组件逐件退回）；断钩/断线清饵已有
   独立链路（gear_damage），不受本改动影响；拟饵钩槽（lure_hooks）语义不动。

## Implementation Decisions

- 入口：`internal/gameserver/rigops.go`。
  - 新增 `releaseBaitsForHook(parentGUID, hookGUID)`：按 `rod_setting`
    `bait_hook:<饵>` → 钩 GUID 的实例关联（不按数组下标猜配对）找饵，校验
    `container='rig'` 且 `rig_guid` 指向本竿、组件品类 = catBait（拟饵跳过），
    然后复用既有退回语义（拆分子实例删除 + 来源堆加份；普通物品退背包），
    从 f5 组件表与 rig_component 派生表同时摘除，最后删
    `bait_hook:<饵>` 与 `rebait:<槽位>` 两行 rod_setting。
  - 17/2 替换路径：`rigSetComponent` 替换钩组件（slot==catHook 且旧组件非空）
    后调用上述助手，退回素材并入 `res.Released` / `RemovedGUIDs` /
    `UpdatedTrees`（快照与来源堆刷新由既有 `queueReleasedStackRefresh` 链路承接）。
  - 17/3 拆卸路径：`rigDetachAt` 拆钩（slot==catHook）时调用助手，素材挂进
    `rigDetachResult` 新增的 Released 字段；`trees()` / `removedGUIDs()` /
    `queueSourceStackRefresh` 三个折叠加入该字段，17/3 与 17/4 快照自动携带。
  - 17/4 全拆：迭代前预清本竿 `bait_hook:%` 关联（饵随后作为独立组件逐件退回），
    避免钩迭代触发退饵与饵自身迭代重复处理同一实例。
- 命令/字段：无新增命令、无响应形状变化——素材全部走既有 800 快照三段
  （f0 新增 / f1 更新 / f2 移除）与 4/21+4/22 来源堆刷新。
- 数据与迁移：无表结构变更。旧档中已存在的 `bait_hook` 关联天然兼容；
  没有关联行的旧稀疏饵不按数组下标补猜（与 `rememberMountedBaitHook` 注释同口径）。
- 兼容：客户端基线 4.0.25034 不变。回滚 = revert 单个提交，无数据迁移残留
  （多删的关联行会在下次装配/判鱼链路重建或保持缺失，缺失时行为回到改动前）。

## Testing Decisions

- 测试层：行为层（真实 SQLite，`newTestServer` 既有接缝），文件
  `internal/gameserver/hook_swap_bait_release_test.go`。
- 期望值来源：现有装配/拆卸测试的断言模式（退背包 = container 变更 + 树进 f1；
  拆分子实例 = 行删除 + 来源堆 qty 增加 + GUID 进 f2），不新造口径。
- 覆盖用例：
  1. 单钩竿换钩（17/2 路径直调 `rigSetComponent`）：旧钩退包、饵退回
     （拆分子实例 → 来源堆 +n、f5/rig_component 无饵行、bait_hook 与 rebait 行删除）。
  2. 非计件饵退背包（Returned 分支）。
  3. 17/3 卸钩：饵随钩退回，快照素材包含饵。
  4. 单钩双饵：两份都退。
  5. 17/4 全拆：不重复退、不残留关联行。
- 回滚后：上述新用例变红（行为回到「饵跟随新钩」）；既有装配/补饵测试不受影响。
  实证（2026-11-12）：回退 rigops.go 后，17/3、17/4 用例因旧 `rigDetachResult`
  无 Released 字段编译失败（红）；17/2 路径用自包含探针验证到断言级失败
  （`换钩后来源饵堆应并回到 3，实得 2`，即旧行为「饵跟随新钩」）。
  邻近套件（TestBaitedHook*/TestBaitConsumption*/TestRigDetach*/TestGearBreak*/
  TestRodBreak*/TestReloadBait*）全部通过；全量套件唯一失败
  TestGiftBoxUseOpensBundle 属预存负载抖动（单跑 -count=5 全过）。

## Out of Scope

- 不下发竿 def 的 `omilihjoenk`（允许钩号数组）——需要 `applyDefAttrs` 数组分支
  与 lpndpbbbejh 枚举核对，属独立改动（换钩选钩过滤），不在本次范围。
- 不做服务端「竿白名单拒钩」校验。
- 不动 17/1 换钓组类型时钩被去重清理的路径（饵在该路径仍会转挂新钩，
  官方语义未验证，留待单独确认）。
- 不动拟饵钩槽（lure_hooks）与断件清饵（gear_damage）链路。

## Further Notes

- `[事实]` 当前行为：`rigops.go` 17/2 替换路径只退旧钩；判鱼链路
  `baitHookAssignments`（bait_consumption.go）在旧钩 GUID 失效时删关联并把单钩
  钓组的饵重绑到 `hooks[0]`（即新钩）。
- `[事实]` 客户端反编译：def wire 类 `gihcahlgpkj` 字段 16 = `omilihjoenk`
  （支持钩号数组）；钓组弹窗门卫 `effjdeblglh.inmhidnlcbg`、钩列表过滤
  `jlddnapbapb.emielammbla` 按竿白名单过滤钩号；客户端存在
  `kItemState_NotSupportedHookSize` / `kItemState_NotSupportedBaitSize` 警示文案。
  静态库中 1043 条竿 def 带该数组，饵/钩不带。
- `[假设]` 官服在换钩时把饵退回背包（用户断言，官方服体验；无抓包样本）。
- 待确认未纳入本次：17/1 换型去重路径是否同样退饵。
