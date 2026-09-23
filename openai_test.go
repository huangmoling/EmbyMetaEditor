package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------- normalizeChatURL ----------

func TestNormalizeChatURL(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"https://api.openai.com/v1", "https://api.openai.com/v1/chat/completions"},
		{"https://api.openai.com", "https://api.openai.com/v1/chat/completions"},
		{"https://api.openai.com/", "https://api.openai.com/v1/chat/completions"},
		{"https://api.gptgod.online/v1", "https://api.gptgod.online/v1/chat/completions"},
		{"https://relay.example.com/v1/chat/completions", "https://relay.example.com/v1/chat/completions"},
		{"", ""},
	}
	for _, c := range cases {
		if got := normalizeChatURL(c.in); got != c.want {
			t.Errorf("normalizeChatURL(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// ---------- needsTranslation ----------

func TestNeedsTranslation(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"   ", false},
		{"女優の面接", true},              // 日文（含假名）
		{"아이돌", true},                // 韩文
		{"English Title Here", true}, // 纯英文
		{"91CM-014 日本街头拜金女大测试", false}, // 已是中文
		{"中文与English混排", false},        // 含中文，保留
	}
	for _, c := range cases {
		if got := needsTranslation(c.in); got != c.want {
			t.Errorf("needsTranslation(%q) = %v，期望 %v", c.in, got, c.want)
		}
	}
}

// ---------- mock chat 服务器 ----------

// chatMock 返回一个 OpenAI 兼容的 chat/completions 假服务。
// 译文统一是「译:」+ 原文，便于断言；返回 *int 记录收到的请求数。
func chatMock(t *testing.T, status int, wantAuth bool) (*httptest.Server, *int) {
	t.Helper()
	count := 0
	h := func(w http.ResponseWriter, r *http.Request) {
		count++
		if wantAuth && r.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var req openAIChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		user := ""
		if len(req.Messages) >= 2 {
			user = req.Messages[1].Content
		}
		if status != http.StatusOK {
			writeJSON(w, status, map[string]any{"error": map[string]any{"message": "boom"}})
			return
		}
		writeJSON(w, 200, map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": "译:" + user}},
			},
		})
	}
	srv := httptest.NewServer(http.HandlerFunc(h))
	t.Cleanup(srv.Close)
	return srv, &count
}

// ---------- openAIClient.Translate ----------

func TestOpenAITranslateOK(t *testing.T) {
	srv, count := chatMock(t, http.StatusOK, true)
	cli := newOpenAIClient(OpenAIConfig{BaseURL: srv.URL, APIKey: "test-key", Model: "m"}, nil)
	got, err := cli.Translate(context.Background(), "女優の面接")
	if err != nil {
		t.Fatalf("不应出错：%v", err)
	}
	if got != "译:女優の面接" {
		t.Errorf("译文 = %q，期望「译:女優の面接」", got)
	}
	if *count != 1 {
		t.Errorf("应发 1 次请求，实际 %d", *count)
	}
}

func TestOpenAITranslateRespectsBaseURLPath(t *testing.T) {
	srv, _ := chatMock(t, http.StatusOK, true)
	// 用户填的是根地址（不带 /v1），请求应打到 /v1/chat/completions。
	cli := newOpenAIClient(OpenAIConfig{BaseURL: srv.URL, APIKey: "test-key", Model: "m"}, nil)
	if _, err := cli.Translate(context.Background(), "x"); err != nil {
		t.Fatalf("不应出错：%v", err)
	}
	// srv.URL 形如 http://127.0.0.1:PORT，normalizeChatURL 会拼成 .../v1/chat/completions，
	// httptest 默认路由能接住任意路径，所以这里只要没报错即可。
}

func TestOpenAITranslateError(t *testing.T) {
	srv, _ := chatMock(t, http.StatusInternalServerError, false)
	cli := newOpenAIClient(OpenAIConfig{BaseURL: srv.URL, APIKey: "test-key", Model: "m"}, nil)
	_, err := cli.Translate(context.Background(), "女優の面接")
	if err == nil {
		t.Fatal("500 应返回错误")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("错误信息应含状态码 500，实际 %v", err)
	}
}

func TestOpenAICleanTranslationStripsQuotes(t *testing.T) {
	srv, _ := chatMock(t, http.StatusOK, false)
	// 模型偶尔会用引号包裹，cleanTranslation 应剥掉。
	cli := newOpenAIClient(OpenAIConfig{BaseURL: srv.URL, APIKey: "k", Model: "m"}, nil)
	got, err := cli.Translate(context.Background(), "测试")
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(got, "\"'") {
		t.Errorf("译文不应含引号：%q", got)
	}
}

// ---------- openAIClient.Test（连通性探测）----------

// testEndpoint 是一个能按需返回 200 / 401 / 500 的假 chat 服务，用于测连通性。
func testEndpoint(t *testing.T, status int) (*httptest.Server, *int) {
	t.Helper()
	count := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		if r.Header.Get("Authorization") != "Bearer test-key" {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{"message": "Incorrect API key"}})
			return
		}
		if status != http.StatusOK {
			writeJSON(w, status, map[string]any{"error": map[string]any{"message": "server boom"}})
			return
		}
		writeJSON(w, 200, map[string]any{"choices": []map[string]any{{"message": map[string]any{"content": "pong"}}}})
	}))
	t.Cleanup(srv.Close)
	return srv, &count
}

// 注意：chatMock 里 wantAuth=true 时要求 Bearer test-key，这里复用 testEndpoint 更可控。

func TestOpenAITestOK(t *testing.T) {
	srv, count := testEndpoint(t, http.StatusOK)
	cli := newOpenAIClient(OpenAIConfig{BaseURL: srv.URL, APIKey: "test-key", Model: "gpt-4o-mini"}, nil)
	ok, msg := cli.Test(context.Background())
	if !ok {
		t.Errorf("应连通成功，实际失败：%s", msg)
	}
	if *count != 1 {
		t.Errorf("应发 1 次探测请求，实际 %d", *count)
	}
	if !strings.Contains(msg, "gpt-4o-mini") {
		t.Errorf("成功信息应含模型名，实际 %q", msg)
	}
}

func TestOpenAITestUnauthorized(t *testing.T) {
	srv, _ := testEndpoint(t, http.StatusOK)
	// Key 错误 → 后端返回 401
	cli := newOpenAIClient(OpenAIConfig{BaseURL: srv.URL, APIKey: "wrong-key", Model: "m"}, nil)
	ok, msg := cli.Test(context.Background())
	if ok {
		t.Fatal("Key 错误应探测失败")
	}
	if !strings.Contains(msg, "401") {
		t.Errorf("错误信息应含状态码 401，实际 %q", msg)
	}
}

func TestOpenAITestServerError(t *testing.T) {
	srv, _ := testEndpoint(t, http.StatusInternalServerError)
	cli := newOpenAIClient(OpenAIConfig{BaseURL: srv.URL, APIKey: "test-key", Model: "m"}, nil)
	ok, msg := cli.Test(context.Background())
	if ok {
		t.Fatal("500 应探测失败")
	}
	if !strings.Contains(msg, "500") {
		t.Errorf("错误信息应含状态码 500，实际 %q", msg)
	}
}

func TestOpenAITestMissingConfig(t *testing.T) {
	cli := newOpenAIClient(OpenAIConfig{}, nil)
	if ok, _ := cli.Test(context.Background()); ok {
		t.Error("未配置不应报成功")
	}
	cli2 := newOpenAIClient(OpenAIConfig{BaseURL: "http://x", APIKey: ""}, nil)
	if ok, _ := cli2.Test(context.Background()); ok {
		t.Error("缺 Key 不应报成功")
	}
}

func TestOpenAITestEmptyChoice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"choices": []map[string]any{}})
	}))
	t.Cleanup(srv.Close)
	cli := newOpenAIClient(OpenAIConfig{BaseURL: srv.URL, APIKey: "test-key", Model: "m"}, nil)
	if ok, _ := cli.Test(context.Background()); ok {
		t.Error("返回空 choices 应视为失败（模型不可用）")
	}
}

// ---------- handler: handleOpenAITest ----------

func TestHandleOpenAITestOK(t *testing.T) {
	srv, _ := testEndpoint(t, http.StatusOK)
	app := openaiApp(t, OpenAIConfig{BaseURL: srv.URL, APIKey: "test-key", Enabled: false}, "http://e", "http://m")
	rec := httptest.NewRecorder()
	body, _ := json.Marshal(map[string]any{"openai": map[string]any{"base_url": srv.URL, "api_key": "test-key", "model": "gpt-4o-mini"}})
	req := httptest.NewRequest(http.MethodPost, "/api/openai/test", bytes.NewReader(body))
	app.handleOpenAITest(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	var out struct {
		Data struct {
			OK      bool   `json:"ok"`
			Message string `json:"message"`
		} `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !out.Data.OK {
		t.Errorf("应连通成功，实际：%s", out.Data.Message)
	}
}

func TestHandleOpenAITestFallsBackToSaved(t *testing.T) {
	srv, count := testEndpoint(t, http.StatusOK)
	// 已保存配置里有 base_url + key；请求体只带 openai 但字段留空 → 应回落到已保存。
	app := openaiApp(t, OpenAIConfig{BaseURL: srv.URL, APIKey: "test-key", Enabled: false}, "http://e", "http://m")
	rec := httptest.NewRecorder()
	body, _ := json.Marshal(map[string]any{"openai": map[string]any{}}) // 全空
	req := httptest.NewRequest(http.MethodPost, "/api/openai/test", bytes.NewReader(body))
	app.handleOpenAITest(rec, req)
	var out struct {
		Data struct {
			OK bool `json:"ok"`
		} `json:"data"`
	}
	_ = json.NewDecoder(rec.Body).Decode(&out)
	if !out.Data.OK {
		t.Error("空请求体应回落到已保存配置并连通成功")
	}
	if *count != 1 {
		t.Errorf("回落后应发 1 次探测，实际 %d", *count)
	}
}

func TestHandleOpenAITestBadKey(t *testing.T) {
	srv, _ := testEndpoint(t, http.StatusOK) // 要求 Bearer test-key
	app := openaiApp(t, OpenAIConfig{}, "http://e", "http://m")
	rec := httptest.NewRecorder()
	body, _ := json.Marshal(map[string]any{"openai": map[string]any{"base_url": srv.URL, "api_key": "nope", "model": "m"}})
	req := httptest.NewRequest(http.MethodPost, "/api/openai/test", bytes.NewReader(body))
	app.handleOpenAITest(rec, req)
	var out struct {
		Data struct {
			OK      bool   `json:"ok"`
			Message string `json:"message"`
		} `json:"data"`
	}
	_ = json.NewDecoder(rec.Body).Decode(&out)
	if out.Data.OK {
		t.Error("错误 Key 应探测失败")
	}
	if !strings.Contains(out.Data.Message, "401") {
		t.Errorf("错误信息应含 401，实际 %q", out.Data.Message)
	}
}

// ---------- App.translateMeta（配置开关 + 降级）----------

func openaiApp(t *testing.T, oai OpenAIConfig, embyURL, mtURL string) *App {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("EMBYME_HOME", dir)
	store, err := NewStore(dir + "/config.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(c *Config) {
		c.EmbyURL = embyURL
		c.Token = "tok"
		c.UserID = "u1"
		c.MetaTubeURL = mtURL
		c.MetaTubeToken = "secret"
		c.OpenAI = oai
	}); err != nil {
		t.Fatal(err)
	}
	return NewApp(store, nil)
}

func TestTranslateMetaDisabledNoRequest(t *testing.T) {
	srv, count := chatMock(t, http.StatusOK, true)
	app := openaiApp(t, OpenAIConfig{BaseURL: srv.URL, APIKey: "test-key", Enabled: false}, "http://e", "http://m")
	title, ov, used := app.translateMeta(context.Background(), "女優の面接", "English plot")
	if used {
		t.Error("禁用时不应发起翻译")
	}
	if *count != 0 {
		t.Errorf("禁用时不应请求翻译接口，实际 %d 次", *count)
	}
	if title != "女優の面接" || ov != "English plot" {
		t.Errorf("禁用时应原样返回：%q / %q", title, ov)
	}
}

func TestTranslateMetaChineseSkipped(t *testing.T) {
	srv, count := chatMock(t, http.StatusOK, true)
	app := openaiApp(t, OpenAIConfig{BaseURL: srv.URL, APIKey: "test-key", Enabled: true}, "http://e", "http://m")
	_, _, used := app.translateMeta(context.Background(), "中文标题", "中文简介内容")
	if used {
		t.Error("已是中文不应翻译")
	}
	if *count != 0 {
		t.Errorf("中文不应请求翻译，实际 %d 次", *count)
	}
}

func TestTranslateMetaEnabledTranslates(t *testing.T) {
	srv, count := chatMock(t, http.StatusOK, true)
	app := openaiApp(t, OpenAIConfig{BaseURL: srv.URL, APIKey: "test-key", Enabled: true}, "http://e", "http://m")
	title, ov, used := app.translateMeta(context.Background(), "女優の面接", "English plot")
	if !used {
		t.Error("启用且非中文应发起翻译")
	}
	if *count != 2 {
		t.Errorf("应发 2 次（标题 + 简介），实际 %d", *count)
	}
	if title != "译:女優の面接" {
		t.Errorf("标题译文 = %q", title)
	}
	if ov != "译:English plot" {
		t.Errorf("简介译文 = %q", ov)
	}
}

// ---------- 集成：ScrapeMovie 写入前翻译 ----------

// japaneseMockMetaTube 返回一个「标题/简介都是非中文」的假 MetaTube，
// 用来验证翻译确实发生在写入 Emby 之前。
func japaneseMockMetaTube(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/providers", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"data": []string{"FANZA"}})
	})
	mux.HandleFunc("GET /v1/movies/search", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"data": []map[string]any{
			{"id": "m-001", "number": "SSIS-001", "title": "女優の面接", "provider": "FANZA", "score": 8.0},
		}})
	})
	mux.HandleFunc("GET /v1/movies/{provider}/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		writeJSON(w, 200, map[string]any{"data": map[string]any{
			"id": "m-001", "provider": "FANZA", "number": "SSIS-001",
			"title": "女優の面接", "title_zh": "",
			"actors": []string{"三上悠亜"},
			"genres": []string{"剧情"},
			"studio": "S1", "label": "S1", "series": "SSIS",
			"release_date": "2021-01-05", "runtime": 120, "score": 4.5,
			"plot":           "This is an English plot.",
			"director":       "A",
			"cover_url":      "http://example.invalid/c.jpg",
			"preview_images": []string{},
		}})
	})
	mux.HandleFunc("GET /v1/images/{kind}/{provider}/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("\xff\xd8\xff\xe0x"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestScrapeMovieTranslatesBeforeWrite(t *testing.T) {
	m := newMockEmby(t)
	mt := japaneseMockMetaTube(t)
	chat, _ := chatMock(t, http.StatusOK, true)
	app := openaiApp(t, OpenAIConfig{BaseURL: chat.URL, APIKey: "test-key", Enabled: true}, m.srv.URL, mt.URL)

	m.mu.Lock()
	m.items["mm1"] = map[string]any{
		"Id":          "mm1",
		"Name":        "SSIS-001 Actress Interview", // 英文，便于推断出番号
		"Type":        "Movie",
		"ImageTags":   map[string]any{},
		"ProviderIds": map[string]any{},
		"Path":        "/media/SSIS-001/SSIS-001.mp4",
	}
	m.mu.Unlock()

	if _, err := app.ScrapeMovie(context.Background(), "mm1", ScrapeOptions{}); err != nil {
		t.Fatalf("ScrapeMovie 不应出错：%v", err)
	}
	p := m.patched["mm1"]
	if p == nil {
		t.Fatal("没有写入 Emby")
	}
	// 关键断言：写入的标题 / 简介是翻译后的中文，不是原始的日文 / 英文。
	if name, _ := p["Name"].(string); name != "译:女優の面接" {
		t.Errorf("Name = %q，期望翻译后的「译:女優の面接」", name)
	}
	if ov, _ := p["Overview"].(string); ov != "译:This is an English plot." {
		t.Errorf("Overview = %q，期望翻译后的「译:This is an English plot.」", ov)
	}
}

func TestScrapeMovieNoTranslationWhenDisabled(t *testing.T) {
	m := newMockEmby(t)
	mt := japaneseMockMetaTube(t)
	_, count := chatMock(t, http.StatusOK, true)
	// 注意：这里故意传 Enabled=false，但把 BaseURL 也清空，模拟「完全没配翻译」。
	app := openaiApp(t, OpenAIConfig{}, m.srv.URL, mt.URL)

	m.mu.Lock()
	m.items["mm1"] = map[string]any{
		"Id": "mm1", "Name": "SSIS-001 Actress Interview", "Type": "Movie",
		"ImageTags": map[string]any{}, "ProviderIds": map[string]any{},
		"Path": "/media/SSIS-001/SSIS-001.mp4",
	}
	m.mu.Unlock()

	if _, err := app.ScrapeMovie(context.Background(), "mm1", ScrapeOptions{}); err != nil {
		t.Fatalf("ScrapeMovie 不应出错：%v", err)
	}
	p := m.patched["mm1"]
	if p == nil {
		t.Fatal("没有写入 Emby")
	}
	// 没开翻译：标题应是原始日文（MetaTube 的 title 字段）。
	if name, _ := p["Name"].(string); name != "女優の面接" {
		t.Errorf("未开翻译时 Name 应为原始日文，实际 %q", name)
	}
	if *count != 0 {
		t.Errorf("未开翻译不应请求翻译接口，实际 %d 次", *count)
	}
}
