package main

import (
	"strings"
	"testing"
)

const starPageFixture = `<!DOCTYPE html>
<html><head><title>三上悠亜 - JavBus</title></head>
<body>
<div class="container">
  <div class="row movie-list">
    <div class="item">
      <a class="movie-box" href="https://www.javbus.com/SSIS-001">
        <div class="photo-frame"><img src="https://www.javbus.com/pics/cover/8q5t_b.jpg" title="SSIS-001"></div>
        <div class="photo-info"><span>SSIS-001<br><date>2021-01-01</date></span></div>
      </a>
    </div>
    <div class="item">
      <a class="movie-box" href="/SSIS-002">
        <div class="photo-frame"><img src="/pics/cover/aaaa_b.jpg" title="SSIS-002"></div>
        <div class="photo-info"><span>SSIS-002<br><date>2021-02-01</date></span></div>
      </a>
    </div>
    <div class="item">
      <a class="movie-box" href="https://www.javbus.com/SSIS-001">
        <div class="photo-frame"><img src="/pics/cover/8q5t_b.jpg" title="SSIS-001"></div>
        <div class="photo-info"><span>SSIS-001<br><date>2021-01-01</date></span></div>
      </a>
    </div>
  </div>
  <ul class="pagination">
    <li class="active"><a>1</a></li>
    <li><a id="next" href="/star/1v/2">下一頁</a></li>
  </ul>
</div>
</body></html>`

const starPageAltFixture = `<!DOCTYPE html>
<html><body>
<div class="movie-list">
  <div class="item"><a href="https://www.javbus.com/ABP-123">
    <div class="photo-frame"><img data-src="/pics/cover/x_b.jpg"></div>
    <div class="photo-info"><span>ABP-123<br>2020-05-05</span></div>
  </a></div>
</div>
<ul class="pagination"><li><a href="/star/9k/2">下一頁</a></li></ul>
</body></html>`

const magnetAjaxFixture = `<table class="table table-hover"><tbody>
<tr>
  <td><a class="btn btn-mini btn-primary" href="magnet:?xt=urn:btih:AAA111&amp;dn=SSIS-001-C">磁力鏈接</a>
      <a class="btn btn-mini btn-default" href="magnet:?xt=urn:btih:AAA111&amp;dn=SSIS-001-C">HD-1080p</a></td>
  <td>5.6GB</td>
  <td>2021-01-05</td>
</tr>
<tr>
  <td><a class="btn btn-mini btn-primary" href="magnet:?xt=urn:btih:BBB222&amp;dn=SSIS-001-4K">磁力鏈接</a>
      <a class="btn btn-mini btn-default" href="magnet:?xt=urn:btih:BBB222&amp;dn=SSIS-001-4K">4K</a></td>
  <td>12.3 GB</td>
  <td>2021-01-06</td>
</tr>
<tr>
  <td><a class="btn btn-mini btn-primary" href="magnet:?xt=urn:btih:AAA111&amp;dn=SSIS-001-C">磁力鏈接</a></td>
  <td>5.6GB</td>
  <td>2021-01-05</td>
</tr>
</tbody></table>`

func TestParseStarPage(t *testing.T) {
	movies, next := parseStarPage([]byte(starPageFixture), "https://www.javbus.com")
	if len(movies) != 2 {
		t.Fatalf("期望去重后 2 部作品，实际 %d", len(movies))
	}
	if movies[0].Number != "SSIS-001" {
		t.Errorf("番号错误: %q", movies[0].Number)
	}
	if movies[0].Date != "2021-01-01" {
		t.Errorf("日期错误: %q", movies[0].Date)
	}
	if movies[0].Cover != "https://www.javbus.com/pics/cover/8q5t_b.jpg" {
		t.Errorf("封面错误: %q", movies[0].Cover)
	}
	if movies[1].URL != "https://www.javbus.com/SSIS-002" {
		t.Errorf("相对链接未补全: %q", movies[1].URL)
	}
	if movies[1].Cover != "https://www.javbus.com/pics/cover/aaaa_b.jpg" {
		t.Errorf("相对封面未补全: %q", movies[1].Cover)
	}
	if next != "https://www.javbus.com/star/1v/2" {
		t.Errorf("下一页错误: %q", next)
	}
}

func TestParseStarPageFallback(t *testing.T) {
	movies, next := parseStarPage([]byte(starPageAltFixture), "https://www.javbus.com")
	if len(movies) != 1 {
		t.Fatalf("回退策略应解析出 1 部，实际 %d", len(movies))
	}
	if movies[0].Number != "ABP-123" {
		t.Errorf("番号错误: %q", movies[0].Number)
	}
	if movies[0].Date != "2020-05-05" {
		t.Errorf("无 <date> 标签时应从文本提取日期，实际 %q", movies[0].Date)
	}
	if movies[0].Cover != "https://www.javbus.com/pics/cover/x_b.jpg" {
		t.Errorf("data-src 封面未取到: %q", movies[0].Cover)
	}
	if next != "https://www.javbus.com/star/9k/2" {
		t.Errorf("按锚文本翻页失败: %q", next)
	}
}

func TestParseMagnets(t *testing.T) {
	mags := parseMagnets([]byte(magnetAjaxFixture))
	if len(mags) != 2 {
		t.Fatalf("期望 2 条去重磁力，实际 %d", len(mags))
	}
	if mags[0].Name != "HD-1080p" {
		t.Errorf("磁力名称错误: %q", mags[0].Name)
	}
	if mags[0].Size != "5.6GB" {
		t.Errorf("体积错误: %q", mags[0].Size)
	}
	if mags[0].Date != "2021-01-05" {
		t.Errorf("日期错误: %q", mags[0].Date)
	}
	if mags[1].Size != "12.3GB" {
		t.Errorf("带空格的体积应归一化: %q", mags[1].Size)
	}
}

func TestParseMagnetsEmpty(t *testing.T) {
	if got := parseMagnets([]byte("<table><tbody></tbody></table>")); len(got) != 0 {
		t.Errorf("空表应返回 0 条，实际 %d", len(got))
	}
	if got := parseMagnets([]byte("")); len(got) != 0 {
		t.Errorf("空输入应返回 0 条，实际 %d", len(got))
	}
}

// magnetAjaxBareFixture 是**线上真实返回的形态**：一串裸 <tr>，没有 <table> 包裹，
// 每行三个 <td>（名称 / 体积 / 日期），且三个 <td> 里的 <a> 指向同一条磁力。
//
// 这个夹具存在的意义：HTML5 树构造会把游离的 <tr> 丢弃，如果夹具写成
// <table>…</table> 就永远测不出这个 bug（曾经就是这样漏掉的）。
const magnetAjaxBareFixture = `       
            <tr onmouseover="this.style.backgroundColor='#F4F9FD';this.style.cursor='pointer';" onmouseout="this.style.backgroundColor='#FFFFFF'" height="35px" style=" border-top:#DDDDDD solid 1px">
                <td width="70%" onclick="window.open('magnet:?xt=urn:btih:8B7C886F6E3B409653CC21DFEEB67CBA8D98D754&amp;dn=SSNI-989','_self')">
                	<a style="color:#333" rel="nofollow" title="滑鼠右鍵點擊並選擇【複製連結網址】" href="magnet:?xt=urn:btih:8B7C886F6E3B409653CC21DFEEB67CBA8D98D754&amp;dn=SSNI-989">
                	SSNI-989                 	</a>
                </td>
                <td style="text-align:center;white-space:nowrap" onclick="window.open('magnet:?xt=urn:btih:8B7C886F6E3B409653CC21DFEEB67CBA8D98D754&amp;dn=SSNI-989','_self')">
                	<a style="color:#333" rel="nofollow" title="滑鼠右鍵點擊並選擇【複製連結網址】" href="magnet:?xt=urn:btih:8B7C886F6E3B409653CC21DFEEB67CBA8D98D754&amp;dn=SSNI-989">
                	1.83GB                	</a>
                </td>
                <td style="text-align:center;white-space:nowrap" onclick="window.open('magnet:?xt=urn:btih:8B7C886F6E3B409653CC21DFEEB67CBA8D98D754&amp;dn=SSNI-989','_self')">
                	<a style="color:#333" rel="nofollow" title="滑鼠右鍵點擊並選擇【複製連結網址】" href="magnet:?xt=urn:btih:8B7C886F6E3B409653CC21DFEEB67CBA8D98D754&amp;dn=SSNI-989">
                	2025-10-20                	</a>
                </td>            
            </tr>
            <tr height="35px">
                <td width="70%"><a href="magnet:?xt=urn:btih:7D383D0E5A119934972DF1EC5FFA22A9514C1C07&amp;dn=SSNI-989">SSNI-989</a></td>
                <td>2.57 GB</td>
                <td>2025-08-09</td>
            </tr>
            <tr height="35px">
                <td width="70%"><a href="magnet:?xt=urn:btih:8B7C886F6E3B409653CC21DFEEB67CBA8D98D754&amp;dn=SSNI-989">SSNI-989</a></td>
                <td>1.83GB</td>
                <td>2025-10-20</td>
            </tr>`

// TestParseMagnetsBareFragment 是线上那个 bug 的回归测试。
func TestParseMagnetsBareFragment(t *testing.T) {
	mags := parseMagnets([]byte(magnetAjaxBareFixture))
	if len(mags) != 2 {
		t.Fatalf("期望 2 条去重磁力（裸 <tr> 片段），实际 %d —— 大概率是没套 <table> 被 HTML 解析器丢了", len(mags))
	}
	if mags[0].Size != "1.83GB" {
		t.Errorf("体积错误: %q", mags[0].Size)
	}
	if mags[0].Date != "2025-10-20" {
		t.Errorf("日期错误: %q", mags[0].Date)
	}
	if mags[1].Size != "2.57GB" {
		t.Errorf("带空格的体积应归一化: %q", mags[1].Size)
	}
	if !strings.Contains(mags[0].Link, "8B7C886F6E3B409653CC21DFEEB67CBA8D98D754") {
		t.Errorf("磁力链接错误: %q", mags[0].Link)
	}
	// 真实页面上三个 <td> 的 <a> 文本都一样，名称取第一个即可，但必须有值
	if mags[0].Name == "" {
		t.Errorf("名称不应为空")
	}
}

// 没有 <table> 包裹也不该丢数据；有包裹时同样要能解析。
func TestParseMagnetsBothShapes(t *testing.T) {
	bare := parseMagnets([]byte(magnetAjaxBareFixture))
	wrapped := parseMagnets([]byte(magnetAjaxFixture))
	if len(bare) != 2 {
		t.Errorf("裸片段应解析出 2 条，实际 %d", len(bare))
	}
	if len(wrapped) != 2 {
		t.Errorf("表格包裹应解析出 2 条，实际 %d", len(wrapped))
	}
}

func TestParseStarPageEmpty(t *testing.T) {
	movies, next := parseStarPage([]byte("<html><body>没有内容</body></html>"), "https://www.javbus.com")
	if len(movies) != 0 || next != "" {
		t.Errorf("空页面应无结果，得到 %d 部 / next=%q", len(movies), next)
	}
}

func TestPathSegment(t *testing.T) {
	cases := map[string]string{
		"https://www.javbus.com/SSIS-001":     "SSIS-001",
		"https://www.javbus.com/SSIS-001?a=1": "SSIS-001",
		"/SSIS-002/":                          "SSIS-002",
		"/star/1v/2":                          "2",
		"":                                    "",
	}
	for in, want := range cases {
		if got := pathSegment(in); got != want {
			t.Errorf("pathSegment(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestMagnetDisplayName(t *testing.T) {
	got := magnetDisplayName("magnet:?xt=urn:btih:ABC&dn=SSIS-001-C%20HD")
	if got != "SSIS-001-C HD" {
		t.Errorf("dn 解析错误: %q", got)
	}
	if magnetDisplayName("not-a-magnet") != "" {
		t.Error("非磁力链接应返回空")
	}
}
