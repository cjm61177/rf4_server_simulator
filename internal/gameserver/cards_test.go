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
	srv, _ := newDetachTestServer(t)
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
// feaidppnpch）的 f9 必须带卡组数组（args code 340），有卡的卡组 f1>0 且
// f0 卡树齐全，空卡组 f1==0。
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
	if decks == nil || decks.ArgCodeStr() != "340" || decks.ArgCount() == 0 {
		t.Fatalf("f9 应为非空 340 卡组数组: tree=%v", decks)
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
	if cards == nil || cards.ArgCodeStr() != "61" || cards.ArgCount() != 2 {
		t.Fatalf("目标卡组 f0 应含 2 棵卡树: tree=%v", cards)
	}
	for i := 0; i < cards.ArgCount(); i++ {
		if cls := cards.ArgItem(i).Class; cls != "dapkdcghfmg" {
			t.Fatalf("卡树元素类错误: %s", cls)
		}
	}
	if started := target.Field(1); started.Num == nil || *started.Num == 0 {
		t.Fatalf("有卡卡组 f1 mkkhpfgnjei 应 >0: %+v", started.Num)
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
	if cards := emptyF.Tree(); cards == nil || cards.ArgCodeStr() != "61" || cards.ArgCount() != 0 {
		t.Fatalf("空卡组 f0 应为空 61 数组: tree=%v", cards)
	}
}
