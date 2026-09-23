package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------- Mock Emby ----------

type mockEmby struct {
	mu       sync.Mutex
	items    map[string]map[string]any
	uploaded []string // "itemID/type/index/contentType/raw|b64"
	// bodies 记录每次上传的**解码后净荷**（b64 形式会先解码），
	// 与 uploaded 一一对应 —— 用来断言「上传的到底是哪一张图」。
	bodies  [][]byte
	deleted []string // "itemID/type"
	patched map[string]map[string]any
	persons []Person
	// personParent 模拟「演员 → 所属媒体库」的关系。真实 Emby 的 /Persons
	// 支持 ParentId 过滤（实测：全局 10592 人 → 按库过滤后 464 / 2664 / 594 人），
	// mock 必须照抄这个行为，否则「按库查看演员」的功能在单测里是假的。
	personParent map[string]string
	lastParentID string // 记录最近一次 /Persons 收到的 ParentId，供断言客户端确实传了
	refresh      int
	srv          *httptest.Server
}

// mockServerManaged 是 POST /Items/{id} 不会改动的服务端托管字段。
var mockServerManaged = []string{
	"ImageTags", "Path", "Type", "Etag", "ServerId", "DateCreated",
	"MediaSources", "MediaStreams", "Chapters",
}

// isGUIDish 判断字符串是否是 Emby 能解析的 GUID 表示。
//
// Emby 接受「去横线、可短写」的形式 —— 真实媒体库 Id 就是 `502847` 这种 6 位十六进制。
// 所以规则是「非空时全部由十六进制字符或横线组成」。空串表示不按库过滤，也算合法。
func isGUIDish(s string) bool {
	if s == "" {
		return true
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		case c == '-':
		default:
			return false
		}
	}
	return true
}

func newMockEmby(t *testing.T) *mockEmby {
	m := &mockEmby{
		items:        map[string]map[string]any{},
		patched:      map[string]map[string]any{},
		personParent: map[string]string{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /Users/AuthenticateByName", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["Pw"] == "bad" {
			http.Error(w, `{"error":"invalid"}`, http.StatusUnauthorized)
			return
		}
		writeJSON(w, 200, map[string]any{
			"AccessToken": "tok-123", "ServerId": "srv1",
			"User": map[string]any{"Id": "u1", "Name": in["Username"]},
		})
	})
	mux.HandleFunc("GET /Users/Me", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Emby-Token") == "" {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		writeJSON(w, 200, map[string]any{"Id": "u1", "Name": "admin"})
	})
	mux.HandleFunc("GET /System/Info/Public", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ServerName": "MockEmby", "Version": "4.8.0.0"})
	})
	mux.HandleFunc("GET /Users/{uid}/Views", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"Items": []map[string]any{
			{"Id": "lib1", "Name": "电影", "CollectionType": "movies"},
		}})
	})
	mux.HandleFunc("GET /Items/Counts", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"MovieCount": 3, "SeriesCount": 1, "EpisodeCount": 12})
	})
	mux.HandleFunc("GET /Items", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		list := []map[string]any{}
		for _, it := range m.items {
			// 真实服务器上 /Items 是**投影**：SortName 不在列表里（实测全是 null），
			// 只有详情接口才返回。mock 必须照抄，否则「前端拿 SortName 当番号」
			// 这种 bug 在单测里永远发现不了。
			cp := map[string]any{}
			for k, v := range it {
				if k == "SortName" {
					continue
				}
				cp[k] = v
			}
			list = append(list, cp)
		}
		writeJSON(w, 200, map[string]any{"Items": list, "TotalRecordCount": len(list)})
	})
	// ---- 读：真实 4.9 构建只注册了用户作用域的详情路由 ----
	//
	// 实测（4.9.0.42）：GET /Items/{id} 会落到静态文件处理器，返回
	// 404「找不到文件 "/Items/xxx"」；GET /Users/{uid}/Items/{id} 才 200。
	mux.HandleFunc("GET /Users/{uid}/Items/{id}", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		it, ok := m.items[r.PathValue("id")]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeJSON(w, 200, it)
	})
	mux.HandleFunc("GET /Items/{id}", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `找不到文件 "`+r.URL.Path+`"`, http.StatusNotFound)
	})
	// ---- 写：反过来，只有全局路径存在 ----
	mux.HandleFunc("POST /Items/{id}", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body == nil {
			body = map[string]any{}
		}
		id := r.PathValue("id")
		m.mu.Lock()
		next := map[string]any{}
		// 服务端托管字段不受 POST 影响（真实 Emby 同样如此）
		if prev, ok := m.items[id]; ok {
			for _, k := range mockServerManaged {
				if v, ok := prev[k]; ok {
					next[k] = v
				}
			}
		}
		for k, v := range body {
			next[k] = v
		}
		next["Id"] = id
		// 真实行为是整对象替换：body 里没带的元数据字段会被清空。
		m.items[id] = next
		m.patched[id] = body
		m.mu.Unlock()
		writeJSON(w, 200, map[string]any{})
	})
	mux.HandleFunc("POST /Users/{uid}/Items/{id}", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `找不到文件 "`+r.URL.Path+`"`, http.StatusNotFound)
	})
	mux.HandleFunc("POST /Items/{id}/Refresh", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.refresh++
		m.mu.Unlock()
		writeJSON(w, 200, map[string]any{})
	})
	mux.HandleFunc("DELETE /Items/{id}/Images/{type}", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.deleted = append(m.deleted, r.PathValue("id")+"/"+r.PathValue("type"))
		m.mu.Unlock()
		writeJSON(w, 200, map[string]any{})
	})
	mux.HandleFunc("POST /Items/{id}/Images/{type}", func(w http.ResponseWriter, r *http.Request) {
		m.recordUpload(r.PathValue("id"), r.PathValue("type"), "-1", w, r)
	})
	mux.HandleFunc("POST /Items/{id}/Images/{type}/{idx}", func(w http.ResponseWriter, r *http.Request) {
		m.recordUpload(r.PathValue("id"), r.PathValue("type"), r.PathValue("idx"), w, r)
	})
	mux.HandleFunc("GET /Persons", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		parent := r.URL.Query().Get("ParentId")
		m.lastParentID = parent
		// 照抄线上怪癖：ParentId 不是 GUID 时 Emby 直接 500，而不是返回空列表。
		// 实测 `__no_such_library__` → 500 "Unrecognized Guid format."，
		// 而 `00000000000000000000000000000000` → 200 且 0 条。
		if !isGUIDish(parent) {
			http.Error(w, "Unrecognized Guid format.", http.StatusInternalServerError)
			return
		}
		items := m.persons
		if parent != "" {
			filtered := make([]Person, 0, len(items))
			for _, p := range items {
				if m.personParent[p.Id] == parent {
					filtered = append(filtered, p)
				}
			}
			items = filtered
		}
		if items == nil {
			items = []Person{}
		}
		writeJSON(w, 200, map[string]any{"Items": items, "TotalRecordCount": len(items)})
	})
	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

// recordUpload 模拟真实 4.9 构建的图片上传：body 必须是 **base64 文本**。
//
// 实测这个构建发原始字节会 500：
//
//	The input is not a valid Base-64 string as it contains a non-base 64 character...
//
// 标准 Emby 则要原始字节。mock 站在「严格构建」这一侧，
// 这样客户端「先原始、失败再 base64」的回退顺序才真正被测到。
func (m *mockEmby) recordUpload(id, typ, idx string, w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	ct := r.Header.Get("Content-Type")
	form := "raw"
	if isBase64Text(body) {
		form = "b64"
	}
	m.mu.Lock()
	m.uploaded = append(m.uploaded, id+"/"+typ+"/"+idx+"/"+ct+"/"+form)
	if form == "b64" {
		if dec, err := base64.StdEncoding.DecodeString(string(body)); err == nil {
			m.bodies = append(m.bodies, dec)
		} else {
			m.bodies = append(m.bodies, body)
		}
	} else {
		m.bodies = append(m.bodies, body)
	}
	if form == "b64" {
		if it, ok := m.items[id]; ok {
			tags, _ := it["ImageTags"].(map[string]any)
			if tags == nil {
				tags = map[string]any{}
			}
			tags[typ] = "tag-" + typ
			it["ImageTags"] = tags
		}
	}
	m.mu.Unlock()

	if len(body) == 0 {
		http.Error(w, "empty body", http.StatusBadRequest)
		return
	}
	if form == "raw" {
		http.Error(w, "The input is not a valid Base-64 string as it contains a non-base 64 character, more than two padding characters, or an illegal character among the padding characters.",
			http.StatusInternalServerError)
		return
	}
	if !strings.HasPrefix(strings.ToLower(ct), "image/") {
		http.Error(w, "Unable to determine image file extension from mime type "+ct, http.StatusBadRequest)
		return
	}
	writeJSON(w, 200, map[string]any{})
}

// isBase64Text 判断 body 是否是合法的 base64 文本（允许尾部换行）。
func isBase64Text(b []byte) bool {
	s := strings.TrimRight(string(b), "\r\n")
	if s == "" {
		return false
	}
	_, err := base64.StdEncoding.DecodeString(s)
	return err == nil
}

// ---------- Mock MetaTube ----------

func newMockMetaTube(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/providers", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"data": []string{"FANZA", "DUMMY"}})
	})
	mux.HandleFunc("GET /v1/movies/search", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		writeJSON(w, 200, map[string]any{"data": []map[string]any{
			{"id": "wrong", "number": "SSIS-999", "title": "不相关", "provider": "DUMMY", "score": 9.9},
			{"id": "m-001", "number": "SSIS-001", "title": "SSIS-001 原标题", "title_zh": "中文标题", "provider": "FANZA", "score": 8.0},
		}})
		_ = q
	})
	mux.HandleFunc("GET /v1/movies/{provider}/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		writeJSON(w, 200, map[string]any{"data": map[string]any{
			"id": "m-001", "provider": "FANZA", "number": "SSIS-001",
			"title": "SSIS-001 原标题", "title_zh": "中文标题",
			"actors": []string{"三上悠亜"}, "genres": []string{"剧情"},
			"studio": "S1", "label": "S1", "series": "SSIS",
			"release_date": "2021-01-05", "runtime": 120, "score": 4.5,
			"plot": "剧情简介", "director": "监督A",
			"cover_url":      "http://example.invalid/c.jpg",
			"preview_images": []string{"http://example.invalid/p1.jpg", "http://example.invalid/p2.jpg"},
		}})
	})
	mux.HandleFunc("GET /v1/images/{kind}/{provider}/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("\xff\xd8\xff\xe0fake-jpeg-data"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// ---------- 测试辅助 ----------

func testApp(t *testing.T, embyURL, mtURL string) *App {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("EMBYME_HOME", dir)
	store, err := NewStore(dir + "/config.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(c *Config) {
		c.EmbyURL = embyURL
		c.Token = "tok-123"
		c.UserID = "u1"
		c.MetaTubeURL = mtURL
		c.MetaTubeToken = "secret"
		c.JavBusInterval = 10
	}); err != nil {
		t.Fatal(err)
	}
	return NewApp(store, nil)
}

// ---------- 用例 ----------

func TestEmbyLogin(t *testing.T) {
	m := newMockEmby(t)
	e := NewEmby(Config{EmbyURL: m.srv.URL, DeviceID: "dev1"})
	lr, err := e.AuthenticateByName(context.Background(), "admin", "pw")
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	if lr.Token != "tok-123" || lr.UserID != "u1" || lr.UserName != "admin" {
		t.Errorf("登录返回异常: %+v", lr)
	}

	e2 := NewEmby(Config{EmbyURL: m.srv.URL, DeviceID: "dev1"})
	if _, err := e2.AuthenticateByName(context.Background(), "admin", "bad"); err == nil {
		t.Error("错误密码应当返回错误")
	}
}

func TestEmbyAPIKeyLogin(t *testing.T) {
	m := newMockEmby(t)
	e := NewEmby(Config{EmbyURL: m.srv.URL, Token: "apikey-xyz", DeviceID: "dev1"})
	me, err := e.Me(context.Background())
	if err != nil {
		t.Fatalf("API Key 登录失败: %v", err)
	}
	if me.UserID != "u1" {
		t.Errorf("用户 ID 异常: %q", me.UserID)
	}
}

func TestEmbyUpdateItemMergesProviderIds(t *testing.T) {
	m := newMockEmby(t)
	m.items["m1"] = map[string]any{
		"Id": "m1", "Name": "旧名称",
		"ProviderIds": map[string]any{"Tmdb": "12345", "MetaTube": ""},
	}
	e := NewEmby(Config{EmbyURL: m.srv.URL, Token: "tok-123", DeviceID: "dev1"})
	err := e.UpdateItem(context.Background(), "m1", map[string]any{
		"Name":        "新名称",
		"ProviderIds": map[string]any{"MetaTube": "FANZA:m-001"},
	})
	if err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	body := m.patched["m1"]
	pids, _ := body["ProviderIds"].(map[string]any)
	if pids["Tmdb"] != "12345" {
		t.Errorf("原有 ProviderIds 被覆盖: %v", pids)
	}
	if pids["MetaTube"] != "FANZA:m-001" {
		t.Errorf("新 ProviderIds 未写入: %v", pids)
	}
	if body["Name"] != "新名称" {
		t.Errorf("名称未更新: %v", body["Name"])
	}
}

func TestScrapeMovieEndToEnd(t *testing.T) {
	m := newMockEmby(t)
	mt := newMockMetaTube(t)
	app := testApp(t, m.srv.URL, mt.URL)

	m.items["m1"] = map[string]any{
		"Id": "m1", "Name": "SSIS-001 某个片名", "Type": "Movie",
		"Path":        "F:\\媒体\\SSIS-001\\SSIS-001.strm",
		"ProviderIds": map[string]any{}, "ImageTags": map[string]any{},
	}

	res, err := app.ScrapeMovie(context.Background(), "m1", ScrapeOptions{Refresh: true})
	if err != nil {
		t.Fatalf("刮削失败: %v", err)
	}
	if res.Provider != "FANZA" || res.MovieID != "m-001" {
		t.Errorf("应选中番号匹配的结果，实际 %s:%s", res.Provider, res.MovieID)
	}
	if res.Number != "SSIS-001" {
		t.Errorf("番号异常: %q", res.Number)
	}
	body := m.patched["m1"]
	// 源标题「中文标题」不带番号，写入时应主动补上番号前缀
	if body["Name"] != "SSIS-001 中文标题" {
		t.Errorf("标题未写入: %v", body["Name"])
	}
	// JSON 往返后数字统一为 float64
	if y, ok := body["ProductionYear"].(float64); !ok || int(y) != 2021 {
		t.Errorf("年份未写入: %v", body["ProductionYear"])
	}
	if tk, ok := body["RunTimeTicks"].(float64); !ok || int64(tk) != int64(120)*60*10_000_000 {
		t.Errorf("时长未写入: %v", body["RunTimeTicks"])
	}
	pids, _ := body["ProviderIds"].(map[string]any)
	if pids["MetaTube"] != "FANZA:m-001" {
		t.Errorf("ProviderIds 未写入: %v", pids)
	}

	// 海报 + 缩略图 + 2 张剧照
	joined := strings.Join(m.uploaded, " | ")
	if !strings.Contains(joined, "m1/Primary/-1/image/jpeg") {
		t.Errorf("未上传海报: %s", joined)
	}
	if !strings.Contains(joined, "m1/Thumb/-1/image/jpeg") {
		t.Errorf("未上传缩略图: %s", joined)
	}
	if !strings.Contains(joined, "m1/Backdrop/0") || !strings.Contains(joined, "m1/Backdrop/1") {
		t.Errorf("未上传剧照: %s", joined)
	}
	if m.refresh != 1 {
		t.Errorf("应触发 1 次刷新，实际 %d", m.refresh)
	}
}

func TestScrapeMovieSkipsExistingImages(t *testing.T) {
	m := newMockEmby(t)
	mt := newMockMetaTube(t)
	app := testApp(t, m.srv.URL, mt.URL)
	m.items["m1"] = map[string]any{
		"Id": "m1", "Name": "SSIS-001", "Type": "Movie",
		"ImageTags": map[string]any{"Primary": "t", "Thumb": "h", "Backdrop": "b"},
	}
	if _, err := app.ScrapeMovie(context.Background(), "m1", ScrapeOptions{}); err != nil {
		t.Fatalf("刮削失败: %v", err)
	}
	if len(m.uploaded) != 0 {
		t.Errorf("已有图片且未勾选覆盖时不应上传，实际 %v", m.uploaded)
	}
}

func TestScrapeMovieOverwrite(t *testing.T) {
	m := newMockEmby(t)
	mt := newMockMetaTube(t)
	app := testApp(t, m.srv.URL, mt.URL)
	m.items["m1"] = map[string]any{
		"Id": "m1", "Name": "SSIS-001", "Type": "Movie",
		"ImageTags": map[string]any{"Primary": "t", "Backdrop": "b"},
	}
	if _, err := app.ScrapeMovie(context.Background(), "m1", ScrapeOptions{OverwriteImages: true}); err != nil {
		t.Fatalf("刮削失败: %v", err)
	}
	if len(m.uploaded) == 0 {
		t.Error("勾选覆盖后应当重新上传图片")
	}
}

func TestScrapeMovieNoNumber(t *testing.T) {
	m := newMockEmby(t)
	mt := newMockMetaTube(t)
	app := testApp(t, m.srv.URL, mt.URL)
	m.items["m1"] = map[string]any{"Id": "m1", "Name": "随便一个名字", "Type": "Movie"}
	_, err := app.ScrapeMovie(context.Background(), "m1", ScrapeOptions{})
	if err != nil {
		t.Fatalf("无番号时应回退用名称搜索，实际报错: %v", err)
	}
}

func TestScrapeMovieManualProvider(t *testing.T) {
	m := newMockEmby(t)
	mt := newMockMetaTube(t)
	app := testApp(t, m.srv.URL, mt.URL)
	m.items["m1"] = map[string]any{"Id": "m1", "Name": "没有番号", "Type": "Movie"}
	res, err := app.ScrapeMovie(context.Background(), "m1", ScrapeOptions{Provider: "FANZA", MovieID: "m-001"})
	if err != nil {
		t.Fatalf("手动指定 provider 失败: %v", err)
	}
	if res.MovieID != "m-001" {
		t.Errorf("未使用手动指定的 id: %q", res.MovieID)
	}
}

func TestScrapePersonAvatarFromGfriends(t *testing.T) {
	m := newMockEmby(t)
	mt := newMockMetaTube(t)
	app := testApp(t, m.srv.URL, mt.URL)

	// 准备一个假的 gfriends 索引与图片源
	imgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("\xff\xd8\xff\xe0avatar"))
	}))
	defer imgSrv.Close()
	app.gf.byName = map[string][]GfriendEntry{
		normName("三上悠亜"): {{Group: "y-AVDC", File: "三上悠亜.jpg"}},
	}
	app.gf.loadedAt = time.Now() // 模拟索引已就绪
	_ = app.store.Update(func(c *Config) { c.GfriendsCDN = imgSrv.URL })

	m.persons = []Person{{Id: "p1", Name: "三上悠亜"}}
	m.items["p1"] = map[string]any{"Id": "p1", "Name": "三上悠亜", "ImageTags": map[string]any{}}

	res, err := app.ScrapePersonAvatar(context.Background(), "", "三上悠亜", AvatarOptions{Source: "gfriends"})
	if err != nil {
		t.Fatalf("头像刮削失败: %v", err)
	}
	if res.PersonID != "p1" {
		t.Errorf("未按名字找到演员: %+v", res)
	}
	if res.Detail != "y-AVDC/三上悠亜.jpg" {
		t.Errorf("来源记录异常: %q", res.Detail)
	}
	if !strings.Contains(strings.Join(m.uploaded, " "), "p1/Primary/-1") {
		t.Errorf("未上传头像: %v", m.uploaded)
	}
}

func TestScrapePersonAvatarSkipExisting(t *testing.T) {
	m := newMockEmby(t)
	mt := newMockMetaTube(t)
	app := testApp(t, m.srv.URL, mt.URL)
	m.persons = []Person{{Id: "p1", Name: "三上悠亜", ImageTags: map[string]string{"Primary": "t"}}}
	m.items["p1"] = map[string]any{"Id": "p1", "Name": "三上悠亜", "ImageTags": map[string]any{"Primary": "t"}}
	res, err := app.ScrapePersonAvatar(context.Background(), "", "三上悠亜", AvatarOptions{Source: "gfriends"})
	if err != nil {
		t.Fatalf("应当跳过而非报错: %v", err)
	}
	if !res.Skipped {
		t.Error("已有头像时应标记为跳过")
	}
	if len(m.uploaded) != 0 {
		t.Errorf("不应上传: %v", m.uploaded)
	}
}

func TestScrapePersonAvatarNotFound(t *testing.T) {
	m := newMockEmby(t)
	mt := newMockMetaTube(t)
	app := testApp(t, m.srv.URL, mt.URL)
	app.gf.byName = map[string][]GfriendEntry{}
	// 模拟刚失败过，命中负缓存，不应再去联网
	app.gf.lastAttempt = time.Now()
	app.gf.lastErr = "模拟失败"
	m.persons = []Person{{Id: "p1", Name: "查无此人"}}
	m.items["p1"] = map[string]any{"Id": "p1", "Name": "查无此人", "ImageTags": map[string]any{}}
	if _, err := app.ScrapePersonAvatar(context.Background(), "p1", "查无此人", AvatarOptions{Source: "gfriends"}); err == nil {
		t.Error("找不到头像时应当返回错误")
	}
}

func TestBuildLocalIndex(t *testing.T) {
	m := newMockEmby(t)
	mt := newMockMetaTube(t)
	app := testApp(t, m.srv.URL, mt.URL)
	m.items["m1"] = map[string]any{
		"Id": "m1", "Name": "SSIS-001 标题", "Type": "Movie",
		"Path": "F:\\媒体\\SSIS-001\\SSIS-001.strm", "ImageTags": map[string]any{"Primary": "t"},
	}
	m.items["m2"] = map[string]any{
		"Id": "m2", "Name": "ABP-123", "Type": "Movie",
		"Path": "F:\\媒体\\ABP-123\\ABP-123.strm", "ImageTags": map[string]any{},
	}
	idx, err := app.BuildLocalIndex(context.Background(), "")
	if err != nil {
		t.Fatalf("建索引失败: %v", err)
	}
	if idx.Scanned != 2 {
		t.Fatalf("应扫描到 2 个条目，实际 %d", idx.Scanned)
	}
	for _, key := range []string{"SSIS-1", "SSIS1", "ABP-123", "ABP123"} {
		if _, ok := idx.ByKey[key]; !ok {
			t.Errorf("索引缺少键 %q（现有键：%v）", key, keysOf(idx.ByKey))
		}
	}
	if ref := idx.ByKey["SSIS-1"]; ref.Number != "SSIS-001" || !ref.HasPrimary {
		t.Errorf("条目信息异常: %+v", ref)
	}
	if ref := idx.ByKey["ABP123"]; ref.Number != "ABP-123" || ref.HasPrimary {
		t.Errorf("条目信息异常: %+v", ref)
	}
}

func keysOf(m map[string]LocalItemRef) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestMetaTubeProvidersAndAuth(t *testing.T) {
	mt := newMockMetaTube(t)
	c := NewMetaTube(Config{MetaTubeURL: mt.URL, MetaTubeToken: "secret"})
	list, err := c.Providers(context.Background())
	if err != nil {
		t.Fatalf("providers 失败: %v", err)
	}
	if len(list) != 2 || list[0] != "FANZA" {
		t.Errorf("providers 异常: %v", list)
	}
	mv, err := c.Movie(context.Background(), "FANZA", "m-001")
	if err != nil {
		t.Fatalf("详情失败: %v", err)
	}
	if mv.TitleZh != "中文标题" || len(mv.Actors) != 1 {
		t.Errorf("详情解析异常: %+v", mv)
	}
	if mv.ID != "m-001" || mv.Provider != "FANZA" {
		t.Errorf("id/provider 未回填: %+v", mv)
	}

	// 错误的 token 应报错
	bad := NewMetaTube(Config{MetaTubeURL: mt.URL, MetaTubeToken: "wrong"})
	if _, err := bad.Movie(context.Background(), "FANZA", "m-001"); err == nil {
		t.Error("token 错误时应返回错误")
	}
}

func TestEmbyStatsAndPersons(t *testing.T) {
	m := newMockEmby(t)
	mt := newMockMetaTube(t)
	app := testApp(t, m.srv.URL, mt.URL)
	m.persons = []Person{
		{Id: "p1", Name: "A", ImageTags: map[string]string{"Primary": "t"}},
		{Id: "p2", Name: "B", ImageTags: map[string]string{}},
		{Id: "p3", Name: "C"},
	}
	e := NewEmby(app.store.Get())
	counts, err := e.ItemCounts(context.Background())
	if err != nil {
		t.Fatalf("counts 失败: %v", err)
	}
	if counts.MovieCount != 3 {
		t.Errorf("电影数异常: %d", counts.MovieCount)
	}
	stats := app.personStats(context.Background(), e, 1000)
	if stats["total"] != 3 || stats["with_image"] != 1 || stats["missing_image"] != 2 {
		t.Errorf("演员统计异常: %v", stats)
	}
	libs, err := e.LibraryStats(context.Background())
	if err != nil {
		t.Fatalf("媒体库统计失败: %v", err)
	}
	if len(libs) != 1 || libs[0].Name != "电影" {
		t.Errorf("媒体库统计异常: %+v", libs)
	}
}
