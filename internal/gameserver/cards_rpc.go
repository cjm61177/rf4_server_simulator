package gameserver

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"rf4server/internal/proto"
	"rf4server/internal/store"
	"rf4server/internal/wiregen"
)

// 集换式卡牌 RPC（27 命令族）：客户端收藏册服务的卖卡/指派/摘卡。
//
// 命令号三重实锤（2026-11-07）：①实机抓包载荷与卡 guid/def 售价精确吻合；
// ②capstone 反汇编 send 0x1818740c0 调用点立即数（sell@0x603d9c sub=1、
// assign@0x5f9bbc sub=2、remove@0x60a747 sub=3，均 mov r9b,0x1b）；③
// script.json 方法地址。旧实现把 27/1-3 当 RemoteSettings 回容器快照——
// 客户端成功回调 kbpadkaejea 对响应体 pResult.Invoke(true,0) 全盘无视，
// 弹「已售出金额」但服务端没删卡没发钱（假成功，2026-11-07 实机四症状之二）。
//
// 请求体是紧凑编码（与响应的 proto marker 编码不同，DecodeValue 解不动）：
// args("524") 头后 [参数个数 u16LE] + 每参数 [u8 类型码][载荷]——
//   06 <u16LE n> <n×16B 裸 guid>   guid 数组（27/1 首参）
//   11 <8B LE u64>                 整数（27/1 次参=客户端金额）
//   0c <16B 裸 guid>               单 guid（27/3 唯一参数）
//   14 <u8 len> <ASCII>            字符串（27/2 首参=卡组 def 名）
//   17                             Guid.Empty（27/2 次参可空=客户端未缓存卡组行，
//                                  2026-11-08 实抓样本实证，见 parseCardAssignBody）
// 27/2 参数序 = (卡组名, 卡组实例 guid 可空, 卡 guid)。
//
// wire guid 字节序 = DB guid hex 串原序（不换字节）：实机 27/3 载荷
// c189c547… 与 card_alb_barbel 行 guid 完全一致。
//
// 响应类型 = pcnefbpgdhl（wire 类 dlbahppbdad / id 800，字段 VAL×3）。客户端
// 链实锤（2026-11-07 反编译）：RPC 成功续延 idmiblakefe.cpgkalnjigd(r) → 主线
// 程 hfnjdfadmbd → eegjkjenhoe.eiihgcpdpbb(r)：f0/f1（onobdmohahp[]）逐个
// upsert（ppcleebjjpk），f2（Guid[]）逐个按 guid 删除（cgjfehafkdf），null/空
// 数组跳过；应用完才 pSuccess.Invoke()。
// 旧版空 ACK（args("524") 空列表 03 03 '5' '2' '4' 00 00）解码出 list 而非
// pcnefbpgdhl → 走错误回调 → 客户端弹「错误发生内部错误」并于 6 秒后重发同
// 一请求（实机 23:07:32 出售成功→23:07:38 重发 call=76 报无有效卡）。形状
// 先例：3/31 hRigSyncRig 回 (null, 物品列表, Guid[]) 同为 800 已被客户端接受。
// 失败无信号通道（框架层错误才会进错误回调），故校验失败只记日志照常 ACK。
//
// 回体 ACK 的 obj-800 应用链只写中央物品字典，收藏册页面（容器 18 管理器
// ibaammebeej 的 guid 集 + 视图）不随之重建——2026-11-08 实机：摘卡/卖卡/
// 指派后台全生效但页面停留旧态，弹「添加」框也因客户端父 guid 陈旧而恒空。
// 客户端 4/x 推送分发（aedpdgpbcho，main=4）：sub=21 按 guid 删（对已存在
// 物品可靠生效），sub=22 批量 upsert——但 upsert 不替换本地库中已存在的
// 物品对象（同会话摘卡后重开页面仍显旧归属；卖卡走 21 却立即生效）。可靠
// 的会话内同步 = 先 4/21 删再 4/22 重推全量（新物品入柜路径可靠：管理发卡
// 即时可见；搏鱼恢复 Push4_22Items 注释同款「移除后重装回」模式），见
// pushCardAlbumResync。银币入账另推 2/1。

// tcardFreeKVKey 收藏册「用户摘卡意图」kv 键。值 = JSON
// {"<deckID>:<cardItemID>": true}。官方语义：指派/摘卡是用户显式动作，
// 摘掉的物种不得被 prepareDeckItems 的自动补位复活（否则移除按钮等于
// 失效）；用户指派过的卡组同理不得自动重选其它副本（会与手选副本冲突
// 形成双指派）。存 kv 免迁移（kv 表 00004_runtime）。
func tcardFreeKVKey(uid int64) string { return fmt.Sprintf("tcard-free:%d", uid) }

// loadCardReleasePins 读摘卡意图集（kv JSON；缺行/坏 JSON 视为空集）。
// q 可传 s.DB.SQL（事务外）或 *sql.Tx（事务内），两者都实现 queryRowContext。
func loadCardReleasePins(ctx context.Context, q queryRowContext, uid int64) (map[string]bool, error) {
	out := map[string]bool{}
	var raw string
	err := q.QueryRowContext(ctx,
		`SELECT v FROM kv WHERE k=?`, tcardFreeKVKey(uid)).Scan(&raw)
	if err != nil {
		if err == sql.ErrNoRows {
			return out, nil
		}
		return nil, err
	}
	if raw == "" {
		return out, nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return out, nil // 坏值不致命：视作无意图，行为退回自动补位
	}
	for k, v := range m {
		if b, ok := v.(bool); ok && b {
			out[k] = true
		}
	}
	return out, nil
}

// setCardReleasePinTx 事务内改一条摘卡意图；pinned=false 且无剩余条目时
// 删整行，避免 kv 无限累积。
func setCardReleasePinTx(ctx context.Context, tx *sql.Tx, uid int64,
	deckID, cardItemID string, pinned bool) error {
	key := deckID + ":" + cardItemID
	pins, err := loadCardReleasePins(ctx, tx, uid)
	if err != nil {
		return err
	}
	if pinned {
		pins[key] = true
	} else {
		delete(pins, key)
	}
	if len(pins) == 0 {
		_, err = tx.ExecContext(ctx, `DELETE FROM kv WHERE k=?`, tcardFreeKVKey(uid))
		return err
	}
	b, err := json.Marshal(pins)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO kv(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`,
		tcardFreeKVKey(uid), string(b))
	return err
}


// ---- 紧凑请求体解析 ----

// stripArgsHeader 剥请求体头部的 args("524") 包装（03 03 '5' '2' '4'，
// env.Body 含它；参数序列在其后）。无头（测试直传）原样返回。
func stripArgsHeader(body []byte) []byte {
	if len(body) >= 5 && body[0] == 0x03 && body[1] == 0x03 &&
		body[2] == '5' && body[3] == '2' && body[4] == '4' {
		return body[5:]
	}
	return body
}

// parseCardSellBody 27/1：[5B 头] 02 00 06 <n u16LE> <n×16B guid> 11 <8B 金额>。
// （实抓布局：marker 02 + {0x00, 元素码} + 计数 LE，2026-11-07 样本）
func parseCardSellBody(body []byte) (guids []string, clientAmount int64, ok bool) {
	body = stripArgsHeader(body)
	if len(body) < 6 || body[0] != 0x02 || body[1] != 0x00 || body[2] != 0x06 {
		return nil, 0, false
	}
	n := int(binary.LittleEndian.Uint16(body[3:5]))
	pos := 5
	if len(body) < pos+n*16 {
		return nil, 0, false
	}
	guids = make([]string, 0, n)
	for i := 0; i < n; i++ {
		guids = append(guids, hex.EncodeToString(body[pos:pos+16]))
		pos += 16
	}
	if len(body) == pos+9 && body[pos] == 0x11 {
		clientAmount = int64(binary.LittleEndian.Uint64(body[pos+1 : pos+9]))
	}
	return guids, clientAmount, true
}

// parseCardRemoveBody 27/3：[5B 头] 01 00 0c <16B guid>。
func parseCardRemoveBody(body []byte) (string, bool) {
	body = stripArgsHeader(body)
	if len(body) != 19 || body[0] != 0x01 || body[1] != 0x00 || body[2] != 0x0c {
		return "", false
	}
	return hex.EncodeToString(body[3:19]), true
}

// parseCardAssignBody 27/2 指派请求：args("524") 头后 [n u16LE] + n 个类型化
// 参数 [u8 码][值]。码表（实抓两样本 2026-11-08 实证）：0x14=字符串(u8 长+
// ASCII，卡组 def 名)、0x0c=guid(16B)、0x17=Guid.Empty。参数序 =
// (卡组 def 名, 卡组实例 guid 可空, 卡 guid)——客户端未缓存卡组行时实例
// guid 发 Guid.Empty（样本1 "tcdeck_moscow"+0x17；样本2 "tcdeck_rare"+
// 实例 guid d58c2801… 与 DB 行吻合）。27/1 的 06/11 与 27/3 的 0c 同族。
func parseCardAssignBody(body []byte) (deckName, deckGUID, cardGUID string, ok bool) {
	body = stripArgsHeader(body)
	if len(body) < 2 {
		return "", "", "", false
	}
	n := int(binary.LittleEndian.Uint16(body[0:2]))
	pos := 2
	var guids []string
	for i := 0; i < n; i++ {
		if pos >= len(body) {
			return "", "", "", false
		}
		switch body[pos] {
		case 0x14: // 字符串
			pos++
			if pos >= len(body) {
				return "", "", "", false
			}
			l := int(body[pos])
			pos++
			if pos+l > len(body) {
				return "", "", "", false
			}
			deckName = string(body[pos : pos+l])
			pos += l
		case 0x0c: // guid
			pos++
			if pos+16 > len(body) {
				return "", "", "", false
			}
			guids = append(guids, hex.EncodeToString(body[pos:pos+16]))
			pos += 16
		case 0x17: // Guid.Empty（可空卡组实例 guid）
			pos++
			guids = append(guids, "")
		case 0x11: // long，跳过（防御未知位）
			pos++
			if pos+8 > len(body) {
				return "", "", "", false
			}
			pos += 8
		default:
			return "", "", "", false
		}
	}
	if len(guids) != 2 {
		return "", "", "", false
	}
	return deckName, guids[0], guids[1], true
}

// resolveDeckID 27/2 双通道解析卡组：实例 guid 优先（uuid5 派生自 (uid,
// deckID)，权威）；为空（客户端未缓存卡组行发 Guid.Empty）回退 def 名——
// DeckID 即卡组行 item_id（"tcdeck_<名>"，cardTestDeck 同款），须在静态
// 卡组表内（防任意名注入）。
func (s *Server) resolveDeckID(ctx context.Context, uid int64, deckGUID, deckName string) string {
	if deckGUID != "" {
		if id := s.deckIDByGUID(ctx, uid, deckGUID); id != "" {
			return id
		}
	}
	if deckName == "" {
		return ""
	}
	decks, err := loadCardDecks(ctx, s.DB)
	if err != nil {
		return ""
	}
	for _, d := range decks {
		if d.DeckID == deckName {
			return deckName
		}
	}
	return ""
}

// cardParentGUID 读物品行父 guid（odflendafcg OPTGUID）；未设置返回 ""。
func cardParentGUID(it *store.Item) string {
	fields, err := FieldsFromJSON(it.FieldsJSON)
	if err != nil {
		return ""
	}
	if idx := fieldIdxByCS(it.WireClass, "odflendafcg"); idx >= 0 && idx < len(fields) {
		return strings.ToLower(fields[idx].Hex)
	}
	return ""
}

// deckSealedField 读卡组行的塑封时间戳（f1 mkkhpfgnjei，.NET ticks；
// >0=已塑封）。非卡组行/缺字段返回 0。
func deckSealedField(row *store.Item) int64 {
	if row == nil {
		return 0
	}
	fields, err := FieldsFromJSON(row.FieldsJSON)
	if err != nil {
		return 0
	}
	if idx := fieldIdxByCS(row.WireClass, "mkkhpfgnjei"); idx >= 0 && idx < len(fields) && fields[idx].Num != nil {
		return int64(*fields[idx].Num)
	}
	return 0
}

// deckSealedAt 按卡组 id 读当前塑封时间戳；行不存在返回 0。
func (s *Server) deckSealedAt(ctx context.Context, uid int64, deckID string) int64 {
	row, err := s.DB.GetItemByGUID(ctx, deckItemGUID(uid, deckID))
	if err != nil || row == nil {
		return 0
	}
	return deckSealedField(row)
}

// deckIDByGUID 从静态卡组 def 反查 deckID（deckItemGUID 是 uuid5 派生，与
// 卡组行是否存在无关——摘掉唯一指派卡后重建会把行删掉，但指派/摘卡仍须
// 可用）。查不到返回 ""。
func (s *Server) deckIDByGUID(ctx context.Context, uid int64, deckGUID string) string {
	decks, err := loadCardDecks(ctx, s.DB)
	if err != nil {
		return ""
	}
	for _, d := range decks {
		if deckItemGUID(uid, d.DeckID) == deckGUID {
			return d.DeckID
		}
	}
	return ""
}

// cardSellPrice 查卡 def 售价（obcephkkfhi，静态属性）；缺表回 0。
func (s *Server) cardSellPrice(ctx context.Context, itemID string) int64 {
	def, err := s.DB.GetItemDefByItemID(ctx, itemID)
	if err != nil || def == nil {
		return 0
	}
	v, _ := defAttrU64(def, "obcephkkfhi")
	return int64(v)
}

// syncDecksCommit 独立短事务落地卡组同步计划。调用方需先把卡片行变更提交
// （prepareDeckItems 读已提交状态）；SQLite 单连接纪律，静态库查询不进事务。
func (s *Server) syncDecksCommit(ctx context.Context, uid int64) error {
	plan, err := s.prepareDeckItems(ctx, uid)
	if err != nil {
		return err
	}
	tx, err := s.DB.SQL.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := syncDeckItemsTx(ctx, tx, uid, plan); err != nil {
		return err
	}
	return tx.Commit()
}

// ---- 27/x 响应构造（pcnefbpgdhl / obj-800）----

// cardAckNoop 全空 800：无本地变更的成功 ACK（幂等、解析失败、无有效卡等
// 路径——该 RPC 无失败信号通道，只能成功但不推任何变更）。
func cardAckNoop(table *proto.OpsTable) []byte {
	body, err := proto.EncodeValue(build800(proto.Null(), proto.Null(), proto.Null()), table)
	if err != nil {
		return emptyArgsBody() // 固定形状不可编码只可能是 wiregen 表损坏；退回空 ACK
	}
	return body
}

// cardAckGUIDs f2=删除 guid 列表（卖卡）：客户端按 guid 逐个本地移除
// （cgjfehafkdf），卡片即时从收藏册消失，无需重开页面。
func cardAckGUIDs(table *proto.OpsTable, guids []string) []byte {
	body, err := proto.EncodeValue(build800(proto.Null(), proto.Null(), proto.GuidArr(guids)), table)
	if err != nil {
		return emptyArgsBody()
	}
	return body
}

// cardWireTree18 卡/卡组行的发送树副本（容器枚举 18 回填，见
// patchNativeContainer 注释）。ACK 与 4/22 推送共用。
func (s *Server) cardWireTree18(ctx context.Context, row *store.Item) *proto.Value {
	if row == nil {
		return nil
	}
	tree, err := s.ItemToTree(ctx, row)
	if err != nil || tree == nil {
		return nil
	}
	patchNativeContainer(row, tree)
	return tree
}

// cardAckAfterCardOp 摘卡/指派成功后的增量 ACK：f0 = 更新后的卡行 + 受影响
// 卡组行（行可能已被重建或删空，取不到就跳过）。卡组行重建发生在
// syncDecksCommit 之后，因此这里读的是已提交终态。
func (s *Server) cardAckAfterCardOp(ctx context.Context, uid int64, table *proto.OpsTable,
	card *store.Item, deckIDs ...string) []byte {
	trees := proto.Args(proto.S("61"))
	if tree := s.cardWireTree18(ctx, card); tree != nil {
		trees.Items = append(trees.Items, tree)
	}
	for _, id := range deckIDs {
		if id == "" {
			continue
		}
		if row, err := s.DB.GetItemByGUID(ctx, deckItemGUID(uid, id)); err == nil && row != nil {
			if tree := s.cardWireTree18(ctx, row); tree != nil {
				trees.Items = append(trees.Items, tree)
			}
		}
	}
	body, err := proto.EncodeValue(build800(trees, proto.Null(), proto.Null()), table)
	if err != nil {
		return emptyArgsBody()
	}
	return body
}

// Push4_22CardTrees18 4/22 推送卡/卡组树（容器 18 发送副本，见 cardWireTree18
// 注释）。空列表忽略。
func (s *Session) Push4_22CardTrees18(guids []string) {
	pctx := s.pushCtx()
	var trees []*proto.Value
	seen := map[string]bool{}
	for _, g := range guids {
		if g == "" || seen[g] {
			continue
		}
		seen[g] = true
		row, err := s.Srv.DB.GetItemByGUID(pctx, g)
		if err != nil || row == nil || row.UID != s.Userid {
			continue
		}
		if tree := s.Srv.cardWireTree18(pctx, row); tree != nil {
			trees = append(trees, tree)
		}
	}
	if len(trees) > 0 {
		s.Push(4, 22, proto.Args(proto.S("61"), trees...))
	}
}

// pushCardAlbumResync 收藏册全量重同步：玩家全部卡/卡组行先 4/21 从客户端
// 本地库移除、再 4/22 重推全量树（容器 18 副本）。为什么绕圈：客户端对已
// 存在物品的 upsert 不替换本地对象（2026-11-08 实机，见文件头注释），先删
// 后推迫使按新物品路径重新入柜。卡组行 guid 由 uuid5 派生恒定，重建后 guid
// 不变，同样必须走先删后推。注意：单次调用只推进「一代」——卡牌列表视图
// 重建滞后一代（见 progress_ops 掉卡推送处的跨帧第二代说明）。
func (s *Server) pushCardAlbumResync(sess *Session, uid int64) {
	sess.pushCardAlbumResync(uid)
}

func (sess *Session) pushCardAlbumResync(uid int64) {
	ctx := sess.pushCtx()
	guids := sess.Srv.deckItemGUIDs(ctx, uid)
	if rows, err := sess.Srv.DB.ListItemsByItemIDPrefix(ctx, uid, cardItemIDPrefix); err == nil {
		for _, r := range rows {
			guids = append(guids, r.GUID)
		}
	}
	if len(guids) == 0 {
		return
	}
	sess.Push4_21(guids)
	sess.Push4_22CardTrees18(guids)
}

// hCardSell 27/1 卖卡：校验归属（无卡组状态限制——客户端出售入口已过滤
// 已归属卡，能到这里的就是用户要卖的），事务内删卡+按 def 售价发银。
// 客户端报的金额只作对账日志，金额以服务端 def 价为准（防作弊口径）。
// 部分无效 guid 跳过并记 WARN（该 RPC 无失败信号通道，照常 ACK）。
func hCardSell(ctx *Ctx) ([]byte, bool) {
	srv := ctx.Sess.Srv
	if !srv.requireDB(ctx) {
		return nil, false
	}
	uid := ctx.Sess.Userid
	guids, clientAmount, ok := parseCardSellBody(ctx.Body)
	if !ok {
		ctx.Log.Warn("收藏册出售请求解析失败", "hex", hex.EncodeToString(ctx.Body))
		return cardAckNoop(ctx.Sess.Table()), true
	}
	if len(guids) == 0 {
		return cardAckNoop(ctx.Sess.Table()), true
	}
	cctx := ctx.Sess.pushCtx()
	// 静态库查价不进事务（SQLite 单连接纪律）
	total := int64(0)
	sellable := make([]string, 0, len(guids))
	priceCache := map[string]int64{}
	for _, g := range guids {
		row, err := srv.DB.GetItemByGUID(cctx, g)
		if err != nil || row.UID != uid || !strings.HasPrefix(row.ItemID, cardItemIDPrefix) {
			ctx.Log.Warn("收藏册出售跳过无效卡", "guid", g, "err", fmt.Sprint(err))
			continue
		}
		price, cached := priceCache[row.ItemID]
		if !cached {
			price = srv.cardSellPrice(cctx, row.ItemID)
			priceCache[row.ItemID] = price
		}
		if price <= 0 {
			ctx.Log.Warn("收藏册出售跳过无价卡", "item", row.ItemID, "guid", g)
			continue
		}
		sellable = append(sellable, g)
		total += price
	}
	if len(sellable) == 0 {
		ctx.Log.Error("收藏册出售无有效卡", "uid", uid, "want", len(guids))
		return cardAckNoop(ctx.Sess.Table()), true
	}
	if clientAmount != total {
		ctx.Log.Info("收藏册出售金额与客户端报价不一致（以服务端为准）",
			"uid", uid, "server", total, "client", clientAmount)
	}
	tx, err := beginEconomyTx(cctx, srv.DB)
	if err != nil {
		ctx.Log.Error("收藏册出售开事务失败", "err", err)
		return nil, false
	}
	defer tx.Rollback()
	if _, err := removeItemsTx(cctx, tx, uid, sellable); err != nil {
		ctx.Log.Error("收藏册出售删卡失败", "err", err)
		return nil, false
	}
	if err := applyWalletTx(cctx, tx, uid, total, 0, "sell_card", "", "",
		fmt.Sprintf("收藏册出售 %d 张", len(sellable))); err != nil {
		ctx.Log.Error("收藏册出售入账失败", "err", err)
		return nil, false
	}
	if err := tx.Commit(); err != nil {
		ctx.Log.Error("收藏册出售提交失败", "err", err)
		return nil, false
	}
	// 被卖卡若占着卡组槽位：重建卡组行（无自动补位，槽位空则该物种离组）
	if err := srv.syncDecksCommit(cctx, uid); err != nil {
		ctx.Log.Error("收藏册出售后卡组重建失败", "err", err)
	}
	// 已售卡在库中已删、不在重同步清单里，单独 4/21 移除；其余卡/卡组先删
	// 后推全量重同步（见 pushCardAlbumResync 注释）；银币入账走 2/1
	ctx.Sess.Push4_21(sellable)
	srv.pushCardAlbumResync(ctx.Sess, uid)
	ctx.Sess.Push2_1()
	ctx.Log.Info("收藏册出售完成", "uid", uid, "cards", len(sellable), "silver", total)
	return cardAckGUIDs(ctx.Sess.Table(), sellable), true
}

// hCardRemove 27/3 摘卡：卡回自由态（父 guid 清空+bit4 清除）并记摘卡
// 意图（防 prepareDeckItems 自动补位复活），再重建卡组行。静态库查卡组
// def 不进事务：先读后写两段式。
func hCardRemove(ctx *Ctx) ([]byte, bool) {
	srv := ctx.Sess.Srv
	if !srv.requireDB(ctx) {
		return nil, false
	}
	uid := ctx.Sess.Userid
	guid, ok := parseCardRemoveBody(ctx.Body)
	if !ok {
		ctx.Log.Warn("收藏册摘卡请求解析失败", "hex", hex.EncodeToString(ctx.Body))
		return cardAckNoop(ctx.Sess.Table()), true
	}
	cctx := ctx.Sess.pushCtx()
	card, err := srv.DB.GetItemByGUID(cctx, guid)
	if err != nil || card.UID != uid {
		ctx.Log.Warn("收藏册摘卡卡不存在", "uid", uid, "guid", guid, "err", fmt.Sprint(err))
		return cardAckNoop(ctx.Sess.Table()), true
	}
	if !strings.HasPrefix(card.ItemID, cardItemIDPrefix) {
		ctx.Log.Warn("收藏册摘卡目标不是卡", "item", card.ItemID, "guid", guid)
		return cardAckNoop(ctx.Sess.Table()), true
	}
	parent := cardParentGUID(card)
	if parent == "" {
		return cardAckNoop(ctx.Sess.Table()), true // 已是自由卡：幂等 ACK
	}
	deckID := srv.deckIDByGUID(cctx, uid, parent)
	if deckID == "" {
		ctx.Log.Warn("收藏册摘卡父 guid 不对应已知卡组（仅放卡，未记意图）", "parent", parent)
	}
	if deckID != "" && srv.deckSealedAt(cctx, uid, deckID) > 0 {
		// 塑封组冻结：密封快照（f0）与指派都不得变动
		ctx.Log.Warn("收藏册摘卡被拒：卡组已塑封", "uid", uid, "deck", deckID)
		return cardAckNoop(ctx.Sess.Table()), true
	}
	if err := releaseCardToFree(card); err != nil {
		ctx.Log.Error("收藏册摘卡清字段失败", "err", err)
		return nil, false
	}
	tx, err := beginEconomyTx(cctx, srv.DB)
	if err != nil {
		ctx.Log.Error("收藏册摘卡开事务失败", "err", err)
		return nil, false
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(cctx,
		`UPDATE item SET fields_json=? WHERE uid=? AND guid=?`,
		card.FieldsJSON, uid, card.GUID); err != nil {
		ctx.Log.Error("收藏册摘卡落库失败", "err", err)
		return nil, false
	}
	if deckID != "" {
		if err := setCardReleasePinTx(cctx, tx, uid, deckID, card.ItemID, true); err != nil {
			ctx.Log.Error("收藏册摘卡意图落库失败", "err", err)
			return nil, false
		}
	}
	if err := tx.Commit(); err != nil {
		ctx.Log.Error("收藏册摘卡提交失败", "err", err)
		return nil, false
	}
	if err := srv.syncDecksCommit(cctx, uid); err != nil {
		ctx.Log.Error("收藏册摘卡后卡组重建失败", "err", err)
	}
	// ACK 的 f0 upsert 不驱动收藏册刷新（见文件头注释）：先删后推全量重同步
	srv.pushCardAlbumResync(ctx.Sess, uid)
	ctx.Log.Info("收藏册摘卡完成", "uid", uid, "card", card.ItemID, "deck", deckID)
	return srv.cardAckAfterCardOp(cctx, uid, ctx.Sess.Table(), card, deckID), true
}

// hCardAssign 27/2 指派：卡进卡组（父 guid=卡组实例 guid+bit4），清该
// 卡组的摘卡意图（用户重新指派=意图撤销），再重建卡组行。
func hCardAssign(ctx *Ctx) ([]byte, bool) {
	srv := ctx.Sess.Srv
	if !srv.requireDB(ctx) {
		return nil, false
	}
	uid := ctx.Sess.Userid
	ctx.Log.Info("收藏册指派请求", "hex", hex.EncodeToString(ctx.Body))
	deckName, deckGUID, cardGUID, ok := parseCardAssignBody(ctx.Body)
	if !ok {
		ctx.Log.Warn("收藏册指派请求解析失败", "hex", hex.EncodeToString(ctx.Body))
		return cardAckNoop(ctx.Sess.Table()), true
	}
	cctx := ctx.Sess.pushCtx()
	card, err := srv.DB.GetItemByGUID(cctx, cardGUID)
	if err != nil || card.UID != uid {
		ctx.Log.Warn("收藏册指派卡不存在", "uid", uid, "guid", cardGUID, "err", fmt.Sprint(err))
		return cardAckNoop(ctx.Sess.Table()), true
	}
	if !strings.HasPrefix(card.ItemID, cardItemIDPrefix) {
		ctx.Log.Warn("收藏册指派目标不是卡", "item", card.ItemID, "guid", cardGUID)
		return cardAckNoop(ctx.Sess.Table()), true
	}
	deckID := srv.resolveDeckID(cctx, uid, deckGUID, deckName)
	if deckID == "" {
		ctx.Log.Warn("收藏册指派卡组不存在", "uid", uid, "deck", deckGUID, "name", deckName)
		return cardAckNoop(ctx.Sess.Table()), true
	}
	// 后续一律用解析出的卡组实例 guid：wire 次参可能是 Guid.Empty（客户端
	// 未缓存卡组行），拿原始值比对会把自由卡误判成「已在卡组」幂等跳过
	// （2026-11-08 实机 00:51：解析成功却恒回 noop）。
	deckInstGUID := deckItemGUID(uid, deckID)
	if srv.deckSealedAt(cctx, uid, deckID) > 0 {
		// 塑封组冻结：不得再指派新卡（客户端入口 kMessage_Confirmation_
		// TradingCardAddToLaminatedDeck 只是确认框，服务端语义=拒绝）
		ctx.Log.Warn("收藏册指派被拒：卡组已塑封", "uid", uid, "deck", deckID)
		return cardAckNoop(ctx.Sess.Table()), true
	}
	if cur := cardParentGUID(card); cur == deckInstGUID {
		return cardAckNoop(ctx.Sess.Table()), true // 已在该卡组：幂等 ACK
	}
	oldDeckID := ""
	if old := cardParentGUID(card); old != "" {
		// 客户端守卫只允许指派自由卡，走到这=状态漂移：按改指派处理，
		// 对旧卡组记摘卡意图（WARN 留证，意图随下方同一事务落库）
		ctx.Log.Warn("收藏册指派卡已有归属（按改指派处理）", "old", old, "new", deckInstGUID)
		oldDeckID = srv.deckIDByGUID(cctx, uid, old)
		if oldDeckID != "" && srv.deckSealedAt(cctx, uid, oldDeckID) > 0 {
			// 塑封组冻结：成员卡不得改派
			ctx.Log.Warn("收藏册指派被拒：旧卡组已塑封", "uid", uid, "deck", oldDeckID)
			return cardAckNoop(ctx.Sess.Table()), true
		}
	}
	if err := assignCardToDeck(card, deckInstGUID); err != nil {
		ctx.Log.Error("收藏册指派改字段失败", "err", err)
		return nil, false
	}
	tx, err := beginEconomyTx(cctx, srv.DB)
	if err != nil {
		ctx.Log.Error("收藏册指派开事务失败", "err", err)
		return nil, false
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(cctx,
		`UPDATE item SET fields_json=? WHERE uid=? AND guid=?`,
		card.FieldsJSON, uid, card.GUID); err != nil {
		ctx.Log.Error("收藏册指派落库失败", "err", err)
		return nil, false
	}
	if oldDeckID != "" {
		if err := setCardReleasePinTx(cctx, tx, uid, oldDeckID, card.ItemID, true); err != nil {
			ctx.Log.Error("收藏册指派旧意图落库失败", "err", err)
			return nil, false
		}
	}
	if err := setCardReleasePinTx(cctx, tx, uid, deckID, card.ItemID, false); err != nil {
		ctx.Log.Error("收藏册指派意图清理失败", "err", err)
		return nil, false
	}
	if err := tx.Commit(); err != nil {
		ctx.Log.Error("收藏册指派提交失败", "err", err)
		return nil, false
	}
	if err := srv.syncDecksCommit(cctx, uid); err != nil {
		ctx.Log.Error("收藏册指派后卡组重建失败", "err", err)
	}
	// 同摘卡：先删后推全量重同步（见 pushCardAlbumResync 注释）
	srv.pushCardAlbumResync(ctx.Sess, uid)
	ctx.Log.Info("收藏册指派完成", "uid", uid, "card", card.ItemID, "deck", deckID)
	return srv.cardAckAfterCardOp(cctx, uid, ctx.Sess.Table(), card, deckID, oldDeckID), true
}

// hCardLaminate 27/4 塑封卡组。请求与 27/3 同形（args524[1][0x0c 卡组实例
// guid]）；响应类型 llgjfoldbcc（wire gglppphenoi/809 = pcnefbpgdhl 派生加
// f0 plhpmhpadcg[] 奖励描述数组），四值序 (f0 描述, f1 物品树, f2 备用树,
// f3 Guid[])。命令号与四值序反汇编/解码器实锤：0x1805f994d
// mov byte[rsp+0x20],4（sub=4）+ mov r9b,0x1b；gglppphenoi.plphdfehkjl
// 依次读 plhpmhpadcg[]→items[]→items[]→Guid[]。客户端奖励弹窗
// （blehdkolhfl）吃 f1 物品树，响应先经 eiihgcpdpbb 应用再 pSuccess(r)。
// 语义：集齐（每物种都有指派进本卡组的副本）才塑封；f1 密封时间戳
// （.NET ticks）落卡组行 DB，prepareDeckItems 重建保留（见 cards.go sealed
// 表）；奖座（def 属性 pnaapkplkfo）经 checkCardDecks 幂等补发——发卡/
// 结算路径的自动发放若已发过则只回树不重发。
func hCardLaminate(ctx *Ctx) ([]byte, bool) {
	srv := ctx.Sess.Srv
	if !srv.requireDB(ctx) {
		return nil, false
	}
	uid := ctx.Sess.Userid
	guid, ok := parseCardRemoveBody(ctx.Body) // 27/4 与 27/3 同形：单 guid
	if !ok {
		ctx.Log.Warn("收藏册塑封请求解析失败", "hex", hex.EncodeToString(ctx.Body))
		return cardAckLaminate(ctx.Sess.Table(), nil), true
	}
	cctx := ctx.Sess.pushCtx()
	deckID := srv.deckIDByGUID(cctx, uid, guid)
	if deckID == "" {
		ctx.Log.Warn("收藏册塑封卡组不存在", "uid", uid, "deck", guid)
		return cardAckLaminate(ctx.Sess.Table(), nil), true
	}
	var deck *cardDeck
	if decks, err := loadCardDecks(cctx, srv.DB); err == nil {
		for _, d := range decks {
			if d.DeckID == deckID {
				deck = d
				break
			}
		}
	}
	if deck == nil {
		ctx.Log.Warn("收藏册塑封卡组定义缺失", "uid", uid, "deck", deckID)
		return cardAckLaminate(ctx.Sess.Table(), nil), true
	}
	// 集齐校验：每物种都有指派进本卡组的副本（客户端只在集齐时给塑封入口，
	// 服务端复核防状态漂移）
	rows, err := srv.DB.ListItemsByItemIDPrefix(cctx, uid, cardItemIDPrefix)
	if err != nil {
		return nil, false
	}
	deckGUID := deckItemGUID(uid, deckID)
	assigned := map[string]bool{}
	for _, r := range rows {
		if cardParentGUID(r) == deckGUID {
			assigned[r.ItemID] = true
		}
	}
	for _, cid := range deck.Cards {
		if !assigned[cid] {
			ctx.Log.Warn("收藏册塑封被拒：未集齐", "uid", uid, "deck", deckID, "missing", cid)
			return cardAckLaminate(ctx.Sess.Table(), nil), true
		}
	}
	already := srv.deckSealedAt(cctx, uid, deckID) > 0
	if !already {
		row, gerr := srv.DB.GetItemByGUID(cctx, deckGUID)
		if gerr != nil || row == nil {
			ctx.Log.Warn("收藏册塑封卡组行缺失", "uid", uid, "deck", deckID)
			return cardAckLaminate(ctx.Sess.Table(), nil), true
		}
		fields, ferr := FieldsFromJSON(row.FieldsJSON)
		if ferr != nil {
			return nil, false
		}
		if idx := fieldIdxByCS(row.WireClass, "mkkhpfgnjei"); idx >= 0 && idx < len(fields) {
			// FILETIME（1601 起算）：客户端按 FromFileTime 渲染密封日期，
			// 发 .NET Ticks（0001 起算）会多出 1600 年 → 3626/11/8
			// （2026-11-08 实机）。同款先例：物品 f9 csUpdated 用
			// proto.WindowsFiletimeNow。
			fields[idx].Num = ptrU64(uint64(proto.WindowsFiletimeNow()))
		}
		fj, jerr := FieldsToJSON(fields)
		if jerr != nil {
			return nil, false
		}
		tx, terr := beginEconomyTx(cctx, srv.DB)
		if terr != nil {
			ctx.Log.Error("收藏册塑封开事务失败", "err", terr)
			return nil, false
		}
		defer tx.Rollback()
		if _, uerr := tx.ExecContext(cctx,
			`UPDATE item SET fields_json=? WHERE uid=? AND guid=?`, fj, uid, deckGUID); uerr != nil {
			ctx.Log.Error("收藏册塑封落库失败", "err", uerr)
			return nil, false
		}
		if cerr := tx.Commit(); cerr != nil {
			ctx.Log.Error("收藏册塑封提交失败", "err", cerr)
			return nil, false
		}
	}
	// 奖座幂等补发（checkCardDecks：拥有全物种且未拥有奖座才发）
	if granted, gerr := srv.checkCardDecks(cctx, uid, ""); gerr == nil {
		for _, g := range granted {
			if row, err := srv.DB.GetItemByGUID(cctx, g); err == nil && row != nil {
				srv.pushItemUpdate(uid, row)
			}
		}
	}
	// ACK f1 = 本卡组奖座树（已拥有也回——奖励弹窗要展示）
	rewards := proto.Args(proto.S("61"))
	for _, st := range deck.Statuettes {
		if st == "" {
			continue
		}
		stRows, err := srv.DB.ListItemsByItemIDPrefix(cctx, uid, st)
		if err != nil {
			continue
		}
		for _, r := range stRows {
			if r.ItemID == st {
				if tree, terr := srv.ItemToTree(cctx, r); terr == nil && tree != nil {
					rewards.Items = append(rewards.Items, tree)
				}
			}
		}
	}
	// 卡组行 f1 已变：先删后推全量重同步（塑封渲染靠 f0+f1）
	srv.pushCardAlbumResync(ctx.Sess, uid)
	ctx.Log.Info("收藏册塑封完成", "uid", uid, "deck", deckID, "already", already,
		"rewards", len(rewards.Items))
	return cardAckLaminate(ctx.Sess.Table(), rewards), true
}

// cardAckLaminate 27/4 ACK（llgjfoldbcc/obj-809 四值）：f0 奖励描述数组
// null（静态描述客户端本地有）、f1=奖座树（可为 null=无奖励）、f2/f3 无
// 语义恒 null。
func cardAckLaminate(table *proto.OpsTable, rewards *proto.Value) []byte {
	if rewards == nil {
		rewards = proto.Null()
	}
	body, err := proto.EncodeValue(wiregen.MustMakeObj("gglppphenoi", map[int]*proto.Value{
		0: proto.Null(), 1: rewards, 2: proto.Null(), 3: proto.Null(),
	}), table)
	if err != nil {
		return emptyArgsBody()
	}
	return body
}
