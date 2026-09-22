package main

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
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
