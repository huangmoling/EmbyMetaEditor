package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"
)

// 国产传媒专项刮削。
//
// 四个站点各写一个适配器，输出统一成 CNResult，再按站点优先级合并字段。
//
// 实测形态（2026-09-22 逐个抓包确认，**别照文档或印象猜**）：
//
//   - xchina.co  —— 搜索结果**在服务端渲染的 HTML 里**，是四家里质量最高的：
//     番号 / 标题 / 封面 / 分类标签一次拿全，标题还能和用户库里的条目对上。
//     但详情页 /video/id-xxx.html 会被 Cloudflare 403，所以只用搜索页。
//   - madouqu.com —— WordPress，`?s=` 搜索**是模糊匹配**：搜 `91CM-014` 会返回
//     `91CM074` / `91CM084` / `91CM094`。**必须按番号精确比对过滤**，
//     否则会把别的作品的封面写到当前条目上。
//   - madou.club  —— 同样是 WordPress，但它的库不含 91CM 系列（搜出来 0 条是正常的，
//     不是抓取失败）。它主打 `MDHG0010` 这类自家番号。
//   - 7mmtv.sx    —— 日系聚合站，国产番号基本搜不到，命中率低但保留（用户点名要）。
//     搜索走 `/{type}_search/all/{kw}/1.html` 的 GET 形式，等价于站内 POST 表单。
//
// 封面图**四家都没有 Referer 防盗链**（实测无 Referer / 带本机 Referer 均 200），
// 所以不需要加进 imageCDNHosts —— 前端 imgSrc 会 302 回原址让浏览器直连。

const (
	// cnRequestInterval 是同一站点两次请求之间的最小间隔。
	// 这几家都是小站，跑批量时别把人家打挂。
	cnRequestInterval = 700 * time.Millisecond
	cnUserAgent       = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
)

// CNSiteMeta 描述一个国产传媒站点。
type CNSiteMeta struct {
	Key  string
	Name string
}

// cnSiteOrder 是站点优先级：封面 / 标题 / 日期 / 标签按这个顺序取**第一个命中**的。
var cnSiteOrder = []CNSiteMeta{
	{Key: "xchina", Name: "xChina"},
	{Key: "madouqu", Name: "麻豆区"},
	{Key: "madou", Name: "麻豆社"},
	{Key: "7mmtv", Name: "7mmtv"},
}

// defaultCNSites 是四个站点的默认地址。用户可在设置里改成镜像域名。
func defaultCNSites() map[string]string {
	return map[string]string{
		"xchina":  "https://xchina.co",
		"madouqu": "https://madouqu.com",
		"madou":   "https://madou.club",
		"7mmtv":   "https://7mmtv.sx",
	}
}

// cnSiteName 返回站点的展示名。
func cnSiteName(key string) string {
	for _, m := range cnSiteOrder {
		if m.Key == key {
			return m.Name
		}
	}
	return key
}

// CNResult 是一个站点上的一条搜索结果。
type CNResult struct {
	Site   string   `json:"site"`
	Name   string   `json:"site_name"`
	Number string   `json:"number"`
	Title  string   `json:"title"`
	Cover  string   `json:"cover"`
	Tags   []string `json:"tags"`
	Date   string   `json:"date"`
	URL    string   `json:"url"`
	// Exact 表示这条结果的番号与搜索词是同一个。
	// 前端只展示精确命中的，模糊命中的用来告诉用户「这家搜到了但番号对不上」。
	Exact bool `json:"exact"`
}

// CNSiteHits 是一个站点的搜索输出（含失败原因，失败也要回给前端看）。
type CNSiteHits struct {
	Site  string     `json:"site"`
	Name  string     `json:"site_name"`
	URL   string     `json:"url"`
	OK    bool       `json:"ok"`
	Error string     `json:"error,omitempty"`
	Hits  []CNResult `json:"hits"`
}

// CNMedia 是国产传媒站点客户端。
type CNMedia struct {
	sites map[string]string
	sig   string
	HTTP  *http.Client

	mu  sync.Mutex
	rls map[string]*rateLimiter
}

// NewCNMedia 依据配置构造客户端。
func NewCNMedia(cfg Config) *CNMedia {
	sites := map[string]string{}
	for _, m := range cnSiteOrder {
		v := strings.TrimRight(strings.TrimSpace(cfg.CNSites[m.Key]), "/")
		if v == "" {
			v = defaultCNSites()[m.Key]
		}
		sites[m.Key] = v
	}
	return &CNMedia{
		sites: sites,
		sig:   cnSitesSig(sites),
		HTTP:  newHTTPClient(cfg),
		rls:   map[string]*rateLimiter{},
	}
}

// cnSitesSig 生成站点地址指纹，用于判断缓存的客户端是否还有效。
func cnSitesSig(sites map[string]string) string {
	keys := make([]string, 0, len(sites))
	for k := range sites {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+sites[k])
	}
	return strings.Join(parts, "|")
}

// limiter 返回某站点专属的限速器（按站点隔离，站点之间互不阻塞）。
func (c *CNMedia) limiter(key string) *rateLimiter {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.rls[key]
	if !ok {
		r = &rateLimiter{interval: cnRequestInterval}
		c.rls[key] = r
	}
	return r
}

// searchURL 拼出某站点的搜索地址。
func (c *CNMedia) searchURL(key, keyword string) string {
	base := c.sites[key]
	if base == "" {
		return ""
	}
	switch key {
	case "xchina":
		return base + "/search.html?keyword=" + url.QueryEscape(keyword)
	case "madouqu", "madou":
		return base + "/?s=" + url.QueryEscape(keyword)
	case "7mmtv":
		return base + "/zh/searchall_search/all/" + url.PathEscape(keyword) + "/1.html"
	}
	return ""
}

// fetch 抓一个页面，带站点限速与浏览器请求头。
func (c *CNMedia) fetch(ctx context.Context, key, rawURL string) ([]byte, error) {
	if err := c.limiter(key).wait(ctx); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", cnUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,ja;q=0.8")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := readAllLimit(resp.Body, 16<<20)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		if resp.StatusCode == 403 && (bytes.Contains(data, []byte("Just a moment")) ||
			bytes.Contains(data, []byte("cf-browser-verification"))) {
			return nil, fmt.Errorf("被 Cloudflare 拦截（HTTP 403）")
		}
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return data, nil
}

// SearchSite 在单个站点上搜番号。失败不返回 error，而是塞进 CNSiteHits.Error ——
// 一家挂了不该让整个刮削失败。
func (c *CNMedia) SearchSite(ctx context.Context, key, keyword string) CNSiteHits {
	out := CNSiteHits{Site: key, Name: cnSiteName(key)}
	out.URL = c.searchURL(key, keyword)
	if out.URL == "" {
		out.Error = "未配置该站点地址"
		return out
	}
	data, err := c.fetch(ctx, key, out.URL)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	var hits []CNResult
	switch key {
	case "xchina":
		hits = parseXChina(out.URL, data)
	case "madouqu":
		hits = parseMadouqu(out.URL, data)
	case "madou":
		hits = parseMadouClub(out.URL, data)
	case "7mmtv":
		hits = parse7MMTV(out.URL, data)
	}
	if hits == nil {
		hits = []CNResult{} // 序列化成 []，别让调用方拿到 null
	}
	for i := range hits {
		hits[i].Site = key
		hits[i].Name = out.Name
		hits[i].Exact = cnExact(keyword, firstNonEmpty(hits[i].Number, hits[i].Title))
	}
	out.OK = true
	out.Hits = hits
	return out
}

// SearchAll 并发搜索全部站点，返回顺序与 cnSiteOrder 一致。
func (c *CNMedia) SearchAll(ctx context.Context, keyword string) []CNSiteHits {
	out := make([]CNSiteHits, len(cnSiteOrder))
	var wg sync.WaitGroup
	for i, m := range cnSiteOrder {
		wg.Add(1)
		go func(i int, key string) {
			defer wg.Done()
			out[i] = c.SearchSite(ctx, key, keyword)
		}(i, m.Key)
	}
	wg.Wait()
	return out
}

// ---------- 番号归一化（国产传媒专用） ----------

var (
	// reCNNumber 匹配带可选数字前缀的番号：91CM-014 / 91CM074 / MDHG0010 / SSNI-989。
	reCNNumber = regexp.MustCompile(`([0-9]{0,4}[A-Z]{2,10})[-_ ]?([0-9]{1,6})`)
	reTailNum  = regexp.MustCompile(`([0-9]+)$`)
	reBgURL    = regexp.MustCompile(`url\(\s*['"]?([^'")]+)['"]?\s*\)`)

	// reCNWatermark 匹配下载站水印 / 网址，用来判断标题是不是「原始文件名」。
	reCNWatermark = regexp.MustCompile(`(?i)(https?://|www\.|[a-z0-9][a-z0-9-]{1,30}\.(com|net|org|xyz|cc|top|vip|club|info|sx|me)\b)`)
)

// cnNoisePrefix 是压制组 / 编码标记的前缀，不该被当成番号。
//
// 只做**尽力而为**的过滤：猜错了最坏结果是「这个条目没搜到」，
// 会在任务日志里明确写出来，不会把数据写坏。
var cnNoisePrefix = map[string]bool{
	"HEVC": true, "AVC": true, "AVC1": true, "WEB": true, "WEBDL": true, "WEBRIP": true,
	"BLURAY": true, "BDRIP": true, "BRRIP": true, "HDRIP": true, "DVDRIP": true,
	"REMUX": true, "HDTV": true, "HDCAM": true, "HDR": true, "SDR": true,
	"AAC": true, "DTS": true, "AC": true, "MPEG": true, "XVID": true, "DIVX": true,
	"PART": true, "VOL": true, "DISC": true, "SEASON": true, "EPISODE": true,
	// 容器 / 分卷标记：`.mp4` 会被拆成 "MP"+"4"，`movie.CD1.mkv` 会变成 "CD-1"。
	// 末尾那个扩展名已经被 reFileExt 抹掉了，但夹在中间的分卷标记抹不掉，
	// 只能靠前缀表挡一下。
	"MP": true, "CD": true,
}

// cnExtractNumber 从标题 / 标签里抠出番号，统一成「前缀-数字」形式（91CM-014 / MDHG-0010）。
//
// 为什么不用现成的 numKeys：它的正则要求前缀是**纯字母**，
// `91CM-014` 会被压成 `CM-14` —— 前缀里的 `91` 丢了，
// 而国产传媒的番号恰好大量是「数字开头」（91CM / 91BCM / 18BT），压完就搜不到东西。
func cnExtractNumber(s string) string {
	s = strings.ToUpper(toHalfWidth(s))
	for _, m := range reCNNumber.FindAllStringSubmatch(s, -1) {
		if cnNoisePrefix[m[1]] {
			continue
		}
		return m[1] + "-" + m[2]
	}
	return ""
}

// cnItemNumber 从条目推断国产传媒番号。
//
// **不能用 itemNumber()**：它的正则要求前缀是纯字母，
// `91CM-014` 会被压成 `CM-014`，拿去搜索必然一无所获。
// 这里优先用国产传媒的提取规则，提取不到再退回通用逻辑。
func cnItemNumber(item Item) string {
	if n := cnExtractNumber(strings.Join(numberSourceFields(item), " ")); n != "" {
		return n
	}
	return itemNumber(item)
}

// reFileExt 匹配文件名末尾的扩展名。
//
// 为什么要抹掉：`.mp4` 会被番号正则拆成 "MP" + "4"，于是**每个 mp4 条目都
// 凭空多出一个「番号 MP-4」**（`.mp3` 同理，`.CD1` 会变成 CD-1）。
// 扩展名里不可能藏着番号，抽之前先去掉最省事，也不会误伤真实番号
// —— `/91CM-014/91CM-014.mp4` 去掉扩展名后番号还在。
var reFileExt = regexp.MustCompile(`\.[A-Za-z0-9]{1,5}$`)

// numberSourceFields 收集用来推断番号的文本（片名 / 原始标题 / 路径 / 排序名），
// 每一项都先去掉文件扩展名。
func numberSourceFields(item Item) []string {
	parts := []string{}
	for _, k := range []string{"Name", "OriginalTitle", "Path", "SortName"} {
		s, ok := item[k].(string)
		if !ok {
			continue
		}
		parts = append(parts, reFileExt.ReplaceAllString(s, ""))
	}
	return parts
}

// cnNorm 生成番号比对用的强归一化 key：去掉所有非字母数字，再折叠尾部数字的前导零。
//
//	91CM-014 -> 91CM14     91CM-14 -> 91CM14     （同一条）
//	91CM074  -> 91CM74                            （不是同一条）
//	MDHG0010 -> MDHG10
func cnNorm(s string) string {
	s = strings.ToUpper(toHalfWidth(strings.TrimSpace(s)))
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if m := reTailNum.FindStringSubmatch(out); m != nil {
		trimmed := strings.TrimLeft(m[1], "0")
		if trimmed == "" {
			trimmed = "0"
		}
		out = out[:len(out)-len(m[1])] + trimmed
	}
	return out
}

// cnExact 判断候选文本里的番号是否就是目标番号。
func cnExact(want, got string) bool {
	w := cnNorm(want)
	if w == "" {
		return false
	}
	if g := cnNorm(got); g != "" && g == w {
		return true
	}
	return cnExtractNumber(got) != "" && cnNorm(cnExtractNumber(got)) == w
}

// cnDate 把各种日期写法裁成 YYYY-MM-DD，认不出来返回空串。
func cnDate(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 10 && s[4] == '-' && s[7] == '-' {
		return s[:10]
	}
	return ""
}

// ---------- HTML 解析辅助 ----------

// cnFindAll 在子树里找出所有满足条件的元素节点。
func cnFindAll(n *html.Node, pred func(*html.Node) bool) []*html.Node {
	return htmlFindAll(n, func(x *html.Node) bool {
		return x.Type == html.ElementNode && pred(x)
	})
}

// cnChildren 返回直接子元素节点（只下探一层，避免把容器的文本也算进去）。
func cnChildren(n *html.Node, tag string) []*html.Node {
	var out []*html.Node
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if isElem(c, tag) {
			out = append(out, c)
		}
	}
	return out
}

// cnHasIcon 判断节点里是否含 <i> 图标 —— 用来区分「标签」和「时长/评论数」。
func cnHasIcon(n *html.Node) bool {
	return htmlFind(n, func(x *html.Node) bool { return isElem(x, "i") }) != nil
}

// cnAbsURL 把相对地址补成绝对地址。
func cnAbsURL(pageURL, href string) string {
	href = strings.TrimSpace(href)
	if href == "" {
		return ""
	}
	base, err := url.Parse(pageURL)
	if err != nil {
		return href
	}
	ref, err := url.Parse(href)
	if err != nil {
		return href
	}
	return base.ResolveReference(ref).String()
}

// ---------- 各站点适配器 ----------

// parseXChina 解析 xchina.co 搜索结果。
//
// 结构：
//
//	<div class="list video-list">
//	  <div class="item video">
//	    <a href="/video/id-xxx.html" title="标题">
//	      <div role="img" class="img" style="background-image:url('https://upload.xchina.io/video/xxx.webp')"></div>
//	    </a>
//	    <div class="text"><div class="title"><a href="…">标题</a></div></div>
//	    <div class="tags">
//	      <div>果冻传媒</div>                       <- 分类
//	      <div><i class="far fa-comments"></i>6</div> <- 评论数
//	      <div>91CM-014</div>                       <- 番号
//	      <div><i class="far fa-clock"></i>01:19:24</div> <- 时长
//	    </div>
//	  </div>
//	</div>
func parseXChina(pageURL string, data []byte) []CNResult {
	doc, err := html.Parse(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	var out []CNResult
	cards := cnFindAll(doc, func(n *html.Node) bool {
		return htmlHasClass(n, "item") && htmlHasClass(n, "video")
	})
	for _, card := range cards {
		r := CNResult{}
		if a := htmlFind(card, func(n *html.Node) bool {
			return isElem(n, "a") && strings.TrimSpace(htmlAttr(n, "title")) != ""
		}); a != nil {
			r.Title = strings.TrimSpace(htmlAttr(a, "title"))
			r.URL = cnAbsURL(pageURL, htmlAttr(a, "href"))
		}
		if img := htmlFind(card, func(n *html.Node) bool {
			return isElem(n, "div") && htmlHasClass(n, "img")
		}); img != nil {
			if m := reBgURL.FindStringSubmatch(htmlAttr(img, "style")); m != nil {
				r.Cover = strings.TrimSpace(m[1])
			}
		}
		if r.Title == "" {
			if t := htmlFind(card, func(n *html.Node) bool {
				return isElem(n, "div") && htmlHasClass(n, "title")
			}); t != nil {
				r.Title = htmlText(t)
			}
		}
		if tags := htmlFind(card, func(n *html.Node) bool { return htmlHasClass(n, "tags") }); tags != nil {
			for _, d := range cnChildren(tags, "div") {
				if htmlHasClass(d, "empty") || cnHasIcon(d) {
					continue // empty 占位 / 评论数 / 时长
				}
				t := strings.TrimSpace(htmlText(d))
				if t == "" {
					continue
				}
				if n := cnExtractNumber(t); n != "" {
					if r.Number == "" {
						r.Number = n
					}
					continue
				}
				r.Tags = append(r.Tags, t)
			}
		}
		if r.Title == "" && r.Cover == "" {
			continue
		}
		out = append(out, r)
	}
	return out
}

// parseMadouqu 解析 madouqu.com 搜索结果（WordPress）。
//
//	<article id=post-N class="post post-grid … category-gd tag-rae">
//	  <div class=entry-media><div class=placeholder><a href=…/video/91cm074/>
//	    <img class=lazyload data-src="https://i0.wp.com/…" alt="91CM074 女優面試"></a></div></div>
//	  <div class=entry-wrapper>
//	    <div class=custom-cat-and-tags><a>果冻传媒</a><a>Rae</a></div>
//	    <header class=entry-header><h2 class=entry-title><a title="…">91CM074 女優面試</a></h2></header>
//	    <div class=entry-footer><ul class=post-meta-box>
//	      <li class=meta-date><time datetime=2021-04-06T16:47:47+08:00>2021-04-06</time></li>
//	    </ul></div>
//	  </div>
//	</article>
func parseMadouqu(pageURL string, data []byte) []CNResult {
	doc, err := html.Parse(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	var out []CNResult
	for _, card := range cnFindAll(doc, func(n *html.Node) bool {
		return isElem(n, "article") && htmlHasClass(n, "post")
	}) {
		r := CNResult{}
		if h := htmlFind(card, func(n *html.Node) bool {
			return isElem(n, "h2") && htmlHasClass(n, "entry-title")
		}); h != nil {
			if a := htmlFind(h, func(n *html.Node) bool { return isElem(n, "a") }); a != nil {
				r.Title = firstNonEmpty(strings.TrimSpace(htmlAttr(a, "title")), htmlText(a))
				r.URL = cnAbsURL(pageURL, htmlAttr(a, "href"))
			}
		}
		if img := htmlFind(card, func(n *html.Node) bool { return isElem(n, "img") }); img != nil {
			ds := strings.TrimSpace(htmlAttr(img, "data-src"))
			src := strings.TrimSpace(htmlAttr(img, "src"))
			if strings.HasPrefix(src, "data:") {
				src = "" // lazyload 占位 gif
			}
			r.Cover = firstNonEmpty(ds, src)
			if r.Title == "" {
				r.Title = strings.TrimSpace(htmlAttr(img, "alt"))
			}
		}
		if t := htmlFind(card, func(n *html.Node) bool { return isElem(n, "time") }); t != nil {
			r.Date = cnDate(firstNonEmpty(htmlAttr(t, "datetime"), htmlText(t)))
		}
		if box := htmlFind(card, func(n *html.Node) bool { return htmlHasClass(n, "custom-cat-and-tags") }); box != nil {
			for _, a := range cnFindAll(box, func(n *html.Node) bool { return isElem(n, "a") }) {
				if t := strings.TrimSpace(htmlText(a)); t != "" {
					r.Tags = append(r.Tags, t)
				}
			}
		}
		r.Number = cnExtractNumber(r.Title)
		if r.Title == "" && r.Cover == "" {
			continue
		}
		out = append(out, r)
	}
	return out
}

// parseMadouClub 解析 madou.club 搜索结果（WordPress，主题不同）。
//
//	<article class="excerpt excerpt-c5">
//	  <a class="thumbnail" href="…"><img src="占位图" data-src="…/covers/…jpg" class="thumb"></a>
//	  <h2><a href="…">MDHG0010 这个面试有点硬 …</a></h2>
//	  <footer>
//	    <a class="post-like" …><span>114</span></a>
//	    <a href="…/category/麻豆传媒" rel="category tag">麻豆传媒</a>
//	    <time></time><span class="post-view">观看(45.36K)</span>
//	  </footer>
//	</article>
func parseMadouClub(pageURL string, data []byte) []CNResult {
	doc, err := html.Parse(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	var out []CNResult
	for _, card := range cnFindAll(doc, func(n *html.Node) bool {
		return isElem(n, "article") && htmlHasClass(n, "excerpt")
	}) {
		r := CNResult{}
		if a := htmlFind(card, func(n *html.Node) bool {
			return isElem(n, "a") && htmlHasClass(n, "thumbnail")
		}); a != nil {
			r.URL = cnAbsURL(pageURL, htmlAttr(a, "href"))
		}
		if h := htmlFind(card, func(n *html.Node) bool { return isElem(n, "h2") }); h != nil {
			if a := htmlFind(h, func(n *html.Node) bool { return isElem(n, "a") }); a != nil {
				r.Title = strings.TrimSpace(htmlText(a))
				if r.URL == "" {
					r.URL = cnAbsURL(pageURL, htmlAttr(a, "href"))
				}
			}
		}
		if img := htmlFind(card, func(n *html.Node) bool { return isElem(n, "img") }); img != nil {
			// src 是 /showcase/img/thumb.png 占位图，真图在 data-src
			if ds := strings.TrimSpace(htmlAttr(img, "data-src")); ds != "" {
				r.Cover = ds
			} else if src := strings.TrimSpace(htmlAttr(img, "src")); !strings.Contains(src, "/showcase/") {
				r.Cover = src
			}
		}
		if f := htmlFind(card, func(n *html.Node) bool { return isElem(n, "footer") }); f != nil {
			for _, a := range cnFindAll(f, func(n *html.Node) bool { return isElem(n, "a") }) {
				rel := htmlAttr(a, "rel")
				if !strings.Contains(rel, "category") && !strings.Contains(htmlAttr(a, "href"), "/category/") {
					continue
				}
				if t := strings.TrimSpace(htmlText(a)); t != "" {
					r.Tags = append(r.Tags, t)
				}
			}
			if t := htmlFind(f, func(n *html.Node) bool { return isElem(n, "time") }); t != nil {
				r.Date = cnDate(firstNonEmpty(htmlAttr(t, "datetime"), htmlText(t)))
			}
		}
		r.Number = cnExtractNumber(r.Title)
		if r.Title == "" && r.Cover == "" {
			continue
		}
		out = append(out, r)
	}
	return out
}

// parse7MMTV 解析 7mmtv.sx 搜索结果。
//
//	<div class="col-6 col-md-4 col-lg-3 col-item">
//	  <div class="video">
//	    <figure class="video-preview"><a href="…/xxx_content/id/NUM.html">
//	      <img class="lazyload" data-src="https://n1.1026cdn.sx/censored/m/xxx_SSNI-989.jpg" alt="SSNI-989 标题"></a></figure>
//	    <h3 class="video-title"><a href="…">[中字]SSNI-989 标题</a></h3>
//	    <div class="video-info">…<div class="video-channel">频道</div>
//	      <span class="small text-muted"> 2023-02-21 23:14:35 </span></div>
//	  </div>
//	</div>
func parse7MMTV(pageURL string, data []byte) []CNResult {
	doc, err := html.Parse(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	var out []CNResult
	for _, card := range cnFindAll(doc, func(n *html.Node) bool { return htmlHasClass(n, "col-item") }) {
		r := CNResult{}
		if a := htmlFind(card, func(n *html.Node) bool {
			return isElem(n, "a") && strings.TrimSpace(htmlAttr(n, "href")) != ""
		}); a != nil {
			r.URL = cnAbsURL(pageURL, htmlAttr(a, "href"))
		}
		if h := htmlFind(card, func(n *html.Node) bool {
			return isElem(n, "h3") && htmlHasClass(n, "video-title")
		}); h != nil {
			r.Title = strings.TrimSpace(htmlText(h))
		}
		if img := htmlFind(card, func(n *html.Node) bool { return isElem(n, "img") }); img != nil {
			r.Cover = firstNonEmpty(strings.TrimSpace(htmlAttr(img, "data-src")), strings.TrimSpace(htmlAttr(img, "src")))
			if r.Title == "" {
				r.Title = strings.TrimSpace(htmlAttr(img, "alt"))
			}
		}
		if sp := htmlFind(card, func(n *html.Node) bool {
			return isElem(n, "span") && htmlHasClass(n, "text-muted")
		}); sp != nil {
			r.Date = cnDate(htmlText(sp))
		}
		if ch := htmlFind(card, func(n *html.Node) bool { return htmlHasClass(n, "video-channel") }); ch != nil {
			if t := strings.TrimSpace(htmlText(ch)); t != "" {
				r.Tags = append(r.Tags, t)
			}
		}
		r.Number = cnExtractNumber(r.Title)
		if r.Title == "" && r.Cover == "" {
			continue
		}
		out = append(out, r)
	}
	return out
}

// ---------- 合并与写入 ----------

// CNPicked 是按站点优先级合并出来的最终字段。
type CNPicked struct {
	Title     string   `json:"title"`
	Cover     string   `json:"cover"`
	Tags      []string `json:"tags"`
	Date      string   `json:"date"`
	TitleFrom string   `json:"title_from"`
	CoverFrom string   `json:"cover_from"`
	TagFrom   string   `json:"tag_from"`
	DateFrom  string   `json:"date_from"`
}

// pickCN 按 cnSiteOrder 合并各站结果，**只认番号精确匹配的那几条**。
//
// 必须精确匹配的原因见文件头：madouqu 的搜索是模糊的，
// 不筛就会把 91CM074 的封面写到 91CM-014 上。
func pickCN(hits []CNSiteHits, want string) (*CNPicked, []string) {
	out := &CNPicked{}
	matched := []string{} // 序列化成 []，别让前端拿到 null
	for _, meta := range cnSiteOrder {
		for _, h := range hits {
			if h.Site != meta.Key || !h.OK {
				continue
			}
			for _, r := range h.Hits {
				if !cnExact(want, firstNonEmpty(r.Number, r.Title)) {
					continue
				}
				if !cnHasString(matched, meta.Key) {
					matched = append(matched, meta.Key)
				}
				if out.Title == "" && r.Title != "" {
					out.Title, out.TitleFrom = r.Title, meta.Key
				}
				if out.Cover == "" && r.Cover != "" {
					out.Cover, out.CoverFrom = r.Cover, meta.Key
				}
				if out.Date == "" && r.Date != "" {
					out.Date, out.DateFrom = r.Date, meta.Key
				}
				if len(out.Tags) == 0 && len(r.Tags) > 0 {
					out.Tags, out.TagFrom = dedupeStrings(r.Tags), meta.Key
				}
			}
		}
	}
	return out, matched
}

// maxCNBatchTargets 是一次批量刮削的条目数上限。
// 勾选路径由界面控制数量，但接口不能假设调用方守规矩。
const maxCNBatchTargets = 500

// CNScrape 是单个条目的国产传媒刮削结果。
type CNScrape struct {
	ItemID    string       `json:"item_id"`
	ItemName  string       `json:"item_name"`
	Number    string       `json:"number"`
	DryRun    bool         `json:"dry_run"`
	Applied   bool         `json:"applied"`
	Skipped   bool         `json:"skipped"`
	Message   string       `json:"message"`
	Title     string       `json:"title"`
	Cover     string       `json:"cover"`
	Tags      []string     `json:"tags"`
	Date      string       `json:"date"`
	TitleFrom string       `json:"title_from"`
	CoverFrom string       `json:"cover_from"`
	TagFrom   string       `json:"tag_from"`
	DateFrom  string       `json:"date_from"`
	AppliedTo []string     `json:"applied_fields"`
	Matched   []string     `json:"matched"`
	Sites     []CNSiteHits `json:"sites"`
}

// CNOptions 是一次国产传媒刮削的选项。
type CNOptions struct {
	Fields          map[string]bool // cover / title / tags / date
	OverwriteImages bool
	OverwriteTitle  bool
	DryRun          bool
}

// cnFieldsFrom 把前端传来的字段名列表转成集合，空列表 = 全选。
func cnFieldsFrom(names []string) map[string]bool {
	all := map[string]bool{"cover": true, "title": true, "tags": true, "date": true}
	if len(names) == 0 {
		return all
	}
	out := map[string]bool{}
	for _, n := range names {
		if all[strings.ToLower(strings.TrimSpace(n))] {
			out[strings.ToLower(strings.TrimSpace(n))] = true
		}
	}
	return out
}

// ScrapeCN 对单个条目执行国产传媒刮削（自建客户端，适合单条调用）。
func (a *App) ScrapeCN(ctx context.Context, itemID, numberOverride string, opts CNOptions) (*CNScrape, error) {
	return a.scrapeCNWith(ctx, a.cnMedia(), NewEmby(a.store.Get()), itemID, numberOverride, opts)
}

// scrapeCNWith 是实际干活的版本。批量任务传入共享的客户端与 Emby 实例：
// CNMedia 的限速器按站点共享，每条目新建一个等于没有限速。
func (a *App) scrapeCNWith(ctx context.Context, cn *CNMedia, e *Emby, itemID, numberOverride string, opts CNOptions) (*CNScrape, error) {
	item, err := e.ItemDetail(ctx, itemID)
	if err != nil {
		return nil, fmt.Errorf("读取条目失败：%w", err)
	}
	res := &CNScrape{ItemID: itemID, DryRun: opts.DryRun}
	res.ItemName, _ = item["Name"].(string)
	res.Number = firstNonEmpty(numberOverride, cnItemNumber(item))
	if res.Number == "" {
		return nil, fmt.Errorf("条目「%s」推断不出番号，无法搜索", res.ItemName)
	}

	res.Sites = cn.SearchAll(ctx, res.Number)
	pick, matched := pickCN(res.Sites, res.Number)
	res.Matched = matched
	res.Title, res.Cover, res.Tags, res.Date = pick.Title, pick.Cover, pick.Tags, pick.Date
	res.TitleFrom, res.CoverFrom = pick.TitleFrom, pick.CoverFrom
	res.TagFrom, res.DateFrom = pick.TagFrom, pick.DateFrom

	if len(matched) == 0 {
		res.Message = "四个站点都没有该番号的精确结果"
		return res, nil
	}
	if opts.DryRun {
		res.Message = "试运行：命中 " + itoa(len(matched)) + " 个站点，未写入"
		return res, nil
	}

	applied, note, err := a.applyCN(ctx, e, item, pick, opts)
	if err != nil {
		return res, err
	}
	res.AppliedTo = applied
	res.Applied = len(applied) > 0
	res.Skipped = len(applied) == 0
	if res.Skipped {
		res.Message = firstNonEmpty(note, "没有需要写入的字段")
	} else {
		res.Message = "已写入：" + strings.Join(applied, " / ")
		if note != "" {
			res.Message += "（" + note + "）"
		}
	}
	return res, nil
}

// applyCN 把合并出来的字段写进 Emby。
func (a *App) applyCN(ctx context.Context, e *Emby, item Item, p *CNPicked, opts CNOptions) ([]string, string, error) {
	itemID, _ := item["Id"].(string)
	var applied, notes []string
	patch := map[string]any{}

	if opts.Fields["title"] && p.Title != "" {
		cur, _ := item["Name"].(string)
		if opts.OverwriteTitle || cnShouldSetTitle(cur, cnItemNumber(item)) {
			patch["Name"] = p.Title
			applied = append(applied, "标题")
		} else {
			notes = append(notes, "标题已存在且不像文件名，跳过")
		}
	}

	// Tags 里始终保留番号本身，再叠加站点标签。
	tags := []string{}
	if n := cnItemNumber(item); n != "" {
		tags = append(tags, n)
	}
	if opts.Fields["tags"] {
		tags = append(tags, p.Tags...)
	}
	if len(tags) > 0 {
		patch["Tags"] = dedupeStrings(tags)
	}

	if opts.Fields["date"] && p.Date != "" {
		if d := normalizeDate(p.Date); d != "" {
			patch["PremiereDate"] = d
			patch["ProductionYear"] = atoiSafe(d[:4])
			applied = append(applied, "日期")
		}
	}

	if len(patch) > 0 {
		if err := e.UpdateItem(ctx, itemID, patch); err != nil {
			return applied, "", fmt.Errorf("写入元数据失败：%w", err)
		}
	}

	if opts.Fields["cover"] && p.Cover != "" {
		if opts.OverwriteImages || !imageTagExists(item, "Primary") {
			data, ct, err := fetchImageBytes(ctx, e.HTTP, p.Cover, "")
			switch {
			case err != nil:
				notes = append(notes, "封面下载失败："+err.Error())
			case len(data) == 0:
				notes = append(notes, "封面响应为空")
			default:
				if err := e.UploadImage(ctx, itemID, "Primary", -1, data, ct); err != nil {
					notes = append(notes, "封面上传失败："+err.Error())
				} else {
					applied = append(applied, "封面")
				}
			}
		} else {
			notes = append(notes, "已有封面，跳过（可勾选「覆盖已有封面」）")
		}
	}
	return applied, strings.Join(notes, "；"), nil
}

// cnShouldSetTitle 判断是否该用站点标题覆盖现有标题。
//
// 策略刻意保守 —— 覆盖标题是**不可逆**的（Emby 上没有历史版本），所以只有
// 明确「现有标题不含片名信息」时才自动覆盖，其余一律等用户勾「覆盖已有标题」：
//   - 标题为空
//   - 标题就是番号本身（91CM-014 / 91cm014）
//   - 标题是原始文件名或带下载站水印（xxx.mp4 / hhd800.com@91CM014）
//
// 已经是像样片名的（「日本街头拜金女大测试」）不动。
func cnShouldSetTitle(cur, number string) bool {
	cur = strings.TrimSpace(cur)
	if cur == "" {
		return true
	}
	if number != "" && cnExact(number, cur) {
		return true
	}
	return cnIsRawTitle(cur)
}

// cnIsRawTitle 判断标题是不是「原始文件名 / 下载站水印」而不是片名。
func cnIsRawTitle(s string) bool {
	low := strings.ToLower(strings.TrimSpace(s))
	for _, ext := range []string{".mp4", ".mkv", ".avi", ".wmv", ".ts", ".mov", ".rmvb", ".iso", ".strm", ".flv", ".m2ts"} {
		if strings.HasSuffix(low, ext) {
			return true
		}
	}
	return reCNWatermark.MatchString(low)
}

// ---------- 通用小工具 ----------

func cnHasString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func dedupeStrings(list []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(list))
	for _, v := range list {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}
