package main

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 夹具来自 cache/debug/ 里落盘的**真实响应**（用 tools/extract_cn_fixtures.py 逐字节截取）。
// 不要手写、也不要"顺手补全"结构 —— 夹具和线上不一致时，单测全绿线上全挂。

func cnFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "cn", name))
	if err != nil {
		t.Fatalf("读取夹具失败：%v", err)
	}
	return b
}

// ---------- 番号归一化 ----------

func TestCNNormAndExact(t *testing.T) {
	cases := []struct{ in, want string }{
		{"91CM-014", "91CM14"},
		{"91cm014", "91CM14"},
		{"91CM074", "91CM74"},
		{"MDHG0010", "MDHG10"},
		{"SSNI-989", "SSNI989"},
		{"91BCM-002", "91BCM2"},
		{"", ""},
	}
	for _, c := range cases {
		if got := cnNorm(c.in); got != c.want {
			t.Errorf("cnNorm(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}

	same := [][2]string{
		{"91CM-014", "91CM014"},
		{"91CM-014", "91cm-14"},
		{"91CM-014", "【91CM-014】标题"},
		{"MDHG0010", "MDHG0010 这个面试有点硬"},
	}
	for _, c := range same {
		if !cnExact(c[0], c[1]) {
			t.Errorf("cnExact(%q, %q) 应为 true", c[0], c[1])
		}
	}

	// 关键：madouqu 的模糊搜索会把 91CM074/084/094 一起返回，
	// 这些**绝不能**被判成 91CM-014 的精确命中。
	diff := [][2]string{
		{"91CM-014", "91CM074"},
		{"91CM-014", "91CM084 換妻"},
		{"91CM-014", "91CM094"},
		{"91CM-014", "SSNI-989"},
	}
	for _, c := range diff {
		if cnExact(c[0], c[1]) {
			t.Errorf("cnExact(%q, %q) 应为 false", c[0], c[1])
		}
	}
}

func TestCNExtractNumber(t *testing.T) {
	cases := []struct{ in, want string }{
		{"91CM-014 女優面試", "91CM-014"},
		{"91CM074 女優面試", "91CM-074"}, // 统一成「前缀-数字」形式
		{"[無碼破解]SSNI-989 出差的旅馆…", "SSNI-989"},
		{"MDHG0010 这个面试有点硬", "MDHG-0010"},
		{"91BCM-002", "91BCM-002"},
		{"果冻传媒", ""},
		{"01:19:24", ""},
		{"2021-04-06", ""},
		// 压制组标记不能被当成番号
		{"HEVC10 1080P WEB-DL", ""},
		{"[WEBRIP]SSNI-989", "SSNI-989"},
	}
	for _, c := range cases {
		if got := cnExtractNumber(c.in); got != c.want {
			t.Errorf("cnExtractNumber(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// TestCNItemNumber 守住一个真实的坑：
// 通用 itemNumber() 的正则要求前缀是纯字母，`91CM-014` 会被压成 `CM-014`，
// 拿这个去搜索国产传媒站点必然一无所获。
func TestCNItemNumber(t *testing.T) {
	cases := []struct {
		item Item
		want string
	}{
		{Item{"Name": "91CM-014", "Path": "/media/国产传媒/91CM-014.mp4"}, "91CM-014"},
		{Item{"Name": "91CM014", "Path": "/media/国产传媒/91CM014.mp4"}, "91CM-014"},
		{Item{"Name": "【麻豆传媒原创】MDHG0010", "Path": "/m/a.mp4"}, "MDHG-0010"},
		{Item{"Name": "SSNI-989 三上悠亜", "Path": "/m/b.mp4"}, "SSNI-989"},
	}
	for _, c := range cases {
		if got := cnItemNumber(c.item); got != c.want {
			t.Errorf("cnItemNumber(%v) = %q，期望 %q", c.item, got, c.want)
		}
	}
	// 通用逻辑会把前缀数字吃掉 —— 这是 cnItemNumber 存在的意义。
	// itemNumber 现在也做了针对性修正（只在确认「前缀被砍」时才改用 CN 规则）。
	if got := itemNumber(Item{"Name": "91CM-014"}); got != "91CM-014" {
		t.Errorf("itemNumber(91CM-014) = %q，期望 91CM-014（前缀数字不该被砍掉）", got)
	}
	if got := itemNumber(Item{"Name": "HEVC10 1080P"}); got != "" {
		t.Errorf("itemNumber(HEVC10 1080P) = %q，压制组标记不该被当番号", got)
	}
}

// 文件扩展名不该被当成番号。
//
// 踩过的坑：番号正则允许「2 位字母 + 1 位数字」，于是 `.mp4` 被拆成 "MP"+"4"，
// **每一个 mp4 条目都凭空多出一个「番号 MP-4」** —— 角标、写进 Tags 的内容、
// javbus 搜索关键词全跟着错。修复前这个库里 200 条有 196 条「有番号」，
// 其中相当一部分就是这种误报。
func TestCNItemNumberIgnoresFileExtension(t *testing.T) {
	cases := []struct {
		name string
		item Item
	}{
		{"mp4", Item{"Name": "【麻豆传媒原创】办公室内偷情", "Path": "/media/国产传媒/未知/未知.mp4"}},
		{"mp3", Item{"Name": "某个片名", "Path": "/media/x.mp3"}},
		{"mkv", Item{"Name": "某个片名", "Path": "/media/x.mkv"}},
		{"分卷标记", Item{"Name": "某个片名", "Path": "/media/某个片名.CD1.mkv"}},
		{"只有扩展名", Item{"Name": "HHH", "Path": "/x/abc.mp4"}},
	}
	for _, c := range cases {
		if got := cnItemNumber(c.item); got != "" {
			t.Errorf("%s：cnItemNumber 应为空，实际 %q（扩展名被当成番号了）", c.name, got)
		}
		if got := itemNumber(c.item); got != "" {
			t.Errorf("%s：itemNumber 应为空，实际 %q", c.name, got)
		}
	}
	// 反过来：扩展名去掉之后，真番号必须还在。
	ok := Item{"Name": "91CM-014", "Path": "/media/91CM-014/91CM-014.mp4"}
	if got := cnItemNumber(ok); got != "91CM-014" {
		t.Errorf("去掉扩展名后番号丢了：%q", got)
	}
}

// ---------- 各站点解析（夹具 = 真实响应） ----------

func TestParseXChina(t *testing.T) {
	hits := parseXChina("https://xchina.co/search.html?keyword=91CM-014",
		cnFixture(t, "xchina_search_91cm014.html"))
	if len(hits) != 1 {
		t.Fatalf("期望 1 条结果，实际 %d 条", len(hits))
	}
	r := hits[0]
	if r.Title != "日本街头拜金女大测试" {
		t.Errorf("标题 = %q", r.Title)
	}
	if r.Number != "91CM-014" {
		t.Errorf("番号 = %q，期望 91CM-014（从 .tags 里取）", r.Number)
	}
	if r.Cover != "https://upload.xchina.io/video/63b104a71ed7f.webp" {
		t.Errorf("封面 = %q（应从 style 的 background-image 里取）", r.Cover)
	}
	if r.URL != "https://xchina.co/video/id-63b104a71ed7f.html" {
		t.Errorf("详情地址 = %q（相对路径要补成绝对）", r.URL)
	}
	if !cnHasString(r.Tags, "果冻传媒") {
		t.Errorf("分类标签缺失，实际 %v", r.Tags)
	}
	// 评论数（6）和时长（01:19:24）带图标，不能被当成标签
	for _, bad := range []string{"6", "01:19:24"} {
		if cnHasString(r.Tags, bad) {
			t.Errorf("带图标的数字 %q 混进了标签：%v", bad, r.Tags)
		}
	}
	if !cnExact("91CM-014", r.Number) {
		t.Error("应判为精确命中")
	}
}

func TestParseMadouqu(t *testing.T) {
	hits := parseMadouqu("https://madouqu.com/?s=91CM-014",
		cnFixture(t, "madouqu_search_91cm014.html"))
	if len(hits) != 12 {
		t.Fatalf("期望 12 条结果（夹具里 12 个 article），实际 %d 条", len(hits))
	}
	r := hits[0]
	if r.Number != "91CM-074" || r.Title != "91CM074 女優面試" {
		t.Errorf("首条 = %q / %q", r.Number, r.Title)
	}
	if r.Date != "2021-04-06" {
		t.Errorf("日期 = %q（应从 <time datetime> 取）", r.Date)
	}
	if !strings.HasPrefix(r.Cover, "https://i0.wp.com/") {
		t.Errorf("封面 = %q（应从 lazyload 的 data-src 取）", r.Cover)
	}
	if !cnHasString(r.Tags, "果冻传媒") {
		t.Errorf("分类缺失，实际 %v", r.Tags)
	}

	// 这一条是本次功能的**核心防线**：madouqu 是模糊搜索，
	// 搜 91CM-014 返回的全是别的番号，一条都不该被判成精确命中。
	for _, h := range hits {
		if cnExact("91CM-014", h.Number) {
			t.Errorf("模糊结果 %q 被判成了 91CM-014 的精确命中", h.Number)
		}
	}
}

func TestParseMadouClub(t *testing.T) {
	hits := parseMadouClub("https://madou.club/?s=MDHG0010",
		cnFixture(t, "madou_club_search.html"))
	if len(hits) != 1 {
		t.Fatalf("期望 1 条结果，实际 %d 条", len(hits))
	}
	r := hits[0]
	if r.Number != "MDHG-0010" {
		t.Errorf("番号 = %q", r.Number)
	}
	if !strings.HasPrefix(r.Title, "MDHG0010 ") {
		t.Errorf("标题 = %q", r.Title)
	}
	// src 是 /showcase/img/thumb.png 占位图，必须用 data-src
	if r.Cover != "https://madou.club/covers/2024/09/6bbec8bde75b343-240x180.jpg" {
		t.Errorf("封面 = %q（不能用占位图）", r.Cover)
	}
	if !cnHasString(r.Tags, "麻豆传媒") {
		t.Errorf("分类缺失，实际 %v", r.Tags)
	}
	if !cnExact("MDHG0010", r.Number) {
		t.Error("应判为精确命中")
	}
}

func TestParse7MMTV(t *testing.T) {
	hits := parse7MMTV("https://7mmtv.sx/zh/searchall_search/all/SSNI-989/1.html",
		cnFixture(t, "7mmtv_search_ssni989.html"))
	if len(hits) != 3 {
		t.Fatalf("期望 3 条结果，实际 %d 条", len(hits))
	}
	exact := 0
	for _, h := range hits {
		if cnExact("SSNI-989", h.Number) {
			exact++
		}
		if !strings.Contains(h.Title, "SSNI-989") {
			t.Errorf("标题里没有番号：%q", h.Title)
		}
		if h.Cover == "" {
			t.Errorf("封面为空：%q", h.Title)
		}
	}
	if exact != 3 {
		t.Errorf("精确命中 %d 条，期望 3 条", exact)
	}
	if hits[0].Date != "2023-02-21" {
		t.Errorf("日期 = %q", hits[0].Date)
	}
}

// ---------- 合并：只认精确命中 ----------

func TestPickCNOnlyExact(t *testing.T) {
	hits := []CNSiteHits{
		{Site: "xchina", OK: true, Hits: []CNResult{
			{Number: "91CM-014", Title: "日本街头拜金女大测试", Cover: "https://x/cover.webp", Tags: []string{"果冻传媒"}},
		}},
		{Site: "madouqu", OK: true, Hits: []CNResult{
			// 模糊命中，必须被丢掉
			{Number: "91CM074", Title: "91CM074 女優面試", Cover: "https://m/wrong.jpg", Date: "2021-04-06"},
		}},
		{Site: "madou", OK: false, Error: "HTTP 500"},
	}
	pick, matched := pickCN(hits, "91CM-014")
	if len(matched) != 1 || matched[0] != "xchina" {
		t.Fatalf("命中站点 = %v，期望只有 xchina", matched)
	}
	if pick.Cover != "https://x/cover.webp" || pick.CoverFrom != "xchina" {
		t.Errorf("封面取错：%q（来自 %s）", pick.Cover, pick.CoverFrom)
	}
	if pick.Date != "" {
		t.Errorf("不该从模糊命中的 madouqu 取日期，实际 %q（来自 %s）", pick.Date, pick.DateFrom)
	}
	if pick.Title != "日本街头拜金女大测试" {
		t.Errorf("标题 = %q", pick.Title)
	}
}

func TestPickCNPriority(t *testing.T) {
	// 两个站点都精确命中时，封面/标题按 cnSiteOrder（xchina 优先）取，
	// 但缺失字段要能由后面的站点补上。
	hits := []CNSiteHits{
		{Site: "xchina", OK: true, Hits: []CNResult{
			{Number: "SSNI-989", Title: "xchina 标题", Cover: "https://x/c.jpg"},
		}},
		{Site: "7mmtv", OK: true, Hits: []CNResult{
			{Number: "SSNI-989", Title: "7mmtv 标题", Cover: "https://7/c.jpg", Date: "2023-02-21", Tags: []string{"windwalker"}},
		}},
	}
	pick, matched := pickCN(hits, "SSNI-989")
	if len(matched) != 2 {
		t.Fatalf("命中站点 = %v，期望 2 个", matched)
	}
	if pick.Title != "xchina 标题" || pick.TitleFrom != "xchina" {
		t.Errorf("标题应取 xchina 的，实际 %q（来自 %s）", pick.Title, pick.TitleFrom)
	}
	if pick.CoverFrom != "xchina" {
		t.Errorf("封面应取 xchina 的，实际来自 %s", pick.CoverFrom)
	}
	if pick.Date != "2023-02-21" || pick.DateFrom != "7mmtv" {
		t.Errorf("xchina 没有日期，应由 7mmtv 补上，实际 %q（来自 %s）", pick.Date, pick.DateFrom)
	}
	if pick.TagFrom != "7mmtv" {
		t.Errorf("标签应取 7mmtv 的，实际来自 %s", pick.TagFrom)
	}
}

// ---------- 搜索地址构造 ----------

func TestCNSearchURL(t *testing.T) {
	cfg := Config{CNSites: defaultCNSites()}
	cn := NewCNMedia(cfg)
	cases := []struct{ key, want string }{
		{"xchina", "https://xchina.co/search.html?keyword=91CM-014"},
		{"madouqu", "https://madouqu.com/?s=91CM-014"},
		{"madou", "https://madou.club/?s=91CM-014"},
		{"7mmtv", "https://7mmtv.sx/zh/searchall_search/all/91CM-014/1.html"},
	}
	for _, c := range cases {
		if got := cn.searchURL(c.key, "91CM-014"); got != c.want {
			t.Errorf("searchURL(%s) = %q，期望 %q", c.key, got, c.want)
		}
	}
	// 空配置要回落到默认地址，而不是拼出相对路径
	cn2 := NewCNMedia(Config{})
	if got := cn2.searchURL("xchina", "ABC-1"); !strings.HasPrefix(got, "https://") {
		t.Errorf("空配置没回落到默认地址：%q", got)
	}
}

// ---------- 标题覆盖策略 ----------

func TestCNShouldSetTitle(t *testing.T) {
	yes := []struct{ cur, num string }{
		{"", "91CM-014"},
		{"91CM-014", "91CM-014"},           // 标题就是番号
		{"91cm014", "91CM-014"},            // 番号的无横线写法
		{"91CM-014.mp4", "91CM-014"},       // 文件名
		{"ABC-123.mkv", "ABC-123"},         // 文件名
		{"hhd800.com@91CM014", "91CM-014"}, // 短标题且含番号
	}
	for _, c := range yes {
		if !cnShouldSetTitle(c.cur, c.num) {
			t.Errorf("cnShouldSetTitle(%q, %q) 应为 true", c.cur, c.num)
		}
	}
	no := []struct{ cur, num string }{
		{"日本街头拜金女大测试", "91CM-014"},
		{"这个面试有点硬 女优私密档案 麻豆活泼可爱担当", "MDHG0010"},
	}
	for _, c := range no {
		if cnShouldSetTitle(c.cur, c.num) {
			t.Errorf("cnShouldSetTitle(%q, %q) 应为 false（已经是像样的片名）", c.cur, c.num)
		}
	}
}

// ---------- 端到端：真实夹具 + mock 站点 + mock Emby ----------

// cnTestServers 起两个 mock 站点：xchina/madouqu/7mmtv 共用一个，
// madou.club 单独一个（它和 madouqu 都用 /?s=，同一个服务器没法区分）。
func cnTestServers(t *testing.T) (mainSrv, madouSrv *httptest.Server) {
	t.Helper()
	mainMux := http.NewServeMux()
	mainMux.HandleFunc("GET /search.html", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(cnFixture(t, "xchina_search_91cm014.html"))
	})
	mainMux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(cnFixture(t, "madouqu_search_91cm014.html"))
	})
	mainMux.HandleFunc("GET /zh/searchall_search/all/{kw}/{page}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(cnFixture(t, "7mmtv_search_ssni989.html"))
	})
	madouMux := http.NewServeMux()
	madouMux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(cnFixture(t, "madou_club_search.html"))
	})
	mainSrv = httptest.NewServer(mainMux)
	madouSrv = httptest.NewServer(madouMux)
	t.Cleanup(mainSrv.Close)
	t.Cleanup(madouSrv.Close)
	return mainSrv, madouSrv
}

func cnTestApp(t *testing.T, m *mockEmby, mainSrv, madouSrv *httptest.Server) *App {
	t.Helper()
	app := testApp(t, m.srv.URL, "http://127.0.0.1:1")
	if err := app.store.Update(func(c *Config) {
		c.CNSites = map[string]string{
			"xchina":  mainSrv.URL,
			"madouqu": mainSrv.URL,
			"madou":   madouSrv.URL,
			"7mmtv":   mainSrv.URL,
		}
	}); err != nil {
		t.Fatal(err)
	}
	return app
}

func TestCNScrapeDryRunDoesNotWrite(t *testing.T) {
	m := newMockEmby(t)
	mainSrv, madouSrv := cnTestServers(t)
	app := cnTestApp(t, m, mainSrv, madouSrv)

	m.mu.Lock()
	m.items["it-cn"] = map[string]any{
		"Id": "it-cn", "Name": "91CM-014", "Type": "Movie",
		"ImageTags":   map[string]any{},
		"ProviderIds": map[string]any{},
		"Path":        "/media/国产传媒/91CM-014.mp4",
	}
	m.mu.Unlock()

	res, err := app.ScrapeCN(context.Background(), "it-cn", "", CNOptions{
		Fields: cnFieldsFrom(nil),
		DryRun: true,
	})
	if err != nil {
		t.Fatalf("刮削失败：%v", err)
	}
	if len(res.Matched) != 1 || res.Matched[0] != "xchina" {
		t.Errorf("命中站点 = %v，期望只有 xchina（madouqu 是模糊命中，madou 与 7mmtv 没有该番号）", res.Matched)
	}
	if res.Cover == "" || res.Title == "" {
		t.Errorf("应合并出封面与标题，实际 cover=%q title=%q", res.Cover, res.Title)
	}
	if res.Applied {
		t.Error("试运行不该写入任何数据")
	}
	// 站点列表要如实回给前端，包括「搜到了但番号对不上」
	if len(res.Sites) != 4 {
		t.Fatalf("站点结果数 = %d，期望 4", len(res.Sites))
	}
	for _, s := range res.Sites {
		if !s.OK {
			t.Errorf("站点 %s 抓取失败：%s", s.Site, s.Error)
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.patched) != 0 || len(m.uploaded) != 0 || m.refresh != 0 {
		t.Errorf("试运行却发生了写操作：patched=%v uploaded=%v refresh=%d", m.patched, m.uploaded, m.refresh)
	}
}

func TestCNScrapeDryRunFindsSevenMMTV(t *testing.T) {
	m := newMockEmby(t)
	mainSrv, madouSrv := cnTestServers(t)
	app := cnTestApp(t, m, mainSrv, madouSrv)

	m.mu.Lock()
	m.items["it-7mm"] = map[string]any{
		"Id": "it-7mm", "Name": "SSNI-989", "Type": "Movie",
		"ImageTags": map[string]any{}, "ProviderIds": map[string]any{},
	}
	m.mu.Unlock()

	res, err := app.ScrapeCN(context.Background(), "it-7mm", "", CNOptions{Fields: cnFieldsFrom(nil), DryRun: true})
	if err != nil {
		t.Fatalf("刮削失败：%v", err)
	}
	if len(res.Matched) != 1 || res.Matched[0] != "7mmtv" {
		t.Errorf("命中站点 = %v，期望只有 7mmtv", res.Matched)
	}
	if res.Date != "2023-02-21" {
		t.Errorf("日期 = %q，期望 2023-02-21", res.Date)
	}
}

func TestCNApplyWritesFieldsAndCover(t *testing.T) {
	m := newMockEmby(t)
	mainSrv, madouSrv := cnTestServers(t)
	app := cnTestApp(t, m, mainSrv, madouSrv)

	// 封面走测试服务器，避免单测访问外网
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	coverSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(buf.Bytes())
	}))
	defer coverSrv.Close()

	m.mu.Lock()
	m.items["it-cn"] = map[string]any{
		"Id": "it-cn", "Name": "91CM-014", "Type": "Movie",
		"ImageTags":   map[string]any{},
		"ProviderIds": map[string]any{},
		"Overview":    "原有简介不能丢",
	}
	m.mu.Unlock()

	e := NewEmby(app.store.Get())
	item, err := e.ItemDetail(context.Background(), "it-cn")
	if err != nil {
		t.Fatal(err)
	}
	pick := &CNPicked{
		Title: "日本街头拜金女大测试", TitleFrom: "xchina",
		Cover: coverSrv.URL + "/c.jpg", CoverFrom: "xchina",
		Tags: []string{"果冻传媒"}, TagFrom: "xchina",
		Date: "2021-04-06", DateFrom: "madouqu",
	}
	applied, note, err := app.applyCN(context.Background(), e, item, pick, CNOptions{
		Fields: cnFieldsFrom(nil),
	})
	if err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	if note != "" {
		t.Errorf("不该有告警：%s", note)
	}
	for _, want := range []string{"标题", "日期", "封面"} {
		if !cnHasString(applied, want) {
			t.Errorf("应写入 %s，实际 %v", want, applied)
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	patched := m.patched["it-cn"]
	if patched == nil {
		t.Fatal("没有发生 POST /Items/it-cn")
	}
	if patched["Name"] != "日本街头拜金女大测试" {
		t.Errorf("Name = %v", patched["Name"])
	}
	if patched["PremiereDate"] != "2021-04-06T00:00:00.0000000Z" {
		t.Errorf("PremiereDate = %v", patched["PremiereDate"])
	}
	// 整对象替换：原有的 Overview 不能被抹掉
	if patched["Overview"] != "原有简介不能丢" {
		t.Errorf("Overview 丢了：%v", patched["Overview"])
	}
	tags, _ := patched["Tags"].([]any)
	if len(tags) != 2 || tags[0] != "91CM-014" || tags[1] != "果冻传媒" {
		t.Errorf("Tags = %v，期望 [91CM-014 果冻传媒]", patched["Tags"])
	}
	// 图片上传要走 base64 回退（mock 是严格构建）：
	// 先发原始字节被 500 拒掉，再换 base64。封面（Primary）+ 缩略图（Thumb）
	// 复用同一份字节各传一次，共 4 次请求记录，最后一条必须是 Thumb 的 b64。
	if len(m.uploaded) == 0 {
		t.Fatal("没有发生封面上传")
	}
	last := m.uploaded[len(m.uploaded)-1]
	if !strings.Contains(last, "it-cn/Thumb/-1/image/jpeg/b64") {
		t.Errorf("上传最后一条 = %q，期望缩略图 base64 形态", last)
	}
	b64 := 0
	for _, u := range m.uploaded {
		if strings.HasSuffix(u, "/b64") {
			b64++
		}
	}
	if b64 != 2 {
		t.Errorf("base64 上传次数 = %d，期望 2（封面 + 缩略图各 1，回退成功就不该再试）", b64)
	}
}

func TestCNApplySkipsExistingCoverAndTitle(t *testing.T) {
	m := newMockEmby(t)
	mainSrv, madouSrv := cnTestServers(t)
	app := cnTestApp(t, m, mainSrv, madouSrv)

	m.mu.Lock()
	m.items["it-2"] = map[string]any{
		"Id": "it-2", "Name": "日本街头拜金女大测试", "Type": "Movie",
		"ImageTags":   map[string]any{"Primary": "tag1"},
		"ProviderIds": map[string]any{},
	}
	m.mu.Unlock()

	e := NewEmby(app.store.Get())
	item, err := e.ItemDetail(context.Background(), "it-2")
	if err != nil {
		t.Fatal(err)
	}
	pick := &CNPicked{Title: "站点标题", Cover: "https://example.com/c.jpg", Date: "2021-04-06"}
	applied, note, err := app.applyCN(context.Background(), e, item, pick, CNOptions{Fields: cnFieldsFrom(nil)})
	if err != nil {
		t.Fatal(err)
	}
	if cnHasString(applied, "封面") || cnHasString(applied, "标题") {
		t.Errorf("已有封面 + 已是像样片名，不该覆盖：%v", applied)
	}
	if !strings.Contains(note, "已有封面") {
		t.Errorf("应说明跳过原因，实际 %q", note)
	}
	if !strings.Contains(note, "标题") {
		t.Errorf("应说明标题跳过，实际 %q", note)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.uploaded) != 0 {
		t.Errorf("不该上传封面：%v", m.uploaded)
	}
	if p := m.patched["it-2"]; p != nil && p["Name"] != "日本街头拜金女大测试" {
		t.Errorf("标题被覆盖了：%v", p["Name"])
	}
}

// ---------- API 层 ----------

// decodeJSON 把响应体解析到 out。
func decodeJSON(r io.Reader, out any) error {
	return json.NewDecoder(r).Decode(out)
}

func TestHandleCNSitesAndSearch(t *testing.T) {
	m := newMockEmby(t)
	mainSrv, madouSrv := cnTestServers(t)
	app := cnTestApp(t, m, mainSrv, madouSrv)

	srv := httptest.NewServer(app.route())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/cn/sites")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sites struct {
		Data []struct{ Key, Name, URL string } `json:"data"`
	}
	if err := decodeJSON(resp.Body, &sites); err != nil {
		t.Fatal(err)
	}
	if len(sites.Data) != 4 {
		t.Fatalf("站点数 = %d，期望 4", len(sites.Data))
	}
	if sites.Data[0].Key != "xchina" {
		t.Errorf("第一个站点应是 xchina（优先级最高），实际 %s", sites.Data[0].Key)
	}

	resp2, err := http.Get(srv.URL + "/api/cn/search?q=91CM-014")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var found struct {
		Data struct {
			Matched []string `json:"matched"`
			Sites   []struct {
				Site string `json:"site"`
				OK   bool   `json:"ok"`
				Hits []struct {
					Number string `json:"number"`
					Exact  bool   `json:"exact"`
				} `json:"hits"`
			} `json:"sites"`
		} `json:"data"`
	}
	if err := decodeJSON(resp2.Body, &found); err != nil {
		t.Fatal(err)
	}
	if len(found.Data.Matched) != 1 || found.Data.Matched[0] != "xchina" {
		t.Errorf("matched = %v，期望 [xchina]", found.Data.Matched)
	}
	// 模糊结果要如实回传，但 exact 必须是 false（前端靠它区分「搜到了但番号不对」）
	var fuzzy int
	for _, s := range found.Data.Sites {
		if s.Site != "madouqu" {
			continue
		}
		for _, h := range s.Hits {
			if h.Exact {
				t.Errorf("madouqu 的 %s 被标成精确命中", h.Number)
			}
			fuzzy++
		}
	}
	if fuzzy == 0 {
		t.Error("madouqu 的模糊结果没有被回传")
	}

	// 缺参数要 400
	resp3, err := http.Get(srv.URL + "/api/cn/search")
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusBadRequest {
		t.Errorf("缺参数应 400，实际 %d", resp3.StatusCode)
	}
}

func TestCNMediaCacheInvalidatesOnConfigChange(t *testing.T) {
	m := newMockEmby(t)
	app := testApp(t, m.srv.URL, "http://127.0.0.1:1")

	first := app.cnMedia()
	if again := app.cnMedia(); again != first {
		t.Error("配置没变时应复用同一个客户端（限速器按站点共享，不能每次新建）")
	}
	if err := app.store.Update(func(c *Config) {
		c.CNSites = map[string]string{"xchina": "https://mirror.example.com"}
	}); err != nil {
		t.Fatal(err)
	}
	second := app.cnMedia()
	if second == first {
		t.Error("站点地址改了应重建客户端")
	}
	if got := second.searchURL("xchina", "ABC-1"); !strings.HasPrefix(got, "https://mirror.example.com/") {
		t.Errorf("新客户端没读到新地址：%q", got)
	}
}

// ---------- 批量按勾选的 id 处理 ----------

// cnWaitJob 轮询到任务结束。批量接口是异步的，断言前必须等它跑完。
func cnWaitJob(t *testing.T, srv *httptest.Server, id string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(srv.URL + "/api/jobs/" + id)
		if err != nil {
			t.Fatalf("轮询任务失败: %v", err)
		}
		var out struct {
			Data map[string]any `json:"data"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if st, _ := out.Data["status"].(string); st == "done" || st == "canceled" || st == "failed" {
			return out.Data
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("任务超时未结束")
	return nil
}

// 界面上是「勾选哪几张卡就刮哪几张」，后端必须按 id 精确处理，
// 不能退化成「从库头开始取 N 条」—— 那样用户勾的和实际刮的会对不上。
func TestCNScrapeBatchUsesExplicitIDs(t *testing.T) {
	m := newMockEmby(t)
	mainSrv, madouSrv := cnTestServers(t)
	app := cnTestApp(t, m, mainSrv, madouSrv)

	m.mu.Lock()
	// 这个库里第一条**没有**番号（片名是纯中文），第二条才有。
	// 如果后端退化成「取库里前 N 条」，命中数就会是 0，测试立刻红。
	m.items["cn-nonum"] = map[string]any{
		"Id": "cn-nonum", "Name": "【麻豆传媒原创】办公室内偷情", "Type": "Movie",
		"ImageTags": map[string]any{}, "ProviderIds": map[string]any{},
		"Path": "/media/国产传媒/未知/未知.mp4",
	}
	m.items["cn-hit"] = map[string]any{
		"Id": "cn-hit", "Name": "91CM-014", "Type": "Movie",
		"ImageTags": map[string]any{}, "ProviderIds": map[string]any{},
		"Path": "/media/国产传媒/91CM-014/91CM-014.mp4",
	}
	m.mu.Unlock()

	srv := httptest.NewServer(app.route())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/cn/scrape-batch", "application/json",
		strings.NewReader(`{"ids":["cn-hit"]}`))
	if err != nil {
		t.Fatal(err)
	}
	var started struct {
		Data struct {
			JobID string `json:"job_id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&started); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if started.Data.JobID == "" {
		t.Fatal("没有返回 job_id")
	}

	job := cnWaitJob(t, srv, started.Data.JobID)
	if st, _ := job["status"].(string); st != "done" {
		t.Fatalf("任务状态 = %s，期望 done", st)
	}
	list, _ := job["result"].([]any)
	if len(list) != 1 {
		t.Fatalf("结果应只有勾选的那 1 条，实际 %d 条", len(list))
	}
	r, _ := list[0].(map[string]any)
	if got := r["item_id"]; got != "cn-hit" {
		t.Errorf("处理的条目 = %#v，期望 cn-hit", got)
	}
	if applied, _ := r["applied"].(bool); !applied {
		t.Errorf("应已写入，实际 applied=%v message=%v", applied, r["message"])
	}
}

// 勾选的条目全都推不出番号时，要明确报错，而不是开一个 0 条目的任务。
func TestCNScrapeBatchRejectsIDsWithoutNumber(t *testing.T) {
	m := newMockEmby(t)
	mainSrv, madouSrv := cnTestServers(t)
	app := cnTestApp(t, m, mainSrv, madouSrv)

	m.mu.Lock()
	m.items["cn-nonum"] = map[string]any{
		"Id": "cn-nonum", "Name": "【麻豆传媒原创】办公室内偷情", "Type": "Movie",
		"ImageTags": map[string]any{}, "ProviderIds": map[string]any{},
		"Path": "/media/国产传媒/未知/未知.mp4",
	}
	m.mu.Unlock()

	srv := httptest.NewServer(app.route())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/cn/scrape-batch", "application/json",
		strings.NewReader(`{"ids":["cn-nonum"]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("期望 400，实际 %d：%s", resp.StatusCode, b)
	}
}
