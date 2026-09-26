package main

// 演员资料抓取：把外部资料源的结果归一化成 ActorFacts，再由 buildActorProfile
// 合并成「可以写回 Emby 的字段清单」。
//
// 这一层只负责「抓」和「解析」，不碰 Emby、不落盘、不写东西 ——
// 写入策略（只填空白）在 profile.go 里。
//
// 关于来源的取舍（对齐原版「Emby演员扩展器」的行为）：
//   - AVデータバンク（av-db.net）：资料最全（事务所/生日/出生地/血型/身高/三围/罩杯/
//     出道/兴趣/别名/自由简介），且实测可连通 → 首选。
//   - AV-League（av-league.com）：`search.php?k=` 是标准 GET 搜索，详情页字段在
//     一张干净的 info-tbl 表里；资料常缺（大量「不明」），但命中率高、可作为
//     生日/出生地/出道/标签的补充。
//   - Wikipedia：有正式 API，中/日文都能查，主要提供生卒/出身/读音与别名。
//   - みんなのAV / Javden：实测在本机网络下不可达（DNS 解析不出来），不实现。
//   - 离线演员资料库：原版用的是 SQLCipher 加密库（e_sqlcipher.dll），无法读取。
//   - Graphis：**在原版里它是「头像」源不是「资料」源**（日志里是 `头像[Graphis]`），
//     而本项目头像只做「强制覆盖 gfriends 已有头像」，所以这里不需要它。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// ---------- 数据模型 ----------

// ActorFacts 是单个来源抓到的原始事实。
//
// 字段一律保留**源站原文**（日文），不在这里翻译 —— 翻译是上层的事，
// 由「设置 → OpenAI 翻译」那个开关决定，没配 key 也能正常用。
type ActorFacts struct {
	Source      string `json:"source"`       // 稳定 key，会写进 ProviderIds
	SourceLabel string `json:"source_label"` // 界面显示名
	SourceURL   string `json:"source_url"`
	MatchScore  int    `json:"match_score"` // 姓名置信度 0-100
	MatchedName string `json:"matched_name"`
	ElapsedMS   int64  `json:"elapsed_ms"`

	Aliases    []string `json:"aliases,omitempty"`
	BirthDate  string   `json:"birth_date,omitempty"` // YYYY-MM-DD
	BirthPlace string   `json:"birth_place,omitempty"`
	Height     string   `json:"height,omitempty"`
	Bust       string   `json:"bust,omitempty"`
	Waist      string   `json:"waist,omitempty"`
	Hip        string   `json:"hip,omitempty"`
	Cup        string   `json:"cup,omitempty"`
	BloodType  string   `json:"blood_type,omitempty"`
	DebutDate  string   `json:"debut_date,omitempty"`
	DebutSpan  string   `json:"debut_span,omitempty"`
	Hobby      string   `json:"hobby,omitempty"`
	Agency     string   `json:"agency,omitempty"`
	AgencySpan string   `json:"agency_span,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	Summary    string   `json:"summary,omitempty"`
	ProviderID string   `json:"provider_id,omitempty"` // 源站内部 ID / slug
}

// ProfileSource 是资料源的统一接口。
type ProfileSource interface {
	Key() string
	Label() string
	// Fetch 按姓名（含别名候选）抓取；未命中返回 (nil, nil)，网络/解析错误返回 error。
	Fetch(ctx context.Context, client *http.Client, name string, aliases []string) (*ActorFacts, error)
}

// actorSources 是注册进来的全部源，**顺序即优先级**（靠前的字段先到先得）。
func actorSources() []ProfileSource {
	return []ProfileSource{
		&avDBSource{},
		&avLeagueSource{},
		&wikipediaSource{},
	}
}

func findSource(key string) ProfileSource {
	for _, s := range actorSources() {
		if s.Key() == key {
			return s
		}
	}
	return nil
}

// ---------- 姓名匹配 ----------

// commonNameSuffixes 是搜索/标题里常见的噪声尾巴，比对前先削掉。
var commonNameSuffixes = []string{
	"の別名義・プロフィール", "のプロフィール", "完全ガイド", "公式サイト",
}

// cleanCandidateName 去掉标题类噪声与各种括号后缀。
func cleanCandidateName(s string) string {
	s = strings.TrimSpace(toHalfWidth(s))
	for _, cut := range commonNameSuffixes {
		if i := strings.Index(s, cut); i > 0 {
			s = s[:i]
		}
	}
	// 去掉「（さかいなな）」这类读音后缀
	if i := strings.IndexAny(s, "（("); i > 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// nameWithReading 是「主名 + 括号里的读音」的拆解结果。
type nameWithReading struct {
	name    string
	reading string
}

// splitReading 拆「坂井なな（さかいなな）」→ {坂井なな, さかいなな}。
//
// 全程按 rune 操作：全角括号是 3 字节，按字节下标切会切出乱码
// （单测当场抓到了这个 bug）。
func splitReading(s string) nameWithReading {
	rs := []rune(strings.TrimSpace(s))
	for i, r := range rs {
		if r != '（' && r != '(' {
			continue
		}
		for j := i + 1; j < len(rs); j++ {
			if rs[j] == '）' || rs[j] == ')' {
				return nameWithReading{
					name:    strings.TrimSpace(string(rs[:i])),
					reading: cleanValue(string(rs[i+1 : j])),
				}
			}
		}
		break
	}
	return nameWithReading{name: cleanCandidateName(s)}
}

// nameMatchScore 计算「想要的名字」与「源站返回的名字」的置信度。
//
// 不做假名→罗马音转换（那需要整套五十音表、收益有限）：源站自己的搜索已经
// 做了模糊匹配，我们只要确认返回的确实是同一个人，避免把「坂井なな」的资料
// 写到「坂井美桜」头上。
func nameMatchScore(want, got string, aliases []string) int {
	g := normName(cleanCandidateName(got))
	if g == "" {
		return 0
	}
	w := normName(want)
	if w != "" && g == w {
		return 100
	}
	for _, a := range aliases {
		an := normName(a)
		if an == "" {
			continue
		}
		if g == an {
			return 95
		}
		// 源站返回「别名 + 噪声」的情况，去掉后缀后仍包含别名
		if len([]rune(an)) >= 2 && strings.Contains(g, an) {
			return 90
		}
	}
	// 源站返回的名字里包含我们要找的名字（通常是「名字 + 标题噪声」）：
	// 可以接受，因为源站的搜索已经筛过一轮了。
	if w != "" && len([]rune(w)) >= 2 && strings.Contains(g, w) {
		return 80
	}
	// 注意：**没有**「源站返回的名字比我们要找的更短」这一档。
	// 「水户香奈」vs「水户」看起来像，实际极可能是另一个人 —— 宁可漏，不可错。
	return 0
}

// minMatchScore 低于这个分数一律丢弃 —— 宁可没资料，也不能写错人。
//
// 这档允许「源站名字里包含我们要找的名字」(80)：搜索结果的链接文字常常是
// 整张卡片摘要（「若宮穂乃 出演801本 164cm Gカップ…」），不放松就全抓不到。
const minMatchScore = 80

// minDetailMatchScore 是**详情页姓名**的闸门，比上面严得多。
//
// 详情页的 <h1> 是权威姓名，本来就不该带噪声 —— 这里只认精确 (100) 或
// 别名命中 (95)。否则搜索把「坂井ななせ」当成「坂井なな」时，
// 「坂井ななせ」包含「坂井なな」能拿 80，会把资料写到别人头上。
const minDetailMatchScore = 95

// confirmDetailName 用**详情页自己的姓名**做最终确认。
//
// 为什么不能只信搜索结果：结果卡片的链接文字往往是整张摘要，靠「包含」判定
// 很容易认错人。详情页的 <h1> 才是权威姓名。
//
// 返回 (最终分数, 是否通过, 权威姓名)。详情页没有姓名元素时退回搜索分数。
func confirmDetailName(want string, aliases []string, detailName string, fallbackScore int) (int, bool, string) {
	detailName = strings.TrimSpace(detailName)
	if detailName == "" {
		return fallbackScore, fallbackScore >= minMatchScore, ""
	}
	score := nameMatchScore(want, detailName, aliases)
	if score < minDetailMatchScore {
		return score, false, detailName
	}
	return score, true, detailName
}

// ---------- 小工具 ----------

var (
	reBirthYMD = regexp.MustCompile(`(\d{4})\s*[-/年]\s*(\d{1,2})\s*[-/月]\s*(\d{1,2})\s*日?`)
	reDigits   = regexp.MustCompile(`\d+`)
	reWS       = regexp.MustCompile(`\s+`)
)

// normDate 把各种写法的日期归一成 YYYY-MM-DD，失败返回空串。
func normDate(s string) string {
	m := reBirthYMD.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	y, _ := strconv.Atoi(m[1])
	mo, _ := strconv.Atoi(m[2])
	d, _ := strconv.Atoi(m[3])
	if y < 1900 || y > 2100 || mo < 1 || mo > 12 || d < 1 || d > 31 {
		return ""
	}
	return fmt.Sprintf("%04d-%02d-%02d", y, mo, d)
}

// cleanValue 清理字段值：去空白、去「不明/なし/-」这类占位。
func cleanValue(s string) string {
	s = strings.TrimSpace(toHalfWidth(s))
	s = reWS.ReplaceAllString(s, " ")
	switch s {
	case "", "-", "―", "—", "不明", "なし", "無し", "非公開", "未公開", "?", "？", "N/A", "n/a":
		return ""
	}
	return s
}

// digitsOnly 只保留数字（身高/三围用）。
func digitsOnly(s string) string {
	return reDigits.FindString(s)
}

// fetchHTML 取页面并解析成 DOM。
func fetchHTML(ctx context.Context, client *http.Client, rawURL string) (*html.Node, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Accept-Language", "ja,en;q=0.8")
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := readAllLimit(resp.Body, 8<<20)
	if err != nil {
		return nil, nil, err
	}
	doc, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return nil, nil, fmt.Errorf("解析 HTML 失败：%w", err)
	}
	return doc, body, nil
}

// browserUA 复用 imageproxy.go 里的常量（抓图床与抓资料页要求一致）。

// 1 个重要前提：htmlText(nil) 会 panic，所以取子节点文字统一走这个包装。
func textOf(n *html.Node) string {
	if n == nil {
		return ""
	}
	return strings.TrimSpace(htmlText(n))
}

// dlPairs 把 <dl> 里的 <dt>标签</dt><dd>值</dd> 读成有序的键值对。
func dlPairs(doc *html.Node) [][2]string {
	var out [][2]string
	for _, dl := range htmlFindAll(doc, func(n *html.Node) bool { return isElem(n, "dl") }) {
		var label string
		for c := dl.FirstChild; c != nil; c = c.NextSibling {
			switch {
			case isElem(c, "dt"):
				label = textOf(c)
			case isElem(c, "dd"):
				if label != "" {
					out = append(out, [2]string{label, textOf(c)})
				}
			}
		}
	}
	return out
}

// trPairs 把 <table> 里 <th>标签</th><td>值</td> 读成键值对。
func trPairs(doc *html.Node, tableClass string) [][2]string {
	var out [][2]string
	for _, tb := range htmlFindAll(doc, func(n *html.Node) bool { return isElem(n, "table") }) {
		if tableClass != "" && !htmlHasClass(tb, tableClass) {
			continue
		}
		for _, tr := range htmlFindAll(tb, func(n *html.Node) bool { return isElem(n, "tr") }) {
			var label, val string
			for c := tr.FirstChild; c != nil; c = c.NextSibling {
				switch {
				case isElem(c, "th"):
					label = textOf(c)
				case isElem(c, "td"):
					val = textOf(c)
				}
			}
			if label != "" {
				out = append(out, [2]string{label, val})
			}
		}
	}
	return out
}

// pick 在键值对里按标签名取值（支持多个候选标签）。
func pick(pairs [][2]string, labels ...string) string {
	for _, want := range labels {
		for _, kv := range pairs {
			if strings.TrimSpace(kv[0]) == want {
				if v := cleanValue(kv[1]); v != "" {
					return v
				}
			}
		}
	}
	return ""
}

// ---------- 源 1：AVデータバンク（av-db.net）----------

type avDBSource struct{}

func (s *avDBSource) Key() string   { return "AvDataBank" }
func (s *avDBSource) Label() string { return "AVデータバンク" }

const avDBBase = "https://av-db.net"

// actressLinkRe 从搜索结果里捞 /actress/<slug> 链接。
var actressLinkRe = regexp.MustCompile(`^/actress/([A-Za-z0-9_\-]+)$`)

func (s *avDBSource) Fetch(ctx context.Context, client *http.Client, name string, aliases []string) (*ActorFacts, error) {
	start := time.Now()
	// 搜索页是 GET，直接把名字塞进 q。
	searchURL := avDBBase + "/search?q=" + url.QueryEscape(name)
	doc, _, err := fetchHTML(ctx, client, searchURL)
	if err != nil {
		return nil, fmt.Errorf("AVデータバンク 搜索失败：%w", err)
	}

	// 收集候选：slug -> 页面上显示的名字
	type cand struct {
		slug string
		name string
	}
	var cands []cand
	seen := map[string]bool{}
	for _, a := range htmlFindAll(doc, func(n *html.Node) bool { return isElem(n, "a") }) {
		href := htmlAttr(a, "href")
		m := actressLinkRe.FindStringSubmatch(href)
		if m == nil || seen[m[1]] {
			continue
		}
		txt := cleanCandidateName(htmlText(a))
		if txt == "" {
			continue
		}
		seen[m[1]] = true
		cands = append(cands, cand{slug: m[1], name: txt})
	}
	if len(cands) == 0 {
		return nil, nil // 未命中
	}

	// 挑置信度最高且不低于阈值的那个
	best := cands[0]
	bestScore := -1
	for _, c := range cands {
		if sc := nameMatchScore(name, c.name, aliases); sc > bestScore {
			best, bestScore = c, sc
		}
	}
	if bestScore < minMatchScore {
		return nil, nil
	}

	pageURL := avDBBase + "/actress/" + best.slug
	doc2, _, err := fetchHTML(ctx, client, pageURL)
	if err != nil {
		return nil, fmt.Errorf("AVデータバンク 详情页失败：%w", err)
	}
	// 详情页 <h1> 是权威姓名，用它做最终确认（搜索结果的卡片文字只是线索）
	score, ok, detailName := confirmDetailName(name, aliases,
		textOf(firstTag(doc2, "h1")), bestScore)
	if !ok {
		return nil, nil
	}
	if detailName == "" {
		detailName = best.name
	}
	f := parseAVDBDoc(doc2, best.slug, detailName, score)
	if f == nil {
		return nil, nil
	}
	f.SourceURL = pageURL
	f.ElapsedMS = time.Since(start).Milliseconds()
	return f, nil
}

// parseAVDBDoc 把 AVデータバンク 的演员详情页解析成 ActorFacts。
// 与网络请求分开，便于用真实页面夹具做回归。
func parseAVDBDoc(doc *html.Node, slug, name string, score int) *ActorFacts {
	f := &ActorFacts{
		Source:      (&avDBSource{}).Key(),
		SourceLabel: (&avDBSource{}).Label(),
		MatchScore:  score,
		MatchedName: name,
		ProviderID:  slug,
	}

	pairs := dlPairs(doc)
	f.BirthDate = normDate(pick(pairs, "生年月日"))
	f.BirthPlace = pick(pairs, "出身地", "出身")
	f.BloodType = pick(pairs, "血液型")
	if h := pick(pairs, "身長"); h != "" {
		f.Height = digitsOnly(h)
	}
	if sizes := pick(pairs, "スリーサイズ"); sizes != "" {
		f.Bust, f.Waist, f.Hip = parseThreeSizes(sizes)
		// 罩杯就写在三围里（"(Gカップ)"）—— 只在这个字段里找，
		// 别全页扫：页面上到处是「同じGカップの女優一覧」这类链接，会认错人。
		f.Cup = parseCup(sizes)
	}
	f.Agency = pick(pairs, "所属", "事務所")
	f.AgencySpan = pick(pairs, "所属期間")
	f.DebutDate = normDate(pick(pairs, "デビュー日", "デビュー"))
	f.DebutSpan = pick(pairs, "活動期間")
	f.Hobby = pick(pairs, "趣味・特技", "趣味")
	f.Aliases = avDBAliases(doc)
	f.Summary = avDBSummary(doc)

	if f.isEmpty() {
		return nil
	}
	return f
}

// parseCup 从三围字符串里取罩杯字母（「(Gカップ)」「G cup」）。
func parseCup(s string) string {
	m := regexp.MustCompile(`(?i)([A-Z])\s*(?:カップ|cup)`).FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	return strings.ToUpper(m[1])
}

// parseThreeSizes 解析「B93 / W61 / H89」「B93 (Gカップ) W61 H89」这类写法。
func parseThreeSizes(s string) (bust, waist, hip string) {
	get := func(prefix string) string {
		m := regexp.MustCompile(`(?i)` + prefix + `\s*:?\s*(\d{2,3})`).FindStringSubmatch(s)
		if m == nil {
			return ""
		}
		return m[1]
	}
	return get("B"), get("W"), get("H")
}

// avDBAliases 取 <dt>別名/旧名</dt> 后面 <dd> 里的按钮/链接文字。
func avDBAliases(doc *html.Node) []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		s = cleanCandidateName(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, dl := range htmlFindAll(doc, func(n *html.Node) bool { return isElem(n, "dl") }) {
		var isAlias bool
		for c := dl.FirstChild; c != nil; c = c.NextSibling {
			if isElem(c, "dt") {
				isAlias = strings.Contains(htmlText(c), "別名")
				continue
			}
			if !isAlias || !isElem(c, "dd") {
				continue
			}
			// dd 里可能是 button 或 a，逐个取文字
			for _, el := range htmlFindAll(c, func(n *html.Node) bool {
				return isElem(n, "button") || isElem(n, "a")
			}) {
				add(htmlText(el))
			}
			if len(out) == 0 {
				add(htmlText(c))
			}
		}
	}
	return out
}

// avDBSummary 取资料区那段自由简介（通常是 <section> 里第一段较长的 <p>）。
func avDBSummary(doc *html.Node) string {
	best := ""
	for _, p := range htmlFindAll(doc, func(n *html.Node) bool { return isElem(n, "p") }) {
		t := strings.TrimSpace(htmlText(p))
		// 简介的特征：含「は、日本」或「出身」且够长
		if len([]rune(t)) < 30 || len([]rune(t)) > 600 {
			continue
		}
		if strings.Contains(t, "日本") && (strings.Contains(t, "出身") || strings.Contains(t, "女優")) {
			if len([]rune(t)) > len([]rune(best)) {
				best = t
			}
		}
	}
	return best
}

func (f *ActorFacts) isEmpty() bool {
	return f.BirthDate == "" && f.BirthPlace == "" && f.Height == "" &&
		f.Bust == "" && f.BloodType == "" && f.DebutDate == "" &&
		f.Hobby == "" && f.Agency == "" && f.Summary == "" && len(f.Aliases) == 0
}

// ---------- 源 2：AV-League ----------

type avLeagueSource struct{}

func (s *avLeagueSource) Key() string   { return "AvLeague" }
func (s *avLeagueSource) Label() string { return "AV-League" }

const avLeagueBase = "https://www.av-league.com"

var avLeagueLinkRe = regexp.MustCompile(`^/actress/(\d+)\.html$`)

func (s *avLeagueSource) Fetch(ctx context.Context, client *http.Client, name string, aliases []string) (*ActorFacts, error) {
	start := time.Now()
	searchURL := avLeagueBase + "/search/search.php?k=" + url.QueryEscape(name)
	doc, _, err := fetchHTML(ctx, client, searchURL)
	if err != nil {
		return nil, fmt.Errorf("AV-League 搜索失败：%w", err)
	}

	type cand struct{ id, name string }
	var cands []cand
	seen := map[string]bool{}
	for _, a := range htmlFindAll(doc, func(n *html.Node) bool { return isElem(n, "a") }) {
		m := avLeagueLinkRe.FindStringSubmatch(htmlAttr(a, "href"))
		if m == nil || seen[m[1]] {
			continue
		}
		txt := cleanCandidateName(htmlText(a))
		// 搜索结果里链接文字常带主演作品名，用 alt/img 或标题兜底
		if txt == "" {
			txt = cleanCandidateName(htmlAttr(a, "title"))
		}
		if txt == "" {
			continue
		}
		// 链接文字可能很长（作品名），截到名字长度附近没有可靠办法，
		// 这里保留原文，交给 nameMatchScore 的「包含」分支判定。
		seen[m[1]] = true
		cands = append(cands, cand{id: m[1], name: txt})
	}
	if len(cands) == 0 {
		return nil, nil
	}
	best := cands[0]
	bestScore := -1
	for _, c := range cands {
		if sc := nameMatchScore(name, c.name, aliases); sc > bestScore {
			best, bestScore = c, sc
		}
	}
	if bestScore < minMatchScore {
		return nil, nil
	}

	pageURL := fmt.Sprintf("%s/actress/%s.html", avLeagueBase, best.id)
	doc2, _, err := fetchHTML(ctx, client, pageURL)
	if err != nil {
		return nil, fmt.Errorf("AV-League 详情页失败：%w", err)
	}

	// 详情页 h1（「坂井なな（さかいなな）」）才是权威姓名，去掉括号读音后确认
	h1 := splitReading(textOf(firstTag(doc2, "h1")))
	score, ok, _ := confirmDetailName(name, aliases, h1.name, bestScore)
	if !ok {
		return nil, nil
	}
	matched := h1.name
	if matched == "" {
		matched = best.name
	}
	f := parseAVLeagueDoc(doc2, best.id, matched, score)
	if f == nil {
		return nil, nil
	}
	f.SourceURL = pageURL
	f.ElapsedMS = time.Since(start).Milliseconds()
	return f, nil
}

// parseAVLeagueDoc 解析 AV-League 的演员详情页。与网络请求分开便于用夹具回归。
func parseAVLeagueDoc(doc *html.Node, id, name string, score int) *ActorFacts {
	src := &avLeagueSource{}
	f := &ActorFacts{
		Source:      src.Key(),
		SourceLabel: src.Label(),
		MatchScore:  score,
		MatchedName: name,
		ProviderID:  id,
	}
	// 页头 h1 形如「坂井なな（さかいなな）」，括号里是读音 → 当别名。
	// 注意：IndexAny 返回的是**字节**下标，全角括号占 3 字节，
	// 直接 h1[i+1:] 会切进多字节字符中间产生乱码 —— 必须按 rune 切。
	if h1 := textOf(firstTag(doc, "h1")); h1 != "" {
		if r := splitReading(h1); r.name != "" {
			f.MatchedName = r.name
			if r.reading != "" {
				f.Aliases = append(f.Aliases, r.reading)
			}
		} else if n := cleanCandidateName(h1); n != "" {
			f.MatchedName = n
		}
	}

	pairs := trPairs(doc, "info-tbl")
	f.BirthDate = normDate(pick(pairs, "生年月日", "誕生日"))
	f.BirthPlace = pick(pairs, "出身", "出身地")
	f.BloodType = pick(pairs, "血液型")
	if h := pick(pairs, "身長"); h != "" {
		f.Height = digitsOnly(h)
	}
	if sizes := pick(pairs, "3サイズ", "スリーサイズ"); sizes != "" {
		f.Bust, f.Waist, f.Hip = parseThreeSizes(sizes)
		f.Cup = parseCup(sizes)
	}
	f.DebutDate = normDate(pick(pairs, "デビュー"))
	f.Tags = append(f.Tags, splitList(pick(pairs, "タグ"))...)
	// 简介就用页面的 meta description（含「名前：/生年月日：」这类摘要）
	f.Summary = metaContent(doc, "description")

	if f.isEmpty() {
		return nil
	}
	return f
}

// metaContent 取 <meta name="..."> 的 content。
func metaContent(doc *html.Node, name string) string {
	for _, m := range htmlFindAll(doc, func(n *html.Node) bool { return isElem(n, "meta") }) {
		if strings.EqualFold(htmlAttr(m, "name"), name) {
			if c := strings.TrimSpace(htmlAttr(m, "content")); c != "" {
				return c
			}
		}
	}
	return ""
}

// firstTag 找文档里第一个指定标签。
func firstTag(doc *html.Node, tag string) *html.Node {
	found := htmlFind(doc, func(n *html.Node) bool { return isElem(n, tag) })
	return found
}

// splitList 按中英文顿号/逗号拆列表。
func splitList(s string) []string {
	var out []string
	for _, part := range regexp.MustCompile(`[、,，/]`).Split(s, -1) {
		if p := cleanValue(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ---------- 源 3：Wikipedia ----------

type wikipediaSource struct{}

func (s *wikipediaSource) Key() string   { return "Wikipedia" }
func (s *wikipediaSource) Label() string { return "Wikipedia" }

var wikiLangs = []struct {
	lang, label string
}{
	{"ja", "日文"},
	{"zh", "中文"},
}

type wikiAPIResp struct {
	Query struct {
		Pages map[string]struct {
			Title    string `json:"title"`
			Missing  string `json:"missing"`
			Extract  string `json:"extract"`
			Redirect string `json:"redirect"`
		} `json:"pages"`
	} `json:"query"`
}

func (s *wikipediaSource) Fetch(ctx context.Context, client *http.Client, name string, aliases []string) (*ActorFacts, error) {
	start := time.Now()
	// 依次拿主名与别名去查，第一个查到的就用。
	candidates := append([]string{name}, aliases...)
	for _, lang := range wikiLangs {
		for _, cand := range candidates {
			cand = strings.TrimSpace(cand)
			if cand == "" {
				continue
			}
			f, err := s.query(ctx, client, lang.lang, lang.label, cand, name, aliases)
			if err != nil {
				return nil, err
			}
			if f != nil {
				f.ElapsedMS = time.Since(start).Milliseconds()
				return f, nil
			}
		}
	}
	return nil, nil
}

func (s *wikipediaSource) query(ctx context.Context, client *http.Client, lang, langLabel, title, want string, aliases []string) (*ActorFacts, error) {
	u := fmt.Sprintf("https://%s.wikipedia.org/w/api.php?action=query&format=json&redirects=1"+
		"&prop=extracts&explaintext=1&exintro=1&titles=%s", lang, url.QueryEscape(title))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", browserUA)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Wikipedia(%s) 请求失败：%w", langLabel, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("Wikipedia(%s) HTTP %d", langLabel, resp.StatusCode)
	}
	body, err := readAllLimit(resp.Body, 4<<20)
	if err != nil {
		return nil, err
	}
	var r wikiAPIResp
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("Wikipedia(%s) 响应解析失败：%w", langLabel, err)
	}
	for _, pg := range r.Query.Pages {
		if pg.Missing != "" || strings.TrimSpace(pg.Extract) == "" {
			continue
		}
		got := pg.Title
		score := nameMatchScore(want, got, aliases)
		if score < minMatchScore {
			// 用别名命中的情况：标题就是别名
			for _, a := range aliases {
				if normName(a) == normName(got) {
					score = 95
					break
				}
			}
		}
		if score < minMatchScore {
			continue
		}
		f := &ActorFacts{
			Source:      s.Key(),
			SourceLabel: s.Label() + "（" + langLabel + "）",
			SourceURL:   fmt.Sprintf("https://%s.wikipedia.org/wiki/%s", lang, url.PathEscape(got)),
			MatchScore:  score,
			MatchedName: got,
			ProviderID:  got,
		}
		parseWikiExtract(f, pg.Extract)
		if f.isEmpty() {
			continue
		}
		return f, nil
	}
	return nil, nil
}

// wikiReadingRe 抓「（わかみや ほの、1996年10月5日 - ）」这种开头的读音/生日。
var (
	wikiReadingRe = regexp.MustCompile(`^([ぁ-んァ-ヶー・\s]{2,20})[、,]`)
	wikiBornPlace = regexp.MustCompile(`([^\s。、]{1,12}[都道府県])出身`)
)

func parseWikiExtract(f *ActorFacts, text string) {
	text = strings.TrimSpace(text)
	f.Summary = clipRunes(text, 600)

	// 开头的读音：取第一个括号里的第一段（日文源才有）
	if i := strings.IndexAny(text, "（("); i > 0 && i < 60 {
		j := strings.IndexAny(text[i:], "）)")
		if j > 1 {
			inner := text[i+1 : i+j]
			if m := wikiReadingRe.FindStringSubmatch(inner); m != nil {
				if r := cleanValue(m[1]); r != "" && normName(r) != normName(f.MatchedName) {
					f.Aliases = append(f.Aliases, r)
				}
			}
			if d := normDate(inner); d != "" {
				f.BirthDate = d
			}
		}
	}
	if f.BirthDate == "" {
		f.BirthDate = normDate(text)
	}
	if m := wikiBornPlace.FindStringSubmatch(text); m != nil {
		f.BirthPlace = cleanValue(m[1])
	}
}

func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// ---------- 合并：facts → 可写入的字段清单 ----------

// mergedFacts 是按源优先级合并后的「单一事实集」。
type mergedFacts struct {
	fields map[string]string // 字段 key → 值
	source map[string]string // 字段 key → 来源显示名
}

func newMergedFacts() *mergedFacts {
	return &mergedFacts{fields: map[string]string{}, source: map[string]string{}}
}

// put 只在字段还空的时候写入 —— 调用方按优先级顺序遍历 facts 即可实现「先到先得」。
func (m *mergedFacts) put(key, val, src string) {
	if strings.TrimSpace(val) == "" || m.fields[key] != "" {
		return
	}
	m.fields[key] = strings.TrimSpace(val)
	m.source[key] = src
}

// mergeFacts 按传入顺序（即源优先级）合并。
func mergeFacts(facts []ActorFacts) *mergedFacts {
	m := newMergedFacts()
	for i := range facts {
		f := &facts[i]
		m.put("birth_date", f.BirthDate, f.SourceLabel)
		m.put("birth_place", f.BirthPlace, f.SourceLabel)
		m.put("height", f.Height, f.SourceLabel)
		m.put("bust", f.Bust, f.SourceLabel)
		m.put("waist", f.Waist, f.SourceLabel)
		m.put("hip", f.Hip, f.SourceLabel)
		m.put("cup", f.Cup, f.SourceLabel)
		m.put("blood_type", f.BloodType, f.SourceLabel)
		m.put("debut_date", f.DebutDate, f.SourceLabel)
		m.put("debut_span", f.DebutSpan, f.SourceLabel)
		m.put("hobby", f.Hobby, f.SourceLabel)
		m.put("agency", f.Agency, f.SourceLabel)
		m.put("agency_span", f.AgencySpan, f.SourceLabel)
		m.put("summary", f.Summary, f.SourceLabel)
	}
	return m
}

// buildOverview 生成结构化简介（中文标签 + 源站原文值）。
//
// 版式对齐原版「Emby演员扩展器」的 Overview：一行一个字段，写回 Emby 的
// 演员简介也长这样。字段缺失的行直接不输出，不留「不明」。
func buildOverview(m *mergedFacts) []string {
	var lines []string
	add := func(label, key string, suffix string) {
		if v := m.fields[key]; v != "" {
			lines = append(lines, label+"："+v+suffix)
		}
	}
	agency := m.fields["agency"]
	if agency != "" {
		if span := m.fields["agency_span"]; span != "" {
			agency += "（" + span + "）"
		}
		lines = append(lines, "事务所："+agency)
	}
	add("出生日期", "birth_date", "")
	add("出生地", "birth_place", "")
	add("身高", "height", " cm")
	if m.fields["bust"] != "" || m.fields["waist"] != "" || m.fields["hip"] != "" {
		lines = append(lines, fmt.Sprintf("三围：B%s / W%s / H%s",
			orDash(m.fields["bust"]), orDash(m.fields["waist"]), orDash(m.fields["hip"])))
	}
	add("罩杯", "cup", " 杯")
	add("血型", "blood_type", "")
	add("出道", "debut_date", "")
	add("兴趣", "hobby", "")
	return lines
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ProfileField 是一个「可能写入 Emby 的字段」的最终状态，直接喂给前端做对比表。
type ProfileField struct {
	Key       string `json:"key"`
	Label     string `json:"label"`
	Value     string `json:"value"`      // 抓到的值
	Source    string `json:"source"`     // 来自哪个源
	EmbyValue string `json:"emby_value"` // Emby 里现有的值
	WillWrite bool   `json:"will_write"` // 只有「Emby 空 && 抓到非空」才为 true
	Note      string `json:"note"`
}

// ActorProfile 是一次抓取的最终结果。
type ActorProfile struct {
	Name     string         `json:"name"`
	PersonID string         `json:"person_id"`
	Facts    []ActorFacts   `json:"facts"`
	Fields   []ProfileField `json:"fields"`
	Aliases  []string       `json:"aliases"`
	Overview string         `json:"overview"`
	// 注意：**没有 Tags**。这个构建对 Person 不保存标签，详情见 buildActorProfile
	// 里把标签并进简介那段注释。
	Sources  []string `json:"sources"`
	Warnings []string `json:"warnings"`
	// WriteCount 是本次「会真正写入」的字段数；0 表示 Emby 里该有的都有了。
	WriteCount int `json:"write_count"`
}

// embyExisting 是 Emby 里当前的演员资料（只取我们要管的字段）。
type embyExisting struct {
	Overview   string
	Year       string
	Birth      string
	Locations  string
	ProviderID string // 该来源对应的 ProviderIds 键值
}

// buildActorProfile 把抓到的 facts 与 Emby 现有值合并成结果。
//
// **写入策略：只填空白。** Emby 里已有值的字段一律不动 —— 这是用户明确选的策略，
// 因为库里 1400 个演员已经有头像/资料，全量覆盖会毁数据。
func buildActorProfile(name, personID string, facts []ActorFacts, ex embyExisting) *ActorProfile {
	p := &ActorProfile{Name: name, PersonID: personID, Facts: facts}
	m := mergeFacts(facts)

	// 别名收集（去重、去掉与本人同名的）
	seen := map[string]bool{}
	addAlias := func(s string) string {
		s = strings.TrimSpace(s)
		n := normName(s)
		if s == "" || n == "" || n == normName(name) || seen[n] {
			return ""
		}
		seen[n] = true
		return s
	}
	for i := range facts {
		for _, a := range facts[i].Aliases {
			if v := addAlias(a); v != "" {
				p.Aliases = append(p.Aliases, v)
			}
		}
	}

	// 结构化简介
	lines := buildOverview(m)
	// 各源的标签（目前只有 AVデータバンク的「タグ」）并成简介的最后一行。
	//
	// **为什么不写进 Emby 的 Tags**：这个构建对 Person **根本不保存 Tags** ——
	// 实测 POST /Items/{id} 返回 204，但详情、列表、TagItems、全局 /Tags 字典里
	// 全都读不回来。写进去的后果是三重的：每次运行都因为「读不到 → 判定为空」
	// 而重复写、界面上谎报「已写入标签」、而且用户哪天在 Emby 里手改了标签，
	// 会被下一次同步直接覆盖掉。并进简介里这些字的归属就清楚了。
	var srcTags []string
	for i := range facts {
		srcTags = append(srcTags, facts[i].Tags...)
	}
	if srcTags = dedupeStrings(srcTags); len(srcTags) > 0 {
		lines = append(lines, "标签："+strings.Join(srcTags, "、"))
	}
	p.Overview = strings.Join(lines, "\n")

	// ProviderIds：每个命中的源各写一个
	prov := map[string]string{}
	for i := range facts {
		if facts[i].ProviderID != "" {
			prov[facts[i].Source] = facts[i].ProviderID
		}
	}

	// 逐字段比对（只填空白）
	add := func(key, label, val, src, embyVal string) {
		f := ProfileField{Key: key, Label: label, Value: val, Source: src, EmbyValue: embyVal}
		switch {
		case val == "":
			f.Note = "未抓取到"
		case embyVal != "":
			f.Note = "Emby 已有值，跳过"
		default:
			f.WillWrite = true
			f.Note = "将写入"
		}
		p.Fields = append(p.Fields, f)
	}
	add("overview", "简介", p.Overview, sourceOf(m, "summary"), ex.Overview)
	add("premiere_date", "出生日期", m.fields["birth_date"], sourceOf(m, "birth_date"), ex.Birth)
	add("production_year", "出生年份", yearOf(m.fields["birth_date"]), sourceOf(m, "birth_date"), ex.Year)
	add("production_locations", "出生地", m.fields["birth_place"], sourceOf(m, "birth_place"), ex.Locations)
	add("provider_ids", "外部 ID", providerSummary(prov), sourceOf(m, "summary"), ex.ProviderID)

	for _, f := range p.Fields {
		if f.WillWrite {
			p.WriteCount++
		}
	}
	if len(facts) == 0 {
		p.Warnings = append(p.Warnings, "所有资料源都没命中，未做任何修改")
	}
	for i := range facts {
		p.Sources = append(p.Sources, facts[i].SourceLabel)
	}
	return p
}

func sourceOf(m *mergedFacts, key string) string {
	if s, ok := m.source[key]; ok {
		return s
	}
	return ""
}

// yearOf 从 YYYY-MM-DD 取年份。
func yearOf(date string) string {
	if len(date) >= 4 && reDigits.MatchString(date[:4]) {
		return date[:4]
	}
	return ""
}

func providerSummary(prov map[string]string) string {
	if len(prov) == 0 {
		return ""
	}
	keys := make([]string, 0, len(prov))
	for k := range prov {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, k+": "+prov[k])
	}
	return strings.Join(parts, "\n")
}

// dedupeStrings 复用 cnmedia.go 里的实现（同语义：去空白、保序、去重）。
