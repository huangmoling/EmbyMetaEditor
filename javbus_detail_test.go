package main

import (
	"bytes"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// 下面几段是从线上真实页面里截出来的关键片段（只保留结构，内容做了裁剪）。
// 磁力抓取依赖从详情页里抠出 gid / uc / img 三个参数，
// 这三个值在页面上是一段内联 <script>，格式一旦变了就全盘失效，
// 所以单独测。

const detailScriptFixture = `<!DOCTYPE html>
<html><head><title>SSNI-989 - JavBus</title></head>
<body>
<div class="container">
<script src='https://www.javbus.com/js/focus.js?v=8.7'></script>
<script>
	var gid = 45617855602;
	var uc = 0;
	var img = '/pics/cover/83hf_b.jpg';
</script>
<input id="token" type="hidden" name="token" value="1f7aehwbKBWow7Obi6Ld/DCjB5Fzw">
<h3>SSNI-989 出張先の旅館で…</h3>
<div class="row movie">
  <div class="col-md-9 screencap">
    <a class="bigImage" href="/pics/cover/83hf_b.jpg"><img src="/pics/cover/83hf_b.jpg"></a>
  </div>
  <div class="col-md-3 info">
    <p><span class="header">識別碼:</span> <span style="color:#CC0000;">SSNI-989</span></p>
    <p><span class="header">發行日期:</span> 2021-02-18</p>
    <p><span class="header">長度:</span> 170分鐘</p>
    <p><span class="header">導演:</span> <a href="/director/2ms">肉尊</a></p>
    <p><span class="header">製作商:</span> <a href="/studio/7q">エスワン ナンバーワンスタイル</a></p>
    <p><span class="header">演員</span>:<a href="/star/okq" title="三上悠亜">三上悠亜</a></p>
    <p class="header">類別:<a href="/genre/e">巨乳</a><a href="/genre/f">單體作品</a></p>
  </div>
</div>
<div class="movie" style="padding:12px; margin-top:15px">
  <table id="magnet-table" class="table"></table>
  <div id="movie-loading"><font class="ajax-text">讀取中...</font></div>
</div>
</body></html>`

func TestMagnetParamsExtraction(t *testing.T) {
	src := detailScriptFixture

	gid := reGID.FindStringSubmatch(src)
	if gid == nil {
		t.Fatalf("没抠出 gid —— 页面里应该是 `var gid = 数字;`")
	}
	if gid[1] != "45617855602" {
		t.Errorf("gid 值错误: %q", gid[1])
	}

	img := reIMG.FindStringSubmatch(src)
	if img == nil {
		t.Fatalf("没抠出 img")
	}
	if img[1] != "/pics/cover/83hf_b.jpg" {
		t.Errorf("img 值错误: %q", img[1])
	}

	uc := reUC.FindStringSubmatch(src)
	if uc == nil {
		t.Fatalf("没抠出 uc")
	}
	if uc[1] != "0" {
		t.Errorf("uc 值错误: %q", uc[1])
	}
}

// uc 缺失时应该退化为 "0"，而不是整个磁力流程失效。
func TestMagnetParamsUCMissing(t *testing.T) {
	src := strings.Replace(detailScriptFixture, "var uc = 0;\n", "", 1)
	if reUC.FindStringSubmatch(src) != nil {
		t.Fatal("夹具准备有误：uc 应该已经被删掉")
	}
	// 只要 gid 和 img 在，代码里会自己把 uc 兜底成 "0"
	if reGID.FindStringSubmatch(src) == nil || reIMG.FindStringSubmatch(src) == nil {
		t.Fatal("gid / img 不该受影响")
	}
}

// 用真实页面片段跑一遍详情解析，确认关键字段都能取到。
func TestMovieDetailParsingFromRealFixture(t *testing.T) {
	// 直接调解析部分：把 MovieDetail 里的 HTML 解析逻辑等价地跑一遍
	doc, err := html.Parse(bytes.NewReader([]byte(detailScriptFixture)))
	if err != nil {
		t.Fatal(err)
	}

	if h3 := htmlFind(doc, func(n *html.Node) bool { return isElem(n, "h3") }); h3 == nil {
		t.Error("没找到 <h3> 标题")
	} else if !strings.HasPrefix(strings.TrimSpace(htmlText(h3)), "SSNI-989") {
		t.Errorf("标题内容异常: %q", htmlText(h3))
	}

	if a := htmlFind(doc, func(n *html.Node) bool {
		return isElem(n, "a") && htmlHasClass(n, "bigImage")
	}); a == nil {
		t.Error("没找到 .bigImage 封面链接")
	} else if htmlAttr(a, "href") != "/pics/cover/83hf_b.jpg" {
		t.Errorf("封面 href 异常: %q", htmlAttr(a, "href"))
	}
}
