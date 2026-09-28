package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// errTestSourceDown 代表某个源整体不可用（网络、被拦、限频）。
var errTestSourceDown = errors.New("连接超时")

// 夹具是**从 javdb 真实响应里逐字节截出来的**（tools/fetch_javdb_fixture.py），
// 不是手写的。手写夹具必然会把「6.33GB」当成体积的权威来源，而实际排序依据是
// `data-size` 属性（单位 MB）—— 这类错只有真页面能测出来。
func javdbFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "javdb", name))
	if err != nil {
		t.Fatalf("读夹具失败：%v", err)
	}
	return b
}

// 搜索结果解析：番号在 <strong> 里、标题是同一个 div 剩下的文本。
func TestParseJavDBSearchRealFixture(t *testing.T) {
	data := javdbFixture(t, "search_ssis001.html")
	hits := parseJavDBSearch(data, "https://javdb.com")
	if len(hits) < 10 {
		t.Fatalf("夹具应解析出十几条搜索结果，实际 %d", len(hits))
	}
	var hit *JavDBHit
	for i := range hits {
		if canonNumber(hits[i].Code) == canonNumber("SSIS-001") {
			hit = &hits[i]
			break
		}
	}
	if hit == nil {
		t.Fatalf("没解析出 SSIS-001 这条；前几条是 %v", codesOf(hits))
	}
	if hit.URL != "https://javdb.com/v/ZY5eq" {
		t.Errorf("详情页地址 = %q", hit.URL)
	}
	if hit.Date != "2021-02-19" {
		t.Errorf("日期 = %q，期望 2021-02-19", hit.Date)
	}
	if strings.Contains(hit.Title, "SSIS-001") {
		t.Errorf("标题里不该还带着番号（番号在 <strong> 里，要摘掉）：%q", hit.Title)
	}
	if hit.Title == "" {
		t.Error("标题不该为空")
	}
	if !strings.Contains(hit.Cover, "jdbstatic.com") {
		t.Errorf("封面地址 = %q", hit.Cover)
	}
}

func codesOf(hits []JavDBHit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.Code)
	}
	return out
}

// 搜索是模糊的：搜 SSIS-001 会带出 SSIS-0014 这些。**必须只认精确匹配**，
// 拿错条目等于把别的片子的种子当成这个番号的列出来。
func TestPickJavDBHitRequiresExactNumber(t *testing.T) {
	hits := []JavDBHit{
		{Code: "SSIS-0014", URL: "https://javdb.com/v/aaaaa"},
		{Code: "SSIS-0015", URL: "https://javdb.com/v/bbbbb"},
	}
	if got := pickJavDBHit(hits, "SSIS-001"); got != nil {
		t.Errorf("不该匹配到 %q —— 这是模糊搜索带出来的别的番号", got.Code)
	}
	hits = append([]JavDBHit{{Code: "SSIS-001", URL: "https://javdb.com/v/ZY5eq"}}, hits...)
	if got := pickJavDBHit(hits, "ssis-001"); got == nil || got.URL != "https://javdb.com/v/ZY5eq" {
		t.Errorf("大小写不同也应该匹配到精确的那条，实际 %+v", got)
	}
	// 番号写法差异（少横线）也要认
	if got := pickJavDBHit([]JavDBHit{{Code: "SSIS001", URL: "x"}}, "SSIS-001"); got == nil {
		t.Error("SSIS001 应该被当成 SSIS-001")
	}
	if got := pickJavDBHit(hits, ""); got != nil {
		t.Error("空番号不该匹配任何东西")
	}
}

// 磁力解析：体积必须取 data-size（MB 整数），不是页面上那行「6.33GB, 1個文件」。
func TestParseJavDBMagnetsRealFixture(t *testing.T) {
	data := javdbFixture(t, "detail_ssis001_magnets.html")
	mags := parseJavDBMagnets(data)
	if len(mags) != 26 {
		t.Fatalf("夹具里有 26 条磁力，实际解析出 %d 条", len(mags))
	}
	for _, m := range mags {
		if !strings.HasPrefix(m.Link, "magnet:?xt=urn:btih:") {
			t.Fatalf("不是磁力链接：%q", m.Link)
		}
		if m.Name == "" {
			t.Fatalf("名称为空：%q", m.Link)
		}
		if magnetSizeBytes(m.Size) <= 0 {
			t.Fatalf("体积解析不出：%q（data-size 是 MB 整数，格式化后必须能被 magnetSizeBytes 认出来）", m.Size)
		}
	}
	// 第一条：data-size="6480" → 6.33GB；页面上的 meta 写的就是 6.33GB。
	if mags[0].Size != "6.33GB" {
		t.Errorf("第一条体积 = %q，期望 6.33GB（data-size=6480 MB）", mags[0].Size)
	}
	if mags[0].Date != "2023-11-18" {
		t.Errorf("第一条日期 = %q，期望 2023-11-18（data-date=20231118）", mags[0].Date)
	}
	if !strings.Contains(mags[0].Name, "SSIS-001") {
		t.Errorf("第一条名称 = %q", mags[0].Name)
	}
}

// 属性里的 `&amp;` 必须被解成 `&`，否则发给下载器的链接是坏的。
func TestParseJavDBMagnetsUnescapesLink(t *testing.T) {
	data := javdbFixture(t, "detail_ssis001_magnets.html")
	mags := parseJavDBMagnets(data)
	if strings.Contains(mags[0].Link, "&amp;") {
		t.Errorf("链接里的 &amp; 没有被还原：%q", mags[0].Link)
	}
	if !strings.Contains(mags[0].Link, "&dn=") {
		t.Errorf("链接缺少 dn 参数：%q", mags[0].Link)
	}
}

// data-size 没了（改版）时的回退路径：从 span.meta 里抠体积。
func TestParseJavDBMagnetsFallbackWithoutDataSize(t *testing.T) {
	html := `<div class="magnet-links">
	  <div class="item">
	    <div class="magnet-name">
	      <a href="magnet:?xt=urn:btih:AAAA&amp;dn=x">
	        <span class="name">ABC-001-1080p</span>
	        <span class="meta">2.57GB, 1個文件</span>
	      </a>
	    </div>
	    <div class="date"><span class="time">2024-01-02</span></div>
	  </div>
	</div>`
	mags := parseJavDBMagnets([]byte(html))
	if len(mags) != 1 {
		t.Fatalf("应解析出 1 条，实际 %d", len(mags))
	}
	if mags[0].Size != "2.57GB" {
		t.Errorf("体积 = %q，期望从 span.meta 兜底的 2.57GB", mags[0].Size)
	}
	if mags[0].Date != "2024-01-02" {
		t.Errorf("日期 = %q", mags[0].Date)
	}
}

// 去重键必须按 btih：同一个种子在不同站点上 tracker 参数不一样，
// 按整串比较会把「同一个种子」当成两条。
func TestMagnetKeyUsesBtih(t *testing.T) {
	a := "magnet:?xt=urn:btih:ABCDEF01&dn=x&tr=http://t1/announce"
	b := "magnet:?xt=urn:btih:abcdef01&dn=y&tr=udp://t2:1337/announce"
	if magnetKey(a) != magnetKey(b) {
		t.Errorf("同一个 btih 的大小写/参数差异应视为同一条：%q vs %q", magnetKey(a), magnetKey(b))
	}
	c := "magnet:?xt=urn:btih:98765432&dn=x"
	if magnetKey(a) == magnetKey(c) {
		t.Error("不同 btih 不该被当成同一条")
	}
}

// mergeMagnets：去重 + 按体积倒序，且保留先出现的那个（源顺序是固定的）。
func TestMergeMagnetsDedupsAndSorts(t *testing.T) {
	javbus := []JBMagnet{
		{Link: "magnet:?xt=urn:btih:AAA&dn=small", Name: "A", Size: "700MB", Source: magnetSourceJavBus},
		{Link: "magnet:?xt=urn:btih:CCC&dn=big", Name: "C", Size: "12.3GB", Source: magnetSourceJavBus},
	}
	javdb := []JBMagnet{
		// 与 javbus 的 CCC 是同一个种子，只是 tracker 参数不同
		{Link: "magnet:?xt=urn:btih:ccc&dn=big&tr=http://x/announce", Name: "C2", Size: "12.30GB", Source: magnetSourceJavDB},
		{Link: "magnet:?xt=urn:btih:DDD&dn=mid", Name: "D", Size: "2.57GB", Source: magnetSourceJavDB},
		{Link: "magnet:?xt=urn:btih:EEE&dn=unknown", Name: "E", Size: "", Source: magnetSourceJavDB},
	}
	out := mergeMagnets(javbus, javdb)
	names := make([]string, 0, len(out))
	for _, m := range out {
		names = append(names, m.Name)
	}
	want := []string{"C", "D", "A", "E"}
	if len(out) != len(want) {
		t.Fatalf("合并后应有 %d 条（同一个种子去重），实际 %d：%v", len(want), len(out), names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("顺序 = %v，期望 %v", names, want)
		}
	}
	// 去重保留先出现的（javbus 那条），源标签也要跟着
	if out[0].Source != magnetSourceJavBus {
		t.Errorf("重复项应保留先出现的源，实际 %q", out[0].Source)
	}
}

// 多源并发抓取：两个源同时问、去重、某个源挂了不影响另一个。
//
// 这条专门盯「helper 测得很足但没接上」这一类错：mergeMagnets 自己有测试，
// 但如果 fetchOneMagnetTarget 直接把各源结果 append 起来返回，单测照样全绿，
// 而界面上同一个种子会出现两遍。
func TestFetchOneMagnetTargetDedupsAcrossSources(t *testing.T) {
	same := "magnet:?xt=urn:btih:ABCDEF01&dn=x"
	fetchers := []magnetFetcher{
		{key: magnetSourceJavBus, fetch: func(context.Context, string) ([]JBMagnet, string, error) {
			return []JBMagnet{{Link: same + "&tr=a", Name: "javbus 版", Size: "1.83GB", Source: magnetSourceJavBus}}, "标题 A", nil
		}},
		{key: magnetSourceJavDB, fetch: func(context.Context, string) ([]JBMagnet, string, error) {
			return []JBMagnet{
				{Link: same + "&tr=b", Name: "javdb 版", Size: "1.83GB", Source: magnetSourceJavDB},
				{Link: "magnet:?xt=urn:btih:99887766&dn=y", Name: "独有的", Size: "5.00GB", Source: magnetSourceJavDB},
			}, "标题 B", nil
		}},
	}
	mags, title, statuses := fetchOneMagnetTarget(context.Background(), fetchers, "SSIS-001")
	if len(mags) != 2 {
		t.Fatalf("同一个种子应去重，实际 %d 条：%+v", len(mags), mags)
	}
	if mags[0].Name != "独有的" {
		t.Errorf("应按体积倒序，实际第一条 %q", mags[0].Name)
	}
	if title != "标题 A" {
		t.Errorf("标题应取第一个非空（源顺序固定），实际 %q", title)
	}
	if len(statuses) != 2 || !statuses[0].OK || !statuses[1].OK {
		t.Errorf("两个源都成功时应都是 OK：%+v", statuses)
	}
	if statuses[1].Count != 2 {
		t.Errorf("javdb 那个源自己返回了 2 条，Count 应记 2，实际 %d", statuses[1].Count)
	}
}

// 一个源挂了不能拖垮另一个。
func TestFetchOneMagnetTargetOneSourceFails(t *testing.T) {
	fetchers := []magnetFetcher{
		{key: magnetSourceJavBus, fetch: func(context.Context, string) ([]JBMagnet, string, error) {
			return nil, "", errTestSourceDown
		}},
		{key: magnetSourceJavDB, fetch: func(context.Context, string) ([]JBMagnet, string, error) {
			return []JBMagnet{{Link: "magnet:?xt=urn:btih:11223344", Name: "还有", Size: "2.57GB"}}, "t", nil
		}},
	}
	mags, _, statuses := fetchOneMagnetTarget(context.Background(), fetchers, "ABC-001")
	if len(mags) != 1 {
		t.Fatalf("javbus 挂了不该影响 javdb，实际 %d 条", len(mags))
	}
	if statuses[0].OK || statuses[0].Error == "" {
		t.Errorf("失败的那个源要带上原因：%+v", statuses[0])
	}
	if !statuses[1].OK {
		t.Errorf("成功的那个源要标 OK：%+v", statuses[1])
	}
	// 结果里要能看出「有个源挂了」，否则用户以为站点只收录了这么多
	out := magnetResultMerged("ABC-001", "", "ABC-001", mags, statuses)
	if !strings.Contains(out.Note, "失败") {
		t.Errorf("来源小结应提到某源失败，实际 %q", out.Note)
	}
}

// enableMagnetSources：勾选 ∩ 有地址。
func TestEnabledMagnetSources(t *testing.T) {
	base := DefaultConfig()
	base.JavDBURL = ""
	base.MagnetSources = nil
	if got := enabledMagnetSources(base); len(got) != 1 || got[0] != magnetSourceJavBus {
		t.Errorf("没配 javdb_url 时只应有 javbus，实际 %v", got)
	}

	cfg := DefaultConfig()
	got := enabledMagnetSources(cfg)
	if len(got) != 2 || got[0] != magnetSourceJavBus || got[1] != magnetSourceJavDB {
		t.Errorf("默认应启用两个源，实际 %v", got)
	}

	cfg.MagnetSources = []string{magnetSourceJavDB}
	if got := enabledMagnetSources(cfg); len(got) != 1 || got[0] != magnetSourceJavDB {
		t.Errorf("只勾 javdb 时不该还有 javbus，实际 %v", got)
	}

	cfg.MagnetSources = []string{}
	if got := enabledMagnetSources(cfg); len(got) != 0 {
		t.Errorf("显式清空应一个都不启用，实际 %v", got)
	}
}

// 配置里 nil 与 [] 的语义不同，别在 normalize 里把它们合并掉。
func TestMagnetSourcesNilVsEmpty(t *testing.T) {
	fresh := DefaultConfig()
	if len(fresh.MagnetSources) != 2 {
		t.Fatalf("默认配置应带两个源，实际 %v", fresh.MagnetSources)
	}
	c := Config{JavDBURL: "https://javdb.com"}
	c.normalize()
	if len(c.MagnetSources) != 2 {
		t.Errorf("nil（老配置没这个键）应落默认值，实际 %v", c.MagnetSources)
	}
	c2 := Config{MagnetSources: []string{}}
	c2.normalize()
	if len(c2.MagnetSources) != 0 {
		t.Errorf("显式空数组应保持为空（表示一个都不用），实际 %v", c2.MagnetSources)
	}
}
