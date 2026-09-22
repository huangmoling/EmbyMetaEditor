package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// probeEnv 把缓存目录指向临时目录，避免诊断落盘污染工作区。
func probeEnv(t *testing.T) {
	t.Helper()
	t.Setenv("EMBYME_HOME", t.TempDir())
}

func TestPageMarkers(t *testing.T) {
	data := []byte(`<html><div class="movie-box">a</div>
	<div class="movie-box">b</div>
	<img src="/pics/cover/x_b.jpg">
	<a href="/star/abc">x</a>
	<title>Just a moment...</title></html>`)
	m := pageMarkers(data)
	if m["movie-box"] != 2 {
		t.Errorf("movie-box 计数错误：期望 2，实际 %d", m["movie-box"])
	}
	if m["pics/cover"] != 1 {
		t.Errorf("pics/cover 计数错误：期望 1，实际 %d", m["pics/cover"])
	}
	if m["/star/"] != 1 {
		t.Errorf("/star/ 计数错误：期望 1，实际 %d", m["/star/"])
	}
	if m["Just a moment"] != 1 {
		t.Errorf("Just a moment 计数错误：期望 1，实际 %d", m["Just a moment"])
	}
	if m["bigImage"] != 0 {
		t.Errorf("bigImage 期望 0，实际 %d", m["bigImage"])
	}
}

func TestSampleText(t *testing.T) {
	got := sampleText([]byte("  hello\n\n\tworld   foo  "), 100)
	if got != "hello world foo" {
		t.Errorf("空白折叠错误：%q", got)
	}
	long := strings.Repeat("a", 300)
	got = sampleText([]byte(long), 200)
	if len([]rune(got)) != 201 { // 200 字符 + 省略号
		t.Errorf("截断长度错误：%d", len([]rune(got)))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("截断后应带省略号：%q", got[len(got)-3:])
	}
}

func TestParseStarLinks(t *testing.T) {
	data := []byte(`<div class="search-item">
	  <a href="/star/1v"><img src="/pics/actress/1v.jpg">三上悠亜</a>
	  <a href="/star/2k">高橋しょう子</a>
	  <a href="/star/1v">三上悠亜（重复）</a>
	  <a href="/SSIS-001">不是演员页</a>
	</div>`)
	got := parseStarLinks(data, "https://www.javbus.com")
	if len(got) != 2 {
		t.Fatalf("期望 2 个演员（去重后），实际 %d：%+v", len(got), got)
	}
	if got[0].ID != "1v" || got[0].Name != "三上悠亜" {
		t.Errorf("第 1 个解析错误：%+v", got[0])
	}
	if got[0].URL != "https://www.javbus.com/star/1v" {
		t.Errorf("URL 拼接错误：%s", got[0].URL)
	}
	if got[1].ID != "2k" {
		t.Errorf("第 2 个解析错误：%+v", got[1])
	}
}

func TestParseStarLinksNoMatch(t *testing.T) {
	got := parseStarLinks([]byte(`<html><body><p>没有结果</p></body></html>`), "https://x")
	if len(got) != 0 {
		t.Errorf("期望 0，实际 %d", len(got))
	}
}

const probeHomeFixture = `<!DOCTYPE html><html><body>
<div class="movie-box"><a href="/SSIS-001"><div class="photo-frame">
<img src="/pics/cover/a_b.jpg"></div></a></div>
</body></html>`

const probeCloudflareFixture = `<!DOCTYPE html><html><head><title>Just a moment...</title></head>
<body><div class="cf-browser-verification"></div></body></html>`

func TestProbeOK(t *testing.T) {
	probeEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(probeHomeFixture))
	}))
	defer srv.Close()

	jb := NewJavBus(Config{JavBusURL: srv.URL})
	p := jb.Probe(context.Background(), "")
	if !p.OK {
		t.Fatalf("期望连通，实际：%+v", p)
	}
	if !p.LooksLikeHome {
		t.Errorf("应识别为首页结构：%+v", p.Markers)
	}
	if p.Status != 200 || p.Size == 0 {
		t.Errorf("状态/大小记录错误：status=%d size=%d", p.Status, p.Size)
	}
	if p.Blocked != "" {
		t.Errorf("不应标记为被拦截：%s", p.Blocked)
	}
	if p.Search != nil {
		t.Errorf("未传关键词时不应有搜索探测结果")
	}
}

func TestProbeEmptyBody(t *testing.T) {
	probeEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200) // 200 但空 body —— 实测中运营商拦截的典型表现
	}))
	defer srv.Close()

	jb := NewJavBus(Config{JavBusURL: srv.URL})
	p := jb.Probe(context.Background(), "")
	if p.OK {
		t.Fatalf("空响应体不应判定为连通：%+v", p)
	}
	if p.Blocked != "empty" {
		t.Errorf("应标记为 empty，实际 %q", p.Blocked)
	}
	if !strings.Contains(p.Message, "响应体为空") {
		t.Errorf("提示信息未说明原因：%s", p.Message)
	}
}

func TestProbeCloudflare(t *testing.T) {
	probeEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(probeCloudflareFixture))
	}))
	defer srv.Close()

	jb := NewJavBus(Config{JavBusURL: srv.URL})
	p := jb.Probe(context.Background(), "")
	if p.OK {
		t.Fatalf("验证页不应判定为连通：%+v", p)
	}
	if p.Blocked != "cloudflare" {
		t.Errorf("应标记为 cloudflare，实际 %q", p.Blocked)
	}
	if !strings.Contains(p.Message, "Cookie") {
		t.Errorf("提示里应引导填 Cookie：%s", p.Message)
	}
	if p.DumpPath == "" {
		t.Errorf("被拦截时应落盘原始页面")
	}
}

func TestProbeUnreachable(t *testing.T) {
	probeEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // 立刻关掉，制造连接失败

	jb := NewJavBus(Config{JavBusURL: url})
	p := jb.Probe(context.Background(), "")
	if p.OK {
		t.Fatalf("不可达不应判定为连通：%+v", p)
	}
	if !strings.Contains(p.Message, "无法连接") {
		t.Errorf("应提示无法连接：%s", p.Message)
	}
}

func TestProbeUnconfigured(t *testing.T) {
	probeEnv(t)
	jb := NewJavBus(Config{})
	p := jb.Probe(context.Background(), "")
	if p.OK || p.Message != "未配置 javbus 地址" {
		t.Errorf("未配置时提示错误：%+v", p)
	}
}

func TestProbeStructureMismatch(t *testing.T) {
	probeEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html><body><h1>站点维护中</h1></body></html>`))
	}))
	defer srv.Close()

	jb := NewJavBus(Config{JavBusURL: srv.URL})
	p := jb.Probe(context.Background(), "")
	if p.OK {
		t.Fatalf("结构不符不应判定为连通：%+v", p)
	}
	if p.LooksLikeHome {
		t.Errorf("不应识别为首页")
	}
	if p.DumpPath == "" {
		t.Errorf("结构不符时应落盘原始页面")
	}
}

func TestProbeWithSearchJSON(t *testing.T) {
	probeEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/searchstar/") {
			w.Write([]byte(`[{"star_id":"1v","name":"三上悠亜"}]`))
			return
		}
		w.Write([]byte(probeHomeFixture))
	}))
	defer srv.Close()

	jb := NewJavBus(Config{JavBusURL: srv.URL})
	p := jb.Probe(context.Background(), "三上悠亜")
	if !p.OK {
		t.Fatalf("主站应连通：%+v", p)
	}
	if p.Search == nil {
		t.Fatal("应带回搜索探测结果")
	}
	if p.Search.Kind != "json" || p.Search.Found != 1 || !p.Search.OK {
		t.Errorf("JSON 搜索探测结果错误：%+v", p.Search)
	}
}

func TestProbeWithSearchEmptyArray(t *testing.T) {
	probeEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/searchstar/") {
			w.Write([]byte(`[]`))
			return
		}
		w.Write([]byte(probeHomeFixture))
	}))
	defer srv.Close()

	jb := NewJavBus(Config{JavBusURL: srv.URL})
	p := jb.Probe(context.Background(), "查无此人")
	if p.Search == nil || p.Search.OK {
		t.Fatalf("空数组不应算命中：%+v", p.Search)
	}
	if !strings.Contains(p.Search.Error, "没有这个演员") {
		t.Errorf("应提示站内无此演员：%s", p.Search.Error)
	}
}

func TestProbeWithSearchHTMLFallback(t *testing.T) {
	probeEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/searchstar/") {
			w.Write([]byte(`<a href="/star/1v">三上悠亜</a>`))
			return
		}
		w.Write([]byte(probeHomeFixture))
	}))
	defer srv.Close()

	jb := NewJavBus(Config{JavBusURL: srv.URL})
	p := jb.Probe(context.Background(), "三上")
	if p.Search == nil {
		t.Fatal("应带回搜索探测结果")
	}
	if p.Search.Kind != "html" || p.Search.Found != 1 || !p.Search.OK {
		t.Errorf("HTML 回退解析错误：%+v", p.Search)
	}
}

func TestProbeWithSearchUnparsable(t *testing.T) {
	probeEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/searchstar/") {
			w.Write([]byte(`<html><body><p>服务异常</p></body></html>`))
			return
		}
		w.Write([]byte(probeHomeFixture))
	}))
	defer srv.Close()

	jb := NewJavBus(Config{JavBusURL: srv.URL})
	p := jb.Probe(context.Background(), "三上")
	if p.Search == nil || p.Search.OK {
		t.Fatalf("解析不出链接不应算命中：%+v", p.Search)
	}
	if p.Search.DumpPath == "" {
		t.Errorf("解析失败时应落盘原始响应")
	}
	if p.Search.RawSample == "" {
		t.Errorf("解析失败时应带响应片段")
	}
}

// TestJavbusProbeEndpoint 走真实 HTTP 路由，确认前端按钮打到的那条链路是通的。
func TestJavbusProbeEndpoint(t *testing.T) {
	jb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/searchstar/") {
			w.Write([]byte(`[{"star_id":"1v","name":"三上悠亜"}]`))
			return
		}
		w.Write([]byte(probeHomeFixture))
	}))
	defer jb.Close()

	app := testApp(t, "", "")
	if err := app.store.Update(func(c *Config) {
		c.JavBusURL = jb.URL
		c.JavBusInterval = 10
	}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app.route())
	defer srv.Close()

	// 不带关键词
	resp, err := http.Get(srv.URL + "/api/javbus/probe")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("状态码 %d", resp.StatusCode)
	}
	var out struct {
		OK   bool    `json:"ok"`
		Data JBProbe `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || !out.Data.OK || !out.Data.LooksLikeHome {
		t.Fatalf("接口返回异常：%+v", out)
	}
	if out.Data.Search != nil {
		t.Errorf("未传 q 时不应有搜索探测")
	}

	// 带关键词
	resp2, err := http.Get(srv.URL + "/api/javbus/probe?q=" + url.QueryEscape("三上悠亜"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var out2 struct {
		Data JBProbe `json:"data"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&out2); err != nil {
		t.Fatal(err)
	}
	if out2.Data.Search == nil || out2.Data.Search.Found != 1 {
		t.Fatalf("q 参数未生效：%+v", out2.Data.Search)
	}
}

// TestJavbusProbeEndpointUnreachable 确认诊断接口在站点不可达时也返回 200 + 诊断信息，
// 而不是抛 500 —— 前端要靠这段信息告诉用户卡在哪。
func TestJavbusProbeEndpointUnreachable(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	app := testApp(t, "", "")
	if err := app.store.Update(func(c *Config) {
		c.JavBusURL = deadURL
		c.JavBusInterval = 10
	}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app.route())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/javbus/probe")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("诊断失败也应返回 200，实际 %d", resp.StatusCode)
	}
	var out struct {
		OK   bool    `json:"ok"`
		Data JBProbe `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !out.OK {
		t.Fatalf("接口本身应成功：%+v", out)
	}
	if out.Data.OK {
		t.Errorf("站点不可达时不应报连通")
	}
	if !strings.Contains(out.Data.Message, "无法连接") {
		t.Errorf("应给出无法连接的提示：%s", out.Data.Message)
	}
}
