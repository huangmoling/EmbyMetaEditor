package main

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// ---------- 基础工具 ----------

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}

func truncateBytes(b []byte, n int) []byte {
	if len(b) > n {
		return b[:n]
	}
	return b
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// newHTTPClient 按配置构造带代理/TLS 选项的 HTTP 客户端。
func newHTTPClient(cfg Config) *http.Client {
	tr := &http.Transport{
		MaxIdleConns:        128,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: cfg.InsecureTLS}, //nolint:gosec // 自签证书的本地 Emby 场景
	}
	if p := strings.TrimSpace(cfg.Proxy); p != "" {
		if pu, err := url.Parse(p); err == nil {
			tr.Proxy = http.ProxyURL(pu)
		}
	} else {
		tr.Proxy = http.ProxyFromEnvironment
	}
	return &http.Client{Transport: tr, Timeout: 180 * time.Second}
}

// cacheDir 返回缓存目录并确保存在。
func cacheDir() string {
	d := filepath.Join(dataDir(), "cache")
	_ = os.MkdirAll(d, 0o755)
	return d
}

// readJSONFile 读取并反序列化 JSON 文件。
func readJSONFile(path string, out any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

// writeJSONFile 原子写入 JSON 文件。
func writeJSONFile(path string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ---------- 名称归一化（gfriends 匹配用） ----------

var (
	reSpace  = regexp.MustCompile(`[\s　]+`)
	rePuncts = regexp.MustCompile(`[.\-·・,，。()（）\[\]【】'"“”‘’]`)
)

// toHalfWidth 把全角 ASCII 与全角空格转为半角。
func toHalfWidth(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == 0x3000:
			b.WriteRune(' ')
		case r >= 0xFF01 && r <= 0xFF5E:
			b.WriteRune(r - 0xFEE0)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// normName 归一化人名，用于宽松匹配。
func normName(s string) string {
	s = toHalfWidth(strings.TrimSpace(s))
	s = strings.ToLower(s)
	s = reSpace.ReplaceAllString(s, "")
	s = rePuncts.ReplaceAllString(s, "")
	return s
}

// nameVariants 生成用于匹配的候选写法。
func nameVariants(name string) []string {
	base := normName(name)
	if base == "" {
		return nil
	}
	out := []string{base}
	// 去掉常见后缀/前缀噪声
	for _, cut := range []string{"(中文)", "-无码", "-有码"} {
		out = append(out, strings.TrimSuffix(base, normName(cut)))
	}
	// 中译名里的空格写法已由 normName 处理；再补一个去掉「的」等助词的弱化形式
	seen := map[string]bool{}
	var uniq []string
	for _, v := range out {
		if v != "" && !seen[v] {
			seen[v] = true
			uniq = append(uniq, v)
		}
	}
	return uniq
}

// ---------- 番号归一化 ----------

var reNumber = regexp.MustCompile(`(?i)([A-Z]{2,10})[-_ ]?(\d{2,6})`)

// reCanon 匹配「规范形式」的番号，允许 1 位数字（numKeys 会把 SSIS-001 压成 SSIS-1）。
var reCanon = regexp.MustCompile(`(?i)^([A-Z]{2,10})[-_ ]?(\d{1,6})$`)

// numKeys 从任意字符串中提取番号的多种规范写法（SSIS-001 / SSIS001 / SSIS-1）。
func numKeys(s string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(k string) {
		if k != "" && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	for _, m := range reNumber.FindAllStringSubmatch(strings.ToUpper(s), -1) {
		prefix := m[1]
		num := strings.TrimLeft(m[2], "0")
		if num == "" {
			num = "0"
		}
		add(prefix + "-" + num)
		add(prefix + num)
	}
	return out
}

// padNumber 去掉前导零后补齐到至少 3 位。
func padNumber(n string) string {
	n = strings.TrimLeft(n, "0")
	if n == "" {
		n = "0"
	}
	for len(n) < 3 {
		n = "0" + n
	}
	return n
}

// canonNumber 把单个番号字符串规范成 PREFIX-NUM 形式，失败返回空串。
func canonNumber(raw string) string {
	ks := numKeys(raw)
	if len(ks) == 0 {
		return ""
	}
	return ks[0]
}

// displayNumber 尽量返回「带横线 + 补零到 3 位」的展示形式。
func displayNumber(raw string) string {
	s := strings.ToUpper(strings.TrimSpace(raw))
	if m := reNumber.FindStringSubmatch(s); m != nil {
		return m[1] + "-" + padNumber(m[2])
	}
	// 已经是被压缩过的规范形式（例如 SSIS-1）
	if m := reCanon.FindStringSubmatch(s); m != nil {
		return m[1] + "-" + padNumber(m[2])
	}
	return s
}

// ---------- HTML 解析辅助 ----------

// htmlFindAll 深度优先收集所有满足条件的节点。
func htmlFindAll(n *html.Node, pred func(*html.Node) bool) []*html.Node {
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(x *html.Node) {
		if pred(x) {
			out = append(out, x)
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return out
}

// htmlFind 返回第一个满足条件的节点。
func htmlFind(n *html.Node, pred func(*html.Node) bool) *html.Node {
	var found *html.Node
	var walk func(*html.Node) bool
	walk = func(x *html.Node) bool {
		if pred(x) {
			found = x
			return true
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			if walk(c) {
				return true
			}
		}
		return false
	}
	walk(n)
	return found
}

func isElem(n *html.Node, tag string) bool {
	return n.Type == html.ElementNode && (tag == "" || n.Data == tag)
}

func htmlAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

func htmlClass(n *html.Node) string { return htmlAttr(n, "class") }

func htmlHasClass(n *html.Node, cls string) bool {
	for _, f := range strings.Fields(htmlClass(n)) {
		if f == cls {
			return true
		}
	}
	return false
}

// htmlText 拼接节点下所有文本。
func htmlText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(x *html.Node) {
		if x.Type == html.TextNode {
			b.WriteString(x.Data)
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.Join(strings.Fields(b.String()), " ")
}

// htmlInnerText 只取直接文本（不深入子元素），用于 <span>ABC-123<br><date>..</date></span>。
func htmlDirectText(n *html.Node) string {
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.TextNode {
			b.WriteString(c.Data)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// ---------- 杂项 ----------

// itoa 便捷整数转字符串。
func itoa(i int) string { return strconv.Itoa(i) }

// readAllLimit 读取响应体并限制大小。
func readAllLimit(r io.Reader, limit int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, limit))
}

// dumpHTML 把抓到的页面落到 cache/debug/ 下，便于排查解析失败。
// 返回落盘路径，失败返回空串（诊断用途，不能影响主流程）。
func dumpHTML(name string, data []byte) string {
	if len(data) == 0 {
		return ""
	}
	dir := filepath.Join(cacheDir(), "debug")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	path := filepath.Join(dir, name+"-"+time.Now().Format("20060102-150405")+".html")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return ""
	}
	return path
}

// pageMarkers 统计页面里关键结构标记的出现次数，用于诊断「抓到了但解析不出来」。
func pageMarkers(data []byte) map[string]int {
	markers := []string{
		"movie-box", "photo-frame", "photo-info", "bigImage",
		"/star/", "searchstar", "uncledatoolsbyajax", "pics/cover",
		"age=verified", "cf-browser-verification", "Just a moment",
	}
	out := make(map[string]int, len(markers))
	for _, m := range markers {
		out[m] = bytes.Count(data, []byte(m))
	}
	return out
}
