package main

// 演员头像刮削的回归测试。
// 重点覆盖 2026-09-23 的 bug：手动挑选头像时前端传回的是**完整 URL**，
// 后端拿裸文件名比对永远不等，静默回落成第一张 ——「点谁都换不上去」。

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"path"
	"testing"
	"time"
)

// makeJPEG 生成指定尺寸的纯色 JPEG，用来区分「哪张图被上传」。
// jpeg 编码是确定性的：同参数两次编码字节相同，可以整段比对。
func makeJPEG(t *testing.T, w, h int, c color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// gfCDNServer 模拟 gfriends 的 CDN：按路径里的文件名返回不同图片。
func gfCDNServer(t *testing.T, files map[string][]byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if data, ok := files[path.Base(r.URL.Path)]; ok {
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(data)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// injectGfIndex 把候选直接塞进 App 的 gfriends 索引，并把 loadedAt 设为现在
// （EnsureLoaded 直接短路，不碰网络）。
func injectGfIndex(app *App, entries ...GfriendEntry) {
	app.gf.byName = map[string][]GfriendEntry{"渚このみ": entries}
	app.gf.loadedAt = time.Now()
}

// useCDN 把 gfriends CDN 指到测试服务器。ScrapePersonAvatar 下载图片用的是
// cfg.GfriendsCDN，不设的话 URL 是相对路径，httptest 服务器根本收不到请求。
func useCDN(t *testing.T, app *App, url string) {
	t.Helper()
	if err := app.store.Update(func(c *Config) { c.GfriendsCDN = url }); err != nil {
		t.Fatal(err)
	}
}

var (
	avRed  = color.RGBA{255, 0, 0, 255}
	avBlue = color.RGBA{0, 0, 255, 255}
)

// 手动选头像：前端传回完整 URL，上传的必须是**选中的那张**，
// 不能像修复前那样静默回落到第一张。
func TestAvatarPickSelectedFile(t *testing.T) {
	m := newMockEmby(t)
	small := makeJPEG(t, 10, 10, avRed)
	big := makeJPEG(t, 30, 40, avBlue)
	cdn := gfCDNServer(t, map[string][]byte{"a.jpg": small, "b.jpg": big})
	app := testApp(t, m.srv.URL, "http://mt.invalid")
	useCDN(t, app, cdn.URL)
	e1 := GfriendEntry{Group: "G1", File: "a.jpg"}
	e2 := GfriendEntry{Group: "G2", File: "b.jpg"}
	injectGfIndex(app, e1, e2)

	// 故意选第一张 a.jpg：修复前无论如何都会落到第一张，
	// 所以这里必须断言「选 a 得到 a」，用第二张区分不出「选中」和「回落」。
	res, err := app.ScrapePersonAvatar(context.Background(), "p1", "渚このみ", AvatarOptions{
		Source: "gfriends", File: e1.URL(cdn.URL), Overwrite: true,
	})
	if err != nil {
		t.Fatalf("不应出错：%v", err)
	}
	if res.Skipped {
		t.Fatalf("不应跳过：%s", res.Message)
	}
	if len(m.bodies) == 0 {
		t.Fatal("没有上传头像")
	}
	if !bytes.Equal(m.bodies[len(m.bodies)-1], small) {
		t.Errorf("上传的不是选中的 a.jpg（%d 字节），而是别张图", len(m.bodies[len(m.bodies)-1]))
	}

	// 再选第二张 b.jpg：验证切换真的生效。
	m.uploaded = nil
	m.bodies = nil
	if _, err := app.ScrapePersonAvatar(context.Background(), "p1", "渚このみ", AvatarOptions{
		Source: "gfriends", File: e2.URL(cdn.URL), Overwrite: true,
	}); err != nil {
		t.Fatalf("不应出错：%v", err)
	}
	if len(m.bodies) == 0 {
		t.Fatal("第二次没有上传头像")
	}
	if !bytes.Equal(m.bodies[len(m.bodies)-1], big) {
		t.Errorf("选 b.jpg 却上传了别张图（%d 字节）", len(m.bodies[len(m.bodies)-1]))
	}
}

// 默认刮削（不手动挑）：多张候选时应选**分辨率最高**的那张（对齐 gfriends 高清优先）。
func TestAvatarPrefersHD(t *testing.T) {
	m := newMockEmby(t)
	small := makeJPEG(t, 10, 10, avRed)
	big := makeJPEG(t, 60, 80, avBlue)
	cdn := gfCDNServer(t, map[string][]byte{"small.jpg": small, "big.jpg": big})
	app := testApp(t, m.srv.URL, "http://mt.invalid")
	useCDN(t, app, cdn.URL)
	injectGfIndex(app,
		GfriendEntry{Group: "G1", File: "small.jpg"},
		GfriendEntry{Group: "G2", File: "big.jpg"},
	)
	if _, err := app.ScrapePersonAvatar(context.Background(), "p1", "渚このみ", AvatarOptions{
		Source: "gfriends", Overwrite: true,
	}); err != nil {
		t.Fatalf("不应出错：%v", err)
	}
	if len(m.bodies) == 0 {
		t.Fatal("没有上传头像")
	}
	if !bytes.Equal(m.bodies[len(m.bodies)-1], big) {
		t.Errorf("应优先高清的 big.jpg（60x80），实际上传了别的图（%d 字节）", len(m.bodies[len(m.bodies)-1]))
	}
}

// 选中文件在索引里找不到：必须明确报错，不能静默用第一张（修复前就是静默回落）。
func TestAvatarSelectedFileNotFound(t *testing.T) {
	m := newMockEmby(t)
	cdn := gfCDNServer(t, map[string][]byte{"a.jpg": makeJPEG(t, 10, 10, avRed)})
	app := testApp(t, m.srv.URL, "http://mt.invalid")
	useCDN(t, app, cdn.URL)
	injectGfIndex(app, GfriendEntry{Group: "G1", File: "a.jpg"})

	_, err := app.ScrapePersonAvatar(context.Background(), "p1", "渚このみ", AvatarOptions{
		Source: "gfriends", File: cdn.URL + "/Content/Other/missing.jpg", Overwrite: true,
	})
	if err == nil {
		t.Fatal("匹配不到所选文件应报错，而不是静默用第一张")
	}
	if len(m.uploaded) != 0 {
		t.Errorf("报错时不应上传，实际 %v", m.uploaded)
	}
}
