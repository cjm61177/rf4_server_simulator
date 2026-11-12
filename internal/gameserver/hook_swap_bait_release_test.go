package gameserver

import (
	"context"
	"testing"
)

// 换钩必退饵回归：钩离开钓组（替换 17/2、拆卸 17/3、全拆 17/4）时，挂在它
// 上面的鱼饵必须一并退回——拆分子实例整条删除并把份数并回来源堆，普通饵退回
// 背包；同时清掉 bait_hook 与 rebait:<槽位> 关联，防止抛竿自动补挂把饵再挂回
// 新钩。官方语义：换钩后饵与钩允许尺寸可能不匹配，不允许饵跟着新钩走。

func hookSwapTestRod(t *testing.T, srv *Server) string {
	t.Helper()
	rod, err := srv.CreateItem(context.Background(), 1000001, "tele_10180_4", "backpack", 1)
	if err != nil {
		t.Fatal(err)
	}
	return rod.GUID
}

func mountHookSwapHook(t *testing.T, srv *Server, rod, hookGUID string) *rigSetResult {
	t.Helper()
	// extra 250 = 钩规范首索引：命中同索引替换分支，确定性顶掉旧钩。
	res := srv.rigSetComponent(context.Background(), 1000001, rod, catHook, hookGUID, 250,
		rigSetOpts{ParentType: familyRod, EnforceCategory: true, SplitStack: true})
	if res.Rejected || res.MountedGUID == "" {
		t.Fatalf("装钩失败: %+v", res)
	}
	return res
}

func hookSwapMountBait(t *testing.T, srv *Server, rod, id string, qty int64) (string, string) {
	t.Helper()
	return baitTestMount(t, srv, rod, id, qty, 350)
}

// assertSplitBaitGone 断言拆分子饵已整删且来源堆并回 sourceQty 份。
func assertSplitBaitGone(t *testing.T, srv *Server, sourceGUID, child string, sourceQty int64) {
	t.Helper()
	ctx := context.Background()
	if row, _ := srv.DB.GetItemByGUID(ctx, child); row != nil {
		t.Fatalf("挂载子饵应整条删除，实得 %+v", row)
	}
	if got := stackQtyForTest(t, srv, sourceGUID); got != sourceQty {
		t.Fatalf("来源饵堆应并回到 %d，实得 %d", sourceQty, got)
	}
}

// TestHookSwapReleasesMountedBait 用例 1：单钩竿换钩，拆分子饵整删并回来源堆，
// 关联与补饵记录同时清掉，新钩正常上组。
func TestHookSwapReleasesMountedBait(t *testing.T) {
	t.Parallel()
	srv, _ := newRigTestServer(t)
	ctx := context.Background()
	rod := hookSwapTestRod(t, srv)

	hookA, err := srv.CreateItem(ctx, 1000001, "clas_20163", "backpack", 1)
	if err != nil {
		t.Fatal(err)
	}
	hookB, err := srv.CreateItem(ctx, 1000001, "clas_20163", "backpack", 1)
	if err != nil {
		t.Fatal(err)
	}
	mountHookSwapHook(t, srv, rod, hookA.GUID)
	source, child := hookSwapMountBait(t, srv, rod, "worm_01", 3)

	hooks := rigSlotGUIDs(t, srv, rod)
	if hooks[catHook] != hookA.GUID || hooks[catBait] == "" {
		t.Fatalf("前置状态不符: hooks=%+v", hooks)
	}
	var mapped string
	if err := srv.DB.SQL.QueryRow(
		`SELECT value FROM rod_setting WHERE guid=? AND key=?`, rod, "bait_hook:"+child).Scan(&mapped); err != nil || mapped != hookA.GUID {
		t.Fatalf("前置：饵-钩关联应指向旧钩 %s: %v", hookA.GUID, err)
	}

	res := mountHookSwapHook(t, srv, rod, hookB.GUID)

	comps := rigSlotGUIDs(t, srv, rod)
	if comps[catHook] != hookB.GUID {
		t.Fatalf("新钩应上组，实得 %+v", comps)
	}
	if comps[catBait] != "" {
		t.Fatalf("饵位应清空，实得 %q", comps[catBait])
	}
	if row, _ := srv.DB.GetItemByGUID(ctx, hookA.GUID); row == nil || row.Container != "backpack" {
		t.Fatalf("旧钩应退回背包，实得 %+v", row)
	}
	assertSplitBaitGone(t, srv, source, child, 3)
	if n := rodSettingCount(t, srv, rod, "bait_hook:%"); n != 0 {
		t.Fatalf("换钩后不应残留饵-钩关联，实得 %d 条", n)
	}
	if n := rodSettingCount(t, srv, rod, "rebait:%"); n != 0 {
		t.Fatalf("换钩后不应残留补饵记录，实得 %d 条", n)
	}
	if len(res.Released) != 1 || res.Released[0].GUID != child || res.Released[0].Returned {
		t.Fatalf("Released 应包含整删子饵 %s: %+v", child, res.Released)
	}
	if res.Released[0].SourceGUID != source || res.Released[0].SourceTree == nil {
		t.Fatalf("Released 应携带来源堆刷新素材: %+v", res.Released[0])
	}
	containsChild := false
	for _, guid := range res.RemovedGUIDs {
		if guid == child {
			containsChild = true
		}
	}
	if !containsChild {
		t.Fatalf("RemovedGUIDs 应包含子饵 %s: %+v", child, res.RemovedGUIDs)
	}
}

// TestHookSwapReturnsNonSplitBait 用例 2：单份挂载（无拆分）的饵换钩时退回背包，
// GUID 不变，退回树进 f1 更新段。
func TestHookSwapReturnsNonSplitBait(t *testing.T) {
	t.Parallel()
	srv, _ := newRigTestServer(t)
	ctx := context.Background()
	rod := hookSwapTestRod(t, srv)

	hookA, err := srv.CreateItem(ctx, 1000001, "clas_20163", "backpack", 1)
	if err != nil {
		t.Fatal(err)
	}
	hookB, err := srv.CreateItem(ctx, 1000001, "clas_20163", "backpack", 1)
	if err != nil {
		t.Fatal(err)
	}
	mountHookSwapHook(t, srv, rod, hookA.GUID)
	source, mounted := hookSwapMountBait(t, srv, rod, "worm_01", 1)
	if mounted != source {
		t.Fatalf("前置：单份挂载不应拆分子实例，实得 %s", mounted)
	}

	res := mountHookSwapHook(t, srv, rod, hookB.GUID)

	if row, _ := srv.DB.GetItemByGUID(ctx, source); row == nil || row.Container != "backpack" {
		t.Fatalf("单份饵应原样退回背包，实得 %+v", row)
	}
	if len(res.Released) != 1 || res.Released[0].GUID != source || !res.Released[0].Returned {
		t.Fatalf("Released 应为退包分支: %+v", res.Released)
	}
	if res.Released[0].ReturnTree == nil {
		t.Fatal("退包分支应携带新树")
	}
	found := false
	for _, tree := range res.UpdatedTrees {
		if tree == res.Released[0].ReturnTree {
			found = true
		}
	}
	if !found {
		t.Fatal("退包树应并入 f1 更新段素材")
	}
	if comps := rigSlotGUIDs(t, srv, rod); comps[catBait] != "" {
		t.Fatalf("饵位应清空，实得 %q", comps[catBait])
	}
}

// TestHookDetachReleasesBait 用例 3：17/3 卸钩，饵随钩退回，快照素材包含饵。
func TestHookDetachReleasesBait(t *testing.T) {
	t.Parallel()
	srv, _ := newRigTestServer(t)
	ctx := context.Background()
	rod := hookSwapTestRod(t, srv)

	hook, err := srv.CreateItem(ctx, 1000001, "clas_20163", "backpack", 1)
	if err != nil {
		t.Fatal(err)
	}
	mountHookSwapHook(t, srv, rod, hook.GUID)
	source, child := hookSwapMountBait(t, srv, rod, "worm_01", 3)

	res := srv.rigRemoveComponentInstance(ctx, 1000001, rod, catHook, hook.GUID, 0)
	if res == nil || res.CompGUID != hook.GUID {
		t.Fatalf("卸钩结果异常: %+v", res)
	}
	if row, _ := srv.DB.GetItemByGUID(ctx, hook.GUID); row == nil || row.Container != "backpack" {
		t.Fatalf("钩应退回背包，实得 %+v", row)
	}
	assertSplitBaitGone(t, srv, source, child, 3)
	if n := rodSettingCount(t, srv, rod, "bait_hook:%"); n != 0 {
		t.Fatalf("卸钩后不应残留饵-钩关联，实得 %d 条", n)
	}
	if len(res.Released) != 1 || res.Released[0].GUID != child || res.Released[0].Returned {
		t.Fatalf("Released 应包含整删子饵: %+v", res.Released)
	}
	removed := res.removedGUIDs()
	if len(removed) != 1 || removed[0] != child {
		t.Fatalf("f2 素材应只含子饵 %s: %+v", child, removed)
	}
	found := false
	for _, tree := range res.trees() {
		if tree == res.Released[0].SourceTree {
			found = true
		}
	}
	if !found || res.Released[0].SourceTree == nil {
		t.Fatal("f1 素材应包含来源堆新树")
	}
}

// TestHookSwapReleasesAllBaitsOnHook 用例 4：单钩双饵（350/351 两个饵位），
// 换钩时两个饵都退回，各并回各自来源堆。
func TestHookSwapReleasesAllBaitsOnHook(t *testing.T) {
	t.Parallel()
	srv, _ := newRigTestServer(t)
	ctx := context.Background()
	rod, err := srv.CreateItem(ctx, 1000001, "feeder_10126_FD390", "backpack", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !srv.rigSetType(ctx, 1000001, rod.GUID, 12) {
		t.Fatal("rigSetType 失败")
	}
	hookA, err := srv.CreateItem(ctx, 1000001, "c_hook_20712", "backpack", 1)
	if err != nil {
		t.Fatal(err)
	}
	hookB, err := srv.CreateItem(ctx, 1000001, "c_hook_20712", "backpack", 1)
	if err != nil {
		t.Fatal(err)
	}
	if res := srv.rigSetComponent(ctx, 1000001, rod.GUID, catHook, hookA.GUID, 250,
		rigSetOpts{ParentType: familyRod, EnforceCategory: true}); res.Rejected || res.MountedGUID == "" {
		t.Fatalf("装钩失败: %+v", res)
	}
	srcA, childA := baitTestMount(t, srv, rod.GUID, "worm_01", 3, 350)
	srcB, childB := baitTestMount(t, srv, rod.GUID, "muckworm", 3, 351)

	res := srv.rigSetComponent(ctx, 1000001, rod.GUID, catHook, hookB.GUID, 250,
		rigSetOpts{ParentType: familyRod, EnforceCategory: true, SplitStack: true})
	if res.Rejected || res.MountedGUID == "" {
		t.Fatalf("换钩失败: %+v", res)
	}

	releasedByGUID := map[string]*rigReleased{}
	for _, rel := range res.Released {
		releasedByGUID[rel.GUID] = rel
	}
	for _, pair := range []struct{ source, child string }{{srcA, childA}, {srcB, childB}} {
		rel := releasedByGUID[pair.child]
		if rel == nil || rel.Returned {
			t.Fatalf("子饵 %s 应整删退回: %+v", pair.child, res.Released)
		}
		if rel.SourceGUID != pair.source || rel.SourceTree == nil {
			t.Fatalf("子饵 %s 来源堆素材缺失: %+v", pair.child, rel)
		}
		if row, _ := srv.DB.GetItemByGUID(ctx, pair.child); row != nil {
			t.Fatalf("子饵 %s 应删除", pair.child)
		}
		if got := stackQtyForTest(t, srv, pair.source); got != 3 {
			t.Fatalf("来源堆应并回到 3，实得 %d", got)
		}
	}
	if n := rodSettingCount(t, srv, rod.GUID, "bait_hook:%"); n != 0 {
		t.Fatalf("换钩后不应残留饵-钩关联，实得 %d 条", n)
	}
	if comps := rigSlotGUIDs(t, srv, rod.GUID); comps[catBait] != "" || comps[catHook] != hookB.GUID {
		t.Fatalf("饵位应清空且新钩上组: %+v", comps)
	}
}

// TestRigRemoveAllClearsBaitHookWithoutDoubleRelease 用例 5：17/4 全拆时饵作为
// 独立组件逐件退回，钩的拆卸不得再触发退饵（同一实例不能处理两次）。
func TestRigRemoveAllClearsBaitHookWithoutDoubleRelease(t *testing.T) {
	t.Parallel()
	srv, _ := newRigTestServer(t)
	ctx := context.Background()
	rod := hookSwapTestRod(t, srv)

	hook, err := srv.CreateItem(ctx, 1000001, "clas_20163", "backpack", 1)
	if err != nil {
		t.Fatal(err)
	}
	mountHookSwapHook(t, srv, rod, hook.GUID)
	source, child := hookSwapMountBait(t, srv, rod, "worm_01", 3)

	results := srv.rigRemoveAll(ctx, 1000001, rod)

	childSeen := 0
	for _, res := range results {
		for _, guid := range res.removedGUIDs() {
			if guid == child {
				childSeen++
			}
		}
		for _, rel := range res.Released {
			if rel.GUID == child {
				childSeen++
			}
		}
	}
	if childSeen != 1 {
		t.Fatalf("子饵应恰好退回一次，实得 %d 次: %+v", childSeen, results)
	}
	if row, _ := srv.DB.GetItemByGUID(ctx, child); row != nil {
		t.Fatal("子饵应删除")
	}
	if got := stackQtyForTest(t, srv, source); got != 3 {
		t.Fatalf("来源堆应并回到 3（不能重复退），实得 %d", got)
	}
	if n := rodSettingCount(t, srv, rod, "bait_hook:%"); n != 0 {
		t.Fatalf("全拆后不应残留饵-钩关联，实得 %d 条", n)
	}
	if n := rodSettingCount(t, srv, rod, "rebait:%"); n != 0 {
		t.Fatalf("全拆后不应残留补饵记录，实得 %d 条", n)
	}
	if rows, _ := srv.DB.ListRigComponents(ctx, rod); len(rows) != 0 {
		t.Fatalf("全拆后组件应清空，实得 %+v", rows)
	}
}
