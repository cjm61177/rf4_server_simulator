package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestStaticSpecialtyShopShelvesReferenceKnownItems(t *testing.T) {
	db, err := OpenWithStatic(filepath.Join(t.TempDir(), "specialty-shops.db"),
		filepath.Join("..", "..", "server_data", "static.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}

	stores := []string{
		"level_004_kvas", "kala_store_003", "milkstore_viiveri", "sawmill_02",
		"foodstore_garage", "bakery_013", "vakula_013", "mill_013", "paseka_sura",
		"arbuz_store_009", "tungus_015", "craft_baits_012", "varyag_012",
	}
	rows, err := db.SQL.QueryContext(context.Background(), `
		SELECT s.store_id, s.item_id
		FROM static.shop_item s
		LEFT JOIN static.item_def d ON d.item_id = s.item_id
		LEFT JOIN static.item_catalog c ON c.item_id = s.item_id
		WHERE s.store_id IN (
			'level_004_kvas','kala_store_003','milkstore_viiveri','sawmill_02',
			'foodstore_garage','bakery_013','vakula_013','mill_013','paseka_sura',
			'arbuz_store_009','tungus_015','craft_baits_012','varyag_012'
		)
		AND (d.item_id IS NULL OR c.item_id IS NULL)
		ORDER BY s.store_id, s.item_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var missing []string
	for rows.Next() {
		var storeID, itemID string
		if err := rows.Scan(&storeID, &itemID); err != nil {
			t.Fatal(err)
		}
		missing = append(missing, storeID+"/"+itemID)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 {
		t.Fatalf("特色商店存在无 item_def/item_catalog 的货架商品: %v", missing)
	}

	var craftRows int
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM static.shop_item WHERE store_id='craft_baits_012'`).Scan(&craftRows); err != nil {
		t.Fatal(err)
	}
	if craftRows != 8254 {
		t.Fatalf("craft_baits_012 基础货架=%d，期望 8254（%d 个目标商店）", craftRows, len(stores))
	}
}

// TestStaticTungusCoopSellsSpecialtyGoods 通古斯卡合作社（tungus_015）必须
// 上架 TUNGUS 特产。实抓货架曾漏掉 tungus_12pack / tungus_bot，而通用补齐
// 规则对品牌店要求品牌前缀匹配：这两件商品 item_def 品牌为空，目录登记的
// brand_tunguska 全库无一命中，永远补不进去，玩家点击购买被 22/3 货架
// 校验拒绝（2026-11-10 实机事故）。价格取静态 item_price 的官服实抓值
// （金币 129 / 22，银价 0），与货架行同步更新。
func TestStaticTungusCoopSellsSpecialtyGoods(t *testing.T) {
	db, err := OpenWithStatic(filepath.Join(t.TempDir(), "tungus-coop.db"),
		filepath.Join("..", "..", "server_data", "static.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}

	want := map[string]struct{ silver, gold int64 }{
		"tungus_12pack": {0, 129},
		"tungus_bot":    {0, 22},
	}
	rows, err := db.SQL.QueryContext(context.Background(), `
		SELECT item_id, silver, gold FROM static.shop_item
		WHERE store_id='tungus_015' AND item_id IN ('tungus_12pack','tungus_bot')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := make(map[string]struct{ silver, gold int64 }, len(want))
	for rows.Next() {
		var itemID string
		var prices struct{ silver, gold int64 }
		if err := rows.Scan(&itemID, &prices.silver, &prices.gold); err != nil {
			t.Fatal(err)
		}
		got[itemID] = prices
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for itemID, prices := range want {
		actual, ok := got[itemID]
		if !ok {
			t.Fatalf("tungus_015 货架缺少特产 %s", itemID)
		}
		if actual != prices {
			t.Fatalf("tungus_015@%s 价格=(%d,%d)，期望=(%d,%d)",
				itemID, actual.silver, actual.gold, prices.silver, prices.gold)
		}
	}
}
