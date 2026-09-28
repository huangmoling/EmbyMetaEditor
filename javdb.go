package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// javdb.com 的磁力源。
//
// 为什么它是「第二个源」而不是「替代 javbus」：两家收录的种子不一样。
// javbus 上的磁力多是原盘/无码破解，javdb 上的中字与高清版本更全，两者
// 经常互补。用户真实诉求是「这个番号能拿到的最大那个种子」，所以两个都问、
// 合并去重、按体积排。
//
// 两个实操坑（都是实测出来的，别按直觉改）：
//
//  1. **限频是硬限制**。短时间内连发几个请求，站点直接返回 403 加一句
//     「操作過於頻繁，請等一会再試」—— 它不是 Cloudflare 挑战，重试也没用，
//     只能等。所以请求必须走 rateLimiter，而且要**全局共用一个**
//     （一个番号一次抓取会发两个请求：搜索 + 详情）。
//  2. **体积的权威来源是 `data-size`**（单位 MB 的整数），不是页面上那行
//     「6.33GB, 1個文件」。后者是给人看的、可能被截断；用来排序会得到
//     和站点自己的「按大小排序」不一致的顺序。夹具见 testdata/javdb/。
type JavDB struct {
	BaseURL string
	Cookie  string
	HTTP    *http.Client
	rl      *rateLimiter
}

// NewJavDB 依据配置构造客户端。
func NewJavDB(cfg Config) *JavDB {
	return &JavDB{
		BaseURL: strings.TrimRight(strings.TrimSpace(cfg.JavDBURL), "/"),
		Cookie:  cfg.JavDBCookie,
		HTTP:    newHTTPClient(cfg),
		rl:      &rateLimiter{interval: time.Duration(cfg.JavBusInterval) * time.Millisecond},
	}
}

// JavDBHit 是 javdb 搜索结果里的一条。
type JavDBHit struct {
	Code  string `json:"code"`
	Title string `json:"title"`
	Date  string `json:"date"`
	Cover string `json:"cover"`
	URL   string `json:"url"`
}

// doRaw 发起一次带限速的 GET。
func (d *JavDB) doRaw(ctx context.Context, rawURL string) ([]byte, int, error) {
	if d.BaseURL == "" {
		return nil, 0, fmt.Errorf("未配置 javdb 地址")
	}
	if err := d.rl.wait(ctx); err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,ja;q=0.8")
	req.Header.Set("Referer", d.BaseURL+"/")
	if d.Cookie != "" {
		req.Header.Set("Cookie", d.Cookie)
	}
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := readAllLimit(resp.Body, 16<<20)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return data, resp.StatusCode, nil
}

// get 在 doRaw 之上加状态码判断与重试。
func (d *JavDB) get(ctx context.Context, rawURL string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		data, status, err := d.doRaw(ctx, rawURL)
		if err != nil {
			lastErr = err
			time.Sleep(time.Duration(attempt+1) * time.Second)
			continue
		}
		// 「操作過於頻繁」是 javdb 自己的限频提示，不是 Cloudflare —— 只能等。
		if bytes.Contains(data, []byte("操作過於頻繁")) || bytes.Contains(data, []byte("操作过于频繁")) {
			lastErr = fmt.Errorf("javdb 提示「操作过于频繁」：已自动放慢重试，若一直如此请把设置里的请求间隔调大")
			time.Sleep(time.Duration(attempt+1) * 3 * time.Second)
			continue
		}
		if status == 403 || status == 503 {
			if bytes.Contains(data, []byte("cf-browser-verification")) || bytes.Contains(data, []byte("Just a moment")) ||
				bytes.Contains(data, []byte("请稍候")) {
				return nil, fmt.Errorf("被 Cloudflare 拦截，请在设置里填入浏览器 Cookie（含 over18=1 与 cf_clearance）后重试")
			}
			lastErr = fmt.Errorf("返回 %d", status)
			time.Sleep(time.Duration(attempt+1) * 2 * time.Second)
			continue
		}
		if status == 404 {
			return nil, fmt.Errorf("页面不存在（404）")
		}
		if status >= 400 {
			lastErr = fmt.Errorf("返回 %d", status)
			continue
		}
		if len(data) == 0 {
			lastErr = fmt.Errorf("返回 200 但响应体为空")
			time.Sleep(time.Duration(attempt+1) * time.Second)
			continue
		}
		return data, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("请求失败")
	}
	return nil, fmt.Errorf("访问 javdb 失败：%w", lastErr)
}

// Search 按番号（或关键词）搜索，返回结果列表。
func (d *JavDB) Search(ctx context.Context, keyword string) ([]JavDBHit, error) {
	u := d.BaseURL + "/search?f=all&q=" + url.QueryEscape(strings.TrimSpace(keyword))
	data, err := d.get(ctx, u)
	if err != nil {
		return nil, err
	}
	return parseJavDBSearch(data, d.BaseURL), nil
}

// Magnets 给一个番号找磁力，返回磁力列表与详情页标题。
func (d *JavDB) Magnets(ctx context.Context, number string) ([]JBMagnet, string, error) {
	hits, err := d.Search(ctx, number)
	if err != nil {
		return nil, "", err
	}
	hit := pickJavDBHit(hits, number)
	if hit == nil {
		return nil, "", fmt.Errorf("javdb 上没有找到 %s", number)
	}
	data, err := d.get(ctx, hit.URL)
	if err != nil {
		return nil, hit.Title, err
	}
	mags := parseJavDBMagnets(data)
	title := firstNonEmpty(parseJavDBDetailTitle(data), hit.Title)
	if len(mags) == 0 {
		return nil, title, fmt.Errorf("javdb 该条目暂无磁力链接")
	}
	return mags, title, nil
}

// Probe 探测 javdb 是否可达、页面结构是否符合预期。
func (d *JavDB) Probe(ctx context.Context) MagnetSourceStatus {
	st := MagnetSourceStatus{Key: magnetSourceJavDB, Name: magnetSourceLabel(magnetSourceJavDB)}
	if d.BaseURL == "" {
		st.Error = "未配置 javdb 地址"
		return st
	}
	start := time.Now()
	data, status, err := d.doRaw(ctx, d.BaseURL+"/")
	st.ElapsedMS = time.Since(start).Milliseconds()
	if err != nil {
		st.Error = "无法连接：" + err.Error()
		return st
	}
	st.HTTPStatus = status
	if bytes.Contains(data, []byte("操作過於頻繁")) {
		st.Error = "被限频：javdb 提示「操作过于频繁」，降低请求频率或稍后再试"
		return st
	}
	if status == 403 || status == 503 {
		st.Error = "被 Cloudflare 拦住（HTTP " + strconv.Itoa(status) + "），请在设置里填入浏览器 Cookie"
		return st
	}
	if status >= 400 {
		st.Error = "站点返回 HTTP " + strconv.Itoa(status)
		return st
	}
	if len(data) == 0 {
		st.Error = "HTTP " + strconv.Itoa(status) + " 但响应体为空"
		return st
	}
	st.OK = true
	return st
}

// parseJavDBSearch 解析搜索结果页。
//
// 结构（真夹具 testdata/javdb/search_*.html）：
//
//	<div class="movie-list …">
//	  <div class="item">
//	    <a href="/v/ZY5eq" class="box" title="…">
//	      <div class="cover"><img src="https://c0.jdbstatic.com/covers/zy/ZY5eq.jpg"></div>
//	      <div class="video-title"><strong>SSIS-001</strong> 标题…</div>
//	      <div class="meta">2021-02-19</div>
//	    </a>
//	  </div>
//	  …
//
// **番号在 `<strong>` 里**，标题是同一个 div 里剩下的文本 —— 只取整个
// video-title 的文本会把番号重复进标题。
func parseJavDBSearch(data []byte, base string) []JavDBHit {
	doc, err := html.Parse(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	var out []JavDBHit
	for _, a := range htmlFindAll(doc, func(n *html.Node) bool {
		return isElem(n, "a") && htmlHasClass(n, "box") && strings.HasPrefix(htmlAttr(n, "href"), "/v/")
	}) {
		href := htmlAttr(a, "href")
		full := href
		if !strings.HasPrefix(full, "http") {
			full = base + "/" + strings.TrimLeft(full, "/")
		}
		h := JavDBHit{URL: full}
		if t := htmlFind(a, func(n *html.Node) bool {
			return isElem(n, "div") && htmlHasClass(n, "video-title")
		}); t != nil {
			if s := htmlFind(t, func(n *html.Node) bool { return isElem(n, "strong") }); s != nil {
				h.Code = strings.TrimSpace(htmlText(s))
			}
			h.Title = strings.TrimSpace(htmlText(t))
			if h.Code != "" {
				h.Title = strings.TrimSpace(strings.TrimPrefix(h.Title, h.Code))
			}
		}
		if h.Code == "" {
			h.Code = canonNumber(h.Title)
		}
		if m := htmlFind(a, func(n *html.Node) bool {
			return isElem(n, "div") && htmlHasClass(n, "meta")
		}); m != nil {
			h.Date = reDate.FindString(htmlText(m))
		}
		if img := htmlFind(a, func(n *html.Node) bool { return isElem(n, "img") }); img != nil {
			h.Cover = strings.TrimSpace(htmlAttr(img, "src"))
		}
		if h.Code == "" && h.Title == "" {
			continue
		}
		out = append(out, h)
	}
	return out
}

// pickJavDBHit 从搜索结果里挑出和番号精确对应的那一条。
//
// javdb 的搜索是模糊的：搜 SSIS-001 会带出 SSIS-0014、SSIS-0015 这些。
// 拿错条目的代价是「磁力列表看着有，但全是别的片子的种子」——
// 比没有结果严重得多，所以这里宁可不匹配（返回 nil）也不猜。
func pickJavDBHit(hits []JavDBHit, number string) *JavDBHit {
	want := canonNumber(number)
	if want == "" {
		return nil
	}
	norm := func(s string) string { return canonNumber(s) }
	for i := range hits {
		if norm(hits[i].Code) == want {
			return &hits[i]
		}
	}
	// 番号写法的细微差异（大小写、连字符）再给一次机会。
	loose := func(s string) string {
		var b strings.Builder
		for _, r := range strings.ToLower(s) {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
				b.WriteRune(r)
			}
		}
		return b.String()
	}
	w := loose(want)
	for i := range hits {
		if loose(hits[i].Code) == w {
			return &hits[i]
		}
	}
	return nil
}

// parseJavDBDetailTitle 从详情页取标题。结构：
//
//	<h2 class="title is-4">
//	  <strong>SSIS-001 </strong>
//	  <strong class="current-title">…标题…</strong>
//	</h2>
func parseJavDBDetailTitle(data []byte) string {
	doc, err := html.Parse(bytes.NewReader(data))
	if err != nil {
		return ""
	}
	h2 := htmlFind(doc, func(n *html.Node) bool {
		return isElem(n, "h2") && htmlHasClass(n, "title")
	})
	if h2 == nil {
		return ""
	}
	if s := htmlFind(h2, func(n *html.Node) bool {
		return isElem(n, "strong") && htmlHasClass(n, "current-title")
	}); s != nil {
		return strings.TrimSpace(htmlText(s))
	}
	return strings.TrimSpace(htmlText(h2))
}

// parseJavDBMagnets 解析详情页的磁力区块。
//
// 结构（真夹具 testdata/javdb/detail_*_magnets.html，外层是
// `<div id="magnets-content" class="magnet-links">`）：
//
//	<div class="item odd" data-rank="0" data-size="6480" data-files="1" data-date="20231118">
//	  <div class="magnet-name">
//	    <a href="magnet:?xt=urn:btih:…&amp;dn=…">
//	      <span class="name">SSIS-001-UC.torrent.无码破解</span>
//	      <div class="tags"><span class="tag …">高清</span><span class="tag …">字幕</span></div>
//	      <span class="meta">6.33GB, 1個文件</span>
//	    </a>
//	  </div>
//	  <div class="date"><span class="time">2023-11-18</span></div>
//	</div>
//
// 命中条件是**条目容器带 `data-size`**，不是某个 class 名 —— 站点改版常换
// class，data-* 属性是它自己排序用的，动得少得多。万一哪天真没了，还有一条
// 「直接收所有 magnet: 链接」的回退路径兜着。
func parseJavDBMagnets(data []byte) []JBMagnet {
	doc, err := html.Parse(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	var out []JBMagnet
	seen := map[string]bool{}
	appendMagnet := func(link, name, size, date string) {
		link = strings.TrimSpace(strings.ReplaceAll(link, "\n", ""))
		if link == "" {
			return
		}
		key := magnetKey(link)
		if seen[key] {
			return
		}
		seen[key] = true
		m := JBMagnet{Link: link, Name: strings.TrimSpace(name), Size: strings.Join(strings.Fields(size), ""), Date: date}
		if m.Name == "" {
			m.Name = magnetDisplayName(link)
		}
		if m.Name == "" {
			m.Name = "磁力链接"
		}
		out = append(out, m)
	}

	boxes := htmlFindAll(doc, func(n *html.Node) bool { return htmlAttr(n, "data-size") != "" })
	for _, box := range boxes {
		a := htmlFind(box, func(n *html.Node) bool {
			return isElem(n, "a") && strings.HasPrefix(htmlAttr(n, "href"), "magnet:")
		})
		if a == nil {
			continue
		}
		name := ""
		if sp := htmlFind(a, func(n *html.Node) bool {
			return isElem(n, "span") && htmlHasClass(n, "name")
		}); sp != nil {
			name = htmlText(sp)
		}
		// 体积：优先 data-size（MB 整数），回退页面上那行「6.33GB, 1個文件」。
		size := ""
		if mb, err := strconv.Atoi(strings.TrimSpace(htmlAttr(box, "data-size"))); err == nil && mb > 0 {
			size = formatSizeMB(mb)
		} else if sp := htmlFind(a, func(n *html.Node) bool {
			return isElem(n, "span") && htmlHasClass(n, "meta")
		}); sp != nil {
			size = reSize.FindString(htmlText(sp))
		}
		// 日期：优先 data-date（20231118），回退 div.date 里的 YYYY-MM-DD。
		date := ""
		if dd := strings.TrimSpace(htmlAttr(box, "data-date")); len(dd) == 8 {
			date = dd[:4] + "-" + dd[4:6] + "-" + dd[6:]
		} else {
			date = reDate.FindString(htmlText(box))
		}
		appendMagnet(htmlAttr(a, "href"), name, size, date)
	}
	if len(out) > 0 {
		return out
	}
	// 回退：页面里没有 data-size 了（改版），按 magnet: 链接硬收一遍。
	for _, a := range htmlFindAll(doc, func(n *html.Node) bool {
		return isElem(n, "a") && strings.HasPrefix(htmlAttr(n, "href"), "magnet:")
	}) {
		row := a
		for i := 0; i < 3 && row.Parent != nil; i++ {
			row = row.Parent
			if len(htmlFindAll(row, func(n *html.Node) bool {
				return isElem(n, "a") && strings.HasPrefix(htmlAttr(n, "href"), "magnet:")
			})) > 1 {
				row = a
				break
			}
		}
		appendMagnet(htmlAttr(a, "href"), htmlText(a), reSize.FindString(htmlText(row)), reDate.FindString(htmlText(row)))
	}
	return out
}

// formatSizeMB 把 javdb 的 data-size（MB 整数）写成和 javbus 一样好认的串，
// 保证 magnetSizeBytes 能解析、排序不会把 GB 当成 MB 级。
func formatSizeMB(mb int) string {
	if mb >= 1024 {
		return fmt.Sprintf("%.2fGB", float64(mb)/1024)
	}
	return fmt.Sprintf("%dMB", mb)
}
