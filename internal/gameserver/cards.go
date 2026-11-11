package gameserver

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"rf4server/internal/proto"
	"rf4server/internal/store"
	"rf4server/internal/wiregen"
)

// 集换式卡牌（category 78，item_id 前缀 card_）：钓获结算按物种稀有度
// 分层掉卡；集齐卡组（category 82，cjmjfnheidh 卡清单）发放奖座
// statuette_cards_*。调查与方案见 docs/trading-cards.md 与
// specs/changes/trading-cards/spec.md。

const cardItemIDPrefix = "card_"
const cardDeckCategory = int64(82)

// 卡组物品行容器：收藏册专用容器 18（lmmhdlhogdc.daeceehaplh）。存储名沿用
// 导入快照的 "native:<枚举>" 约定（native_inventory.go）。归柜只写 DB 列，
// 不写 wire 容器枚举字段（启动修复恒清零，见 prepareNewCard 注释）。
const tcDeckContainer = "native:18"

// cardDropRate 按物种稀有度读分层掉率 tunable；默认 0 = 功能关闭。
func (s *Server) cardDropRate(rarity int64) float64 {
	key := "card_drop_common"
	switch rarity {
	case 1:
		key = "card_drop_rare"
	case 2:
		key = "card_drop_super"
	}
	return s.tunableF(key, 0)
}

// rollCardDrop 结算事务外的掉卡判定：分层掉率命中且物种确有卡 def。
// 静态库查询不得进入经济事务（SQLite 单连接），整个准备阶段都在事务外，
// 事务内只做 grantPreparedItemTx 入库。
func (s *Server) rollCardDrop(ctx context.Context, f *RolledFish) bool {
	if f == nil || f.Species == "" {
		return false
	}
	rate := s.cardDropRate(s.rarityOf25(f.Species))
	if rate <= 0 || rand.Float64() >= rate {
		return false
	}
	def, err := s.DB.GetItemDefByItemID(ctx, cardItemIDPrefix+f.Species)
	if err != nil || def == nil {
		return false // 物种没有对应卡 def：静默跳过，不掉卡也不报错
	}
	return true
}

// prepareNewCard 构造含渔获快照的卡物品（事务外）。f0 peipahjmida 嵌
// 与结算卡同一棵 25 鱼树：地点取 f.LocLevel（咬钩时写入的当前地图）、
// 饵料 bait 由结算调用方从 FishState.Bait 快照传入（管理发卡传空）——
// 卡详情页的钓获地点/饵料两行就来自这两个字段，漏填会缺行（2026-11-11
// 实机：卡详情缺字段，嵌树 f8/f10 恒 null）。鱼 guid 由卡 guid 派生，
// 避免与鱼护内原鱼同 guid（同标本板 mountedFishTreeTx 手法）。
func (s *Server) prepareNewCard(ctx context.Context, uid int64, f *RolledFish, bait string) (*store.Item, error) {
	card, err := s.prepareNewItem(ctx, uid, cardItemIDPrefix+f.Species, tcDeckContainer, 1, "")
	if err != nil {
		return nil, err
	}
	entry := &store.KeepnetEntry{
		EchoGUID: f.FishGUID, Species: f.Species, NameZh: f.Name,
		WeightG: f.WeightG, LengthM: f.LengthM, Cat: f.Grade,
		KeepBait: bait,
		TS:       float64(time.Now().UnixNano()) / 1e9,
	}
	fish := buildKeepnet25(entry, f.LocLevel, s.rarityOf25(f.Species), s.fishModelVariantFor(f))
	if idx := fieldIdxByCS("ecknbkpeeji", csGUID); idx >= 0 && idx < len(fish.Fields) {
		fish.Fields[idx].Hex = uuid5OIDHex(card.GUID + "/card_fish")
	}
	if err := embedMountedFish(card, &mountedFish{Tree: fish}); err != nil {
		return nil, err
	}
	// 新卡=自由卡：父 guid 空、dbgaheblmei 不打 bit4。官方语义（2026-11-07
	// script.json 对照纠正反编译名）：
	//   - 归属：卡进卡组 = odflendafcg(父guid)=卡组实例 guid，由指派路径
	//     设置（客户端 RPC alpciclpmnn/hlmjcnikdpl；见 prepareDeckItems）。
	//   - bit4（dbgaheblmei>>4&1）= 已收集进卡组标记：自由卡带 bit4 时
	//     卡牌页多选/右键出售全部置灰（2026-11-07 实机：27 张全 16 → 全
	//     不可选）。只有被指派的副本才打 bit4。
	//   - 自由卡谓词 bedifiidhhf.pmibgafefdf = !efhaepoijno()（父空）；
	//     出售流程 deghjmihlcf.imcmddhkdch 对已归属卡（ljebfflncgd=
	//     efhaepoijno）直接弹 kDlg_TradingCards_Sell_CollectedCard_Error。
	// 收藏册两页都在容器 18 管理器上取数——卡行必须 container=native:18，
	// 放 backpack 则卡牌页恒空。不写 wire 容器字段 f3（mfllbndghhe）：
	// 启动修复 RepairItemPhysics → canonicalizeItemPhysicalFields 会把根
	// 物品 csSlotEnum BYTE 恒清零（rig 时代规则），写 18 下次启动就被抹掉
	// （2026-11-07 实测 24 行归 0）；归柜真源是 DB 行 container 列。
	return card, nil
}

// syntheticCardFish 为管理发卡合成展示用渔获（无真实钓获可嵌）：
// 取物种最大体重六成，缺表回退 1kg，档位固定 2。
func (s *Server) syntheticCardFish(ctx context.Context, species string) *RolledFish {
	weight := int64(1000)
	var maxG int64
	if err := s.DB.SQL.QueryRowContext(ctx,
		`SELECT COALESCE(max_g,0) FROM species_weight WHERE fish_key=?`, species).Scan(&maxG); err == nil && maxG > 0 {
		weight = maxG * 3 / 5
	}
	return &RolledFish{FishGUID: newFishGUID(), Species: species, WeightG: weight, Grade: 2}
}

// grantCard 发一张卡（管理路径）：构造 + 短事务入库 + 卡组同步计划落地。
// 卡一律自由态（父空）——进组只能由玩家 27/2 手动指派。
func (s *Server) grantCard(ctx context.Context, uid int64, species string) (*store.Item, error) {
	card, err := s.prepareNewCard(ctx, uid, s.syntheticCardFish(ctx, species), "")
	if err != nil {
		return nil, err
	}
	plan, err := s.prepareDeckItems(ctx, uid, card)
	if err != nil {
		return nil, err
	}
	tx, err := s.DB.SQL.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, _, err := grantPreparedItemTx(ctx, tx, card, false); err != nil {
		return nil, err
	}
	if err := syncDeckItemsTx(ctx, tx, uid, plan); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return card, nil
}

// deckItemGUID 卡组物品实例 guid：uuid5 派生，保证重发幂等（与 24/18
// f9 数组用的实例 guid 同一派生式）。
func deckItemGUID(uid int64, deckID string) string {
	return uuid5OIDHex(fmt.Sprintf("tcard-deck:%d:%s", uid, deckID))
}

// deckSyncPlan 一次卡组同步计划：重建的 tcdeck_* 行 + 全部持卡行（已按
// 官方归属语义在内存里改好字段，syncDeckItemsTx 落地时整行 UPDATE）。
type deckSyncPlan struct {
	Decks []*store.Item
	Cards []*store.Item
}

// assignCardToDeck 把卡标记为「已收集进卡组」：odflendafcg(父guid)=卡组
// 实例 guid + dbgaheblmei bit4。客户端收藏集槽位渲染=容器 18 里
// parent==卡组 guid 的卡（ojahelhjpef.fbiflhjglah 非塑封分支
// nadlhnijnlb.peffocialef 谓词），官方指派/摘卡走 RPC alpciclpmnn/
// hlmjcnikdpl——服务端等价物即本函数（2026-11-07 逆向实锤）。
func assignCardToDeck(card *store.Item, deckGUID string) error {
	fields, err := FieldsFromJSON(card.FieldsJSON)
	if err != nil {
		return err
	}
	if idx := fieldIdxByCS(card.WireClass, "odflendafcg"); idx >= 0 && idx < len(fields) {
		fields[idx] = proto.Field{Op: "OPTGUID", Hex: strings.ToLower(deckGUID)}
	}
	if idx := fieldIdxByCS(card.WireClass, "dbgaheblmei"); idx >= 0 && idx < len(fields) {
		cur := uint64(0)
		if fields[idx].Num != nil {
			cur = *fields[idx].Num
		}
		fields[idx].Num = ptrU64(cur | 0x10)
	}
	fj, err := FieldsToJSON(fields)
	if err != nil {
		return err
	}
	card.FieldsJSON = fj
	return nil
}

// releaseCardToFree 卡回到自由态：父 guid 清空 + bit4 清除（自由卡带
// bit4 时卡牌页多选/右键出售全灰，2026-11-07 实机；见 prepareNewCard 注释）。
func releaseCardToFree(card *store.Item) error {
	fields, err := FieldsFromJSON(card.FieldsJSON)
	if err != nil {
		return err
	}
	if idx := fieldIdxByCS(card.WireClass, "odflendafcg"); idx >= 0 && idx < len(fields) {
		fields[idx] = proto.Field{Op: "OPTGUID"}
	}
	if idx := fieldIdxByCS(card.WireClass, "dbgaheblmei"); idx >= 0 && idx < len(fields) {
		cur := uint64(0)
		if fields[idx].Num != nil {
			cur = *fields[idx].Num
		}
		fields[idx].Num = ptrU64(cur &^ 0x10)
	}
	fj, err := FieldsToJSON(fields)
	if err != nil {
		return err
	}
	card.FieldsJSON = fj
	return nil
}

// prepareDeckItems 事务外构造卡组同步计划（tcdeck_*，ckfmndabaei/11304）。
// 官方收藏集模型（2026-11-07 script.json 对照纠正反编译名后实锤）：
//   - 卡组行 f1 mkkhpfgnjei=塑封时间戳：>0 且 f0 非空 → 客户端
//     imdcjllgdcn/ojahelhjpef 的 eekechidjae()=true，整组按「已塑封」
//     渲染并改读 f0 密封快照（旧实现把 f1 当"开始收集时间戳"每次都写
//     now → 全部卡组恒显已塑封，2026-11-07 实机）。收集中 f1 必须为 0。
//   - 收集中槽位从容器 18 卡行实时取 parent==卡组 guid 的卡填充
//     （nadhnljnjb…nadlhnijnlb.peffocialef），进度=已指派物种数/总数。
//   - 只保留用户 27/2 手动指派的副本；空槽不自动补位（2026-11-08 改：
//     官方语义=钓到的卡落卡牌列表供出售/手动进组，自动补位会把新钓的卡
//     吃进卡组导致卡牌列表看不到）。未指派卡全部自由（父空、无 bit4）。
//   - f0 mjbciidhldo 仍内嵌每物种一棵卡树（args 362 类型化卡数组，注册
//     号实证见循环处注释）：塑封后快照 + 24/18 档案聚合展示用。
// extra 为本次即将入库、事务外读不到的新卡（结算掉卡/发卡路径）。
// 静态库查询不进事务（SQLite 单连接纪律），事务内只做 syncDeckItemsTx。
func (s *Server) prepareDeckItems(ctx context.Context, uid int64, extra ...*store.Item) (*deckSyncPlan, error) {
	decks, err := loadCardDecks(ctx, s.DB)
	if err != nil {
		return nil, err
	}
	if len(decks) == 0 {
		return &deckSyncPlan{}, nil
	}
	rows, err := s.DB.ListItemsByItemIDPrefix(ctx, uid, cardItemIDPrefix)
	if err != nil {
		return nil, err
	}
	// 存量卡组行的塑封时间戳（f1 mkkhpfgnjei>0=已塑封，27/4 RPC 落）：重建
	// 必须保留，否则任何卡组同步都会把塑封抹掉（客户端 eekechidjae 判 f1>0
	// 后整组改读 f0 密封快照，归 0 会退回收集中渲染并可再摘卡）。
	sealed := map[string]int64{}
	if deckRows, derr := s.DB.ListItemsByItemIDPrefix(ctx, uid, "tcdeck_"); derr == nil {
		for _, r := range deckRows {
			if ts := deckSealedField(r); ts > 0 {
				sealed[r.ItemID] = ts
			}
		}
	}
	byItem := map[string][]*store.Item{}
	for _, r := range rows {
		byItem[r.ItemID] = append(byItem[r.ItemID], r)
	}
	for _, c := range extra {
		if c == nil {
			continue
		}
		byItem[c.ItemID] = append(byItem[c.ItemID], c)
	}
	claimed := map[string]bool{}
	out := make([]*store.Item, 0, len(decks))
	for _, d := range decks {
		deckSealed := sealed[d.DeckID] > 0
		// 每物种只保留用户指派的副本（27/2 手选，原位保留——不得自动重选
		// 其它副本，否则手选被顶掉/形成双指派）；空槽不补位。官方语义 =
		// 钓到的卡落卡牌列表（自由卡），玩家手动指派进收藏集——自动补位
		// 会把新钓的卡直接吃进卡组，卡牌列表看不到新卡（2026-11-08 实机；
		// 旧摘卡意图 pin 只为拦补位而设，补位既除，pin 数据留库无害）。
		// 塑封组同理：f0 快照只反映密封时的指派。
		var trees []*proto.Value
		for _, cid := range d.Cards {
			deckGUID := deckItemGUID(uid, d.DeckID)
			var pick *store.Item
			for _, r := range byItem[cid] {
				if cardParentGUID(r) == deckGUID {
					pick = r
					break
				}
			}
			if pick == nil {
				continue // 无用户指派：槽位留空
			}
			claimed[pick.GUID] = true
			tree, terr := s.ItemToTree(ctx, pick)
			if terr != nil || tree == nil {
				continue
			}
			trees = append(trees, tree)
		}
		if len(trees) == 0 {
			continue
		}
		// 容器必须是收藏册专用容器 18（lmmhdlhogdc.daeceehaplh，存储名
		// "native:18"）：收藏册服务 lifdpflbdbd.jjejpmfmhdm 订阅的是
		// idadlifichf(lmmhdlhogdc.daeceehaplh) 即容器 18 的物品管理器，客户端
		// 开卡牌手册时按 4/4(18)→4/5(18) 拉取该容器 GUID/详情。放 backpack 的
		// 卡组行只有容器 0 管理器收得到，收藏册永远看不到（2026-11-07 实测：
		// 4/4(18) 回空 → 面板空；TcItemEvent 0 命中）。
		it, perr := s.prepareNewItem(ctx, uid, d.DeckID, tcDeckContainer, 1, deckItemGUID(uid, d.DeckID))
		if perr != nil {
			return nil, perr
		}
		fields, ferr := FieldsFromJSON(it.FieldsJSON)
		if ferr != nil {
			return nil, ferr
		}
		if idx := fieldIdxByCS(it.WireClass, "mjbciidhldo"); idx >= 0 && idx < len(fields) {
			// f0 解码端是类型化卡数组 ipdpoaiekem[]（ckfmndabaei.plphdfehkjl
			// 直接 lpdfanjcalm<ipdpoaiekem[]>）。args code 既不是 bicjdghfleb
			// codec 注册号（341→dapkdcghfmg 是另一张表），也不是 wire 类型
			// ID（11301 不存在），而是客户端 OpenTypeResolver（njhhjeijlim）
			// 的注册序号：带 hgjpmjhjkgg 特性的类型按特性值排序后顺序编号，
			// dipajjenkhl 按串查该表得 Array.CreateInstance 的元素类型。
			// 2026-11-07 用 hook 直读客户端注册表全量 526 条实证：
			// 61=onobdmohahp、341=eapbnnpdooo（此前两次异常的撞型完全吻合）、
			// 362=ipdpoaiekem（卡）、364=imdcjllgdcn（卡组）。
			fields[idx] = proto.Field{Op: "VAL", Val: proto.Args(proto.S("362"), trees...)}
		}
		// f1 mkkhpfgnjei：收集中恒 0；塑封后=密封时间戳（.NET ticks，27/4
		// 落、上方 sealed 表跨重建保留）。
		if deckSealed {
			if idx := fieldIdxByCS(it.WireClass, "mkkhpfgnjei"); idx >= 0 && idx < len(fields) {
				fields[idx].Num = ptrU64(uint64(sealed[d.DeckID]))
			}
		}
		// 不写 wire 容器字段 f4（csSlotEnum）：同卡行 f3——启动
		// 修复恒清零该 BYTE 字段，写 18 活不过下次重启；归柜真源=DB container。
		// dbgaheblmei 置 0x10（bit4）：客户端容器快照分类谓词
		// nanhojflndk.afkieinjcnb = 静态 mgkfaekpnoj==2 || (dbgaheblmei>>4)&1。
		// 卡组行静态 attrs 无 mgkfaekpnoj 键（=0≠2），bit4 不置则物品进不了
		// eegjkjenhoe.bcgohaecdif 容器快照；ibaammebeej 三门广播的第③门要求
		// Guid 在快照 HashSet 里，事件不广播 → 收藏册 mailhgoence 的
		// TcItemEvent 永不命中（hook 实测 TcSubscribe×2 / TcItemEvent×0）。
		// 官方钓鱼游装备同样打 16（见 markTripTourFlag），该位真实存在。
		if idx := fieldIdxByCS(it.WireClass, "dbgaheblmei"); idx >= 0 && idx < len(fields) {
			cur := uint64(0)
			if fields[idx].Num != nil {
				cur = *fields[idx].Num
			}
			fields[idx].Num = ptrU64(cur | 0x10)
		}
		fj, ferr := FieldsToJSON(fields)
		if ferr != nil {
			return nil, ferr
		}
		it.FieldsJSON = fj
		out = append(out, it)
	}
	// 未被任何卡组占用的卡=自由卡：清父 guid + 清 bit4（归一化存量行——
	// 旧实现给全部卡打过 16，不清则卡牌页多选/出售恒灰）。
	cards := make([]*store.Item, 0, len(rows)+len(extra))
	seen := map[string]bool{}
	for _, r := range rows {
		if claimed[r.GUID] {
			cards = append(cards, r)
		} else if err := releaseCardToFree(r); err != nil {
			return nil, err
		} else {
			cards = append(cards, r)
		}
		seen[r.GUID] = true
	}
	for _, c := range extra {
		if c == nil || seen[c.GUID] {
			continue
		}
		if !claimed[c.GUID] {
			if err := releaseCardToFree(c); err != nil {
				return nil, err
			}
		}
		cards = append(cards, c)
		seen[c.GUID] = true
	}
	return &deckSyncPlan{Decks: out, Cards: cards}, nil
}

// syncDeckItemsTx 事务内落地卡组同步计划：先清玩家全部 tcdeck_* 行再按
// 当前持卡重建；然后整行 UPDATE 计划里的卡行（指派副本带上父 guid+bit4、
// 自由副本清干净）。同 guid 的 DELETE+INSERT 幂等；组内卡全部卖光时该行
// 自动消失。extra 卡可能刚由调用方在本事务 INSERT 过，UPDATE 覆盖即终态。
func syncDeckItemsTx(ctx context.Context, tx *sql.Tx, uid int64, plan *deckSyncPlan) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM item WHERE uid=? AND item_id LIKE 'tcdeck\_%' ESCAPE '\'`, uid); err != nil {
		return err
	}
	for _, d := range plan.Decks {
		if err := insertItemTx(ctx, tx, d); err != nil {
			return err
		}
	}
	for _, c := range plan.Cards {
		if _, err := tx.ExecContext(ctx,
			`UPDATE item SET fields_json=? WHERE uid=? AND guid=?`, c.FieldsJSON, uid, c.GUID); err != nil {
			return err
		}
	}
	return nil
}

// deckItemGUIDs 玩家当前卡组物品 guid 清单（4/22 推送用）。
func (s *Server) deckItemGUIDs(ctx context.Context, uid int64) []string {
	rows, err := s.DB.ListItemsByItemIDPrefix(ctx, uid, "tcdeck_")
	if err != nil {
		return nil
	}
	guids := make([]string, 0, len(rows))
	for _, r := range rows {
		guids = append(guids, r.GUID)
	}
	return guids
}

// cardDeck 是一个卡组判定所需的字段（item_def cat82）。
type cardDeck struct {
	DeckID     string
	Cards      []string
	Statuettes []string
}

// attrStringList 读 attrs_json 里的字符串数组属性；缺失返回 nil。
func attrStringList(def *store.ItemDef, key string) []string {
	attrs := defAttrs(def)
	raw, ok := attrs[key]
	if !ok {
		return nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, v := range list {
		if str, ok := v.(string); ok && str != "" {
			out = append(out, str)
		}
	}
	return out
}

// loadCardDecks 读全部卡组定义；解析失败的卡组跳过。
func loadCardDecks(ctx context.Context, db *store.DB) ([]*cardDeck, error) {
	defs, err := db.ListItemDefsByCategory(ctx, cardDeckCategory)
	if err != nil {
		return nil, err
	}
	decks := make([]*cardDeck, 0, len(defs))
	for _, def := range defs {
		cards := attrStringList(def, "cjmjfnheidh")
		if len(cards) == 0 {
			continue
		}
		decks = append(decks, &cardDeck{
			DeckID:     def.ItemID,
			Cards:      cards,
			Statuettes: attrStringList(def, "pnaapkplkfo"),
		})
	}
	return decks, nil
}

// ownedCardCountTx 统计玩家已拥有的指定卡 item_id 数（去重）。
func ownedCardCountTx(ctx context.Context, q queryRowContext, uid int64, cards []string) (int64, error) {
	args := make([]any, 0, len(cards)+1)
	args = append(args, uid)
	ph := make([]string, 0, len(cards))
	for _, c := range cards {
		args = append(args, c)
		ph = append(ph, "?")
	}
	var owned int64
	err := q.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT item_id) FROM item WHERE uid=? AND item_id IN (`+strings.Join(ph, ",")+`)`,
		args...).Scan(&owned)
	return owned, err
}

// queryRowContext 抽象池化/事务两种查询入口（checkCardDecks 两处都要用）。
type queryRowContext interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// checkCardDecks 检查卡组集齐并发奖座；hint 为卡 item_id（只查含它的卡组），
// 空串检查全部。返回本次新发奖座 guid（调用方负责推送）。
// 幂等：完成度复查与奖座查重在同一个短事务内，并发/重复触发只发一次。
func (s *Server) checkCardDecks(ctx context.Context, uid int64, hint string) ([]string, error) {
	decks, err := loadCardDecks(ctx, s.DB)
	if err != nil || len(decks) == 0 {
		return nil, err
	}
	granted := make([]string, 0, 1)
	for _, deck := range decks {
		if hint != "" && !containsString(deck.Cards, hint) {
			continue
		}
		statuette := ""
		for _, st := range deck.Statuettes {
			if st != "" {
				statuette = st
				break
			}
		}
		if statuette == "" {
			continue
		}
		// 完成度预查（事务外）；未集齐直接跳过，不做无谓的奖座构造。
		owned, err := ownedCardCountTx(ctx, s.DB.SQL, uid, deck.Cards)
		if err != nil || int(owned) < len(deck.Cards) {
			continue
		}
		prepared, err := s.prepareNewItem(ctx, uid, statuette, "backpack", 1, "")
		if err != nil {
			s.log.Warn("卡组奖座构造失败", "uid", uid, "item", statuette, "err", err)
			continue
		}
		tx, err := s.DB.SQL.BeginTx(ctx, nil)
		if err != nil {
			return granted, err
		}
		// 短事务内复查：集齐与未拥有两者在提交前都可能被并发改变。
		owned2, err := ownedCardCountTx(ctx, tx, uid, deck.Cards)
		if err == nil && int(owned2) >= len(deck.Cards) {
			var have int64
			if err := tx.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM item WHERE uid=? AND item_id=?`, uid, statuette).Scan(&have); err == nil && have == 0 {
				row, _, gerr := grantPreparedItemTx(ctx, tx, prepared, false)
				if gerr == nil {
					if cerr := tx.Commit(); cerr == nil {
						granted = append(granted, row.GUID)
						s.log.Info("卡组集齐发奖座", "uid", uid, "deck", deck.DeckID, "item", statuette)
						continue
					}
				}
			}
		}
		tx.Rollback()
	}
	return granted, nil
}

// adminGrantCardQueue 处理管理指令发卡：qty 张同名卡逐张独立入包
// （卡不可堆叠），随后按该卡所属卡组补查奖座。所有新物品 guid 由
// 返回值交给调用方推送。
func (s *Server) adminGrantCardQueue(ctx context.Context, uid int64, itemID string, qty int64) []string {
	species := strings.TrimPrefix(itemID, cardItemIDPrefix)
	if species == "" {
		return nil
	}
	if qty < 1 {
		qty = 1
	}
	if qty > 50 {
		qty = 50
	}
	guids := make([]string, 0, qty)
	for i := int64(0); i < qty; i++ {
		card, err := s.grantCard(ctx, uid, species)
		if err != nil {
			s.log.Warn("管理指令：发卡失败", "uid", uid, "item", itemID, "err", err)
			break
		}
		guids = append(guids, card.GUID)
	}
	if len(guids) > 0 {
		s.log.Info("管理指令：发卡", "uid", uid, "item", itemID, "count", len(guids))
	}
	if deckGuids, err := s.checkCardDecks(ctx, uid, itemID); err == nil && len(deckGuids) > 0 {
		guids = append(guids, deckGuids...)
	}
	return guids
}

// adminGrantDeckQueue 处理管理指令发整组卡：跳过已拥有的卡，
// 之后全量卡组检查（该组必然集齐 → 奖座发放）。
func (s *Server) adminGrantDeckQueue(ctx context.Context, uid int64, deckID string) []string {
	def, err := s.DB.GetItemDefByItemID(ctx, deckID)
	if err != nil || def == nil || def.Category != cardDeckCategory {
		s.log.Warn("管理指令：卡组不存在", "uid", uid, "deck", deckID)
		return nil
	}
	cards := attrStringList(def, "cjmjfnheidh")
	if len(cards) == 0 {
		return nil
	}
	guids := make([]string, 0, len(cards))
	for _, cardItemID := range cards {
		var have int64
		if err := s.DB.SQL.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM item WHERE uid=? AND item_id=?`, uid, cardItemID).Scan(&have); err == nil && have > 0 {
			continue
		}
		species := strings.TrimPrefix(cardItemID, cardItemIDPrefix)
		if species == "" {
			continue
		}
		card, err := s.grantCard(ctx, uid, species)
		if err != nil {
			s.log.Warn("管理指令：发卡组失败", "uid", uid, "item", cardItemID, "err", err)
			continue
		}
		guids = append(guids, card.GUID)
	}
	s.log.Info("管理指令：发卡组", "uid", uid, "deck", deckID, "new", len(guids), "total", len(cards))
	if deckGuids, err := s.checkCardDecks(ctx, uid, ""); err == nil && len(deckGuids) > 0 {
		guids = append(guids, deckGuids...)
	}
	return guids
}

// buildCardDeckArray 构造 24/18 档案聚合（2003 lnhgjpfnhhi）的 f9 卡组数组
// imdcjllgdcn[]。该聚合服务 Profile 族的他人档案查看（ddkpcmhdfdb 模型，
// 唯一拉取入口 afmlibpohcl.bjipldipkph）；独立收藏册界面不查此命令（它从
// 库存 tcdeck_* 物品取数，见 prepareDeckItems）。args code 340 = ckfmndabaei
// 在 bicjdghfleb 注册表的显式序号（341=dapkdcghfmg 同表，174=icadfbpiboh[]
// 同理，已在 801.f0 验证）。卡组对象：f0=拥有的卡树（与 4/22 同一棵，args 61）、
// f1=mkkhpfgnjei 恒 0（>0+f0 非空=客户端 eekechidjae 判「已塑封」，同
// prepareDeckItems）、f2=uid+deck
// 派生实例 guid（与 deckItemGUID 同一派生式）、f5=deck numeric id。
func (s *Server) buildCardDeckArray(ctx context.Context, uid int64) (*proto.Value, error) {
	decks, err := loadCardDecks(ctx, s.DB)
	if err != nil {
		return nil, err
	}
	objs := make([]*proto.Value, 0, len(decks))
	if len(decks) == 0 {
		return proto.Args(proto.S("340")), nil
	}
	rows, err := s.DB.ListItemsByItemIDPrefix(ctx, uid, cardItemIDPrefix)
	if err != nil {
		return nil, err
	}
	byItem := map[string][]*store.Item{}
	for _, r := range rows {
		byItem[r.ItemID] = append(byItem[r.ItemID], r)
	}
	for _, d := range decks {
		// f0 每物种一张（同 prepareDeckItems 的卡组槽位语义）。
		var trees []*proto.Value
		for _, c := range d.Cards {
			if rs := byItem[c]; len(rs) > 0 {
				tree, terr := s.ItemToTree(ctx, rs[0])
				if terr != nil || tree == nil {
					continue
				}
				trees = append(trees, tree)
			}
		}
		var started uint64
		// f1 恒 0（收集中）：f1>0+f0 非空=客户端判「已塑封」，同
		// prepareDeckItems——档案聚合也不得写 now。
		numID := uint32(0)
		if def, derr := s.DB.GetItemDefByItemID(ctx, d.DeckID); derr == nil && def != nil && def.NumericID > 0 {
			numID = uint32(def.NumericID)
		}
		obj, oerr := wiregen.MakeObj("ckfmndabaei", map[int]*proto.Value{
			// f0 类型化卡数组（ipdpoaiekem[]），args code=OpenTypeResolver
			// 注册号 362（同 prepareDeckItems，hook 实证见该处注释）。
			0: proto.Args(proto.S("362"), trees...),
			1: proto.U64(started),
			2: proto.Guid(uuid5OIDHex(fmt.Sprintf("tcard-deck:%d:%s", uid, d.DeckID))),
			5: proto.U32(numID),
		})
		if oerr != nil {
			return nil, oerr
		}
		objs = append(objs, obj)
	}
	// 外层同理：code=元素 DTO 的 OpenTypeResolver 注册号 364
	// （imdcjllgdcn，hook 实证见 prepareDeckItems 注释）。旧 340 与 wire
	// 类型 ID 假设（11301/11304）都不在 526 项注册表内；空数组（count=0）
	// 两处不受影响，保持原样。
	return proto.Args(proto.S("364"), objs...), nil
}

// CardDeckProgress 管理端只读视图：全部卡组 + 玩家拥有情况。
// 包级导出供 admin 包直接调用（同 AdminResolveLevelXP 的桥接先例）。
func CardDeckProgress(ctx context.Context, db *store.DB, uid int64) ([]map[string]any, error) {
	decks, err := loadCardDecks(ctx, db)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(decks))
	for _, deck := range decks {
		owned := make([]string, 0, len(deck.Cards))
		missing := make([]string, 0, len(deck.Cards))
		for _, c := range deck.Cards {
			var have int64
			if err := db.SQL.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM item WHERE uid=? AND item_id=?`, uid, c).Scan(&have); err == nil && have > 0 {
				owned = append(owned, c)
			} else {
				missing = append(missing, c)
			}
		}
		statuette := ""
		if len(deck.Statuettes) > 0 {
			statuette = deck.Statuettes[0]
		}
		out = append(out, map[string]any{
			"deck_id":   deck.DeckID,
			"total":     len(deck.Cards),
			"owned":     len(owned),
			"missing":   missing,
			"statuette": statuette,
		})
	}
	return out, nil
}
