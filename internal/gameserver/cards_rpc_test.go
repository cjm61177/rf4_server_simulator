package gameserver

// 收藏册 27/x RPC 测试：紧凑请求体解析（实机抓包样本）+ 卖卡删卡发银 +
// 摘卡意图（kv pin）阻断自动补位回归 + 指派清理意图。

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"rf4server/internal/logging"
	"rf4server/internal/proto"
	"rf4server/internal/store"
	"rf4server/internal/tunnel"
)

// ---- 请求体构造（紧凑编码，见 cards_rpc.go 注释） ----

func sellBody(guids []string, amount int64) []byte {
	b := []byte{0x02, 0x00, 0x06, 0x00, 0x00}
	b[3] = byte(len(guids))
	b[4] = byte(len(guids) >> 8)
	for _, g := range guids {
		b = append(b, mustHexGUID(g)...)
	}
	b = append(b, 0x11)
	var amt [8]byte
	binary.LittleEndian.PutUint64(amt[:], uint64(amount))
	return append(b, amt[:]...)
}

func mustHexGUID(guid string) []byte {
	b, _ := hex.DecodeString(guid)
	return b
}

func removeBody(guid string) []byte {
	return append([]byte{0x01, 0x00, 0x0c}, mustHexGUID(guid)...)
}

// assignBody 27/2 真实格式（2026-11-08 实抓）：args524[3]
// [0x14 卡组名, 卡组实例 guid（空=0x17 Guid.Empty）, 0x0c 卡 guid]。
func assignBody(deckName, deckGUID, cardGUID string) []byte {
	b := []byte{0x03, 0x03, '5', '2', '4', 0x03, 0x00}
	b = append(b, 0x14, byte(len(deckName)))
	b = append(b, deckName...)
	if deckGUID == "" {
		b = append(b, 0x17)
	} else {
		b = append(b, 0x0c)
		b = append(b, mustHexGUID(deckGUID)...)
	}
	b = append(b, 0x0c)
	return append(b, mustHexGUID(cardGUID)...)
}

// ---- 解析（样本取自 2026-11-07 实机 TRACE，去 5 字节头） ----

func TestParseCardRPCBodies(t *testing.T) {
	guids, amount, ok := parseCardSellBody(mustDecodeHex(t,
		"0200060200c99291ac4c171b73eabc21506e009d89688a4ae4daf13c4c7f981b2cfe2bfd021110a4000000000000"))
	if !ok || len(guids) != 2 || amount != 42000 {
		t.Fatalf("双卡出售解析失败: ok=%v n=%d amount=%d want 42000", ok, len(guids), amount)
	}
	if guids[0] != "c99291ac4c171b73eabc21506e009d89" || guids[1] != "688a4ae4daf13c4c7f981b2cfe2bfd02" {
		t.Fatalf("guid 切片错误: %v", guids)
	}
	_, amount, ok = parseCardSellBody(mustDecodeHex(t,
		"0200060100688a4ae4daf13c4c7f981b2cfe2bfd02113075000000000000"))
	if !ok || amount != 30000 {
		t.Fatalf("单卡出售金额解析错误: %d", amount)
	}
	g, ok := parseCardRemoveBody(mustDecodeHex(t, "01000c520481f4e573dc3faf69ee8a30312bee"))
	if !ok || g != "520481f4e573dc3faf69ee8a30312bee" {
		t.Fatalf("摘卡解析错误: %q ok=%v", g, ok)
	}
	// 生产路径带 args("524") 5B 包装头（ctx.Body 含它，2026-11-07 实机 WARN 实证）
	withHeader := append([]byte{0x03, 0x03, '5', '2', '4'},
		mustDecodeHex(t, "01000c078bfec4e4739d365157987590dc4616")...)
	if g, ok := parseCardRemoveBody(withHeader); !ok || g != "078bfec4e4739d365157987590dc4616" {
		t.Fatalf("带头摘卡解析错误: %q ok=%v", g, ok)
	}
	// 27/2 两份实抓样本（2026-11-08 sid=2）：Guid.Empty 变体与带实例 guid 变体
	name, deck, card, ok := parseCardAssignBody(mustDecodeHex(t,
		"03033532340300140d74636465636b5f6d6f73636f77170cc8a39e986bb2093ea1ebe619d53d1448"))
	if !ok || name != "tcdeck_moscow" || deck != "" || card != "c8a39e986bb2093ea1ebe619d53d1448" {
		t.Fatalf("指派解析错误(Guid.Empty 变体): %q %q %q ok=%v", name, deck, card, ok)
	}
	name, deck, card, ok = parseCardAssignBody(mustDecodeHex(t,
		"03033532340300140b74636465636b5f726172650cd58c2801a4ad5466af6fa1c64f7267ec0c520481f4e573dc3faf69ee8a30312bee"))
	if !ok || name != "tcdeck_rare" || deck != "d58c2801a4ad5466af6fa1c64f7267ec" || card != "520481f4e573dc3faf69ee8a30312bee" {
		t.Fatalf("指派解析错误(实例 guid 变体): %q %q %q ok=%v", name, deck, card, ok)
	}
	// 坏体不 panic、返回 false
	if _, _, ok := parseCardSellBody([]byte{0x02, 0x06}); ok {
		t.Fatal("截断出售体应解析失败")
	}
	if _, ok := parseCardRemoveBody([]byte{0x01}); ok {
		t.Fatal("截断摘卡体应解析失败")
	}
	if _, _, _, ok := parseCardAssignBody([]byte{0x03, 0x00, 0x14, 0x05, 'a'}); ok {
		t.Fatal("截断指派体应解析失败")
	}
	if _, _, _, ok := parseCardAssignBody([]byte{0x02, 0x00, 0x99}); ok {
		t.Fatal("未知类型码应解析失败")
	}
}

func mustDecodeHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ---- 夹具 ----

func grantNCards(t *testing.T, srv *Server, uid int64, species string, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		if _, err := srv.grantCard(ctx, uid, species); err != nil {
			t.Fatal(err)
		}
	}
}

// ownedCardRows 列玩家某卡 item_id 的全部行（created_ts 升序，同 grantCard 顺序）。
func ownedCardRows(t *testing.T, db *store.DB, uid int64, itemID string) []*store.Item {
	t.Helper()
	ctx := context.Background()
	rows, err := db.ListItemsByItemIDPrefix(ctx, uid, cardItemIDPrefix)
	if err != nil {
		t.Fatal(err)
	}
	var out []*store.Item
	for _, r := range rows {
		if r.ItemID == itemID {
			out = append(out, r)
		}
	}
	return out
}

func cardRPCCtx(t *testing.T, srv *Server, sub int, body []byte) (*Ctx, <-chan *proto.Envelope) {
	t.Helper()
	connection, peer := cleanupTestTunnel(t, "card-rpc-test")
	sess := registerBiteTestSession(t, srv, uint64(recoveryUID), connection)
	// 采集全部推送：27/x 成功路径现在会推 4/21/4/22/2/1，不排干会堵死
	// net.Pipe 写端；测试用 findPick 按需取用。
	ch := make(chan *proto.Envelope, 64)
	go func() {
		defer close(ch)
		for {
			kind, _, payload, err := peer.ReadRecord()
			if err != nil {
				return
			}
			if kind != tunnel.TypeData {
				continue
			}
			if env, ok := proto.ParseEnvelope(payload); ok {
				ch <- env
			}
		}
	}()
	return &Ctx{Sess: sess, Main: 27, Sub: sub, ExpectResponse: true,
		Body: body, Log: logging.Discard()}, ch
}

// findPush 等待并返回首个匹配 (main,sub) 的推送，2 秒超时。
func findPush(t *testing.T, ch <-chan *proto.Envelope, main, sub uint8) *proto.Envelope {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("推送 %d/%d 未到达", main, sub)
		}
		select {
		case env, ok := <-ch:
			if !ok {
				t.Fatalf("推送通道已关闭，等待 %d/%d", main, sub)
			}
			if env.Main == main && env.Sub == sub {
				return env
			}
		case <-time.After(remaining):
			t.Fatalf("推送 %d/%d 未到达", main, sub)
		}
	}
}

// mustCardAck 解码 27/x ACK 并校验 pcnefbpgdhl（obj-800）外壳——空 args ACK
// 客户端装不了箱会弹「内部错误」并重发（2026-11-07 实机）。
func mustCardAck(t *testing.T, c *Ctx, body []byte) *proto.Value {
	t.Helper()
	v, _, err := proto.DecodeValue(body, 0, c.Sess.Table())
	if err != nil {
		t.Fatalf("ACK 解码失败: %v", err)
	}
	if v.Kind != proto.KindObj || v.TypeID != 800 || len(v.Fields) != 3 {
		t.Fatalf("ACK 应为 pcnefbpgdhl(obj-800, 3 字段): kind=%v id=%d n=%d",
			v.Kind, v.TypeID, len(v.Fields))
	}
	return v
}

// ---- 业务流 ----

func TestCardSellRPCRemovesCardsAndCredits(t *testing.T) {
	srv, db := newDetachTestServer(t)
	ctx := context.Background()
	uid := recoveryUID
	if _, err := db.EnsureProfile(ctx, uid); err != nil {
		t.Fatal(err)
	}
	grantNCards(t, srv, uid, "carp", 2) // 无自动补位：两张全自由
	rows := ownedCardRows(t, db, uid, "card_carp")
	if len(rows) != 2 {
		t.Fatalf("应有两张卡: %d", len(rows))
	}
	var freeGUID string
	for _, r := range rows {
		if cardParentGUID(r) == "" {
			freeGUID = r.GUID
		}
	}
	if freeGUID == "" {
		t.Fatal("新发的卡应为自由卡")
	}
	price := srv.cardSellPrice(ctx, "card_carp")
	if price <= 0 {
		t.Fatalf("card_carp def 售价缺失: %d", price)
	}
	var before int64
	if err := db.SQL.QueryRow(`SELECT silver FROM profile WHERE uid=?`, uid).Scan(&before); err != nil {
		t.Fatal(err)
	}
	rpcCtx, pushes := cardRPCCtx(t, srv, 1, sellBody([]string{freeGUID, strings.Repeat("ff", 16)}, 1))
	body, handled := hCardSell(rpcCtx)
	if !handled || len(body) == 0 {
		t.Fatal("出售应回 ACK")
	}
	ack := mustCardAck(t, rpcCtx, body)
	// f2=删除 guid 列表：只含有效自由卡（外星 guid 不计）
	f2 := ack.Fields[2].Val
	if f2.Kind != proto.KindGuidArr || len(f2.Items) != 1 || f2.Items[0].Hex != freeGUID {
		t.Fatalf("出售 ACK f2 应只含已售自由卡: kind=%v items=%d", f2.Kind, len(f2.Items))
	}
	// 会话内刷新通道：4/21 移除推送只含已售自由卡
	rmEnv := findPush(t, pushes, 4, 21)
	rmVal, _, err := proto.DecodeValue(rmEnv.Body, 0, rpcCtx.Sess.Table())
	if err != nil {
		t.Fatal(err)
	}
	if got := deepGUIDs(rmVal); len(got) != 1 || got[0] != freeGUID {
		t.Fatalf("出售 4/21 推送应只含已售自由卡: %v", got)
	}
	var silver int64
	if err := db.SQL.QueryRow(`SELECT silver FROM profile WHERE uid=?`, uid).Scan(&silver); err != nil {
		t.Fatal(err)
	}
	if silver-before != price {
		t.Fatalf("入账错误: got=%d delta=%d want=%d（无效 guid 不计价）", silver, silver-before, price)
	}
	if n := cardItemCount(t, db, uid, "card_carp"); n != 1 {
		t.Fatalf("应剩一张: %d", n)
	}
}

func TestCardRemoveRPCBlocksAutoReclaim(t *testing.T) {
	srv, db := newDetachTestServer(t)
	ctx := context.Background()
	uid := recoveryUID
	if _, err := db.EnsureProfile(ctx, uid); err != nil {
		t.Fatal(err)
	}
	grantNCards(t, srv, uid, "carp", 2)
	rows := ownedCardRows(t, db, uid, "card_carp")
	assigned := rows[0].GUID
	// 官方语义：进组只能玩家手动 27/2 指派（无自动补位）
	deckGUID := deckItemGUID(uid, cardTestDeck)
	asCtx, _ := cardRPCCtx(t, srv, 2, assignBody(cardTestDeck, deckGUID, assigned))
	if body, handled := hCardAssign(asCtx); !handled || len(body) == 0 {
		t.Fatal("指派应回 ACK")
	}
	removeCtx, removePushes := cardRPCCtx(t, srv, 3, removeBody(assigned))
	body, handled := hCardRemove(removeCtx)
	if !handled || len(body) == 0 {
		t.Fatal("摘卡应回 ACK")
	}
	removeAck := mustCardAck(t, removeCtx, body)
	// f0=更新物品：卡行必推；该物种无指派副本且有意图 → 卡组行已删，只 1 棵
	f0 := removeAck.Fields[0].Val
	if f0.Kind != proto.KindArgs || len(f0.Items) != 1 {
		t.Fatalf("摘卡 ACK f0 应只含卡行: kind=%v n=%d", f0.Kind, len(f0.Items))
	}
	if f0.Items[0].Class != "dapkdcghfmg" {
		t.Fatalf("摘卡 ACK 元素应为卡物品(dapkdcghfmg): %s", f0.Items[0].Class)
	}
	// 会话内刷新通道：全量重同步的 4/22（先 4/21 后 4/22，见 pushCardAlbumResync）
	upEnv := findPush(t, removePushes, 4, 22)
	upVal, _, err := proto.DecodeValue(upEnv.Body, 0, removeCtx.Sess.Table())
	if err != nil {
		t.Fatal(err)
	}
	foundCard := false
	if upVal.Kind != proto.KindArgs {
		t.Fatalf("4/22 推送应为物品树列表: %v", upVal.Kind)
	}
	for _, it := range upVal.Items {
		if it.Class == "dapkdcghfmg" {
			foundCard = true
		}
	}
	if !foundCard {
		t.Fatalf("4/22 重同步推送应含卡树: n=%d", len(upVal.Items))
	}
	// 卡回自由态
	card, err := db.GetItemByGUID(ctx, assigned)
	if err != nil {
		t.Fatal(err)
	}
	if cardParentGUID(card) != "" {
		t.Fatal("摘卡后父 guid 应为空")
	}
	// 摘卡意图已记
	pins, err := loadCardReleasePins(ctx, db.SQL, uid)
	if err != nil {
		t.Fatal(err)
	}
	if !pins[cardTestDeck+":card_carp"] {
		t.Fatalf("摘卡意图缺失: %v", pins)
	}
	// 该物种无指派副本且有意图 → 卡组行消失
	if n := cardItemCount(t, db, uid, "tcdeck_carp"); n != 0 {
		t.Fatalf("摘卡后卡组行应消失: %d", n)
	}
	// 回归核心：下次任意重建（如再发一张卡）不得复活该物种
	grantNCards(t, srv, uid, "carp", 1)
	card, err = db.GetItemByGUID(ctx, assigned)
	if err != nil {
		t.Fatal(err)
	}
	if cardParentGUID(card) != "" {
		t.Fatal("摘卡意图未阻断自动补位：卡又被指派了")
	}
	if n := cardItemCount(t, db, uid, "tcdeck_carp"); n != 0 {
		t.Fatalf("重建后卡组行不应复活: %d", n)
	}
	// 用户重新指派 → 意图清除、卡组行恢复。用 Guid.Empty 变体（实抓格式：
	// 客户端未缓存卡组行时实例 guid 发空）——顺带回归「自由卡不得被幂等
	// 分支误杀」（2026-11-08 实机 bug：空 wire guid == 空父 guid → 恒 noop）
	assignCtx, _ := cardRPCCtx(t, srv, 2, assignBody(cardTestDeck, "", assigned))
	body, handled = hCardAssign(assignCtx)
	if !handled || len(body) == 0 {
		t.Fatal("指派应回 ACK")
	}
	assignAck := mustCardAck(t, assignCtx, body)
	// f0=更新物品：卡树 + 卡组行树，共 2 棵
	af0 := assignAck.Fields[0].Val
	if af0.Kind != proto.KindArgs || len(af0.Items) != 2 {
		t.Fatalf("指派 ACK f0 应含卡+卡组 2 棵树: kind=%v n=%d", af0.Kind, len(af0.Items))
	}
	card, err = db.GetItemByGUID(ctx, assigned)
	if err != nil {
		t.Fatal(err)
	}
	if cardParentGUID(card) != deckGUID {
		t.Fatalf("指派后父 guid 错误: %q", cardParentGUID(card))
	}
	pins, err = loadCardReleasePins(ctx, db.SQL, uid)
	if err != nil {
		t.Fatal(err)
	}
	if len(pins) != 0 {
		t.Fatalf("指派应清除摘卡意图: %v", pins)
	}
	if n := cardItemCount(t, db, uid, "tcdeck_carp"); n != 1 {
		t.Fatalf("指派后卡组行应恢复: %d", n)
	}
	deckRow, err := db.GetItemByGUID(ctx, deckGUID)
	if err != nil {
		t.Fatal(err)
	}
	fields, err := FieldsFromJSON(deckRow.FieldsJSON)
	if err != nil {
		t.Fatal(err)
	}
	df0 := fields[fieldIdxByCS("ckfmndabaei", "mjbciidhldo")]
	if df0.Val == nil || df0.Val.ArgCount() != 1 {
		t.Fatalf("恢复的卡组 f0 应内嵌 1 棵卡树: %v", df0.Val)
	}
}

// TestCardAlbumQueryServesContainerEnum 登录 4/5(18) 查询的卡树必须带容器
// 枚举 18：右键出售（RUITradingCardCell：容器==18 且未指派）、添加弹框过滤
// 都按该字段判断，f3=0 → 恒灰/恒空（2026-11-08 实机三症状）。
func TestCardAlbumQueryServesContainerEnum(t *testing.T) {
	srv, db := newDetachTestServer(t)
	ctx := context.Background()
	uid := recoveryUID
	if _, err := db.EnsureProfile(ctx, uid); err != nil {
		t.Fatal(err)
	}
	grantNCards(t, srv, uid, "carp", 1)
	rows := ownedCardRows(t, db, uid, "card_carp")
	if len(rows) != 1 {
		t.Fatalf("应有一张卡: %d", len(rows))
	}
	qctx, _ := cardRPCCtx(t, srv, 5, nil)
	qctx.Tree = proto.Args524(proto.U32(18), proto.GuidArr([]string{rows[0].GUID}))
	body, handled := hItemDetails(qctx)
	if !handled || len(body) == 0 {
		t.Fatal("4/5(18) 应回卡树")
	}
	v, _, err := proto.DecodeValue(body, 0, qctx.Sess.Table())
	if err != nil {
		t.Fatal(err)
	}
	if v.Kind != proto.KindArgs || len(v.Items) != 1 {
		t.Fatalf("4/5(18) 应回 1 棵卡树: kind=%v n=%d", v.Kind, len(v.Items))
	}
	card := v.Items[0]
	idx := fieldIdxByCS(card.Class, csSlotEnum)
	if idx < 0 || card.Fields[idx].Num == nil || *card.Fields[idx].Num != 18 {
		t.Fatalf("登录卡树应带容器枚举 18: class=%s idx=%d", card.Class, idx)
	}
}

// TestCardLaminateRPCSealsDeckAndRewards 27/4 塑封：集齐卡组 → f1 密封
// 时间戳落库且重建保留、奖座幂等（ACK 恒带奖座树）、塑封后摘卡被拒。
func TestCardLaminateRPCSealsDeckAndRewards(t *testing.T) {
	srv, db := newDetachTestServer(t)
	ctx := context.Background()
	uid := recoveryUID
	if _, err := db.EnsureProfile(ctx, uid); err != nil {
		t.Fatal(err)
	}
	deck := cardTestDeckDef(t, db)
	deckGUID := deckItemGUID(uid, cardTestDeck)
	for _, c := range deck.Cards { // 每物种一张 + 手动指派（官方语义）→ 集齐
		card, err := srv.grantCard(ctx, uid, strings.TrimPrefix(c, cardItemIDPrefix))
		if err != nil {
			t.Fatalf("发卡 %s: %v", c, err)
		}
		asCtx, _ := cardRPCCtx(t, srv, 2, assignBody(cardTestDeck, deckGUID, card.GUID))
		if body, handled := hCardAssign(asCtx); !handled || len(body) == 0 {
			t.Fatalf("指派 %s 应回 ACK", c)
		}
	}
	lamCtx, _ := cardRPCCtx(t, srv, 4, removeBody(deckGUID)) // 27/4 与 27/3 同形
	body, handled := hCardLaminate(lamCtx)
	if !handled || len(body) == 0 {
		t.Fatal("塑封应回 ACK")
	}
	ack, _, err := proto.DecodeValue(body, 0, lamCtx.Sess.Table())
	if err != nil {
		t.Fatal(err)
	}
	// 外壳 = llgjfoldbcc（obj-809，4 字段）；f1=奖座树列表非空（弹窗展示）
	if ack.Kind != proto.KindObj || ack.TypeID != 809 || len(ack.Fields) != 4 {
		t.Fatalf("塑封 ACK 应为 llgjfoldbcc(obj-809, 4 字段): kind=%v id=%d n=%d",
			ack.Kind, ack.TypeID, len(ack.Fields))
	}
	if f1 := ack.Fields[1].Val; f1.Kind != proto.KindArgs || len(f1.Items) == 0 {
		t.Fatalf("塑封 ACK f1 应含奖座树: kind=%v n=%d", f1.Kind, len(f1.Items))
	}
	if sealedAt := srv.deckSealedAt(ctx, uid, cardTestDeck); sealedAt <= 0 {
		t.Fatalf("塑封后 f1 时间戳应>0: %d", sealedAt)
	}
	if n := cardItemCount(t, db, uid, cardTestStatuette); n != 1 {
		t.Fatalf("塑封后应有 1 座奖座: %d", n)
	}
	// 幂等：重复塑封不报错、不重复发奖座，f1 保留
	lamCtx2, _ := cardRPCCtx(t, srv, 4, removeBody(deckGUID))
	if body, handled = hCardLaminate(lamCtx2); !handled || len(body) == 0 {
		t.Fatal("重复塑封应回 ACK")
	}
	if n := cardItemCount(t, db, uid, cardTestStatuette); n != 1 {
		t.Fatalf("重复塑封不得再发奖座: %d", n)
	}
	// 重建保留：任意卡组同步后 f1 仍在
	if err := srv.syncDecksCommit(ctx, uid); err != nil {
		t.Fatal(err)
	}
	if sealedAt := srv.deckSealedAt(ctx, uid, cardTestDeck); sealedAt <= 0 {
		t.Fatal("卡组重建不得清掉塑封时间戳")
	}
	// 冻结：塑封组内的卡摘不掉
	rows := ownedCardRows(t, db, uid, "card_"+strings.TrimPrefix(deck.Cards[0], cardItemIDPrefix))
	if len(rows) == 0 {
		t.Fatal("应存在指派卡")
	}
	rmCtx, _ := cardRPCCtx(t, srv, 3, removeBody(rows[0].GUID))
	if body, handled = hCardRemove(rmCtx); !handled || len(body) == 0 {
		t.Fatal("塑封组摘卡应回 noop ACK（不报错）")
	}
	if cardParentGUID(rows[0]) == "" {
		card, _ := db.GetItemByGUID(ctx, rows[0].GUID)
		if cardParentGUID(card) == "" {
			t.Fatal("塑封组的卡不得被摘出")
		}
	}
}

func TestCardAssignRPCRejectsForeignDeck(t *testing.T) {
	srv, db := newDetachTestServer(t)
	ctx := context.Background()
	uid := recoveryUID
	if _, err := db.EnsureProfile(ctx, uid); err != nil {
		t.Fatal(err)
	}
	grantNCards(t, srv, uid, "carp", 2)
	rows := ownedCardRows(t, db, uid, "card_carp")
	var free string
	for _, r := range rows {
		if cardParentGUID(r) == "" {
			free = r.GUID
		}
	}
	foreignDeck := strings.Repeat("ab", 16)
	// 外星实例 guid + 表外卡组名：双通道都解析不出 → noop
	noopCtx, _ := cardRPCCtx(t, srv, 2, assignBody("tcdeck_bogus", foreignDeck, free))
	body, handled := hCardAssign(noopCtx)
	if !handled || len(body) == 0 {
		t.Fatal("外星卡组指派应回 noop ACK")
	}
	noopAck := mustCardAck(t, noopCtx, body)
	for i, fld := range noopAck.Fields {
		if fld.Val.Kind != proto.KindNull {
			t.Fatalf("外星卡组 noop ACK 字段 %d 应为 null: %v", i, fld.Val.Kind)
		}
	}
	card, err := db.GetItemByGUID(ctx, free)
	if err != nil {
		t.Fatal(err)
	}
	if cardParentGUID(card) != "" {
		t.Fatal("外星卡组 guid 不得被指派")
	}
}
