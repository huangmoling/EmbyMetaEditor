package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// 本文件是线上 bug 与真实环境行为的回归测试。
//
// 起因是三个线上 bug：
//
// 1. 演员头像刮削 500：The input is not a valid Base-64 string…
// 2. 媒体库刮削 / 详情 404：找不到文件 "/Items/xxx"
// 3. 番号补全页缺失番号没有图片（javbus Referer 防盗链）
//
// 后来陆续补进了同类的「按真实服务器行为建模」的用例：
// 番号归一化与列表回填、图片代理的分类与缓存、演员按媒体库过滤。
//
// mockEmby 按真实 4.9 构建的行为建模：读走用户作用域、写走全局作用域、
// POST /Items/{id} 是整对象替换、图片上传只认 base64 文本、/Items 不返回 SortName、
// /Persons 支持 ParentId 过滤。所以下面每个用例都直接对应线上现象。

// ---------- Bug 2：条目详情 404 ----------

// 先确认 mock 真的复现了线上那个 404，否则后面的测试就是假的。
func TestMockEmbyRejectsGlobalItemDetail(t *testing.T) {
	m := newMockEmby(t)
	m.items["m1"] = map[string]any{"Id": "m1", "Name": "SSIS-001"}

	resp, err := http.Get(m.srv.URL + "/Items/m1")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("mock 应复现线上 404，实际 %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "找不到文件") {
		t.Errorf("404 文案应贴近真实服务端，实际 %q", strings.TrimSpace(string(body)))
	}
}

// 用户作用域路由必须能读到条目。
func TestItemDetailUsesUserScopedRoute(t *testing.T) {
	m := newMockEmby(t)
	m.items["m1"] = map[string]any{
		"Id": "m1", "Name": "SSIS-001",
		"Overview": "剧情简介", "ProductionYear": 2021,
	}
	e := NewEmby(Config{EmbyURL: m.srv.URL, Token: "tok-123", UserID: "u1", DeviceID: "d1"})

	it, err := e.ItemDetail(context.Background(), "m1")
	if err != nil {
		t.Fatalf("详情读取失败: %v", err)
	}
	if it["Overview"] != "剧情简介" {
		t.Errorf("Overview 异常: %v", it["Overview"])
	}
}

// userId 没配置时也要能惰性解析出来（API Key 登录场景）。
func TestItemDetailResolvesUserIDLazily(t *testing.T) {
	m := newMockEmby(t)
	m.items["m1"] = map[string]any{"Id": "m1", "Name": "SSIS-001", "Overview": "简介"}
	e := NewEmby(Config{EmbyURL: m.srv.URL, Token: "tok-123", DeviceID: "d1"})

	if _, err := e.ItemDetail(context.Background(), "m1"); err != nil {
		t.Fatalf("未预置 userId 时应惰性解析，实际报错: %v", err)
	}
	if e.UserID != "u1" {
		t.Errorf("解析出的 userId 未回填: %q", e.UserID)
	}
}

// 服务端真的没有这个条目时，错误里要带上用户作用域那次尝试的信息。
func TestItemDetailNotFound(t *testing.T) {
	m := newMockEmby(t)
	e := NewEmby(Config{EmbyURL: m.srv.URL, Token: "tok-123", UserID: "u1", DeviceID: "d1"})
	if _, err := e.ItemDetail(context.Background(), "nope"); err == nil {
		t.Fatal("不存在的条目应当报错")
	}
}

// ---------- Bug 2 的孪生坑：整对象替换会抹字段 ----------

// 这是线上真实踩过的坑：只发 {Name} 的 POST 把 Overview / PremiereDate /
// ProductionYear 等 11 个字段全抹了。UpdateItem 必须以完整 DTO 为底。
func TestUpdateItemKeepsUntouchedFields(t *testing.T) {
	m := newMockEmby(t)
	m.items["m1"] = map[string]any{
		"Id": "m1", "Name": "旧名称",
		"Overview": "原简介", "PremiereDate": "2021-01-05T00:00:00.0000000Z",
		"ProductionYear": 2021, "OriginalTitle": "原标题",
		"OfficialRating": "R", "CommunityRating": 7.5,
		"PreferredMetadataCountryCode": "JP",
		"ProviderIds":                  map[string]any{"Tmdb": "12345"},
	}
	e := NewEmby(Config{EmbyURL: m.srv.URL, Token: "tok-123", UserID: "u1", DeviceID: "d1"})

	if err := e.UpdateItem(context.Background(), "m1", map[string]any{"Name": "新名称"}); err != nil {
		t.Fatalf("更新失败: %v", err)
	}

	got := m.items["m1"]
	for _, k := range []string{
		"Overview", "PremiereDate", "ProductionYear", "OriginalTitle",
		"OfficialRating", "CommunityRating", "PreferredMetadataCountryCode",
	} {
		if _, ok := got[k]; !ok {
			t.Errorf("字段 %s 被清空了（整对象替换未以完整 DTO 为底）", k)
		}
	}
	if got["Name"] != "新名称" {
		t.Errorf("Name 未更新: %v", got["Name"])
	}
	if pids, ok := got["ProviderIds"].(map[string]any); !ok || pids["Tmdb"] != "12345" {
		t.Errorf("ProviderIds 丢失: %v", got["ProviderIds"])
	}
}

// ProviderIds 缺失会让服务端 400（Value cannot be null. (Parameter 'source')），
// 所以哪怕 patch 里没有它，body 里也必须有一个非 nil 的 ProviderIds。
func TestUpdateItemAlwaysSendsProviderIds(t *testing.T) {
	m := newMockEmby(t)
	m.items["m1"] = map[string]any{"Id": "m1", "Name": "x"}
	e := NewEmby(Config{EmbyURL: m.srv.URL, Token: "tok-123", UserID: "u1", DeviceID: "d1"})

	if err := e.UpdateItem(context.Background(), "m1", map[string]any{"Overview": "新简介"}); err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	pids, ok := m.patched["m1"]["ProviderIds"]
	if !ok || pids == nil {
		t.Errorf("body 必须带非 nil 的 ProviderIds，实际 %#v", pids)
	}
}

// 空字符串的 ProviderId 不该覆盖已有值。
func TestUpdateItemSkipsBlankProviderIds(t *testing.T) {
	m := newMockEmby(t)
	m.items["m1"] = map[string]any{
		"Id": "m1", "Name": "x",
		"ProviderIds": map[string]any{"Tmdb": "999"},
	}
	e := NewEmby(Config{EmbyURL: m.srv.URL, Token: "tok-123", UserID: "u1", DeviceID: "d1"})

	err := e.UpdateItem(context.Background(), "m1", map[string]any{
		"ProviderIds": map[string]any{"Tmdb": "", "MetaTube": "FANZA:m-1"},
	})
	if err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	pids, _ := m.items["m1"]["ProviderIds"].(map[string]any)
	if pids["Tmdb"] != "999" {
		t.Errorf("空值不应覆盖已有 ProviderId: %v", pids)
	}
	if pids["MetaTube"] != "FANZA:m-1" {
		t.Errorf("新 ProviderId 未写入: %v", pids)
	}
}

// ---------- Bug 1：头像上传 base64 ----------

func TestUploadImageFallsBackToBase64(t *testing.T) {
	m := newMockEmby(t)
	m.items["p1"] = map[string]any{"Id": "p1", "Name": "三上悠亜", "ImageTags": map[string]any{}}
	e := NewEmby(Config{EmbyURL: m.srv.URL, Token: "tok-123", UserID: "u1", DeviceID: "d1"})

	jpg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 0x4A, 0x46}
	if err := e.UploadImage(context.Background(), "p1", "Primary", -1, jpg, "image/jpeg"); err != nil {
		t.Fatalf("上传失败: %v", err)
	}

	if len(m.uploaded) != 2 {
		t.Fatalf("应当先发原始字节、失败后重试 base64，实际 %v", m.uploaded)
	}
	if !strings.HasSuffix(m.uploaded[0], "/raw") {
		t.Errorf("第一次应当是原始字节（标准 Emby 才是主流），实际 %s", m.uploaded[0])
	}
	if !strings.HasSuffix(m.uploaded[1], "/b64") {
		t.Errorf("第二次应当是 base64 文本，实际 %s", m.uploaded[1])
	}
	// 两次都必须是 image/*，否则服务端 400「Unable to determine image file extension」
	for _, u := range m.uploaded {
		if !strings.Contains(u, "image/jpeg") {
			t.Errorf("Content-Type 丢了: %s", u)
		}
	}
}

// 标准 Emby（认原始字节）上不该退化成 base64——否则图片会被静默存成文本。
func TestUploadImagePrefersRawBytes(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls = append(calls, string(body))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	e := NewEmby(Config{EmbyURL: srv.URL, Token: "t", UserID: "u1", DeviceID: "d"})
	jpg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x01}
	if err := e.UploadImage(context.Background(), "p1", "Primary", -1, jpg, "image/jpeg"); err != nil {
		t.Fatalf("上传失败: %v", err)
	}
	if len(calls) != 1 || calls[0] != string(jpg) {
		t.Errorf("标准服务器上应当只发一次原始字节，实际 %d 次: %q", len(calls), calls)
	}
}

// 非图片数据必须被挡在本地，不要发给 Emby。
func TestUploadImageRejectsNonImage(t *testing.T) {
	m := newMockEmby(t)
	e := NewEmby(Config{EmbyURL: m.srv.URL, Token: "tok-123", UserID: "u1", DeviceID: "d1"})
	err := e.UploadImage(context.Background(), "p1", "Primary", -1, []byte("this is html not an image"), "text/html")
	if err == nil {
		t.Fatal("非图片数据应当被拒绝")
	}
	if len(m.uploaded) != 0 {
		t.Errorf("不该发出请求: %v", m.uploaded)
	}
}

func TestNormalizeImageType(t *testing.T) {
	avif := []byte{0x00, 0x00, 0x00, 0x20, 'f', 't', 'y', 'p', 'a', 'v', 'i', 'f'}
	webp := append([]byte("RIFF\x00\x00\x00\x00"), []byte("WEBPVP8 ")...)
	cases := []struct {
		name string
		data []byte
		ct   string
		want string
	}{
		{"明确 jpeg", []byte("x"), "image/jpeg", "image/jpeg"},
		{"大写带参数", []byte("x"), "IMAGE/JPEG; charset=binary", "image/jpeg"},
		{"octet 靠文件头", []byte{0xFF, 0xD8, 0xFF, 0xDB}, "application/octet-stream", "image/jpeg"},
		{"无 ct 靠文件头", []byte{0xFF, 0xD8, 0xFF, 0xE0}, "", "image/jpeg"},
		{"png", append([]byte("\x89PNG\r\n\x1a\n"), 1, 2, 3), "", "image/png"},
		{"gif", []byte("GIF89a......"), "", "image/gif"},
		{"webp", webp, "", "image/webp"},
		{"avif", avif, "", "image/avif"},
		{"不是图片", []byte("hello world, definitely text"), "text/plain", ""},
		{"空数据", nil, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := normalizeImageType(c.data, c.ct); got != c.want {
				t.Errorf("normalizeImageType() = %q，期望 %q", got, c.want)
			}
		})
	}
}

// ---------- Bug 3：javbus 封面防盗链 ----------

// 分类：白名单主机由服务端代取，普通公网图床交给浏览器直连，内网/非 http 一律拒绝。
func TestImageProxyClassification(t *testing.T) {
	cfg := Config{JavBusURL: "https://www.javbus.com", GfriendsCDN: "https://cdn.jsdelivr.net"}

	proxied := []string{
		"https://www.javbus.com/pics/cover/a.jpg",
		"https://pics.dmm.co.jp/mono/movie/adult/x/x.jpg",
		"https://cdn.jsdelivr.net/gh/x/y.jpg",
		"https://WWW.JavBus.com/pics/a.jpg", // 主机名大小写不敏感
	}
	for _, raw := range proxied {
		u, proxy, err := ClassifyImage(raw, cfg)
		if err != nil {
			t.Errorf("%q 应被放行，实际 %v", raw, err)
			continue
		}
		if !proxy {
			t.Errorf("%q 应由服务端代取（实际 %s）", raw, u)
		}
	}

	direct := []string{
		"https://image.mgstage.com/a.jpg",
		"https://i.ytimg.com/vi/x/hq.jpg",
	}
	for _, raw := range direct {
		_, proxy, err := ClassifyImage(raw, cfg)
		if err != nil {
			t.Errorf("%q 应放行直连，实际 %v", raw, err)
			continue
		}
		if proxy {
			t.Errorf("%q 不该由服务端代取", raw)
		}
	}

	denied := []string{
		"https://127.0.0.1/a.jpg", // 内网地址不能被代理
		"http://localhost:8080/a.jpg",
		"https://10.0.0.5/a.jpg",
		"https://169.254.169.254/latest/meta-data/", // 云元数据
		"file:///etc/passwd",
		"ftp://www.javbus.com/a.jpg",
		"//www.javbus.com/a.jpg",
		"",
	}
	for _, raw := range denied {
		if _, _, err := ClassifyImage(raw, cfg); err == nil {
			t.Errorf("%q 不应被放行", raw)
		}
	}
}

// 用户把镜像配在内网地址上时白名单优先——那是他自己的配置。
func TestImageProxyAllowlistBeatsPrivateCheck(t *testing.T) {
	cfg := Config{GfriendsCDN: "http://192.168.1.10:8080"}
	_, proxy, err := ClassifyImage("http://192.168.1.10:8080/gf/a.jpg", cfg)
	if err != nil || !proxy {
		t.Errorf("内网镜像应在白名单内被代取，实际 proxy=%v err=%v", proxy, err)
	}
}

// noRedirectClient 不跟随重定向，方便断言 3xx 本身。
var noRedirectClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// ---------- 卡片角标：番号必须由服务端算 ----------
//
// Emby 的 /Items 列表**不返回 SortName**（实测全是 null），前端拿它当番号
// 只会退化成显示年份。所以 /api/items 与 /api/items/detail 都要带上算好的 Number。

func TestItemNumber(t *testing.T) {
	cases := []struct {
		name string
		item Item
		want string
	}{
		{"名称里带番号", Item{"Name": "SSIS-001 某个片名"}, "SSIS-001"},
		{"小写番号", Item{"Name": "ssis-001 某个片名"}, "SSIS-001"},
		{"名称没番号但路径有", Item{"Name": "没有番号", "Path": `F:\媒体\SSIS-001\SSIS-001.strm`}, "SSIS-001"},
		{"原始标题里有", Item{"Name": "标题", "OriginalTitle": "ABP-123 原名"}, "ABP-123"},
		{"名称优先于路径", Item{"Name": "ABP-123 标题", "Path": `F:\媒体\SSIS-001\x.strm`}, "ABP-123"},
		{"完全没有番号", Item{"Name": "随便一个名字"}, ""},
		{"空条目", Item{}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := itemNumber(c.item); got != c.want {
				t.Errorf("itemNumber() = %q，期望 %q", got, c.want)
			}
		})
	}
}

// searchKeyword 要在推断不出番号时退回名字（不能被重构搞丢）。
func TestSearchKeywordFallsBackToName(t *testing.T) {
	if got := searchKeyword(Item{"Name": "SSIS-001 标题"}); got != "SSIS-001" {
		t.Errorf("有番号时应用番号，实际 %q", got)
	}
	if got := searchKeyword(Item{"Name": "没有番号的名字"}); got != "没有番号的名字" {
		t.Errorf("没番号时应用名字，实际 %q", got)
	}
}

// mock 必须复现「列表接口不返回 SortName」这个真实行为，否则
// 「前端拿 SortName 当番号」这类 bug 在单测里永远发现不了。
func TestMockEmbyListOmitsSortName(t *testing.T) {
	m := newMockEmby(t)
	m.items["m1"] = map[string]any{
		"Id": "m1", "Name": "SSIS-001 标题", "SortName": "SSIS-001 BIAOTI",
	}
	e := NewEmby(Config{EmbyURL: m.srv.URL, Token: "tok-123", UserID: "u1", DeviceID: "d1"})

	res, err := e.Items(context.Background(), ItemQuery{Limit: 10})
	if err != nil {
		t.Fatalf("列表查询失败: %v", err)
	}
	if len(res.Items) != 1 {
		t.Fatalf("应返回 1 条，实际 %d", len(res.Items))
	}
	if _, ok := res.Items[0]["SortName"]; ok {
		t.Error("真实服务器的列表接口不返回 SortName，mock 不该返回")
	}

	// 详情接口要返回（真实行为）
	it, err := e.ItemDetail(context.Background(), "m1")
	if err != nil {
		t.Fatalf("详情读取失败: %v", err)
	}
	if it["SortName"] != "SSIS-001 BIAOTI" {
		t.Errorf("详情接口应返回 SortName，实际 %#v", it["SortName"])
	}
}

func TestHandleItemsIncludesNumber(t *testing.T) {
	m := newMockEmby(t)
	mt := newMockMetaTube(t)
	app := testApp(t, m.srv.URL, mt.URL)
	m.items["m1"] = map[string]any{"Id": "m1", "Name": "SSIS-001 某个片名", "Type": "Movie"}

	srv := httptest.NewServer(app.route())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/items?limit=10")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Data struct {
			Items []map[string]any `json:"items"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(out.Data.Items) != 1 {
		t.Fatalf("应返回 1 条，实际 %d", len(out.Data.Items))
	}
	if got := out.Data.Items[0]["Number"]; got != "SSIS-001" {
		t.Errorf("列表接口应带上算好的番号，实际 %#v", got)
	}
}

func TestHandleItemDetailIncludesNumber(t *testing.T) {
	m := newMockEmby(t)
	mt := newMockMetaTube(t)
	app := testApp(t, m.srv.URL, mt.URL)
	m.items["m1"] = map[string]any{
		"Id": "m1", "Name": "SSIS-001 某个片名", "Type": "Movie",
		// Emby 在真实服务器上会把 SortName 算成拼音，不该拿它当番号
		"SortName": "SSIS-001 MOUGEPIANMING",
	}

	srv := httptest.NewServer(app.route())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/items/detail?id=m1")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Data map[string]any `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got := out.Data["Number"]; got != "SSIS-001" {
		t.Errorf("详情接口应带上算好的番号，实际 %#v", got)
	}
}

// 端到端：代理必须带上目标自己的 origin 当 Referer，否则 javbus 403。
func TestHandleImageProxyEndToEnd(t *testing.T) {
	var gotReferer string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReferer = r.Header.Get("Referer")
		if r.Header.Get("Referer") == "" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0x01, 0x02})
	}))
	defer upstream.Close()

	app := testApp(t, "http://127.0.0.1:1", "http://127.0.0.1:1")
	if err := app.store.Update(func(c *Config) { c.JavBusURL = upstream.URL }); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app.route())
	defer srv.Close()

	cover := upstream.URL + "/pics/cover/a.jpg"
	resp, err := http.Get(srv.URL + "/api/img?u=" + url.QueryEscape(cover))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("代理应返回 200，实际 %d：%s", resp.StatusCode, body)
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "image/") {
		t.Errorf("Content-Type 异常: %s", resp.Header.Get("Content-Type"))
	}
	if gotReferer != upstream.URL+"/" {
		t.Errorf("Referer 应是目标 origin，实际 %q", gotReferer)
	}
	if resp.Header.Get("Cache-Control") == "" {
		t.Error("应当带 Cache-Control，否则一屏几十张图会反复打上游")
	}

	// 非白名单的公网图床 -> 302 回原地址，让浏览器直连
	// （必须禁掉自动跟随，否则 http.Get 会真的去访问外网）
	other := "https://image.mgstage.com/a.jpg"
	redir, err := noRedirectClient.Get(srv.URL + "/api/img?u=" + url.QueryEscape(other))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	redir.Body.Close()
	if redir.StatusCode != http.StatusFound {
		t.Errorf("非白名单公网主机应 302，实际 %d", redir.StatusCode)
	}
	if loc := redir.Header.Get("Location"); loc != other {
		t.Errorf("302 目标应为原地址，实际 %q", loc)
	}

	// 内网地址 -> 502（拒绝，不能当跳板）
	priv, err := http.Get(srv.URL + "/api/img?u=" + url.QueryEscape("http://192.168.1.1/admin.jpg"))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	priv.Body.Close()
	if priv.StatusCode != http.StatusBadGateway {
		t.Errorf("内网地址应 502，实际 %d", priv.StatusCode)
	}

	// 缺参数 -> 400
	none, err := http.Get(srv.URL + "/api/img")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	none.Body.Close()
	if none.StatusCode != http.StatusBadRequest {
		t.Errorf("缺参数应 400，实际 %d", none.StatusCode)
	}
}

// 上游返回 HTML（被墙/验证码页）时不能当图片透传。
func TestImageProxyRejectsNonImageUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html>captcha</html>"))
	}))
	defer upstream.Close()

	p := NewImageProxy(newHTTPClient(Config{}))
	cfg := Config{JavBusURL: upstream.URL}
	if _, _, err := p.Fetch(context.Background(), upstream.URL+"/a.jpg", cfg); err == nil {
		t.Fatal("上游不是图片时应当报错")
	}
}

// 缓存：同一张图第二次不应再打上游。
func TestImageProxyCaches(t *testing.T) {
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0x01})
	}))
	defer upstream.Close()

	p := NewImageProxy(newHTTPClient(Config{}))
	cfg := Config{JavBusURL: upstream.URL}
	for i := 0; i < 3; i++ {
		if _, _, err := p.Fetch(context.Background(), upstream.URL+"/a.jpg", cfg); err != nil {
			t.Fatalf("第 %d 次取图失败: %v", i+1, err)
		}
	}
	if hits != 1 {
		t.Errorf("同一地址应命中缓存，上游被打了 %d 次", hits)
	}
}

// ---------- 演员按媒体库过滤 ----------

// mock 的 /Persons 必须照抄真实 Emby 的 ParentId 行为。
// 实测（4.9.0.42，10592 个演员）：不传 ParentId 是全局，传了就只算该库出现过的演员。
func TestEmbyPersonsScopesToParent(t *testing.T) {
	m := newMockEmby(t)
	m.persons = []Person{{Id: "p1", Name: "甲"}, {Id: "p2", Name: "乙"}, {Id: "p3", Name: "丙"}}
	m.personParent = map[string]string{"p1": "502847", "p2": "502847", "p3": "502849"}

	e := NewEmby(Config{EmbyURL: m.srv.URL, Token: "tok-123", UserID: "u1"})
	ctx := context.Background()

	all, err := e.Persons(ctx, 0, 50, "", "")
	if err != nil {
		t.Fatalf("全局查询失败: %v", err)
	}
	if all.TotalRecordCount != 3 {
		t.Errorf("不传 ParentId 应返回全部 3 人，实际 %d", all.TotalRecordCount)
	}

	got, err := e.Persons(ctx, 0, 50, "", "502847")
	if err != nil {
		t.Fatalf("按库查询失败: %v", err)
	}
	if got.TotalRecordCount != 2 || len(got.Items) != 2 {
		t.Errorf("502847 应有 2 人，实际 total=%d items=%d", got.TotalRecordCount, len(got.Items))
	}
	if m.lastParentID != "502847" {
		t.Errorf("ParentId 应发给服务端，服务端实际收到 %q", m.lastParentID)
	}

	empty, err := e.Persons(ctx, 0, 50, "", "502850")
	if err != nil {
		t.Fatalf("查询空库失败: %v", err)
	}
	if empty.TotalRecordCount != 0 || len(empty.Items) != 0 {
		t.Errorf("502850 里没有演员，实际 total=%d items=%d", empty.TotalRecordCount, len(empty.Items))
	}
}

// 前端下拉选的媒体库必须一路传到 Emby —— 中间任何一层漏掉，
// 用户看到的就是「选了库但列表没变」。
func TestHandlePersonsPassesParentID(t *testing.T) {
	m := newMockEmby(t)
	mt := newMockMetaTube(t)
	app := testApp(t, m.srv.URL, mt.URL)
	m.persons = []Person{
		{Id: "p1", Name: "甲"},
		{Id: "p2", Name: "乙", ImageTags: map[string]string{"Primary": "t"}},
		{Id: "p3", Name: "丙"},
	}
	m.personParent = map[string]string{"p1": "502847", "p2": "502847", "p3": "502849"}

	srv := httptest.NewServer(app.route())
	defer srv.Close()

	get := func(query string) (int, []string) {
		t.Helper()
		resp, err := http.Get(srv.URL + "/api/persons?limit=10" + query)
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		defer resp.Body.Close()
		var out struct {
			Data struct {
				Items []struct {
					Name string `json:"Name"`
				} `json:"items"`
				Total int `json:"total"`
			} `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		names := make([]string, 0, len(out.Data.Items))
		for _, it := range out.Data.Items {
			names = append(names, it.Name)
		}
		return out.Data.Total, names
	}

	total, names := get("")
	if total != 3 || len(names) != 3 {
		t.Errorf("不选媒体库应返回全部 3 人，实际 total=%d names=%v", total, names)
	}

	total, names = get("&parent_id=502847")
	if total != 2 || len(names) != 2 {
		t.Errorf("502847 应有 2 人，实际 total=%d names=%v", total, names)
	}
	if m.lastParentID != "502847" {
		t.Errorf("parent_id 应透传到 Emby，服务端实际收到 %q", m.lastParentID)
	}

	// 「只看无头像」叠在库过滤之上：502847 里乙有头像，应只剩甲。
	total, names = get("&parent_id=502847&missing_image=true")
	if total != 2 {
		t.Errorf("total 仍是库内演员数 2，实际 %d", total)
	}
	if len(names) != 1 || names[0] != "甲" {
		t.Errorf("502847 里只有甲缺头像，实际 names=%v", names)
	}
}

// 批量刮削也要受媒体库限制，否则用户选了「国产传媒」却把全库演员都刮了。
func TestHandlePersonAvatarBatchPassesParentID(t *testing.T) {
	m := newMockEmby(t)
	mt := newMockMetaTube(t)
	app := testApp(t, m.srv.URL, mt.URL)
	m.persons = []Person{{Id: "p1", Name: "甲"}, {Id: "p2", Name: "乙"}}
	m.personParent = map[string]string{"p1": "502847", "p2": "502849"}

	srv := httptest.NewServer(app.route())
	defer srv.Close()

	body := `{"mode":"missing","limit":10,"source":"gfriends","parent_id":"502847"}`
	resp, err := http.Post(srv.URL+"/api/persons/avatars", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("应返回 200，实际 %d", resp.StatusCode)
	}
	if m.lastParentID != "502847" {
		t.Errorf("批量刮削也要把 parent_id 传给 Emby，服务端实际收到 %q", m.lastParentID)
	}
}

// 真实 Emby 对格式非法的 ParentId 是 500，不是返回空列表。
// 这条用来钉住 mock 的复现能力 —— 否则「客户端传了坏 GUID」这类问题在单测里永远看不见。
func TestMockPersonsRejectsMalformedParent(t *testing.T) {
	m := newMockEmby(t)
	m.persons = []Person{{Id: "p1", Name: "甲"}}
	m.personParent = map[string]string{"p1": "502847"}

	e := NewEmby(Config{EmbyURL: m.srv.URL, Token: "tok-123", UserID: "u1"})
	ctx := context.Background()

	if _, err := e.Persons(ctx, 0, 50, "", "502847"); err != nil {
		t.Fatalf("真实短写形式的库 Id 不应报错: %v", err)
	}
	if _, err := e.Persons(ctx, 0, 50, "", strings.Repeat("0", 32)); err != nil {
		t.Fatalf("全零 GUID 格式合法（只是查不到），不应报错: %v", err)
	}
	_, err := e.Persons(ctx, 0, 50, "", "__no_such_library__")
	if err == nil {
		t.Fatal("格式非法的 ParentId 应当报错（线上是 500 Unrecognized Guid format.）")
	}
	if !strings.Contains(err.Error(), "Unrecognized Guid format") {
		t.Errorf("报错应带上服务端原因，实际: %v", err)
	}
}
