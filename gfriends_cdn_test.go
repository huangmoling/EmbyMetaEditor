package main

// gfriends 的 CDN 容错：索引和图片都要有备用地址。
//
// 盯的是「主基址挂掉时图片还能不能刮到」—— 只给索引加兜底的话，索引照样下得回来，
// 但每个演员的候选图全部下载失败，表现是「刮削头像 / 选图」整体不可用，等于没兜底。
//
// 另一条容易漏的：备用基址必须都在**图片代理白名单**里。漏了的话命令行直接 curl
// 那个地址是 200，而浏览器里经 /api/img 代取会被拒 —— 界面破图，命令行复现不出来。

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"strings"
	"sync/atomic"
	"testing"
)

// deadCDNBase 指向一个必然连不上的地址。端口 1 会立刻拒绝连接（不会挂住等超时），
// 所以「主基址失败」这个场景可以稳定复现。
const deadCDNBase = "http://127.0.0.1:1/"

func TestGfriendsCDNBasesOrderAndCoverage(t *testing.T) {
	primary := "https://cdn.jsdelivr.net/gh/gfriends/gfriends@master"
	got := gfriendsCDNBases(primary)
	if len(got) < 4 {
		t.Fatalf("备用基址太少，实际 %d 个: %v", len(got), got)
	}
	// 配置里的那个必须排第一 —— 顺序变了等于把用户的配置降级成兜底。
	if got[0] != primary+"/" {
		t.Errorf("配置的基址应排第一且补上结尾斜杠: %q", got[0])
	}
	has := func(sub string) bool {
		for _, u := range got {
			if strings.Contains(u, sub) {
				return true
			}
		}
		return false
	}
	for _, want := range []string{
		"gcore.jsdelivr.net", "fastly.jsdelivr.net",
		"raw.githubusercontent.com",
		"xinxin8816/gfriends", // 镜像仓库
	} {
		if !has(want) {
			t.Errorf("图片基址里应有 %s: %v", want, got)
		}
	}
	if len(got) != len(dedupeURLs(got)) {
		t.Errorf("基址里出现重复: %v", got)
	}
	// 带不带结尾斜杠必须归一化成同一个（否则白白多试一轮）
	if a, b := gfriendsCDNBases(primary), gfriendsCDNBases(primary+"/"); len(a) != len(b) {
		t.Errorf("尾斜杠不应产生额外基址: %d vs %d", len(a), len(b))
	}
}

func TestGfriendsCDNBasesKeepsCustom(t *testing.T) {
	custom := "https://mirror.lan:8642/gf"
	got := gfriendsCDNBases(custom)
	if len(got) != 1 || got[0] != custom+"/" {
		t.Errorf("自定义 CDN 不应被追加默认地址: %v", got)
	}
}

// 备用基址都要能过图片代理白名单，否则界面上那一版图是破的。
func TestGfriendsCDNBasesAreProxyAllowed(t *testing.T) {
	cfg := Config{}
	check := func(label, raw string) {
		t.Helper()
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" {
			t.Fatalf("%s 地址无法解析: %q", label, raw)
		}
		if !imageHostAllowed(strings.ToLower(u.Hostname()), cfg) {
			t.Errorf("%s 的主机不在图片代理白名单里: %s", label, u.Hostname())
		}
	}
	for _, base := range gfriendsCDNBases("") {
		check("图片备用基址", base)
	}
	for _, u := range gfriendsTreeCandidates("") {
		check("索引候选地址", u)
	}
}

func TestPickBestGfriendsFallsBackToMirror(t *testing.T) {
	img := makeJPEG(t, 20, 20, avBlue)
	var hits int32
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if path.Base(r.URL.Path) != "a.jpg" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(img)
	}))
	defer mirror.Close()

	entries := []GfriendEntry{{Group: "G1", File: "a.jpg"}}
	got, ct, detail, err := pickBestGfriends(context.Background(), &http.Client{},
		entries, []string{deadCDNBase, mirror.URL + "/"})
	if err != nil {
		t.Fatalf("主基址挂掉时应回落到备用基址: %v", err)
	}
	if !bytes.Equal(got, img) {
		t.Errorf("回落后取到的图不对: %d 字节", len(got))
	}
	if ct != "image/jpeg" {
		t.Errorf("Content-Type 应保留: %q", ct)
	}
	if detail != "G1/a.jpg" {
		t.Errorf("detail 应为 分组/文件名: %q", detail)
	}
	if atomic.LoadInt32(&hits) == 0 {
		t.Error("备用基址一次都没被请求，说明没真的回落")
	}
}

// 主基址正常时**不能**顺带去试备用基址：正常网络下这层容错必须是零开销。
func TestPickBestGfriendsPrimarySuccessSkipsFallback(t *testing.T) {
	img := makeJPEG(t, 12, 12, avRed)
	primary := gfCDNServer(t, map[string][]byte{"a.jpg": img})
	var mirrorHits int32
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&mirrorHits, 1)
		http.NotFound(w, r)
	}))
	defer mirror.Close()

	entries := []GfriendEntry{{Group: "G1", File: "a.jpg"}}
	got, _, _, err := pickBestGfriends(context.Background(), &http.Client{},
		entries, []string{primary.URL + "/", mirror.URL + "/"})
	if err != nil {
		t.Fatalf("主基址可用时不应出错: %v", err)
	}
	if !bytes.Equal(got, img) {
		t.Errorf("取到的图不对: %d 字节", len(got))
	}
	if n := atomic.LoadInt32(&mirrorHits); n != 0 {
		t.Errorf("主基址成功时不该请求备用基址，实际 %d 次", n)
	}
}

func TestPickBestGfriendsAllBasesFail(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer down.Close()
	entries := []GfriendEntry{{Group: "G1", File: "a.jpg"}}
	if _, _, _, err := pickBestGfriends(context.Background(), &http.Client{},
		entries, []string{deadCDNBase, down.URL + "/"}); err == nil {
		t.Error("所有基址都失败时应报错，不能静默返回空图")
	}
}

// 用户可能在**备用基址**上看到那张图（搜索接口下发的是配置基址，但配置随时会改），
// 所以匹配要认全部基址 —— 文件名仍然必须精确相等，不能放宽成「随便来一张」。
func TestMatchGfriendEntryAcceptsAnyBase(t *testing.T) {
	entry := GfriendEntry{Group: "8-GRAPHIS", File: "三上悠亜-1.jpg?t=1657944780"}
	bases := gfriendsCDNBases("")
	mirrorBase := "https://gcore.jsdelivr.net/gh/xinxin8816/gfriends@master/"

	got, ok := matchGfriendEntry([]GfriendEntry{entry}, entry.URL(mirrorBase), bases)
	if !ok || got.File != entry.File {
		t.Errorf("备用基址上的完整 URL 应能精确命中: ok=%v got=%+v", ok, got)
	}
	// 裸文件名（带不带 ?t= 缓存戳）也要命中 —— 前端早期版本传的是裸名字。
	// 不剥查询串的话这条分支永远不成立（索引里的 File 就带 ?t=）。
	for _, name := range []string{entry.File, "三上悠亜-1.jpg"} {
		if _, ok := matchGfriendEntry([]GfriendEntry{entry}, name, bases); !ok {
			t.Errorf("裸文件名 %q 应能命中", name)
		}
	}
	// 换一张图必须匹配不到 —— 2026-09-23 的「点谁都换不上去」就是匹配失败后
	// 静默回落第一张造成的，这里守住不放宽。
	primaryBase := "https://cdn.jsdelivr.net/gh/gfriends/gfriends@master/"
	other := GfriendEntry{Group: entry.Group, File: "other-2.jpg"}.URL(primaryBase)
	if _, ok := matchGfriendEntry([]GfriendEntry{entry}, other, bases); ok {
		t.Errorf("换一张图不应命中: %s", other)
	}
	// 未知基址 + 不存在的文件名：匹配不到。
	if _, ok := matchGfriendEntry([]GfriendEntry{entry},
		"https://evil.example.com/Content/8-GRAPHIS/nope-9.jpg", bases); ok {
		t.Error("不存在的文件名不应命中")
	}
}

// 客户端传来的 URL 只用来**定位是哪一张**，下载永远走服务端配置的基址 ——
// 否则这个字段就成了「让服务端去任意地址取图」的入口。
// 用匹配得上的外链 + 一个会返回不同图片的第三方服务器来验：上传的必须是 CDN 那一张，
// 且第三方服务器一次都不能被访问。
func TestAvatarSelectedFileNeverFetchedFromUserHost(t *testing.T) {
	m := newMockEmby(t)
	cdnImg := makeJPEG(t, 30, 40, avBlue)
	rogueImg := makeJPEG(t, 11, 11, avRed)
	cdn := gfCDNServer(t, map[string][]byte{"a.jpg": cdnImg})
	var rogueHits int32
	rogue := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&rogueHits, 1)
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(rogueImg)
	}))
	defer rogue.Close()

	app := testApp(t, m.srv.URL, "http://mt.invalid")
	useCDN(t, app, cdn.URL)
	entry := GfriendEntry{Group: "G1", File: "a.jpg"}
	injectGfIndex(app, entry)

	res, err := app.ScrapePersonAvatar(context.Background(), "p1", "渚このみ", AvatarOptions{
		Source: "gfriends", File: entry.URL(rogue.URL), Overwrite: true,
	})
	if err != nil {
		t.Fatalf("同名的外链应能定位到这张图：%v", err)
	}
	if res.Skipped || len(m.bodies) == 0 {
		t.Fatal("没有上传头像")
	}
	if !bytes.Equal(m.bodies[len(m.bodies)-1], cdnImg) {
		t.Error("上传的应是配置的 CDN 上的图，不是外链地址的图")
	}
	if n := atomic.LoadInt32(&rogueHits); n != 0 {
		t.Errorf("不应去访问客户端给的地址，实际 %d 次", n)
	}
}
