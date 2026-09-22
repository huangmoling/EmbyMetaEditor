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

// gfriendsTreeCandidates 给出索引文件的多个候选地址，保证单点故障时仍可下载。
func gfriendsTreeCandidates(primary string) []string {
	set := []string{}
	add := func(u string) {
		u = strings.TrimSpace(u)
		if u == "" {
			return
		}
		for _, e := range set {
			if e == u {
				return
			}
		}
		set = append(set, u)
	}
	add(primary)
	// 依据主地址推断同源备用地址
	if strings.Contains(primary, "jsdelivr.net") || strings.Contains(primary, "githubusercontent") || primary == "" {
		add("https://cdn.jsdelivr.net/gh/gfriends/gfriends@master/Filetree.json")
		add("https://gcore.jsdelivr.net/gh/gfriends/gfriends@master/Filetree.json")
		add("https://fastly.jsdelivr.net/gh/gfriends/gfriends@master/Filetree.json")
		add("https://raw.githubusercontent.com/gfriends/gfriends/master/Filetree.json")
	}
	return set
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
