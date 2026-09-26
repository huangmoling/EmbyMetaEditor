package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"
)

// rateLimiter 是按固定间隔放行的令牌桶，避免把站点打挂。
type rateLimiter struct {
	mu       sync.Mutex
	interval time.Duration
	last     time.Time
}

func (r *rateLimiter) wait(ctx context.Context) error {
	if r.interval <= 0 {
		return nil
	}
	r.mu.Lock()
	now := time.Now()
	next := r.last.Add(r.interval)
	if next.After(now) {
		r.last = next
		r.mu.Unlock()
		select {
		case <-time.After(next.Sub(now)):
		case <-ctx.Done():
			return ctx.Err()
		}
		return nil
	}
	r.last = now
	r.mu.Unlock()
	return nil
}

// JavBus 是 javbus 站点客户端，地址可配置（可填镜像站）。
type JavBus struct {
	BaseURL string
	Cookie  string
	HTTP    *http.Client
	rl      *rateLimiter
}

// NewJavBus 依据配置构造客户端。
func NewJavBus(cfg Config) *JavBus {
	return &JavBus{
		BaseURL: strings.TrimRight(strings.TrimSpace(cfg.JavBusURL), "/"),
		Cookie:  cfg.JavBusCookie,
		HTTP:    newHTTPClient(cfg),
		rl:      &rateLimiter{interval: time.Duration(cfg.JavBusInterval) * time.Millisecond},
	}
}

// JBStar 是 javbus 上的演员。
type JBStar struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

// JBMagnet 是一条磁力链接。
type JBMagnet struct {
	Link string `json:"link"`
	Name string `json:"name"`
	Size string `json:"size"`
	Date string `json:"date"`
}

// JBMovie 是 javbus 上的一部作品。
type JBMovie struct {
	Number  string     `json:"number"`
	Title   string     `json:"title"`
	Date    string     `json:"date"`
	Cover   string     `json:"cover"`
	URL     string     `json:"url"`
	Runtime string     `json:"runtime"`
	Studio  string     `json:"studio"`
	Actors  []string   `json:"actors"`
	Genres  []string   `json:"genres"`
	Magnets []JBMagnet `json:"magnets,omitempty"`
}

// doRaw 发起一次带限速的 GET，返回响应体与状态码。
func (j *JavBus) doRaw(ctx context.Context, rawURL string, hdr map[string]string) ([]byte, int, error) {
	if j.BaseURL == "" {
		return nil, 0, fmt.Errorf("未配置 javbus 地址")
	}
	if err := j.rl.wait(ctx); err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,ja;q=0.8")
	if j.Cookie != "" {
		req.Header.Set("Cookie", j.Cookie)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := j.HTTP.Do(req)
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
func (j *JavBus) get(ctx context.Context, rawURL string, hdr map[string]string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		data, status, err := j.doRaw(ctx, rawURL, hdr)
		if err != nil {
			lastErr = err
			time.Sleep(time.Duration(attempt+1) * time.Second)
			continue
		}
		if status == 403 || status == 503 {
			if bytes.Contains(data, []byte("cf-browser-verification")) || bytes.Contains(data, []byte("Just a moment")) {
				return nil, fmt.Errorf("被 Cloudflare 拦截，请在配置里填入浏览器 Cookie（含 cf_clearance）后重试")
			}
			lastErr = fmt.Errorf("返回 %d，可能触发了访问频率限制", status)
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
			lastErr = fmt.Errorf("返回 200 但响应体为空（常见于被运营商/中间设备拦截，或本机网络需要代理）")
			time.Sleep(time.Duration(attempt+1) * time.Second)
			continue
		}
		return data, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("请求失败")
	}
	return nil, fmt.Errorf("访问 javbus 失败：%w", lastErr)
}

// ResolveStar 解析用户输入：支持演员名、/star/xxx 链接、纯 star id。
func (j *JavBus) ResolveStar(ctx context.Context, input string) ([]JBStar, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil, fmt.Errorf("请输入演员名或演员页地址")
	}
	// 直接是演员页地址
	if strings.Contains(input, "/star/") {
		id := input[strings.LastIndex(input, "/star/")+len("/star/"):]
		id = strings.Trim(id, "/")
		if i := strings.IndexAny(id, "?/#"); i >= 0 {
			id = id[:i]
		}
		if id != "" {
			return []JBStar{{ID: id, Name: id, URL: j.BaseURL + "/star/" + id}}, nil
		}
	}
	// 其余情况（演员名或纯 star id）统一走搜索接口
	return j.searchStar(ctx, input)
}

// searchStar 调用 javbus 的演员搜索接口。
func (j *JavBus) searchStar(ctx context.Context, keyword string) ([]JBStar, error) {
	u := j.BaseURL + "/searchstar/" + url.PathEscape(keyword)
	data, err := j.get(ctx, u, map[string]string{"X-Requested-With": "XMLHttpRequest"})
	if err != nil {
		return nil, err
	}
	// 优先按 JSON 解析
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && (trimmed[0] == '[' || trimmed[0] == '{') {
		var arr []struct {
			StarID   string `json:"star_id"`
			StarName string `json:"star_name"`
			StarLink string `json:"star_link"`
		}
		if err := json.Unmarshal(trimmed, &arr); err == nil {
			out := make([]JBStar, 0, len(arr))
			for _, s := range arr {
				if s.StarID == "" {
					continue
				}
				out = append(out, JBStar{ID: s.StarID, Name: s.StarName, URL: j.BaseURL + "/star/" + s.StarID})
			}
			if len(out) > 0 {
				return out, nil
			}
		}
	}
	// 回退：按 HTML 解析
	out := parseStarLinks(data, j.BaseURL)
	if len(out) == 0 {
		dumpHTML("javbus-searchstar", data)
		return nil, fmt.Errorf("没有找到演员「%s」，可尝试直接填演员页地址（原始页面已存到 cache/debug/）", keyword)
	}
	return out, nil
}

// StarMovies 抓取演员页的全部作品（自动翻页）。
func (j *JavBus) StarMovies(ctx context.Context, starID string, maxPages int) ([]JBMovie, error) {
	if maxPages <= 0 {
		maxPages = 50
	}
	seen := map[string]bool{}
	var all []JBMovie
	next := j.BaseURL + "/star/" + url.PathEscape(starID)
	for page := 0; page < maxPages && next != ""; page++ {
		data, err := j.get(ctx, next, map[string]string{"Referer": j.BaseURL + "/"})
		if err != nil {
			if page == 0 {
				return nil, err
			}
			break
		}
		movies, nextURL := parseStarPage(data, j.BaseURL)
		for _, mv := range movies {
			key := canonNumber(mv.Number)
			if key == "" {
				key = mv.URL
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			all = append(all, mv)
		}
		if nextURL == "" {
			break
		}
		next = nextURL
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("演员页没有解析到作品，可能是页面结构变化或需要登录/Cookie")
	}
	return all, nil
}

// parseStarPage 解析演员页，返回作品列表与下一页地址。
// javbus 及其镜像的页面结构偶有差异，这里做多套匹配策略。
func parseStarPage(data []byte, base string) ([]JBMovie, string) {
	doc, err := html.Parse(bytes.NewReader(data))
	if err != nil {
		return nil, ""
	}
	anchors := htmlFindAll(doc, func(n *html.Node) bool {
		return isElem(n, "a") && htmlHasClass(n, "movie-box")
	})
	if len(anchors) == 0 {
		// 回退：任何指向「合法番号路径」且内含封面图的链接
		anchors = htmlFindAll(doc, func(n *html.Node) bool {
			if !isElem(n, "a") {
				return false
			}
			href := htmlAttr(n, "href")
			if href == "" || strings.Contains(href, "javascript") {
				return false
			}
			if canonNumber(pathSegment(href)) == "" {
				return false
			}
			return htmlFind(n, func(c *html.Node) bool {
				return isElem(c, "img") &&
					(htmlAttr(c, "src") != "" || htmlAttr(c, "data-src") != "")
			}) != nil
		})
	}

	var out []JBMovie
	seen := map[string]bool{}
	for _, a := range anchors {
		href := htmlAttr(a, "href")
		if href == "" {
			continue
		}
		full := href
		if !strings.HasPrefix(full, "http") {
			full = base + "/" + strings.TrimLeft(full, "/")
		}
		number := pathSegment(href)
		if number == "" || seen[number] {
			continue
		}
		seen[number] = true
		mv := JBMovie{Number: number, URL: full}
		if img := htmlFind(a, func(n *html.Node) bool { return isElem(n, "img") }); img != nil {
			src := firstNonEmpty(htmlAttr(img, "src"), htmlAttr(img, "data-src"))
			if !strings.HasPrefix(src, "http") && src != "" {
				src = base + "/" + strings.TrimLeft(src, "/")
			}
			mv.Cover = src
			if t := htmlAttr(img, "title"); t != "" && canonNumber(t) != "" {
				mv.Title = t
			}
		}
		if d := htmlFind(a, func(n *html.Node) bool { return isElem(n, "date") }); d != nil {
			mv.Date = strings.TrimSpace(htmlText(d))
		}
		if mv.Date == "" {
			mv.Date = reDate.FindString(htmlText(a))
		}
		if mv.Title == "" {
			if sp := htmlFind(a, func(n *html.Node) bool { return isElem(n, "span") }); sp != nil {
				mv.Title = strings.TrimSpace(htmlDirectText(sp))
			}
		}
		if mv.Title == "" {
			// 演员页通常只列番号没有标题，详情页再补
			mv.Title = number
		}
		out = append(out, mv)
	}

	// 下一页：优先 id/class，其次按锚文本判断
	nextURL := ""
	cands := htmlFindAll(doc, func(n *html.Node) bool {
		if !isElem(n, "a") {
			return false
		}
		return htmlAttr(n, "id") == "next" || strings.Contains(strings.ToLower(htmlClass(n)), "next")
	})
	if len(cands) == 0 {
		for _, n := range htmlFindAll(doc, func(n *html.Node) bool { return isElem(n, "a") }) {
			switch strings.TrimSpace(htmlText(n)) {
			case "下一頁", "下一页", "Next", "次へ", "»", ">":
				cands = append(cands, n)
			}
		}
	}
	if len(cands) > 0 {
		h := htmlAttr(cands[len(cands)-1], "href")
		if h != "" && !strings.Contains(h, "javascript") {
			if strings.HasPrefix(h, "http") {
				nextURL = h
			} else {
				nextURL = base + "/" + strings.TrimLeft(h, "/")
			}
		}
	}
	return out, nextURL
}

// pathSegment 取 URL 的最后一段（去掉查询串与锚点）。
func pathSegment(href string) string {
	s := href
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimRight(s, "/")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSpace(s)
}

var (
	reGID  = regexp.MustCompile(`(?m)var\s+gid\s*=\s*(\d+)`)
	reIMG  = regexp.MustCompile(`(?m)var\s+img\s*=\s*['"]([^'"]+)['"]`)
	reUC   = regexp.MustCompile(`(?m)var\s+uc\s*=\s*(\d+)`)
	reSize = regexp.MustCompile(`(?i)\d+(?:\.\d+)?\s*(?:TB|GB|MB|KB)`)
	reDate = regexp.MustCompile(`\d{4}-\d{2}-\d{2}`)
)

// MovieDetail 抓取作品详情页。
func (j *JavBus) MovieDetail(ctx context.Context, numberOrURL string) (*JBMovie, error) {
	full := numberOrURL
	if !strings.HasPrefix(full, "http") {
		if canonNumber(numberOrURL) == "" && !strings.Contains(numberOrURL, "/") {
			return nil, fmt.Errorf("无法识别的番号：%s", numberOrURL)
		}
		full = j.BaseURL + "/" + strings.TrimLeft(numberOrURL, "/")
	}
	data, err := j.get(ctx, full, map[string]string{"Referer": j.BaseURL + "/"})
	if err != nil {
		return nil, err
	}
	mv := &JBMovie{URL: full}
	doc, perr := html.Parse(bytes.NewReader(data))
	if perr == nil {
		// 标题：优先 <h3>，其次 <title>
		if h3 := htmlFind(doc, func(n *html.Node) bool { return isElem(n, "h3") }); h3 != nil {
			mv.Title = strings.TrimSpace(htmlText(h3))
		}
		if mv.Title == "" {
			if t := htmlFind(doc, func(n *html.Node) bool { return isElem(n, "title") }); t != nil {
				mv.Title = strings.TrimSpace(htmlText(t))
				mv.Title = strings.TrimSuffix(mv.Title, " - JavBus")
			}
		}
		// 封面
		if a := htmlFind(doc, func(n *html.Node) bool {
			return isElem(n, "a") && htmlHasClass(n, "bigImage")
		}); a != nil {
			h := htmlAttr(a, "href")
			if !strings.HasPrefix(h, "http") && h != "" {
				h = j.BaseURL + "/" + strings.TrimLeft(h, "/")
			}
			mv.Cover = h
		}
		// info 区字段
		for _, p := range htmlFindAll(doc, func(n *html.Node) bool {
			return isElem(n, "p") && htmlFind(n, func(c *html.Node) bool {
				return isElem(c, "span") && strings.Contains(htmlClass(c), "header")
			}) != nil
		}) {
			label := ""
			if sp := htmlFind(p, func(n *html.Node) bool {
				return isElem(n, "span") && strings.Contains(htmlClass(n), "header")
			}); sp != nil {
				label = strings.Trim(strings.TrimSpace(htmlText(sp)), ":：")
			}
			// 段落文本形如「品番: SSIS-001」，取冒号之后的部分作为值
			rowText := strings.TrimSpace(htmlText(p))
			val := rowText
			if i := strings.IndexAny(rowText, ":："); i >= 0 {
				val = strings.TrimSpace(rowText[i+1:])
			}
			switch label {
			case "品番", "番號", "番号", "識別碼", "识别码":
				if c := canonNumber(val); c != "" {
					mv.Number = c
				}
			case "發行日期", "发行日期", "配信開始日":
				if d := reDate.FindString(val); d != "" {
					mv.Date = d
				}
			case "長度", "长度", "収録時間":
				mv.Runtime = val
			case "製作商", "制作商", "メーカー":
				mv.Studio = val
			case "演員", "演员", "出演者":
				for _, a := range htmlFindAll(p, func(n *html.Node) bool { return isElem(n, "a") }) {
					if nm := strings.TrimSpace(htmlText(a)); nm != "" {
						mv.Actors = append(mv.Actors, nm)
					}
				}
			case "類別", "类别", "ジャンル":
				for _, a := range htmlFindAll(p, func(n *html.Node) bool { return isElem(n, "a") }) {
					if g := strings.TrimSpace(htmlText(a)); g != "" {
						mv.Genres = append(mv.Genres, g)
					}
				}
			}
		}
		if mv.Cover == "" {
			if img := htmlFind(doc, func(n *html.Node) bool {
				return isElem(n, "img") && strings.Contains(htmlAttr(n, "src"), "/pics/")
			}); img != nil {
				src := htmlAttr(img, "src")
				if !strings.HasPrefix(src, "http") {
					src = j.BaseURL + "/" + strings.TrimLeft(src, "/")
				}
				mv.Cover = src
			}
		}
	}
	if mv.Number == "" {
		mv.Number = canonNumber(numberOrURL)
	}
	// 磁力：需要 gid / img / uc 三个参数
	src := string(data)
	gidM := reGID.FindStringSubmatch(src)
	imgM := reIMG.FindStringSubmatch(src)
	if gidM != nil && imgM != nil {
		uc := "0"
		if ucM := reUC.FindStringSubmatch(src); ucM != nil {
			uc = ucM[1]
		}
		if mags, err := j.fetchMagnets(ctx, full, gidM[1], imgM[1], uc); err == nil {
			mv.Magnets = mags
		}
	}
	return mv, nil
}

// fetchMagnets 调用 ajax 接口取磁力列表。
func (j *JavBus) fetchMagnets(ctx context.Context, referer, gid, img, uc string) ([]JBMagnet, error) {
	q := url.Values{}
	q.Set("gid", gid)
	q.Set("lang", "zh")
	q.Set("img", img)
	q.Set("uc", uc)
	u := j.BaseURL + "/ajax/uncledatoolsbyajax.php?" + q.Encode()
	data, err := j.get(ctx, u, map[string]string{
		"Referer":          referer,
		"X-Requested-With": "XMLHttpRequest",
		"Accept":           "text/html, */*; q=0.01",
	})
	if err != nil {
		return nil, err
	}
	return parseMagnets(data), nil
}

// parseMagnets 从 ajax 返回的 HTML 片段中解析磁力列表。
func parseMagnets(data []byte) []JBMagnet {
	// 真实接口返回的是一串**裸 <tr> 片段**，没有 <table> 包裹。
	// HTML5 的树构造规则会把游离的 <tr>/<td> 直接丢弃，导致解析出 0 条，
	// 所以这里必须先套一层 <table><tbody>。
	// （注意：夹具如果写成 <table>…</table> 就测不出这个坑，见 javbus_test.go。）
	wrapped := make([]byte, 0, len(data)+32)
	wrapped = append(wrapped, "<table><tbody>"...)
	wrapped = append(wrapped, data...)
	wrapped = append(wrapped, "</tbody></table>"...)

	doc, err := html.Parse(bytes.NewReader(wrapped))
	if err != nil {
		return nil
	}
	var out []JBMagnet
	seen := map[string]bool{}
	for _, tr := range htmlFindAll(doc, func(n *html.Node) bool { return isElem(n, "tr") }) {
		var link string
		for _, a := range htmlFindAll(tr, func(n *html.Node) bool { return isElem(n, "a") }) {
			h := htmlAttr(a, "href")
			if strings.HasPrefix(h, "magnet:") && link == "" {
				link = h
			}
		}
		if link == "" || seen[link] {
			continue
		}
		seen[link] = true
		m := JBMagnet{Link: link}
		// 名称：取第一个非「磁力鏈接」的链接文本
		for _, a := range htmlFindAll(tr, func(n *html.Node) bool { return isElem(n, "a") }) {
			t := strings.TrimSpace(htmlText(a))
			if t == "" || strings.Contains(t, "磁力") {
				continue
			}
			m.Name = t
			break
		}
		rowText := htmlText(tr)
		if s := reSize.FindString(rowText); s != "" {
			m.Size = strings.Join(strings.Fields(s), "")
		}
		if d := reDate.FindString(rowText); d != "" {
			m.Date = d
		}
		if m.Name == "" {
			if dn := magnetDisplayName(link); dn != "" {
				m.Name = dn
			} else {
				m.Name = "磁力链接"
			}
		}
		out = append(out, m)
	}
	return out
}

// reMagnetSize 只认「数字 + 单位」这一种形态（解析阶段已经把空白归一化掉了）。
var reMagnetSize = regexp.MustCompile(`(?i)^\s*(\d+(?:\.\d+)?)\s*(TB|GB|MB|KB|B)?\s*$`)

// magnetSizeBytes 把「1.83GB」这类体积串换算成可以比较的字节数。
//
// 解析不出来时返回 -1 而不是 0：0 会跟「0 B」混为一谈，而且排序时会被当成
// 「比 KB 级还小」排到中间去。这里要的是「不认识的排最后」，用一个必定小于
// 任何真实体积的哨兵值最省事。历史上确实出现过空体积（页面结构变动时），
// 排除这种可能比假设它不会发生更划算。
func magnetSizeBytes(s string) int64 {
	m := reMagnetSize.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return -1
	}
	f, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return -1
	}
	var unit float64 = 1
	switch strings.ToUpper(m[2]) {
	case "TB":
		unit = 1 << 40
	case "GB":
		unit = 1 << 30
	case "MB":
		unit = 1 << 20
	case "KB":
		unit = 1 << 10
	}
	return int64(f * unit)
}

// sortMagnetsBySize 就地把磁力列表按体积从大到小排序，体积解析不出来的排最后。
//
// 用 SliceStable：体积相同的两条（同一个种子的不同发布很常见）保持页面上的
// 原始顺序，否则每次刷新顺序都在跳，用户会以为列表变了。
func sortMagnetsBySize(mags []JBMagnet) {
	sort.SliceStable(mags, func(i, j int) bool {
		return magnetSizeBytes(mags[i].Size) > magnetSizeBytes(mags[j].Size)
	})
}

// magnetDisplayName 从磁力链接的 dn 参数里提取可读名称。
func magnetDisplayName(link string) string {
	u, err := url.Parse(link)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(u.Query().Get("dn"))
}

// ---------- 诊断 ----------

// JBProbeSearch 是演员搜索接口的探测结果。
type JBProbeSearch struct {
	Keyword   string `json:"keyword"`
	OK        bool   `json:"ok"`
	Status    int    `json:"status"`
	Size      int    `json:"size"`
	Kind      string `json:"kind"`
	Found     int    `json:"found"`
	Error     string `json:"error,omitempty"`
	RawSample string `json:"raw_sample,omitempty"`
	DumpPath  string `json:"dump_path,omitempty"`
}

// JBProbe 是 javbus 连通性与页面结构的诊断结果。
type JBProbe struct {
	URL           string         `json:"url"`
	OK            bool           `json:"ok"`
	Status        int            `json:"status"`
	Size          int            `json:"size"`
	ElapsedMS     int64          `json:"elapsed_ms"`
	Blocked       string         `json:"blocked,omitempty"`
	LooksLikeHome bool           `json:"looks_like_home"`
	Markers       map[string]int `json:"markers"`
	Search        *JBProbeSearch `json:"search,omitempty"`
	Message       string         `json:"message"`
	DumpPath      string         `json:"dump_path,omitempty"`
}

// Probe 探测 javbus 是否可达、返回的页面是不是我们预期的结构。
// keyword 非空时顺带测一次演员搜索接口。
// 抓取番号失败时先跑这个，能直接区分「网络不通 / 被验证码拦 / 页面改版」。
func (j *JavBus) Probe(ctx context.Context, keyword string) *JBProbe {
	p := &JBProbe{URL: j.BaseURL, Markers: map[string]int{}}
	if j.BaseURL == "" {
		p.Message = "未配置 javbus 地址"
		return p
	}
	start := time.Now()
	data, status, err := j.doRaw(ctx, j.BaseURL+"/", map[string]string{"Referer": j.BaseURL + "/"})
	p.Status, p.Size, p.ElapsedMS = status, len(data), time.Since(start).Milliseconds()
	if err != nil {
		p.Message = "无法连接：" + err.Error() + "（检查网络或代理设置，也可以换一个镜像地址）"
		return p
	}
	p.Markers = pageMarkers(data)
	switch {
	case p.Size == 0:
		p.Blocked = "empty"
		p.Message = fmt.Sprintf("HTTP %d 但响应体为空。通常是本机网络或运营商拦截了该域名，也可能需要走代理。", status)
		return p
	case p.Markers["cf-browser-verification"] > 0 || p.Markers["Just a moment"] > 0:
		p.Blocked = "cloudflare"
		p.DumpPath = dumpHTML("javbus-blocked", data)
		p.Message = "被 Cloudflare 人机验证拦住了。请从浏览器 F12 里复制完整 Cookie（含 cf_clearance）填到设置里。"
		return p
	case status >= 400:
		p.Message = fmt.Sprintf("站点返回 HTTP %d，拒绝了请求。", status)
		return p
	}
	p.LooksLikeHome = p.Markers["movie-box"] > 0 || p.Markers["pics/cover"] > 0
	// OK 的含义是「可以正常抓取」，而不只是「连得上」：
	// 能连上但页面结构不符，抓番号一样会失败，必须让用户看到是红的。
	p.OK = p.LooksLikeHome
	if p.LooksLikeHome {
		p.Message = fmt.Sprintf("连通正常（HTTP %d，%d KB，%d ms），页面结构符合预期，可以开始抓取。",
			status, p.Size/1024, p.ElapsedMS)
	} else {
		p.DumpPath = dumpHTML("javbus-home", data)
		p.Message = fmt.Sprintf("能连上（HTTP %d，%d KB），但页面里没有影片列表标记，可能返回的是验证页或公告页，抓取会失败。原始页面已存到 cache/debug/。",
			status, p.Size/1024)
	}
	if strings.TrimSpace(keyword) != "" {
		p.Search = j.probeSearch(ctx, keyword)
	}
	return p
}

// probeSearch 探测演员搜索接口的返回形态。
func (j *JavBus) probeSearch(ctx context.Context, keyword string) *JBProbeSearch {
	r := &JBProbeSearch{Keyword: keyword}
	data, status, err := j.doRaw(ctx, j.BaseURL+"/searchstar/"+url.PathEscape(keyword),
		map[string]string{"X-Requested-With": "XMLHttpRequest"})
	r.Status, r.Size = status, len(data)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	trimmed := bytes.TrimSpace(data)
	switch {
	case len(trimmed) == 0:
		r.Kind = "empty"
		r.Error = "响应体为空"
	case trimmed[0] == '[' || trimmed[0] == '{':
		r.Kind = "json"
		var arr []struct {
			StarID string `json:"star_id"`
		}
		if err := json.Unmarshal(trimmed, &arr); err != nil {
			r.Error = "像是 JSON 但解析失败：" + err.Error()
			r.RawSample = sampleText(trimmed, 200)
			return r
		}
		r.Found = len(arr)
		r.OK = len(arr) > 0
		if !r.OK {
			r.Error = "接口返回空数组，说明站内没有这个演员名（换个写法或直接填演员页地址）"
		}
	default:
		r.Kind = "html"
		r.Found = len(parseStarLinks(trimmed, j.BaseURL))
		r.OK = r.Found > 0
		if !r.OK {
			r.DumpPath = dumpHTML("javbus-searchstar", data)
			r.Error = "既不是 JSON，也没解析出演员链接；原始响应已存到 cache/debug/"
			r.RawSample = sampleText(trimmed, 200)
		}
	}
	return r
}

// parseStarLinks 从 HTML 片段里提取演员链接（搜索接口的回退路径）。
func parseStarLinks(data []byte, base string) []JBStar {
	doc, err := html.Parse(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []JBStar
	for _, a := range htmlFindAll(doc, func(n *html.Node) bool { return isElem(n, "a") }) {
		href := htmlAttr(a, "href")
		if !strings.Contains(href, "/star/") {
			continue
		}
		id := pathSegment(href)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		name := strings.TrimSpace(htmlText(a))
		if name == "" {
			name = id
		}
		out = append(out, JBStar{ID: id, Name: name, URL: base + "/star/" + id})
	}
	return out
}

// sampleText 截一小段可读文本用于诊断展示。
func sampleText(b []byte, n int) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}
