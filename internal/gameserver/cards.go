package gameserver

import (
	"context"
	"database/sql"
	"math/rand"
	"strings"
	"time"

	"rf4server/internal/store"
)

// 集换式卡牌（category 78，item_id 前缀 card_）：钓获结算按物种稀有度
// 分层掉卡；集齐卡组（category 82，cjmjfnheidh 卡清单）发放奖座
// statuette_cards_*。调查与方案见 docs/trading-cards.md 与
// specs/changes/trading-cards/spec.md。

const cardItemIDPrefix = "card_"
const cardDeckCategory = int64(82)

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
// 与结算卡同一棵 25 鱼树；鱼 guid 由卡 guid 派生，避免与鱼护内原鱼同 guid
// （同标本板 mountedFishTreeTx 手法）。
func (s *Server) prepareNewCard(ctx context.Context, uid int64, f *RolledFish) (*store.Item, error) {
	card, err := s.prepareNewItem(ctx, uid, cardItemIDPrefix+f.Species, "backpack", 1, "")
	if err != nil {
		return nil, err
	}
	entry := &store.KeepnetEntry{
		EchoGUID: f.FishGUID, Species: f.Species, NameZh: f.Name,
		WeightG: f.WeightG, LengthM: f.LengthM, Cat: f.Grade,
		TS: float64(time.Now().UnixNano()) / 1e9,
	}
	fish := buildKeepnet25(entry, "", s.rarityOf25(f.Species))
	if idx := fieldIdxByCS("ecknbkpeeji", csGUID); idx >= 0 && idx < len(fish.Fields) {
		fish.Fields[idx].Hex = uuid5OIDHex(card.GUID + "/card_fish")
	}
	if err := embedMountedFish(card, &mountedFish{Tree: fish}); err != nil {
		return nil, err
	}
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

// grantCard 发一张卡（管理路径）：构造 + 短事务入库。
func (s *Server) grantCard(ctx context.Context, uid int64, species string) (*store.Item, error) {
	card, err := s.prepareNewCard(ctx, uid, s.syntheticCardFish(ctx, species))
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
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return card, nil
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

