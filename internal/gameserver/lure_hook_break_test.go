package gameserver

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// 拟饵上装的玩家钩有独立 item 行，rig_guid 指向拟饵而非竿（装钩即如此，
// lure_hooks.go）。17/9 上报钩 GUID 断件时按竿单级归属裁决会误判
// “断件不在该钓组中”（2026-11-11 实测连发 828 次、真脱钩）；断件必须沿
// rig_guid 链上溯裁决，销毁钩时按卸钩语义从拟饵 f0 钩槽数组摘除元素——
// 拟饵本体留在钓组上，不得被 detachHookBaitsTx 退回背包。
func TestLureMountedHookBreakAcceptedAndLureStaysMounted(t *testing.T) {
	t.Parallel()
	srv, db := newRigTestServer(t)
	ctx := context.Background()
	const uid = int64(1000001)

	rod, err := srv.CreateItem(ctx, uid, "spin_10158_M100HH", "backpack", 1)
	if err != nil {
		t.Fatal(err)
	}
	reel, err := srv.CreateItem(ctx, uid, "spin_5691", "backpack", 1)
	if err != nil {
		t.Fatal(err)
	}
	line, err := srv.CreateItem(ctx, uid, "braid_40898", "backpack", 1)
	if err != nil {
		t.Fatal(err)
	}
	leader, err := srv.CreateItem(ctx, uid, "leader_crafted_60", "backpack", 1)
	if err != nil {
		t.Fatal(err)
	}
	lure, err := srv.CreateItem(ctx, uid, "spinnerbaits_1244", "backpack", 1)
	if err != nil {
		t.Fatal(err)
	}
	hook, err := srv.CreateItem(ctx, uid, "c_hook_20712", "backpack", 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, itemGUID := range []string{reel.GUID, line.GUID, leader.GUID, lure.GUID} {
		mountGearForTest(t, srv, newProbeSess(srv), rod.GUID, itemGUID)
	}
	res := srv.rigSetComponent(ctx, uid, lure.GUID, catHook, hook.GUID, 250,
		rigSetOpts{ParentType: familyLure, EnforceCategory: true, SplitStack: true})
	if res.Rejected || res.MountedGUID != hook.GUID {
		t.Fatalf("拟饵装钩被拒: %+v", res)
	}
	hookRow := mustItem(t, db, hook.GUID)
	if hookRow.RigGUID != lure.GUID || hookRow.Container != "rig" {
		t.Fatalf("钩应链上挂载在拟饵上: rig_guid=%q container=%q", hookRow.RigGUID, hookRow.Container)
	}
	before := lureTestHooks(t, srv, lure.GUID)
	if len(before) == 0 || before[0] != hook.GUID {
		t.Fatalf("钩应替换拟饵槽 0 出厂钩: %v", before)
	}

	outcome, err := srv.breakLineOrRod(ctx, uid, rod.GUID, hook.GUID)
	if err != nil {
		t.Fatalf("拟饵钩断件处理失败（修复前即“断件不在该钓组中”）: %v", err)
	}
	if outcome == nil || !outcome.ComponentBroken || outcome.BreakPart != "component" || outcome.BrokenGUID != hook.GUID {
		t.Fatalf("应断开钩终端件: %+v", outcome)
	}

	if row, _ := db.GetItemByGUID(ctx, hook.GUID); row != nil {
		t.Fatal("断钩应销毁钩实例")
	}
	// 拟饵本体留在钓组上（detachHookBaitsTx 的竿钩挂饵语义不适用）。
	lureRow := mustItem(t, db, lure.GUID)
	if lureRow.Container != "rig" || lureRow.RigGUID != rod.GUID {
		t.Fatalf("拟饵应继续挂在竿上: container=%q rig_guid=%q", lureRow.Container, lureRow.RigGUID)
	}
	after := lureTestHooks(t, srv, lure.GUID)
	for _, guid := range after {
		if guid == hook.GUID {
			t.Fatalf("断钩仍留在拟饵钩槽数组中: %v", after)
		}
	}
	if len(after) == 0 {
		// 数组清空 → bait_hook 关联删除（与卸钩同款），行不存在即通过。
		var got string
		err := db.SQL.QueryRow(`SELECT value FROM rod_setting WHERE guid=? AND key=?`,
			rod.GUID, "bait_hook:"+lure.GUID).Scan(&got)
		if errors.Is(err, sql.ErrNoRows) {
			// 关联已删除，符合预期。
		} else if err != nil {
			t.Fatal(err)
		} else if got != "" {
			t.Fatalf("钩槽数组清空后应删除 bait_hook 关联，实得 %q", got)
		}
	}
	// 断钩后竿 f5 仍含拟饵组件（投影对齐卸钩路径：竿名下无钩行）。
	comps, err := db.ListRigComponents(ctx, rod.GUID)
	if err != nil {
		t.Fatal(err)
	}
	for _, comp := range comps {
		if comp.GUID == hook.GUID {
			t.Fatal("竿投影不应残留断钩行")
		}
	}
	foundLure := false
	for _, comp := range comps {
		if comp.GUID == lure.GUID {
			foundLure = true
		}
	}
	if !foundLure {
		t.Fatal("拟饵应仍在竿组件投影中")
	}
	sawLureTree := false
	for _, tree := range outcome.UpdatedTrees {
		for _, guid := range deepGUIDs(tree) {
			if guid == lure.GUID {
				sawLureTree = true
			}
		}
	}
	if !sawLureTree {
		t.Fatal("更新树应包含拟饵（客户端要对齐钩槽渲染）")
	}
}

// 链上归属裁决不能放行任意挂载：拟饵在背包里时，其上的钩不属于该竿
// 钓组，断件上报必须拒绝——否则对着任何背包拟饵装钩即可凭空销毁钩。
func TestLureHookOnBackpackLureStillRejected(t *testing.T) {
	t.Parallel()
	srv, db := newRigTestServer(t)
	ctx := context.Background()
	const uid = int64(1000001)

	rod, err := srv.CreateItem(ctx, uid, "spin_10158_M100HH", "backpack", 1)
	if err != nil {
		t.Fatal(err)
	}
	lure, err := srv.CreateItem(ctx, uid, "spinnerbaits_1244", "backpack", 1)
	if err != nil {
		t.Fatal(err)
	}
	hook, err := srv.CreateItem(ctx, uid, "c_hook_20712", "backpack", 1)
	if err != nil {
		t.Fatal(err)
	}
	res := srv.rigSetComponent(ctx, uid, lure.GUID, catHook, hook.GUID, 250,
		rigSetOpts{ParentType: familyLure, EnforceCategory: true, SplitStack: true})
	if res.Rejected {
		t.Fatalf("背包拟饵装钩被拒: %+v", res)
	}
	if row := mustItem(t, db, hook.GUID); row.RigGUID != lure.GUID {
		t.Fatalf("钩应挂在拟饵上: rig_guid=%q", row.RigGUID)
	}
	if _, err := srv.validateBreakReport(ctx, uid, rod.GUID, hook.GUID); err == nil {
		t.Fatal("背包拟饵上的钩断件应被拒绝")
	}
}
