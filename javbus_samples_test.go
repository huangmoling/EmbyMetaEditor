package main

// 「磁力预览」接口的回归测试。
//
// 预览本身只是把 javbus 详情页里的样例图搬出来看，但它有两个**容易悄悄退化**
// 的性质，都在这里钉住：
//  1. 它不能顺带去抓磁力列表 —— 那是另一个 ajax，javbus 的请求间隔是 1.5s，
//     用户只想看图却白等一秒、还多打一次站点，纯属浪费；
//  2. 返回的地址必须已经是**绝对地址**，前端要直接喂 /api/img 代取。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func jbFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "javbus", name))
	if err != nil {
		t.Fatalf("读取夹具失败（丢了？重跑 tools/fetch_javbus_fixture.py）：%v", err)
	}
	return b
}

// jbSamplesServer 模拟 javbus：详情页返回「脚本参数片段 + 真实样例图区块」，
// 磁力 ajax 一律 500 并计数 —— 计数不为 0 就说明预览顺带去抓磁力了。
func jbSamplesServer(t *testing.T) (*httptest.Server, *int32) {
	t.Helper()
	var ajax int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /SSIS-001", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// 两段都是真实页面切片：脚本参数那段来自 javbus_detail_test.go 的夹具，
		// 样例图那段由 tools/fetch_javbus_fixture.py 逐字节截取。
		page := append([]byte{}, []byte(detailScriptFixture)...)
		page = append(page, '\n')
		page = append(page, jbFixture(t, "detail_ssis001_samples.html")...)
		_, _ = w.Write(page)
	})
	mux.HandleFunc("GET /ajax/uncledatoolsbyajax.php", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&ajax, 1)
		http.Error(w, "预览不该打这个接口", http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &ajax
}

func TestHandleJavbusSamples(t *testing.T) {
	srv, ajax := jbSamplesServer(t)
	app := testApp(t, "http://127.0.0.1:1", "http://127.0.0.1:1")
	if err := app.store.Update(func(c *Config) { c.JavBusURL = srv.URL }); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	app.handleJavbusSamples(rec, httptest.NewRequest(http.MethodGet, "/api/javbus/samples?number=SSIS-001", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", rec.Code, rec.Body.String())
	}

	var env struct {
		OK   bool `json:"ok"`
		Data struct {
			Number  string   `json:"number"`
			URL     string   `json:"url"`
			Samples []string `json:"samples"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("响应不是 JSON：%v / %s", err, rec.Body.String())
	}
	if !env.OK {
		t.Fatalf("ok 应为 true：%s", rec.Body.String())
	}
	if len(env.Data.Samples) != 10 {
		t.Fatalf("样例图 = %d 张，期望 10：%v", len(env.Data.Samples), env.Data.Samples)
	}
	for i, s := range env.Data.Samples {
		if !strings.HasPrefix(s, srv.URL+"/pics/sample/") {
			t.Errorf("第 %d 张不是 mock 站的绝对地址：%s（前端要拿它去 /api/img 代取）", i+1, s)
		}
	}
	if env.Data.Number == "" {
		t.Error("没带回番号，抽屉标题会缺")
	}
	// 关键性质：预览不碰磁力 ajax。
	if n := atomic.LoadInt32(ajax); n != 0 {
		t.Errorf("预览顺带请求了磁力 ajax %d 次 —— 白等一次限速，且多打站点一次", n)
	}
}

// 缺参数要直接 400，不能把空番号拼进站点地址去撞 404/验证页。
func TestHandleJavbusSamplesMissingNumber(t *testing.T) {
	app := testApp(t, "http://127.0.0.1:1", "http://127.0.0.1:1")
	rec := httptest.NewRecorder()
	app.handleJavbusSamples(rec, httptest.NewRequest(http.MethodGet, "/api/javbus/samples", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400：%s", rec.Code, rec.Body.String())
	}
}

// 上游出错时必须把错误抛出来，不能返回「空列表 + ok:true」——
// 那在界面上表现为「这个番号没有样例图」，把站点故障说成了作品没有样张。
func TestHandleJavbusSamplesUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	app := testApp(t, "http://127.0.0.1:1", "http://127.0.0.1:1")
	if err := app.store.Update(func(c *Config) { c.JavBusURL = srv.URL }); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	app.handleJavbusSamples(rec, httptest.NewRequest(http.MethodGet, "/api/javbus/samples?number=SSIS-001", nil))
	if rec.Code == http.StatusOK {
		t.Fatalf("上游 404 却返回 200：%s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "404") {
		t.Errorf("错误信息里应能看出是 404：%s", rec.Body.String())
	}
}
