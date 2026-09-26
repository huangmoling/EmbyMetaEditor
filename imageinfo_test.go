package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------- 图片尺寸 / 体积探测（imageinfo.go）----------
//
// 这一小块的价值全在「界面上能看出哪张候选更清晰」，所以测的是
// 「服务端如实回报三个数：字节数、宽、高」，以及「不认识的地址不会变成
// 任意 URL 下载器」——后者是这个接口唯一的危险面。

// imageInfoApp 起一个只配了 gfriends CDN 的 App（探测只认白名单主机）。
func imageInfoApp(t *testing.T, cdn string) *App {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("EMBYME_HOME", dir)
	store, err := NewStore(dir + "/config.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(c *Config) { c.GfriendsCDN = cdn }); err != nil {
		t.Fatal(err)
	}
	return NewApp(store, nil)
}

// pngOf 造一张指定尺寸的真 PNG。用真图而不是假字节，
// 因为要验的正是「尺寸是从文件头里读出来的」。
func pngOf(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func jpegOf(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h)), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestProbeImageConfig(t *testing.T) {
	w, h, format := probeImageConfig(pngOf(t, 120, 160))
	if w != 120 || h != 160 || format != "png" {
		t.Errorf("PNG 尺寸/格式 = %d×%d %s，期望 120×160 png", w, h, format)
	}
	// JPEG 也要认得：gfriends 库里两种都有，只认一种等于一半候选没有尺寸。
	if w, h, format = probeImageConfig(jpegOf(t, 300, 400)); w != 300 || h != 400 || format != "jpeg" {
		t.Errorf("JPEG 尺寸/格式 = %d×%d %s，期望 300×400 jpeg", w, h, format)
	}
	// 解不出来时必须是「三个零」而不是乱猜一个数：界面会显示「尺寸未知」，
	// 猜出来的数字用户没法分辨真假。
	if w, h, format = probeImageConfig([]byte("not an image at all")); w != 0 || h != 0 || format != "" {
		t.Errorf("非图片应返回全零，实际 %d×%d %s", w, h, format)
	}
	if w, h, format = probeImageConfig(nil); w != 0 || h != 0 || format != "" {
		t.Errorf("空字节应返回全零，实际 %d×%d %s", w, h, format)
	}
}

func TestHandleImageInfoReportsBytesAndSize(t *testing.T) {
	png300 := pngOf(t, 300, 400)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(png300)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	app := imageInfoApp(t, srv.URL)
	proxy := httptest.NewServer(app.route())
	defer proxy.Close()

	body, _ := json.Marshal(map[string]any{"urls": []string{
		srv.URL + "/ok.png",
		srv.URL + "/missing.png",
	}})
	resp, err := http.Post(proxy.URL+"/api/img/info", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("HTTP %d：%s", resp.StatusCode, raw)
	}
	var out struct {
		Data struct {
			Items []imageInfo `json:"items"`
			Trunc bool        `json:"truncated"`
		} `json:"data"`
	}
	if err := decodeJSON(resp.Body, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data.Items) != 2 {
		t.Fatalf("返回 %d 条，期望 2 条（成功的失败的各一条）", len(out.Data.Items))
	}
	// 顺序必须与请求一致：前端是按下标/地址回填到每张图下面那行小字的。
	if out.Data.Items[0].URL != srv.URL+"/ok.png" {
		t.Errorf("第 0 条地址不对：%s", out.Data.Items[0].URL)
	}
	ok := out.Data.Items[0]
	if !ok.OK {
		t.Fatalf("正常图片应探测成功：%+v", ok)
	}
	if ok.Bytes != len(png300) {
		t.Errorf("字节数 = %d，期望 %d", ok.Bytes, len(png300))
	}
	if ok.Width != 300 || ok.Height != 400 {
		t.Errorf("尺寸 = %d×%d，期望 300×400", ok.Width, ok.Height)
	}
	if ok.Format != "png" {
		t.Errorf("格式 = %q，期望 png", ok.Format)
	}
	// 单张取不到只该让这一条失败，不能连累整批（界面上那一张显示「读不到大小」）。
	bad := out.Data.Items[1]
	if bad.OK || bad.Error == "" {
		t.Errorf("取不到的图应 ok=false 且带原因：%+v", bad)
	}
}

func TestHandleImageInfoRejectsNonWhitelistedHost(t *testing.T) {
	app := imageInfoApp(t, "")
	proxy := httptest.NewServer(app.route())
	defer proxy.Close()

	// 公网域名、不在白名单里。这个接口会**真的去下载**，所以必须逐条拒绝，
	// 否则就成了对外开放的任意 URL 下载器（SSRF 的常规形态）。
	body, _ := json.Marshal(map[string]any{"urls": []string{"https://example.com/a.jpg"}})
	resp, err := http.Post(proxy.URL+"/api/img/info", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Data struct {
			Items []imageInfo `json:"items"`
		} `json:"data"`
	}
	if err := decodeJSON(resp.Body, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data.Items) != 1 {
		t.Fatalf("返回 %d 条，期望 1 条", len(out.Data.Items))
	}
	it := out.Data.Items[0]
	if it.OK {
		t.Errorf("白名单外的地址不该被代取：%+v", it)
	}
	if !strings.Contains(it.Error, "白名单") {
		t.Errorf("错误信息应说清是白名单问题，实际 %q", it.Error)
	}
}

func TestHandleImageInfoRejectsEmptyAndTruncates(t *testing.T) {
	app := imageInfoApp(t, "")
	proxy := httptest.NewServer(app.route())
	defer proxy.Close()

	post := func(urls []string) (int, map[string]any) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"urls": urls})
		resp, err := http.Post(proxy.URL+"/api/img/info", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var v map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&v)
		return resp.StatusCode, v
	}

	if code, _ := post(nil); code != 400 {
		t.Errorf("空 urls 应 400，实际 %d", code)
	}

	// 超上限只截断、不报错：界面上「后面的图没有大小」远好过「整批探测失败」。
	urls := make([]string, maxImageProbe+7)
	for i := range urls {
		urls[i] = fmt.Sprintf("https://example.com/%d.jpg", i)
	}
	code, v := post(urls)
	if code != 200 {
		t.Fatalf("超上限应截断而不是报错，实际 %d", code)
	}
	data, _ := v["data"].(map[string]any)
	items, _ := data["items"].([]any)
	if len(items) != maxImageProbe {
		t.Errorf("截断后应剩 %d 条，实际 %d", maxImageProbe, len(items))
	}
	if data["truncated"] != true {
		t.Errorf("截断时必须如实回报 truncated=true：%+v", data["truncated"])
	}
}
