package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// GfriendEntry 是 gfriends 头像库中的一条记录。
type GfriendEntry struct {
	Group   string `json:"g"`  // 来源分组目录
	File    string `json:"f"`  // 仓库内实际文件名（可能带 ?t= 时间戳）
	AIFix   bool   `json:"ai"` // 是否为 AI 修复版本
	GroupZh string `json:"gz"` // 分组中文备注（若可推断）
}

// URL 依据 CDN 基址拼出可访问的图片地址。
func (g GfriendEntry) URL(cdn string) string {
	base := strings.TrimRight(cdn, "/")
	file := g.File
	query := ""
	if i := strings.Index(file, "?"); i >= 0 {
		query = file[i:]
		file = file[:i]
	}
	return base + "/Content/" + url.PathEscape(g.Group) + "/" + url.PathEscape(file) + query
}

// gfriendsFile 是 Filetree.json 的顶层结构。
type gfriendsFile struct {
	Content     map[string]map[string]string `json:"Content"`
	Information struct {
		TotalNum int `json:"TotalNum"`
	} `json:"Information"`
}

// Gfriends 管理头像索引的下载、缓存与查询。
type Gfriends struct {
	mu          sync.RWMutex
	byName      map[string][]GfriendEntry
	loadedAt    time.Time
	lastAttempt time.Time
	loading     bool
	lastErr     string
	total       int
	cfg         func() Config
	cacheP      string
}

// gfriendsTTL 索引新鲜度；超过则重新下载。
const gfriendsTTL = 7 * 24 * time.Hour

// gfriendsFailCooldown 下载失败后的冷却时间，避免批量任务里反复重试拖慢整体。
const gfriendsFailCooldown = 30 * time.Second

// NewGfriends 构造头像库管理器，索引缓存在 cache/gfriends_index.json。
func NewGfriends(cfgFn func() Config) *Gfriends {
	return &Gfriends{
		byName: map[string][]GfriendEntry{},
		cfg:    cfgFn,
		cacheP: filepath.Join(cacheDir(), "gfriends_index.json"),
	}
}

type gfriendsCache struct {
	Timestamp time.Time                 `json:"timestamp"`
	Total     int                       `json:"total"`
	Index     map[string][]GfriendEntry `json:"index"`
}

// Status 返回当前索引状态，供前端展示。
func (g *Gfriends) Status() map[string]any {
	g.mu.RLock()
	defer g.mu.RUnlock()
	st := map[string]any{
		"loaded":     len(g.byName) > 0,
		"names":      len(g.byName),
		"total":      g.total,
		"loading":    g.loading,
		"last_error": g.lastErr,
	}
	if !g.loadedAt.IsZero() {
		st["loaded_at"] = g.loadedAt.Format(time.RFC3339)
	}
	return st
}

// LoadFromCache 尝试从本地缓存加载索引。
func (g *Gfriends) LoadFromCache() bool {
	var c gfriendsCache
	if err := readJSONFile(g.cacheP, &c); err != nil {
		return false
	}
	if len(c.Index) == 0 {
		return false
	}
	g.mu.Lock()
	g.byName = c.Index
	g.total = c.Total
	g.loadedAt = c.Timestamp
	g.mu.Unlock()
	return true
}

// EnsureLoaded 确保索引可用：本地缓存新鲜就直接用；过期或强制时重新下载。
// 已有旧索引时下载失败会降级使用旧索引，不会让调用方失败。
func (g *Gfriends) EnsureLoaded(ctx context.Context, force bool) error {
	g.mu.RLock()
	has := len(g.byName) > 0
	fresh := !g.loadedAt.IsZero() && time.Since(g.loadedAt) < gfriendsTTL
	recentFail := !g.lastAttempt.IsZero() && time.Since(g.lastAttempt) < gfriendsFailCooldown
	lastErr := g.lastErr
	g.mu.RUnlock()

	if has && fresh && !force {
		return nil
	}
	// 手上没有任何索引，且刚刚才失败过，先不要再撞一次网络
	if !has && recentFail && !force {
		return fmt.Errorf("gfriends 索引暂不可用：%s", lastErr)
	}
	err := g.Download(ctx)
	if err != nil && has {
		return nil // 降级：继续用旧索引
	}
	return err
}

// Download 拉取 Filetree.json 并重建索引。
func (g *Gfriends) Download(ctx context.Context) error {
	g.mu.Lock()
	if g.loading {
		g.mu.Unlock()
		return fmt.Errorf("索引正在加载中")
	}
	g.loading = true
	g.lastAttempt = time.Now()
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.loading = false
		g.mu.Unlock()
	}()

	cfg := g.cfg()
	candidates := gfriendsTreeCandidates(cfg.GfriendsTreeURL)
	client := newHTTPClient(cfg)
	var lastErr error
	var raw []byte
	for _, u := range candidates {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 "+embyClientName)
		resp, err := client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", u, err)
			continue
		}
		data, rerr := readAllLimit(resp.Body, 128<<20)
		resp.Body.Close()
		if rerr != nil {
			lastErr = rerr
			continue
		}
		if resp.StatusCode >= 400 || len(data) < 1024 {
			lastErr = fmt.Errorf("%s 返回 %d", u, resp.StatusCode)
			continue
		}
		raw = data
		break
	}
	if raw == nil {
		g.mu.Lock()
		g.lastErr = fmt.Sprint(lastErr)
		g.mu.Unlock()
		return fmt.Errorf("下载 gfriends 索引失败：%v", lastErr)
	}

	var ff gfriendsFile
	if err := json.Unmarshal(raw, &ff); err != nil {
		g.mu.Lock()
		g.lastErr = err.Error()
		g.mu.Unlock()
		return fmt.Errorf("解析 Filetree.json 失败：%w", err)
	}

	idx := make(map[string][]GfriendEntry, 120000)
	total := 0
	for group, actors := range ff.Content {
		for fname, val := range actors {
			name := strings.TrimSuffix(fname, filepath.Ext(fname))
			key := normName(name)
			if key == "" {
				continue
			}
			entry := GfriendEntry{
				Group:   group,
				File:    val,
				AIFix:   strings.HasPrefix(strings.ToLower(val), "ai-fix-"),
				GroupZh: groupZh(group),
			}
			idx[key] = append(idx[key], entry)
			total++
		}
	}
	if total == 0 {
		return fmt.Errorf("索引为空，请检查地址是否正确")
	}
	// 每个演员的候选排序：原图优先，其次按分组名稳定排序
	for k := range idx {
		list := idx[k]
		sort.SliceStable(list, func(i, j int) bool {
			if list[i].AIFix != list[j].AIFix {
				return !list[i].AIFix
			}
			return list[i].Group < list[j].Group
		})
		idx[k] = list
	}

	g.mu.Lock()
	g.byName = idx
	g.total = total
	g.loadedAt = time.Now()
	g.lastErr = ""
	g.mu.Unlock()

	_ = writeJSONFile(g.cacheP, gfriendsCache{Timestamp: time.Now(), Total: total, Index: idx})
	return nil
}

// Lookup 精确匹配演员头像，返回全部候选（已排序）。
func (g *Gfriends) Lookup(name string) []GfriendEntry {
	g.mu.RLock()
	defer g.mu.RUnlock()
	for _, v := range nameVariants(name) {
		if list, ok := g.byName[v]; ok && len(list) > 0 {
			return list
		}
	}
	return nil
}

// Fuzzy 模糊搜索演员，返回 key -> 候选，最多 limit 个演员。
func (g *Gfriends) Fuzzy(keyword string, limit int) map[string][]GfriendEntry {
	q := normName(keyword)
	out := map[string][]GfriendEntry{}
	if q == "" {
		return out
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	for k, v := range g.byName {
		if strings.Contains(k, q) {
			out[k] = v
			if len(out) >= limit {
				break
			}
		}
	}
	return out
}

// Count 返回索引中的演员数量。
func (g *Gfriends) Count() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.byName)
}

// gfriends 头像库有**两个独立**的故障面，备用地址要同时覆盖：
//
//  1. 仓库：`gfriends/gfriends` 是主仓库，`xinxin8816/gfriends` 是内容一致的镜像
//     （Filetree.json 与全部 Content 图片逐字节相同，实测都 200）。主仓库被删 / 改名 /
//     被 jsdelivr 限流时，镜像还在。
//  2. CDN 节点：jsdelivr 对外有多个公开分片域名，后端是同一份缓存，但 DNS 与边缘节点
//     各自独立 —— 某个分片被污染或限流时，换一个往往立刻可用。
//
// 而 raw.githubusercontent.com 走的是 GitHub 自己的基础设施，连 jsdelivr 整体挂掉都能兜住。
const (
	gfriendsRepo       = "gfriends/gfriends"
	gfriendsMirrorRepo = "xinxin8816/gfriends"
	gfriendsRef        = "master"
	gfriendsTreeFile   = "Filetree.json"
)

// gfriendsCDNNodes 是 jsdelivr 的公开分片域名（按默认节点优先排序）。
var gfriendsCDNNodes = []string{"cdn", "gcore", "fastly"}

// gfriendsRepos 返回索引 / 图片的候选仓库，主仓库在前、镜像仓库在后。
func gfriendsRepos() []string {
	return []string{gfriendsRepo, gfriendsMirrorRepo}
}

// isDefaultGfriendCDN 判断地址是否落在「我们已知备用地址」的 CDN 上。
//
// 只有默认配置（jsdelivr / raw.githubusercontent）才补备用地址。用户自己填了别的
// 镜像站就原样使用 —— 我们不知道那个站点的备用地址是什么，硬塞 jsdelivr 反而
// 可能把他特意配的源绕过去（内网自建镜像就是这么用的）。
func isDefaultGfriendCDN(base string) bool {
	b := strings.TrimSpace(base)
	return b == "" || strings.Contains(b, "jsdelivr.net") || strings.Contains(b, "githubusercontent")
}

// gfriendsCDNBaseURLs 按「仓库 × 节点」展开出全部可用的 CDN 基址（带结尾斜杠）。
func gfriendsCDNBaseURLs() []string {
	out := make([]string, 0, len(gfriendsRepos())*(len(gfriendsCDNNodes)+1))
	for _, repo := range gfriendsRepos() {
		for _, node := range gfriendsCDNNodes {
			out = append(out, fmt.Sprintf("https://%s.jsdelivr.net/gh/%s@%s/", node, repo, gfriendsRef))
		}
		out = append(out, fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/", repo, gfriendsRef))
	}
	return out
}

// dedupeURLs 按顺序去重（保持首次出现的顺序）。
func dedupeURLs(urls []string) []string {
	out := make([]string, 0, len(urls))
	for _, u := range urls {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		dup := false
		for _, e := range out {
			if e == u {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, u)
		}
	}
	return out
}

// gfriendsTreeCandidates 给出索引文件的多个候选地址，保证单点故障时仍可下载。
func gfriendsTreeCandidates(primary string) []string {
	set := []string{primary}
	if isDefaultGfriendCDN(primary) {
		for _, base := range gfriendsCDNBaseURLs() {
			set = append(set, base+gfriendsTreeFile)
		}
	}
	return dedupeURLs(set)
}

// gfriendsCDNBases 给出头像**图片**的候选基址，第一项永远是用配置里的那个。
//
// 索引能换仓库、图片换不了的话只解决一半问题：cdn.jsdelivr.net 挂掉时索引照样能
// 从镜像下回来，但每个演员的候选图全下载失败，表现是「刮削头像 / 选图」整体不可用。
func gfriendsCDNBases(cdn string) []string {
	prim := strings.TrimSpace(cdn)
	// 统一成带结尾斜杠，否则 "…@master" 和 "…@master/" 会被当成两个基址各试一遍。
	if prim != "" {
		prim = strings.TrimRight(prim, "/") + "/"
	}
	set := []string{prim}
	if isDefaultGfriendCDN(cdn) {
		set = append(set, gfriendsCDNBaseURLs()...)
	}
	return dedupeURLs(set)
}

// groupZh 把分组目录名转成可读备注。
func groupZh(group string) string {
	prefix := group
	rest := ""
	if i := strings.Index(group, "-"); i > 0 && i <= 2 {
		prefix = group[:i]
		rest = group[i+1:]
	}
	switch strings.ToLower(prefix) {
	case "y":
		return "有码 · " + rest
	case "z":
		return "综合 · " + rest
	case "x":
		return "无码 · " + rest
	}
	return group
}

// GfriendsImagePath 返回缓存目录中头像的落地路径（调试用）。
func GfriendsImagePath(name string) string {
	return filepath.Join(cacheDir(), "avatars", name)
}
