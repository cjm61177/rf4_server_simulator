package admin

// 卡牌管理端点测试：/api/cards 进度查询与 /api/cards/grant_deck 队列指令。
// 测试库没有静态库，卡组 def 手工插入 item_def（列清单同 shop_catalog_test）。

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"rf4server/internal/config"
	"rf4server/internal/logging"
)

func TestAdminCardEndpoints(t *testing.T) {
	db := testDB(t)
	defer db.Close()
	uid := int64(1000002)
	if _, err := db.EnsureProfile(context.Background(), uid); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`INSERT INTO item_def(numeric_id,item_id,category,attrs_json) VALUES
		(-910001,'tcdeck_test',82,'{"cjmjfnheidh":["card_x_a","card_x_b"],"pnaapkplkfo":["statuette_x"]}'),
		(-910002,'card_x_a',78,'{}'),
		(-910003,'card_x_b',78,'{}'),
		(-910004,'statuette_x',72,'{}')`); err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureAdmin(context.Background(), "admin", "admin123"); err != nil {
		t.Fatal(err)
	}
	cfg := config.Admin{HTTPAddr: "127.0.0.1:0", JWTSecret: "test"}
	srv := NewServer(cfg, db, logging.Discard(), WebFS())
	errCh, err := srv.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Shutdown(context.Background()) }()
	select {
	case <-time.After(100 * time.Millisecond):
	case err := <-errCh:
		t.Fatalf("管理后台提前退出: %v", err)
	}
	base := "http://" + srv.Addr()
	client := &http.Client{Timeout: 5 * time.Second}
	call := func(method, path, token, body string) (int, string) {
		r, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var sb strings.Builder
		_, _ = io.Copy(&sb, resp.Body)
		return resp.StatusCode, sb.String()
	}

	code, body := call("POST", "/api/login", "", `{"username":"admin","password":"admin123"}`)
	if code != 200 {
		t.Fatalf("登录失败: %d %s", code, body)
	}
	token := body[strings.Index(body, `"token":"`)+len(`"token":"`):]
	token = token[:strings.Index(token, `"`)]

	// 进度查询：初始 0/2，缺两卡。
	code, body = call("GET", "/api/cards?uid=1000002", token, "")
	if code != 200 || !strings.Contains(body, `"deck_id":"tcdeck_test"`) ||
		!strings.Contains(body, `"owned":0`) || !strings.Contains(body, "card_x_a") {
		t.Fatalf("卡组进度异常: %d %s", code, body)
	}
	// 缺 uid 参数拒绝。
	if code, _ = call("GET", "/api/cards", token, ""); code != 400 {
		t.Fatalf("缺 uid 应 400, 实得 %d", code)
	}

	// 发整组：指令入队，走 gameserver 消费。
	if code, body = call("POST", "/api/cards/grant_deck", token, `{"uid":1000002,"deck_id":"tcdeck_test"}`); code != 200 {
		t.Fatalf("发整组失败: %d %s", code, body)
	}
	var payload string
	if err := db.SQL.QueryRow(`SELECT payload FROM admin_queue ORDER BY id DESC LIMIT 1`).Scan(&payload); err != nil ||
		!strings.Contains(payload, `"op":"grant_deck"`) || !strings.Contains(payload, "tcdeck_test") {
		t.Fatalf("grant_deck 指令未入队: %q %v", payload, err)
	}
	var action string
	if err := db.SQL.QueryRow(`SELECT action FROM audit_log ORDER BY id DESC LIMIT 1`).Scan(&action); err != nil || action != "grant_deck" {
		t.Fatalf("发整组未写审计: %q %v", action, err)
	}
}
