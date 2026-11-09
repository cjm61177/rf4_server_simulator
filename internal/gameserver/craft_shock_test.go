package gameserver

import (
	"context"
	"math"
	"testing"

	"rf4server/internal/logging"
	"rf4server/internal/proto"
	"rf4server/internal/store"
)

func TestCraftShockLeaderUsesMainLineSlot(t *testing.T) {
	db := openRecipeTestDB(t)
	srv := newSkillsCraftServer(t, db)
	ctx := context.Background()
	const uid = int64(2000043)
	if _, err := db.EnsureProfile(ctx, uid); err != nil {
		t.Fatal(err)
	}
	line, err := srv.CreateItem(ctx, uid, "mono_15065", "backpack", 1)
	if err != nil {
		t.Fatal(err)
	}
	srv.alignItemRowQty(ctx, line)
	sess := &Session{ID: 1, Userid: uid, Srv: srv, log: logging.Discard()}
	hctx := &Ctx{Sess: sess, Main: 9, Sub: 1, CallID: 1, DecodeOK: true,
		Tree: proto.Args524(craftObj8("craft_leader_shock_200", []craftSlot{{ItemID: "craft_mat_line", GUID: line.GUID}}, 1)), Log: logging.Discard()}
	body, ok := hCraft(hctx)
	if !ok || u64Of(decodeTreeValue(t, srv, body).Fields[0].Num) != 1 {
		t.Fatal("Super Line 7lb 制作减震引线应成功")
	}
	rows, err := db.ListItemsByContainers(ctx, uid, []string{"backpack"})
	if err != nil {
		t.Fatal(err)
	}
	var crafted *store.Item
	for _, row := range rows {
		if row.ItemID == "leader_crafted_shock_200" {
			crafted = row
		}
	}
	if crafted == nil {
		t.Fatal("缺少减震引线成品")
	}
	check := func() {
		t.Helper()
		row, err := db.GetItemByGUID(ctx, crafted.GUID)
		if err != nil || row == nil || row.Container != "backpack" {
			t.Fatalf("成品应保留在背包: %+v, %v", row, err)
		}
		tree, err := srv.ItemToTree(ctx, row)
		if err != nil {
			t.Fatal(err)
		}
		inner := tree.Fields[fieldIdxByCS(tree.Class, craftedMainLineKey)].Val
		if inner == nil || inner.Kind != proto.KindObj || inner.Class != categoryWireClass[gearCatLine].Class {
			t.Fatalf("f3 应使用普通主线实例: %+v", inner)
		}
		if got := u64Of(inner.Fields[fieldIdxByCS(inner.Class, csNumericID)].Num); got != 9201 {
			t.Fatalf("主线模型=%d，期望 9201", got)
		}
		if got := u64Of(inner.Fields[fieldIdxByCS(inner.Class, gearAttrLineLen)].Num); got != 200 {
			t.Fatalf("主线长度=%d，期望 200cm", got)
		}
		if got := wireFloatForTest(t, tree, csStrength); math.Abs(got-3.1) > 0.001 {
			t.Fatalf("成品强度=%f，期望 3.1kg", got)
		}
		for _, key := range []string{gearAttrLeaderIn, "lbadjgllglo"} {
			v := tree.Fields[fieldIdxByCS(tree.Class, key)].Val
			if v != nil && v.Kind != proto.KindNull {
				t.Fatalf("减震引线 %s 应为空: %+v", key, v)
			}
		}
	}
	check()
	if err := srv.RepairItemPhysics(ctx); err != nil {
		t.Fatal(err)
	}
	check()
	after, err := db.GetItemByGUID(ctx, line.GUID)
	if err != nil || after == nil || after.Qty != 118 {
		t.Fatalf("应只消耗 2m 主线: %+v, %v", after, err)
	}
}

func TestRepairLegacyShockLeaderMigratesF1ToMainLineSlot(t *testing.T) {
	db := openRecipeTestDB(t)
	srv := newSkillsCraftServer(t, db)
	ctx := context.Background()
	const uid = int64(2000053)
	if _, err := db.EnsureProfile(ctx, uid); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name        string
		lineItemID  string
		sourceKey   string
		workmanship float64
		durability  float64
	}{
		{name: "旧前导线对象", lineItemID: "wrl_sim_monorig_7.5LB", sourceKey: gearAttrLeaderIn, workmanship: 0.9, durability: 0.95},
		{name: "旧普通主线对象", lineItemID: "mono_15247", sourceKey: craftedMainLineKey, workmanship: 0.8, durability: 0.9},
	}
	fixtures := make([]string, len(cases))
	for i, tc := range cases {
		fixtures[i] = makeLegacyShockLeaderWithF1Line(t, srv, db, uid, tc.lineItemID, tc.sourceKey, tc.workmanship, tc.durability)
	}

	if err := srv.RepairItemPhysics(ctx); err != nil {
		t.Fatal(err)
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row, err := db.GetItemByGUID(ctx, fixtures[i])
			if err != nil || row == nil {
				t.Fatalf("读取旧减震引线失败: %v", err)
			}
			fields, err := FieldsFromJSON(row.FieldsJSON)
			if err != nil {
				t.Fatal(err)
			}
			mainLine := fields[fieldIdxByCS(row.WireClass, craftedMainLineKey)].Val
			if mainLine == nil || mainLine.Kind != proto.KindObj || mainLine.Class != categoryWireClass[gearCatLine].Class {
				t.Fatalf("旧减震引线未迁移到 f3 普通主线对象: %+v", mainLine)
			}
			lineDef, err := db.GetItemDefByItemID(ctx, tc.lineItemID)
			if err != nil || lineDef == nil {
				t.Fatalf("读取材料定义失败: %v", err)
			}
			if got := u64Of(mainLine.Fields[fieldIdxByCS(mainLine.Class, csNumericID)].Num); got != uint64(lineDef.NumericID) {
				t.Fatalf("迁移后 numeric_id=%d，期望保留 %d", got, lineDef.NumericID)
			}
			if got := u64Of(mainLine.Fields[fieldIdxByCS(mainLine.Class, gearAttrLineLen)].Num); got != 200 {
				t.Fatalf("迁移后长度=%d，期望 200cm", got)
			}
			if got := wireFloatForTest(t, mainLine, csWorkmanship); math.Abs(got-tc.workmanship) > 0.001 {
				t.Fatalf("迁移后工艺=%f，期望 %f", got, tc.workmanship)
			}
			if got := wireFloatForTest(t, mainLine, csDurability); math.Abs(got-tc.durability) > 0.001 {
				t.Fatalf("迁移后耐久=%f，期望 %f", got, tc.durability)
			}
			wantStrength := itemDefF32(lineDef, gearAttrMaxLoad) * tc.workmanship * tc.durability
			if got := wireFloatForTest(t, mainLine, csStrength); math.Abs(got-wantStrength) > 0.001 {
				t.Fatalf("迁移后内线强度=%f，期望 %f", got, wantStrength)
			}
			wantGUID := uuidLike(row.GUID, "legacy-line")
			if got := mainLine.Fields[fieldIdxByCS(mainLine.Class, csGUID)].Hex; got != wantGUID {
				t.Fatalf("迁移后内线 GUID=%s，期望保留 %s", got, wantGUID)
			}
			if old := fields[fieldIdxByCS(row.WireClass, gearAttrLeaderIn)].Val; old != nil && old.Kind != proto.KindNull {
				t.Fatalf("迁移后 f1 应清空: %+v", old)
			}
		})
	}
}

func makeLegacyShockLeaderWithF1Line(
	t *testing.T,
	srv *Server,
	db *store.DB,
	uid int64,
	lineItemID string,
	sourceKey string,
	workmanship float64,
	durability float64,
) string {
	t.Helper()
	ctx := context.Background()
	row, err := srv.prepareNewItem(ctx, uid, "leader_crafted_shock_200", "backpack", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	lineDef, err := db.GetItemDefByItemID(ctx, lineItemID)
	if err != nil {
		t.Fatal(err)
	}
	material := craftedMaterial{
		row: &store.Item{GUID: uuidLike(row.GUID, "legacy-line"), Workmanship: workmanship, Durability: durability},
		def: lineDef,
	}
	line, err := srv.craftedMaterialObject(row, material, sourceKey, 200)
	if err != nil {
		t.Fatal(err)
	}
	if idx := fieldIdxByCS(line.Class, csGUID); idx >= 0 {
		// craftedMaterialObject 会按槽位派生嵌套 GUID；这里固定成旧对象自己的
		// GUID，确保迁移测试验证的是保留原对象身份，而不是重新生成一个。
		line.Fields[idx].Hex = material.row.GUID
	}
	fields, err := FieldsFromJSON(row.FieldsJSON)
	if err != nil {
		t.Fatal(err)
	}
	fields[fieldIdxByCS(row.WireClass, gearAttrLeaderIn)].Val = line
	setWireFloat(fields, row.WireClass, fieldIdxByCS(row.WireClass, csStrength),
		itemDefF32(lineDef, gearAttrMaxLoad)*workmanship*durability)
	row.FieldsJSON, err = FieldsToJSON(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.InsertItem(ctx, row); err != nil {
		t.Fatal(err)
	}
	return row.GUID
}

// 回归（配方口径收窄）：减震引线只收普通主线。物化器按 f3 主线槽语义
// 拒收前导线（b0ac5d3），配方却曾放行 categories [11,15]，客户端照配方
// 选前导线（如 Atami Leader Nylon 400LB 50m = mn_lm_40809）必然在物化
// 阶段固定回滚。配方收窄到 [11] 后，拒绝应发生在需求匹配阶段：材料不
// 被扣、成品不产出。
func TestCraftShockLeaderRejectsLeaderWireMaterial(t *testing.T) {
	db := openRecipeTestDB(t)
	srv := newSkillsCraftServer(t, db)
	ctx := context.Background()
	const uid = int64(2000043)
	if _, err := db.EnsureProfile(ctx, uid); err != nil {
		t.Fatal(err)
	}
	line, err := srv.CreateItem(ctx, uid, "mn_lm_40809", "backpack", 2)
	if err != nil {
		t.Fatal(err)
	}
	srv.alignItemRowQty(ctx, line)
	if fresh, err := db.GetItemByGUID(ctx, line.GUID); err != nil || fresh == nil {
		t.Fatalf("读取前导线材料: %v", err)
	} else {
		line = fresh
	}
	spoolBefore := line.Qty
	sess := &Session{ID: 1, Userid: uid, Srv: srv, log: logging.Discard()}
	hctx := &Ctx{Sess: sess, Main: 9, Sub: 1, CallID: 1, DecodeOK: true,
		Tree: proto.Args524(craftObj8("craft_leader_shock_200", []craftSlot{{ItemID: "craft_mat_line", GUID: line.GUID}}, 1)), Log: logging.Discard()}
	body, ok := hCraft(hctx)
	if !ok || u64Of(decodeTreeValue(t, srv, body).Fields[0].Num) == 1 {
		t.Fatalf("前导线做减震引线应被拒绝: ok=%v", ok)
	}
	// 材料不得被扣，成品不得产出。
	rows, err := db.ListItemsByContainers(ctx, uid, []string{"backpack"})
	if err != nil {
		t.Fatal(err)
	}
	spoolQty := int64(0)
	for _, row := range rows {
		if row.ItemID == "leader_crafted_shock_200" {
			t.Fatal("不应产出减震引线成品")
		}
		if row.ItemID == "mn_lm_40809" {
			spoolQty += row.Qty
		}
	}
	if spoolQty != spoolBefore {
		t.Fatalf("前导线不得被消耗: before=%d after=%d", spoolBefore, spoolQty)
	}
}

// 回归（明确选料不得静默换料，2026-11-09 事故）：玩家在槽位里明确指定
// 前导线（mn_lm_40809）做减震引线时，服务端不得因它类目不合格就跳过，
// 再从背包/钓组里自选一卷合格主线扣料物化——当时钓组上的 mono_15065
// 被悄悄扣掉，产出了 3.1kg 的尼龙线。现在必须整体拒绝，两卷线都原样保留。
func TestCraftShockLeaderExplicitWrongMaterialNotSubstituted(t *testing.T) {
	db := openRecipeTestDB(t)
	srv := newSkillsCraftServer(t, db)
	ctx := context.Background()
	const uid = int64(2000044)
	if _, err := db.EnsureProfile(ctx, uid); err != nil {
		t.Fatal(err)
	}
	wire, err := srv.CreateItem(ctx, uid, "mn_lm_40809", "backpack", 2)
	if err != nil {
		t.Fatal(err)
	}
	srv.alignItemRowQty(ctx, wire)
	line, err := srv.CreateItem(ctx, uid, "mono_15065", "backpack", 1)
	if err != nil {
		t.Fatal(err)
	}
	srv.alignItemRowQty(ctx, line)
	wireBefore := itemQtyByGUID(t, db, wire.GUID)
	lineBefore := itemQtyByGUID(t, db, line.GUID)
	sess := &Session{ID: 1, Userid: uid, Srv: srv, log: logging.Discard()}
	hctx := &Ctx{Sess: sess, Main: 9, Sub: 1, CallID: 1, DecodeOK: true,
		Tree: proto.Args524(craftObj8("craft_leader_shock_200", []craftSlot{{ItemID: "craft_mat_line", GUID: wire.GUID}}, 1)), Log: logging.Discard()}
	body, ok := hCraft(hctx)
	if !ok || u64Of(decodeTreeValue(t, srv, body).Fields[0].Num) == 1 {
		t.Fatalf("明确指定前导线做减震引线应被拒绝: ok=%v", ok)
	}
	if got := itemQtyByGUID(t, db, wire.GUID); got != wireBefore {
		t.Fatalf("前导线不得被消耗: before=%d after=%d", wireBefore, got)
	}
	if got := itemQtyByGUID(t, db, line.GUID); got != lineBefore {
		t.Fatalf("不得静默换料扣掉合格主线: before=%d after=%d", lineBefore, got)
	}
	rows, err := db.ListItemsByContainers(ctx, uid, []string{"backpack"})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.ItemID == "leader_crafted_shock_200" {
			t.Fatal("不应产出减震引线成品")
		}
	}
}

// 兼容锁：客户端提交库里查不到的临时 GUID 时，补扣扫描仍按类目从背包
// 选取合格线材正常出货，不得被"明确选料拒绝"误伤。
func TestCraftShockLeaderTempGUIDFallsBackToScan(t *testing.T) {
	db := openRecipeTestDB(t)
	srv := newSkillsCraftServer(t, db)
	ctx := context.Background()
	const uid = int64(2000045)
	if _, err := db.EnsureProfile(ctx, uid); err != nil {
		t.Fatal(err)
	}
	line, err := srv.CreateItem(ctx, uid, "mono_15065", "backpack", 1)
	if err != nil {
		t.Fatal(err)
	}
	srv.alignItemRowQty(ctx, line)
	sess := &Session{ID: 1, Userid: uid, Srv: srv, log: logging.Discard()}
	hctx := &Ctx{Sess: sess, Main: 9, Sub: 1, CallID: 1, DecodeOK: true,
		Tree: proto.Args524(craftObj8("craft_leader_shock_200", []craftSlot{{ItemID: "craft_mat_line", GUID: "temp-guid-not-in-db"}}, 1)), Log: logging.Discard()}
	body, ok := hCraft(hctx)
	if !ok || u64Of(decodeTreeValue(t, srv, body).Fields[0].Num) != 1 {
		t.Fatalf("临时 GUID 应按类目补扣正常出货: ok=%v", ok)
	}
	found := false
	rows, err := db.ListItemsByContainers(ctx, uid, []string{"backpack"})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.ItemID == "leader_crafted_shock_200" {
			found = true
		}
	}
	if !found {
		t.Fatal("补扣扫描应产出减震引线成品")
	}
}

func itemQtyByGUID(t *testing.T, db *store.DB, guid string) int64 {
	t.Helper()
	row, err := db.GetItemByGUID(context.Background(), guid)
	if err != nil || row == nil {
		t.Fatalf("读取 %s: %v", guid, err)
	}
	return row.Qty
}
