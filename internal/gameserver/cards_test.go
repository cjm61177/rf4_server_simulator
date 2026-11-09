package gameserver

// 卡牌系统测试：掉率分层、卡物品形状、结算发卡与回执幂等、
// 卡组奖座一次发放、管理发卡/发整组。卡与卡组 def 来自 testDBItems
// 模板导入的真实静态库（tcdeck_carp = 19 卡 + statuette_cards_carp）。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"rf4server/internal/logging"
	"rf4server/internal/proto"
	"rf4server/internal/store"
)

const cardTestDeck = "tcdeck_carp"
const cardTestStatuette = "statuette_cards_carp"

// setCardDropTunable 落库并直写 tunable 缓存（30s 缓存纪律同 enableFastBites）。
func setCardDropTunable(t *testing.T, srv *Server, db *store.DB, key string, v float64) {
	t.Helper()
	if err := db.TunableSet(context.Background(), key, fmt.Sprintf("%v", v), "test"); err != nil {
		t.Fatal(err)
	}
	srv.tunMu.Lock()
	if srv.tunCache == nil {
		srv.tunCache = map[string]float64{}
	}
	srv.tunCache[key] = v
	srv.tunAt = time.Now()
	srv.tunMu.Unlock()
}

func cardItemCount(t *testing.T, db *store.DB, uid int64, itemID string) int {
	t.Helper()
	var n int
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM item WHERE uid=? AND item_id=?`, uid, itemID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func cardTestFish(guid, species string) *RolledFish {
	return &RolledFish{FishGUID: guid, Species: species, Grade: 2, WeightG: 1234, LengthM: .45,
		LocLevel: "level_002_kirzah_river", Name: "CardFish"}
}

func cardTestDeckDef(t *testing.T, db *store.DB) *cardDeck {
	t.Helper()
	decks, err := loadCardDecks(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range decks {
		if d.DeckID == cardTestDeck {
			return d
		}
	}
	t.Fatalf("静态库缺卡组 %s", cardTestDeck)
	return nil
}

func TestCardDropRateLayeredTunables(t *testing.T) {
	srv, db := newDetachTestServer(t)
	if srv.cardDropRate(0) != 0 || srv.cardDropRate(1) != 0 || srv.cardDropRate(2) != 0 {
		t.Fatal("默认掉率必须为 0（功能关闭）")
	}
	setCardDropTunable(t, srv, db, "card_drop_common", 0.25)
	setCardDropTunable(t, srv, db, "card_drop_rare", 0.5)
	setCardDropTunable(t, srv, db, "card_drop_super", 1)
	if srv.cardDropRate(0) != 0.25 || srv.cardDropRate(1) != 0.5 || srv.cardDropRate(2) != 1 {
		t.Fatal("稀有度 0/1/2 未映射到 common/rare/super 三档 tunable")
	}
}

func TestPrepareNewCardEmbedsFishSnapshot(t *testing.T) {
	srv, db := newDetachTestServer(t)
	ctx := context.Background()
	uid := recoveryUID
	if _, err := db.EnsureProfile(ctx, uid); err != nil {
		t.Fatal(err)
	}
	fish := cardTestFish("66666666666666666666666666666666", "carp")
	card, err := srv.prepareNewCard(ctx, uid, fish)
	if err != nil {
		t.Fatal(err)
	}
	if card.ItemID != "card_carp" || card.WireClass != "dapkdcghfmg" {
		t.Fatalf("卡物品形状错误: item=%s wire_class=%s", card.ItemID, card.WireClass)
	}
	fields, err := FieldsFromJSON(card.FieldsJSON)
	if err != nil {
		t.Fatal(err)
	}
	idx := fieldIdxByCS("dapkdcghfmg", "peipahjmida")
	if idx < 0 || idx >= len(fields) || fields[idx].Val == nil {
		t.Fatal("卡 f0 peipahjmida 没有内嵌 25 鱼快照")
	}
	derived := uuid5OIDHex(card.GUID + "/card_fish")
	if derived == strings.ToLower(fish.FishGUID) || !strings.Contains(card.FieldsJSON, derived) {
		t.Fatalf("内嵌鱼 guid 未按卡 guid 派生（与鱼护同 guid 会串标本）: derived=%s", derived)
	}
	// 卡行必须与卡组行同柜（收藏册容器 18）：卡牌页窗口 lagfnoohjip 用
	// 容器 18 管理器按静态类别 78 过滤，卡行放 backpack 则卡牌页恒空
	// （2026-11-07 实机两页症状的根因，详见 cards.go prepareNewCard）。
	if card.Container != tcDeckContainer {
		t.Fatalf("卡行应在收藏册容器 %s: got=%s", tcDeckContainer, card.Container)
	}
	// wire f3 容器枚举不写：启动修复 RepairItemPhysics →
	// canonicalizeItemPhysicalFields 恒清零根物品 csSlotEnum（rig 时代规则），
	// 写 18 活不过下次重启；归柜真源=DB 行 container 列（上面已断言）。
	// 新卡=自由卡，f13 dbgaheblmei 不打 bit4：bit4=已收集进卡组标记，
	// 自由卡带位时卡牌页多选/右键出售全部置灰（2026-11-07 实机），见
	// cards.go prepareNewCard 注释。
	if flags := fields[fieldIdxByCS("dapkdcghfmg", "dbgaheblmei")]; flags.Num != nil && *flags.Num&0x10 != 0 {
		t.Fatalf("自由卡 f13 dbgaheblmei 不应置 bit4(0x10): got=%v", *flags.Num)
	}
	// f2 odflendafcg 父 guid 必须留空：自由卡谓词 bedifiidhhf.pmibgafefdf
	// = !efhaepoijno()（父空=自由卡可售），父非空=已归属卡组、出售键恒灰
	// （2026-11-07 实机）。卡组归属由卡组行 f0 内嵌卡树承载，见 cards.go。
	if parent := fields[fieldIdxByCS("dapkdcghfmg", "odflendafcg")]; parent.Hex != "" {
		t.Fatalf("卡 f2 父 guid 应留空（自由卡可售）: got=%q", parent.Hex)
	}
}

func TestCatchSettlementGrantsCardOnceAndReplayDoesNot(t *testing.T) {
	srv, db := newDetachTestServer(t)
	ctx := context.Background()
	uid := recoveryUID
	if _, err := db.EnsureProfile(ctx, uid); err != nil {
		t.Fatal(err)
	}
	setCardDropTunable(t, srv, db, "card_drop_common", 1)
	fish := cardTestFish("61616161616161616161616161616161", "perch")
	first := srv.settleCatchXP(ctx, uid, fish, 1)
	if first == nil || first.cardGUID == "" {
		t.Fatal("掉率 1.0 的结算没有发卡")
	}
	if n := cardItemCount(t, db, uid, "card_perch"); n != 1 {
		t.Fatalf("发卡数量错误: %d", n)
	}
	raw, _, _ := db.KVGet(ctx, fmt.Sprintf("catch-settlement:%d:%s", uid, strings.ToLower(fish.FishGUID)))
	var receipt catchReceipt
	if err := json.Unmarshal([]byte(raw), &receipt); err != nil || receipt.CardGUID != first.cardGUID {
		t.Fatalf("回执没有记录卡 guid: 回执=%q 实发=%q", receipt.CardGUID, first.cardGUID)
	}
	// 同一尾鱼重放（重连回执路径）：不双发，cardGUID 原样带出供重推同步。
	replay := srv.settleCatchXP(ctx, uid, fish, 1)
	if replay == nil || !replay.replayed || replay.cardGUID != first.cardGUID {
		t.Fatalf("重放结果错误: %+v", replay)
	}
	if n := cardItemCount(t, db, uid, "card_perch"); n != 1 {
		t.Fatalf("回执重放二次发卡: %d", n)
	}
}

func TestCatchSettlementWithoutCardDrop(t *testing.T) {
	srv, db := newDetachTestServer(t)
	ctx := context.Background()
	uid := recoveryUID
	if _, err := db.EnsureProfile(ctx, uid); err != nil {
		t.Fatal(err)
	}
	res := srv.settleCatchXP(ctx, uid, cardTestFish("62626262626262626262626262626262", "perch"), 1)
	if res == nil || res.cardGUID != "" {
		t.Fatalf("默认掉率 0 不该发卡: %+v", res)
	}
	if n := cardItemCount(t, db, uid, "card_perch"); n != 0 {
		t.Fatalf("默认掉率 0 仍产生了卡行: %d", n)
	}
}

func TestCardDeckStatuetteGrantedOnce(t *testing.T) {
	srv, db := newDetachTestServer(t)
	ctx := context.Background()
	uid := recoveryUID
	if _, err := db.EnsureProfile(ctx, uid); err != nil {
		t.Fatal(err)
	}
	deck := cardTestDeckDef(t, db)
	for _, c := range deck.Cards {
		if _, err := srv.grantCard(ctx, uid, strings.TrimPrefix(c, cardItemIDPrefix)); err != nil {
			t.Fatalf("发卡 %s: %v", c, err)
		}
	}
	granted, err := srv.checkCardDecks(ctx, uid, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(granted) != 1 || cardItemCount(t, db, uid, cardTestStatuette) != 1 {
		t.Fatalf("集齐应恰好发一座奖座: got=%v", granted)
	}
	again, err := srv.checkCardDecks(ctx, uid, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 || cardItemCount(t, db, uid, cardTestStatuette) != 1 {
		t.Fatalf("奖座重复发放: %v", again)
	}
}

func TestAdminGrantDeckQueueFillsDeckAndStatuette(t *testing.T) {
	srv, db := newDetachTestServer(t)
	ctx := context.Background()
	uid := recoveryUID
	if _, err := db.EnsureProfile(ctx, uid); err != nil {
		t.Fatal(err)
	}
	deck := cardTestDeckDef(t, db)
	progress, err := CardDeckProgress(ctx, db, uid)
	if err != nil {
		t.Fatal(err)
	}
	var before map[string]any
	for _, p := range progress {
		if p["deck_id"] == cardTestDeck {
			before = p
		}
	}
	if before == nil || before["owned"] != 0 || before["total"] != len(deck.Cards) {
		t.Fatalf("初始进度错误: %v", before)
	}
	guids := srv.adminGrantDeckQueue(ctx, uid, cardTestDeck)
	if len(guids) != len(deck.Cards)+1 {
		t.Fatalf("发整组 guid 数=%d want=%d（缺卡+奖座）", len(guids), len(deck.Cards)+1)
	}
	if n := cardItemCount(t, db, uid, cardTestStatuette); n != 1 {
		t.Fatalf("奖座未随整组发放: %d", n)
	}
	after, err := CardDeckProgress(ctx, db, uid)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range after {
		if p["deck_id"] != cardTestDeck {
			continue
		}
		if p["owned"] != len(deck.Cards) || len(p["missing"].([]string)) != 0 {
			t.Fatalf("整组发放后进度未更新: %v", p)
		}
	}
	if again := srv.adminGrantDeckQueue(ctx, uid, cardTestDeck); len(again) != 0 {
		t.Fatalf("重复发整组产出 %d 个新 guid（应全部跳过）", len(again))
	}
}

func TestAdminGrantCardQueueGrantsSeparateCards(t *testing.T) {
	srv, db := newDetachTestServer(t)
	ctx := context.Background()
	uid := recoveryUID
	if _, err := db.EnsureProfile(ctx, uid); err != nil {
		t.Fatal(err)
	}
	guids := srv.adminGrantCardQueue(ctx, uid, "card_perch", 3)
	if len(guids) != 3 {
		t.Fatalf("发 3 张卡实得 %d 个 guid", len(guids))
	}
	// 卡不在可堆叠类别，qty 张必须逐张独立入包。
	if n := cardItemCount(t, db, uid, "card_perch"); n != 3 {
		t.Fatalf("卡行数=%d want=3", n)
	}
}

func TestFishingEndSettleGrantsCard(t *testing.T) {
	srv, db := newDetachTestServer(t)
	sess := recoveryLogin(t, srv, 881)
	// 新玩家首钓必升级，会触发升级礼物进 801.f8；本测试只验证掉卡素材，
	// 关掉礼物保持 f8 断言单一语义。
	setCardDropTunable(t, srv, db, "level_gifts_enabled", 0)
	setCardDropTunable(t, srv, db, "card_drop_common", 1)
	st := srv.fishingState(recoveryUID)
	st.mu.Lock()
	st.Pending = cardTestFish("63636363636363636363636363636363", "perch")
	st.CastActive, st.Hooked, st.Resolved = true, true, false
	st.mu.Unlock()
	body, handled := srv.fishingEndSettle(&Ctx{Sess: sess, Main: 14, Sub: 4, Log: logging.Discard()})
	if !handled || len(body) == 0 {
		t.Fatal("14/4 未完成结算")
	}
	if n := cardItemCount(t, db, recoveryUID, "card_perch"); n != 1 {
		t.Fatalf("14/4 结算未发卡: %d", n)
	}
	// 801.f8 是结算弹窗卡牌素材（iingicaldbk 筛 ipdpoaiekem），必须带卡树。
	dec, _, err := proto.DecodeValue(body, 0, srv.Table())
	if err != nil || dec.Class != "caagocpofin" {
		t.Fatalf("14/4 应回 801: %v %s", err, dec.Class)
	}
	f8 := dec.Field(8)
	items := f8.Tree()
	if items == nil || items.ArgCodeStr() != "61" || items.ArgCount() != 1 {
		t.Fatalf("801.f8 掉卡时应含 1 张卡: code=%s count=%d nil=%v",
			items.ArgCodeStr(), items.ArgCount(), items == nil)
	}
	if cls := items.ArgItem(0).Class; cls != "dapkdcghfmg" {
		t.Fatalf("801.f8 元素应为卡 wire 类 dapkdcghfmg: %s", cls)
	}
}

func TestFishingEndSettleCardFieldEmptyWithoutDrop(t *testing.T) {
	srv, db := newDetachTestServer(t)
	// 同上：隔离升级礼物对 801.f8 的附加，单测「无掉卡=f8 空数组」。
	setCardDropTunable(t, srv, db, "level_gifts_enabled", 0)
	sess := recoveryLogin(t, srv, 882)
	st := srv.fishingState(recoveryUID)
	st.mu.Lock()
	st.Pending = cardTestFish("64646464646464646464646464646464", "perch")
	st.CastActive, st.Hooked, st.Resolved = true, true, false
	st.mu.Unlock()
	body, handled := srv.fishingEndSettle(&Ctx{Sess: sess, Main: 14, Sub: 4, Log: logging.Discard()})
	if !handled || len(body) == 0 {
		t.Fatal("14/4 未完成结算")
	}
	// 默认掉率 0：f8 恒发空数组（code 61），不发 null。
	dec, _, err := proto.DecodeValue(body, 0, srv.Table())
	if err != nil || dec.Class != "caagocpofin" {
		t.Fatalf("14/4 应回 801: %v %s", err, dec.Class)
	}
	f8 := dec.Field(8)
	items := f8.Tree()
	if items == nil || items.ArgCodeStr() != "61" || items.ArgCount() != 0 {
		t.Fatalf("无卡时 801.f8 应为空 61 数组: tree=%v", items)
	}
}

// TestSocialProfileCarriesCardDecks 收藏册数据链：24/18 档案聚合（2003
// feaidppnpch）的 f9 必须带卡组数组（args code 364），有卡卡组 f0 卡树
// 齐全；f1 mkkhpfgnjei（塑封时间戳）恒 0——>0 且 f0 非空=客户端判
// 「已塑封」，收集中不得写（见 cards.go prepareDeckItems/buildCardDeckArray）。
func TestSocialProfileCarriesCardDecks(t *testing.T) {
	srv, db := newDetachTestServer(t)
	registerSocialHandlers(srv.dispatch)
	sess := recoveryLogin(t, srv, 883)
	ctx := context.Background()
	deck := cardTestDeckDef(t, db)
	if len(deck.Cards) < 2 {
		t.Fatalf("测试卡组 %s 卡数不足", cardTestDeck)
	}
	for _, c := range deck.Cards[:2] {
		if _, err := srv.grantCard(ctx, recoveryUID, strings.TrimPrefix(c, cardItemIDPrefix)); err != nil {
			t.Fatalf("发卡 %s: %v", c, err)
		}
	}
	var deckNumID int64
	if err := db.SQL.QueryRowContext(ctx,
		`SELECT numeric_id FROM item_def WHERE item_id=?`, cardTestDeck).Scan(&deckNumID); err != nil {
		t.Fatal(err)
	}

	h := srv.dispatch.lookup(24, 18)
	if h == nil {
		t.Fatal("24/18 未注册")
	}
	body, handled := h(&Ctx{Sess: sess, Main: 24, Sub: 18,
		Tree: proto.Args524(proto.U32(uint32(recoveryUID))), DecodeOK: true, ExpectResponse: true,
		Log: logging.Discard()})
	if !handled || len(body) == 0 {
		t.Fatal("24/18 未处理")
	}
	v, _, err := proto.DecodeValue(body, 0, srv.Table())
	if err != nil {
		t.Fatal(err)
	}
	inner := decodeWrappedBytes(t, v)
	if inner.Class != "feaidppnpch" {
		t.Fatalf("24/18 聚合类错误: %s", inner.Class)
	}
	f9 := inner.Field(9)
	decks := f9.Tree()
	if decks == nil || decks.ArgCodeStr() != "364" || decks.ArgCount() == 0 {
		t.Fatalf("f9 应为非空 364 卡组数组: tree=%v", decks)
	}
	var target, empty *proto.Value
	for i := 0; i < decks.ArgCount(); i++ {
		dk := decks.ArgItem(i)
		if dk.Class != "ckfmndabaei" {
			t.Fatalf("f9[%d] 类错误: %s", i, dk.Class)
		}
		if n := dk.Field(5); n.Num == nil || int64(*n.Num) != deckNumID {
			continue
		}
		target = dk
	}
	if target == nil {
		t.Fatalf("f9 里没找到 %s（numeric_id=%d）", cardTestDeck, deckNumID)
	}
	cardsF := target.Field(0)
	cards := cardsF.Tree()
	if cards == nil || cards.ArgCodeStr() != "362" || cards.ArgCount() != 2 {
		t.Fatalf("目标卡组 f0 应为 args 362 类型化卡数组且含 2 棵卡树: tree=%v", cards)
	}
	for i := 0; i < cards.ArgCount(); i++ {
		if cls := cards.ArgItem(i).Class; cls != "dapkdcghfmg" {
			t.Fatalf("卡树元素类错误: %s", cls)
		}
	}
	// f1 塑封时间戳收集中恒 0：>0+f0 非空 → 客户端 eekechidjae()=true 判
	// 「已塑封」并锁编辑（2026-11-07 实机「还没集齐就显示已塑封」根因）。
	if started := target.Field(1); started.Num == nil || *started.Num != 0 {
		t.Fatalf("有卡卡组 f1 mkkhpfgnjei 收集中应为 0: %+v", started.Num)
	}
	if guid := target.Field(2); guid.Hex == "" {
		t.Fatalf("卡组实例 guid（f2）缺失")
	}
	// 未开始收集的卡组：f1==0、f0 空 61 数组。（卡组清单跨组可能共享卡，
	// 必须按 f0 实际为空来选，不能只看 numeric id 不同。）
	for i := 0; i < decks.ArgCount(); i++ {
		dk := decks.ArgItem(i)
		if n := dk.Field(5); n.Num != nil && int64(*n.Num) == deckNumID {
			continue
		}
		f0 := dk.Field(0)
		if tr := f0.Tree(); tr == nil || tr.ArgCount() == 0 {
			empty = dk
			break
		}
	}
	if empty == nil {
		t.Fatal("静态库应有多于一个卡组")
	}
	if started := empty.Field(1); started.Num == nil || *started.Num != 0 {
		t.Fatalf("空卡组 f1 应为 0: %+v", started.Num)
	}
	emptyF := empty.Field(0)
	if cards := emptyF.Tree(); cards == nil || cards.ArgCodeStr() != "362" || cards.ArgCount() != 0 {
		t.Fatalf("空卡组 f0 应为空 362 数组: tree=%v", cards)
	}
}

// TestGrantCardCreatesDeckItem 收藏册真链（2026-11-07 hook 实证）：独立
// 收藏册界面从库存 tcdeck_* 物品取数（mailhgoence/dceidcbplaf 消费
// imdcjllgdcn 物品事件），不走 24/18。官方语义（2026-11-08 改）：发卡只落
// 卡牌列表（自由卡），不自动建卡组物品；玩家 27/2 手动指派后才有卡组行：
// wire 类 ckfmndabaei/11304，f0 内嵌该组卡树，f1 恒 0（收集中，>0=塑封），
// guid 派生幂等；被指派副本带 f2=卡组 guid + bit4。
func TestGrantCardCreatesDeckItem(t *testing.T) {
	srv, db := newDetachTestServer(t)
	ctx := context.Background()
	uid := recoveryUID
	if _, err := db.EnsureProfile(ctx, uid); err != nil {
		t.Fatal(err)
	}
	card, err := srv.grantCard(ctx, uid, "carp")
	if err != nil {
		t.Fatal(err)
	}
	// 未指派：无卡组物品行（新卡留在卡牌列表）
	if rows, err := db.ListItemsByItemIDPrefix(ctx, uid, "tcdeck_"); err != nil || len(rows) != 0 {
		t.Fatalf("未指派不应建卡组物品: %d 行 err=%v", len(rows), err)
	}
	// 手动指派 → 卡组物品行出现
	asCtx, _ := cardRPCCtx(t, srv, 2, assignBody(cardTestDeck, deckItemGUID(uid, cardTestDeck), card.GUID))
	if body, handled := hCardAssign(asCtx); !handled || len(body) == 0 {
		t.Fatal("指派应回 ACK")
	}
	rows, err := db.ListItemsByItemIDPrefix(ctx, uid, "tcdeck_")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ItemID != cardTestDeck {
		t.Fatalf("指派后应恰好建 1 个对应卡组物品: %d 行", len(rows))
	}
	d := rows[0]
	if d.WireClass != "ckfmndabaei" || d.TypeID != 11304 {
		t.Fatalf("卡组物品 wire 类错误: class=%s type=%d want ckfmndabaei/11304", d.WireClass, d.TypeID)
	}
	if d.Container != tcDeckContainer {
		t.Fatalf("卡组物品应在收藏册容器 %s: got=%s", tcDeckContainer, d.Container)
	}
	if want := deckItemGUID(uid, cardTestDeck); d.GUID != want {
		t.Fatalf("卡组物品 guid 未按 uuid5 派生: got=%s want=%s", d.GUID, want)
	}
	fields, err := FieldsFromJSON(d.FieldsJSON)
	if err != nil {
		t.Fatal(err)
	}
	deck := cardTestDeckDef(t, db)
	f0 := fields[fieldIdxByCS("ckfmndabaei", "mjbciidhldo")]
	if f0.Val == nil || f0.Val.ArgCodeStr() != "362" || f0.Val.ArgCount() != 1 {
		t.Fatalf("卡组 f0 应内嵌 1 棵卡树（args 362 类型化卡数组）: code=%v n=%d", f0.Val, func() int {
			if f0.Val == nil {
				return -1
			}
			return f0.Val.ArgCount()
		}())
	}
	// f1 塑封时间戳收集中恒 0（>0+f0 非空=客户端判已塑封，同上）。
	if started := fields[fieldIdxByCS("ckfmndabaei", "mkkhpfgnjei")]; started.Num == nil || *started.Num != 0 {
		t.Fatal("卡组 f1 mkkhpfgnjei 收集中应为 0")
	}
	// f14 dbgaheblmei bit4（0x10）：客户端容器快照谓词 afkieinjcnb 的放行位，
	// 不置则卡组物品进不了快照、Guid 事件不广播，收藏册收不到（详见 cards.go）。
	if flags := fields[fieldIdxByCS("ckfmndabaei", "dbgaheblmei")]; flags.Num == nil || *flags.Num&0x10 == 0 {
		t.Fatalf("卡组 f14 dbgaheblmei 应置 bit4(0x10): got=%v", flags.Num)
	}
	// wire 容器字段 f4 不写：同卡行 f3——启动修复恒清零该 BYTE，归柜真源
	// =DB 行 container 列（上面已断言）。
	if numID := fields[fieldIdxByCS("ckfmndabaei", csNumericID)]; numID.Num == nil || int64(*numID.Num) == 0 {
		t.Fatal("卡组 f5 数字 id（def numeric_id）缺失")
	}
	// 组内其它卡未持有：不产生其它卡组物品行（静态库多卡组）。
	if n := cardItemCount(t, db, uid, "tcdeck_float"); n != 0 {
		t.Fatalf("无卡卡组不应建物品行: tcdeck_float x%d", n)
	}
	_ = deck
	// 首张副本被指派进卡组（官方语义：收集中的槽位渲染要求
	// parent==卡组 guid、bit4=已收集标记；见 cards.go prepareDeckItems）。
	cards, err := db.ListItemsByItemIDPrefix(ctx, uid, cardItemIDPrefix)
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 1 {
		t.Fatalf("应恰好 1 张卡行: %d", len(cards))
	}
	cfields, err := FieldsFromJSON(cards[0].FieldsJSON)
	if err != nil {
		t.Fatal(err)
	}
	if parent := cfields[fieldIdxByCS("dapkdcghfmg", "odflendafcg")]; parent.Hex != strings.ToLower(d.GUID) {
		t.Fatalf("卡 f2 应=卡组实例 guid（指派进卡组）: got=%q want=%s", parent.Hex, strings.ToLower(d.GUID))
	}
	if flags := cfields[fieldIdxByCS("dapkdcghfmg", "dbgaheblmei")]; flags.Num == nil || *flags.Num&0x10 == 0 {
		t.Fatalf("被指派卡 f13 应置 bit4(0x10): got=%v", flags.Num)
	}
}

// TestSyncDeckItemsRebuildIsIdempotent 重复发卡组内卡 + 手动指派一张：卡组
// 物品恒为一行（同 guid DELETE+INSERT 重建），卡行两副本都入库，f0 卡树
// 每物种只嵌被指派的一张（卡组槽位=物种格，去重理由见 cards.go）。
func TestSyncDeckItemsRebuildIsIdempotent(t *testing.T) {
	srv, db := newDetachTestServer(t)
	ctx := context.Background()
	uid := recoveryUID
	if _, err := db.EnsureProfile(ctx, uid); err != nil {
		t.Fatal(err)
	}
	var first *store.Item
	for i := 0; i < 2; i++ {
		card, err := srv.grantCard(ctx, uid, "carp")
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = card
		}
	}
	if n := cardItemCount(t, db, uid, "card_carp"); n != 2 {
		t.Fatalf("两次发卡应入库 2 张卡行: %d", n)
	}
	// 官方语义：只有玩家手动指派的副本进组（最早一张），其余自由
	asCtx, _ := cardRPCCtx(t, srv, 2, assignBody(cardTestDeck, deckItemGUID(uid, cardTestDeck), first.GUID))
	if body, handled := hCardAssign(asCtx); !handled || len(body) == 0 {
		t.Fatal("指派应回 ACK")
	}
	rows, err := db.ListItemsByItemIDPrefix(ctx, uid, "tcdeck_")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("指派后卡组物品应恰为 1 行: %d", len(rows))
	}
	fields, err := FieldsFromJSON(rows[0].FieldsJSON)
	if err != nil {
		t.Fatal(err)
	}
	f0 := fields[fieldIdxByCS("ckfmndabaei", "mjbciidhldo")]
	if f0.Val == nil || f0.Val.ArgCount() != 1 {
		t.Fatalf("卡组 f0 每物种应只嵌 1 棵卡树（去重）: %v", f0.Val)
	}
	// 两副本归属态：被指派一张带 f2=卡组 guid+bit4，另一张保持自由
	// （f2 空、无 bit4）供卡牌页出售。
	cards, err := db.ListItemsByItemIDPrefix(ctx, uid, cardItemIDPrefix)
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 2 {
		t.Fatalf("应恰好 2 张卡行: %d", len(cards))
	}
	for _, c := range cards {
		cf, cerr := FieldsFromJSON(c.FieldsJSON)
		if cerr != nil {
			t.Fatal(cerr)
		}
		parent := cf[fieldIdxByCS("dapkdcghfmg", "odflendafcg")]
		flags := cf[fieldIdxByCS("dapkdcghfmg", "dbgaheblmei")]
		if c.GUID == first.GUID {
			if parent.Hex == "" || flags.Num == nil || *flags.Num&0x10 == 0 {
				t.Fatalf("被指派副本应带父 guid+bit4: parent=%q flags=%v", parent.Hex, flags.Num)
			}
		} else if parent.Hex != "" || (flags.Num != nil && *flags.Num&0x10 != 0) {
			t.Fatalf("未指派副本应为自由卡: parent=%q flags=%v", parent.Hex, flags.Num)
		}
	}
	// 卡全部移除后重建：卡组物品行应消失（组内无卡=未开始收集）。
	if _, err := db.SQL.Exec(`DELETE FROM item WHERE uid=? AND item_id LIKE 'card\_%' ESCAPE '\'`, uid); err != nil {
		t.Fatal(err)
	}
	plan, err := srv.prepareDeckItems(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Decks) != 0 {
		t.Fatalf("无卡时不应构造卡组物品: %d", len(plan.Decks))
	}
	tx, err := db.SQL.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := syncDeckItemsTx(ctx, tx, uid, plan); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if rows, err := db.ListItemsByItemIDPrefix(ctx, uid, "tcdeck_"); err != nil || len(rows) != 0 {
		t.Fatalf("卡清光后卡组物品未清理: n=%d err=%v", len(rows), err)
	}
}

// TestSettleGrantsCardSyncsDeckItem 结算掉卡同事务落卡组计划。官方语义
// （2026-11-08 改）：结算产的自由卡不带卡组变更（deckGUIDs 空）、不自动
// 建卡组物品——进组只能玩家手动指派。
func TestSettleGrantsCardSyncsDeckItem(t *testing.T) {
	srv, db := newDetachTestServer(t)
	ctx := context.Background()
	uid := recoveryUID
	if _, err := db.EnsureProfile(ctx, uid); err != nil {
		t.Fatal(err)
	}
	setCardDropTunable(t, srv, db, "card_drop_common", 1)
	fish := cardTestFish("77777777777777777777777777777777", "carp")
	res, err := srv.settleCatchOnce(ctx, uid, fish, 2)
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || res.cardGUID == "" {
		t.Fatal("掉率 1.0 的结算未产卡")
	}
	if len(res.deckGUIDs) != 0 {
		t.Fatalf("结算自由卡不应带卡组变更: %v", res.deckGUIDs)
	}
	if rows, err := db.ListItemsByItemIDPrefix(ctx, uid, "tcdeck_"); err != nil || len(rows) != 0 {
		t.Fatalf("未指派不应有卡组物品: n=%d err=%v", len(rows), err)
	}
	card, err := db.GetItemByGUID(ctx, res.cardGUID)
	if err != nil {
		t.Fatal(err)
	}
	if cardParentGUID(card) != "" {
		t.Fatal("结算产卡应为自由卡（卡牌列表可见）")
	}
}
