package main

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

// 实机写路径验证。默认跳过，只有显式打开才跑：
//
//	EMBY_LIVE=1 go test -run TestLive -v
//
// 设计原则是**数据零变化**：把条目当前的头像原样再上传一次，然后比对前后 md5。
// 这样既真实走了 POST /Items/{id}/Images/Primary（包括 base64 回退），
// 又不会改动用户任何东西。
//
// 想换验证对象就设 EMBY_LIVE_ITEM 与 EMBY_LIVE_TYPE（默认 520826 / Primary）。
func TestLiveUploadImageIsIdempotent(t *testing.T) {
	if os.Getenv("EMBY_LIVE") != "1" {
		t.Skip("未设置 EMBY_LIVE=1，跳过实机写验证")
	}
	cfg := loadLiveConfig(t)
	itemID := firstNonEmpty(os.Getenv("EMBY_LIVE_ITEM"), "520826")
	imgType := firstNonEmpty(os.Getenv("EMBY_LIVE_TYPE"), "Primary")

	e := NewEmby(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	before, ctype := liveFetchImage(t, cfg, itemID, imgType)
	sumBefore := md5.Sum(before)
	t.Logf("改前：%d 字节 / %s / md5 %s", len(before), ctype, hex.EncodeToString(sumBefore[:]))

	if err := e.UploadImage(ctx, itemID, imgType, -1, before, ctype); err != nil {
		t.Fatalf("上传失败（线上那个 base64 报错应当已被回退逻辑吃掉）：%v", err)
	}

	after, ctypeAfter := liveFetchImage(t, cfg, itemID, imgType)
	sumAfter := md5.Sum(after)
	t.Logf("改后：%d 字节 / %s / md5 %s", len(after), ctypeAfter, hex.EncodeToString(sumAfter[:]))

	if sumBefore != sumAfter {
		t.Fatalf("图片被改动了：md5 %s -> %s（原样回传不应产生任何变化）",
			hex.EncodeToString(sumBefore[:]), hex.EncodeToString(sumAfter[:]))
	}
	if !hasImageTag(ctx, t, e, itemID, imgType) {
		t.Errorf("上传后 %s 上应当存在 %s 标签", itemID, imgType)
	}
}

// 实机读路径验证：确认用户作用域路由能取到详情（线上 404 的回归）。
func TestLiveItemDetail(t *testing.T) {
	if os.Getenv("EMBY_LIVE") != "1" {
		t.Skip("未设置 EMBY_LIVE=1，跳过实机验证")
	}
	cfg := loadLiveConfig(t)
	itemID := firstNonEmpty(os.Getenv("EMBY_LIVE_ITEM"), "520826")

	e := NewEmby(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	it, err := e.ItemDetail(ctx, itemID)
	if err != nil {
		t.Fatalf("详情读取失败：%v", err)
	}
	if it["Id"] != itemID {
		t.Errorf("Id 不匹配：%v", it["Id"])
	}
	t.Logf("读到：%v / %v / %d 个字段", it["Name"], it["Type"], len(it))

	// 只读接口不该改动任何东西，这里顺带把「读出来的内容原样写回」也验一遍
	// ——UpdateItem 现在以完整 DTO 为底，字段不该丢。
	before := len(it)
	if err := e.UpdateItem(ctx, itemID, map[string]any{"Overview": it["Overview"]}); err != nil {
		t.Fatalf("原样写回失败：%v", err)
	}
	after, err := e.ItemDetail(ctx, itemID)
	if err != nil {
		t.Fatalf("回读失败：%v", err)
	}
	if len(after) < before {
		t.Errorf("写回后字段变少了：%d -> %d（整对象替换没兜住）", before, len(after))
	}
	for _, k := range []string{"Name", "Overview", "ProviderIds"} {
		if before, ok := it[k]; ok {
			if fmt.Sprint(after[k]) != fmt.Sprint(before) {
				t.Errorf("字段 %s 变了：%v -> %v", k, before, after[k])
			}
		}
	}
}

// 实机「写入 → 回滚」验证。默认跳过，只有显式打开才跑：
//
//	EMBY_LIVE=1 go test -run TestLiveItemWriteRollback -v
//
// 为什么非要在实机上跑一遍：mock Emby 是照真实 4.9 的行为建模的，但「把字段还原回
// 原来的样子，列表字段要发 `[]`」这条是**整对象替换**语义里最反直觉的一处 ——
// mock 里对了不等于真 Emby 里也对（发 `null` 会被 updateItem 的 nil 防护吞掉，
// 于是字段根本没还原，而界面显示「已回滚」）。
//
// 还有一个只有实机能暴露的前提：**这个构建到底认不认这个字段**。
// 详情接口不返回的字段（`Countries` / `Genres` / `Tags`…）写进去也读不回来 ——
// 那说明它不参与这条读写链路，拿它做实验会得到假结论。所以这里先写一次探针值、
// 确认读得回来，读不回来就 Skip 并指出该换哪个字段（见 EMBY_LIVE_FIELD）。
//
// 安全性：先记下字段原值、挂 defer 兜底还原，快照落在 t.TempDir() 里，
// 完全不碰用户自己的 cache/sync_history.json。
func TestLiveItemWriteRollbackIsIdempotent(t *testing.T) {
	if os.Getenv("EMBY_LIVE") != "1" {
		t.Skip("未设置 EMBY_LIVE=1，跳过实机写验证")
	}
	cfg := loadLiveConfig(t) // 必须先读真实配置：testApp 会把 EMBYME_HOME 改到临时目录
	itemID := firstNonEmpty(os.Getenv("EMBY_LIVE_ITEM"), "520826")
	field := firstNonEmpty(os.Getenv("EMBY_LIVE_FIELD"), "ProductionLocations")
	if !itemListFields[field] {
		t.Fatalf("EMBY_LIVE_FIELD=%q 不是列表字段 —— 这个用例专门验「清空列表要发 []」", field)
	}

	app := testApp(t, cfg.EmbyURL, cfg.MetaTubeURL)
	if err := app.store.Update(func(c *Config) {
		c.Token = cfg.Token
		c.UserID = cfg.UserID
	}); err != nil {
		t.Fatal(err)
	}
	e := NewEmby(app.store.Get())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	item, err := e.ItemDetail(ctx, itemID)
	if err != nil {
		t.Fatalf("详情读取失败：%v", err)
	}
	orig, hadOrig := item[field]
	origBlank := !hadOrig || isBlank(orig)
	// 还原目标：原来有值就还原成原值，原来没有就还原成空列表。
	restore := any([]any{})
	if !origBlank {
		restore = orig
	}
	t.Logf("样本：%v；%s 原值 = %s", item["Name"], field, formatFieldValue(orig))

	// defer 兜底：无论中间哪一步失败，都把字段还原回去。
	defer func() {
		if err := e.UpdateItemExact(context.Background(), itemID, map[string]any{field: restore}); err != nil {
			t.Errorf("兜底还原 %s 失败：%v（请手工确认条目 %s）", field, err, itemID)
		}
	}()

	const probe = "__live_rollback_probe__"
	patch := map[string]any{field: []any{probe}}
	recID := app.recordItemWrite(item, patch, "实机写入回滚验证")
	if recID == "" {
		t.Fatal("写入前没有留下快照 —— 这个用例要验的正是快照能不能把字段还原回去")
	}

	if err := e.UpdateItem(ctx, itemID, patch); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	after, err := e.ItemDetail(ctx, itemID)
	if err != nil {
		t.Fatalf("写入后回读失败：%v", err)
	}
	if dumped := fmt.Sprint(after[field]); !strings.Contains(dumped, probe) {
		// 不是产品 bug：这个构建不认这个字段（写进去读不回来），换一个再跑。
		t.Skipf("字段 %s 在条目 %s 上写进去读不回来（读到的还是 %s）—— "+
			"这个构建可能不返回/不接受它。换一个 EMBY_LIVE_FIELD（可用 %v）再跑；"+
			"注意：详情接口不返回的字段不参与这条读写链路，用它验不出结论。",
			field, itemID, formatFieldValue(after[field]), liveListFieldsIn(item))
	}
	t.Logf("写入生效：%s = %s", field, formatFieldValue(after[field]))

	if _, err := app.rollbackItemSync(ctx, recID); err != nil {
		t.Fatalf("回滚失败：%v", err)
	}
	restored, err := e.ItemDetail(ctx, itemID)
	if err != nil {
		t.Fatalf("回滚后回读失败：%v", err)
	}
	// 关键断言：必须真的回到原样。
	// 如果回滚发的是 `null`，它会被 updateItem 跳过，这里就会读到探针值。
	if formatFieldValue(restored[field]) != formatFieldValue(orig) {
		t.Fatalf("回滚没有把 %s 还原：%s → %s（期望 %s）—— 回滚多半是发了 null 被 nil 防护吞掉了",
			field, formatFieldValue(orig), formatFieldValue(restored[field]), formatFieldValue(orig))
	}
	if origBlank && !isBlank(restored[field]) {
		t.Fatalf("回滚没有把 %s 清空：%s", field, formatFieldValue(restored[field]))
	}
	t.Logf("回滚还原正确：%s = %s", field, formatFieldValue(restored[field]))

	// 同一条记录不能回滚两次（否则第二次会用「已经还原过的」状态再写一遍）。
	if _, err := app.rollbackItemSync(ctx, recID); err == nil {
		t.Error("同一条记录被允许回滚两次")
	}
}

// liveListFieldsIn 列出这个条目 DTO 里**真的出现**的列表字段，
// 出错时告诉用户该把 EMBY_LIVE_FIELD 换成什么。
func liveListFieldsIn(item Item) []string {
	var out []string
	for k := range item {
		if itemListFields[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// ---------- 辅助 ----------

func loadLiveConfig(t *testing.T) Config {
	t.Helper()
	store, err := NewStore(dataDir() + "/config.json")
	if err != nil {
		t.Fatalf("加载 config.json 失败：%v", err)
	}
	cfg := store.Get()
	if cfg.EmbyURL == "" || cfg.Token == "" {
		t.Fatal("config.json 里没有 Emby 地址或令牌")
	}
	return cfg
}

func liveFetchImage(t *testing.T, cfg Config, itemID, imgType string) ([]byte, string) {
	t.Helper()
	u := cfg.EmbyURL + "/Items/" + itemID + "/Images/" + imgType
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Emby-Token", cfg.Token)
	resp, err := newHTTPClient(cfg).Do(req)
	if err != nil {
		t.Fatalf("取图片失败：%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("取图片返回 %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return data, resp.Header.Get("Content-Type")
}

func hasImageTag(ctx context.Context, t *testing.T, e *Emby, itemID, imgType string) bool {
	t.Helper()
	it, err := e.ItemDetail(ctx, itemID)
	if err != nil {
		t.Fatalf("回读条目失败：%v", err)
	}
	tags, _ := it["ImageTags"].(map[string]any)
	_, ok := tags[imgType]
	return ok
}
