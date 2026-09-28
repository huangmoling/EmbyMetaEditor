package main

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// 磁力多源：同一个番号同时问多个站点，结果合并去重。
//
// 为什么值得并发：两家收录的种子确实不一样（javbus 偏原盘/无码破解，
// javdb 中字和高清版本更全），而用户的诉求永远是「能拿到的最大那个种子」。
// 串行问两个源等于把延迟叠起来，并发问则只是「慢的那个源」的延迟。
//
// 三条设计约束：
//  1. **一个源失败不能拖垮其他源**。某个站被墙、被 Cloudflare 拦、或者这个
//     番号它没收录，都是常态；结果是「其余源照常返回 + 这一源的失败原因」。
//  2. **去重按 btih 而不是按整条链接**。同一个种子的 magnet 链接里 tracker
//     参数（&tr=…）在不同站点上不一样，按整串去重会得到一堆重复项。
//  3. **排序统一走 magnetSizeBytes**，认不出的排最后 —— 不能让某个源的
//     体积写法（"6.33GB" vs "6480"）影响合并后的顺序。
const (
	magnetSourceJavBus = "javbus"
	magnetSourceJavDB  = "javdb"
)

// magnetSourceKeys 是全部可用的磁力源，顺序即界面与日志里的顺序。
var magnetSourceKeys = []string{magnetSourceJavBus, magnetSourceJavDB}

// magnetSourceLabels 是源的展示名。
var magnetSourceLabels = map[string]string{
	magnetSourceJavBus: "javbus",
	magnetSourceJavDB:  "javdb",
}

func magnetSourceLabel(key string) string {
	if s, ok := magnetSourceLabels[key]; ok {
		return s
	}
	return key
}

// MagnetSourceStatus 是单个源在一次抓取里的结果。
type MagnetSourceStatus struct {
	Key        string `json:"key"`
	Name       string `json:"name"`
	OK         bool   `json:"ok"`
	Count      int    `json:"count"`
	ElapsedMS  int64  `json:"elapsed_ms"`
	HTTPStatus int    `json:"http_status,omitempty"`
	Error      string `json:"error,omitempty"`
}

// enabledMagnetSources 返回真正启用的源：配置里勾选的 ∩ 地址非空的。
// 顺序固定，保证界面与日志稳定。
//
// `MagnetSources == nil` 表示「这份配置没设置过这一项」，按全部可用源处理 ——
// normalize() 之后正常不会出现 nil，但直接构造 Config 的调用方（测试、
// 以后的命令行入口）不该踩「功能被静默关掉」这个坑。
func enabledMagnetSources(cfg Config) []string {
	picked := map[string]bool{}
	if cfg.MagnetSources == nil {
		for _, k := range magnetSourceKeys {
			picked[k] = true
		}
	}
	for _, k := range cfg.MagnetSources {
		picked[strings.TrimSpace(k)] = true
	}
	hasURL := map[string]bool{
		magnetSourceJavBus: strings.TrimSpace(cfg.JavBusURL) != "",
		magnetSourceJavDB:  strings.TrimSpace(cfg.JavDBURL) != "",
	}
	out := make([]string, 0, len(magnetSourceKeys))
	for _, k := range magnetSourceKeys {
		if picked[k] && hasURL[k] {
			out = append(out, k)
		}
	}
	return out
}

// magnetFetcher 是一个源的抓取入口。
//
// `title` 只在源真的给出标题时才被采用；调用方按「先到先得、非空才覆盖」
// 合并，避免某个源把标题写成番号把好的标题盖掉。
type magnetFetcher struct {
	key   string
	title string
	fetch func(ctx context.Context, number string) ([]JBMagnet, string, error)
}

// newMagnetFetchers 按配置构造启用的源。
//
// **客户端在整批抓取里只建一次**：每个客户端自带限速器，逐条新建等于每条
// 都从零开始计时，javbus 会被封、javdb 会直接甩 403「操作過於頻繁」。
func newMagnetFetchers(cfg Config) []magnetFetcher {
	var out []magnetFetcher
	for _, key := range enabledMagnetSources(cfg) {
		switch key {
		case magnetSourceJavBus:
			jb := NewJavBus(cfg)
			out = append(out, magnetFetcher{key: key, fetch: func(ctx context.Context, number string) ([]JBMagnet, string, error) {
				mv, err := jb.MovieDetail(ctx, number)
				if err != nil {
					return nil, "", err
				}
				out := make([]JBMagnet, 0, len(mv.Magnets))
				for _, m := range mv.Magnets {
					m.Source = magnetSourceJavBus
					out = append(out, m)
				}
				return out, mv.Title, nil
			}})
		case magnetSourceJavDB:
			jdb := NewJavDB(cfg)
			out = append(out, magnetFetcher{key: key, fetch: func(ctx context.Context, number string) ([]JBMagnet, string, error) {
				mags, title, err := jdb.Magnets(ctx, number)
				if err != nil {
					return nil, title, err
				}
				for i := range mags {
					mags[i].Source = magnetSourceJavDB
				}
				return mags, title, nil
			}})
		}
	}
	return out
}

// fetchOneMagnetTarget 对一个番号并发问所有源，合并结果。
func fetchOneMagnetTarget(ctx context.Context, fetchers []magnetFetcher, number string) ([]JBMagnet, string, []MagnetSourceStatus) {
	type res struct {
		i    int
		mags []JBMagnet
		it   string
		el   int64
		err  error
	}
	ch := make(chan res, len(fetchers))
	var wg sync.WaitGroup
	for i, f := range fetchers {
		wg.Add(1)
		go func(i int, f magnetFetcher) {
			defer wg.Done()
			start := time.Now().UnixMilli()
			mags, title, err := f.fetch(ctx, number)
			ch <- res{i: i, mags: mags, it: title, el: time.Now().UnixMilli() - start, err: err}
		}(i, f)
	}
	wg.Wait()
	close(ch)

	got := make([]res, 0, len(fetchers))
	for r := range ch {
		got = append(got, r)
	}
	sort.Slice(got, func(a, b int) bool { return got[a].i < got[b].i })

	var raw []JBMagnet
	statuses := make([]MagnetSourceStatus, 0, len(fetchers))
	title := ""
	for _, r := range got {
		f := fetchers[r.i]
		st := MagnetSourceStatus{Key: f.key, Name: magnetSourceLabel(f.key), ElapsedMS: r.el}
		if r.err != nil {
			st.Error = r.err.Error()
		} else {
			st.OK = true
			st.Count = len(r.mags)
			raw = append(raw, r.mags...)
			if title == "" && strings.TrimSpace(r.it) != "" {
				title = strings.TrimSpace(r.it)
			}
		}
		statuses = append(statuses, st)
	}
	// 合并去重放在这里，而不是让调用方自己拼：
	// `mergeMagnets` 单独抽出来以后曾经只在单测里被调用过 —— 生产路径直接把
	// 各源结果 append 起来就返回了，于是两个站都有的种子在界面上出现两遍。
	// 这类「helper 测得很足、但根本没接上」的错，只有把调用点写在同一个函数里
	// 才不会再犯。TestFetchOneMagnetTargetDedupsAcrossSources 盯着这个。
	return mergeMagnets(raw), title, statuses
}

// mergeMagnets 按 btih 去重（保序），再按体积从大到小排。
//
// 去重保留**先出现的那条**：源之间的顺序是固定的（javbus 在前），
// 这样同一批次里显示出来的链接是稳定的，不会每次刷新都换个 tracker 参数。
func mergeMagnets(groups ...[]JBMagnet) []JBMagnet {
	var out []JBMagnet
	seen := map[string]bool{}
	for _, g := range groups {
		for _, m := range g {
			key := magnetKey(m.Link)
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, m)
		}
	}
	sortMagnetsBySize(out)
	return out
}

// magnetKey 是磁力链接的去重键：优先 btih，取不到再退回整条链接。
//
// 必须抽 btih 的原因：同一个种子在 javbus 与 javdb 上带着不同的 tracker
// 参数，整串比较会把「同一个种子」当成两条列出来，用户以为多了个更快的源。
func magnetKey(link string) string {
	l := strings.TrimSpace(link)
	if l == "" {
		return ""
	}
	low := strings.ToLower(l)
	if i := strings.Index(low, "btih:"); i >= 0 {
		rest := l[i+len("btih:"):]
		if j := strings.IndexAny(rest, "&?#"); j >= 0 {
			rest = rest[:j]
		}
		if h := strings.ToLower(strings.TrimSpace(rest)); h != "" {
			return h
		}
	}
	return low
}

// magnetSourceSummary 把各源状态压成一句人能读的话，用于 MagnetResult.Note
// 以及任务日志 —— 「结果比上次少了 3 条」时，用户需要看到是哪一家没给。
func magnetSourceSummary(statuses []MagnetSourceStatus) string {
	var ok, bad []string
	for _, s := range statuses {
		if s.OK {
			ok = append(ok, s.Name+" "+itoa(s.Count)+" 条")
		} else {
			bad = append(bad, s.Name+"："+s.Error)
		}
	}
	parts := []string{}
	if len(ok) > 0 {
		parts = append(parts, strings.Join(ok, "，"))
	}
	if len(bad) > 0 {
		parts = append(parts, "失败 —— "+strings.Join(bad, "；"))
	}
	return strings.Join(parts, "；")
}

// magnetSourceView 是给界面的一个源的配置与状态。
type magnetSourceView struct {
	Key       string              `json:"key"`
	Name      string              `json:"name"`
	URL       string              `json:"url"`
	HasCookie bool                `json:"has_cookie"`
	Enabled   bool                `json:"enabled"`
	Status    *MagnetSourceStatus `json:"status,omitempty"`
}

// handleMagnetSources 返回磁力源的配置；带 probe=1 时顺带并发探一次连通性。
//
// 只读：探测就是各源首页发一个 GET，不改配置也不碰 Emby。
func (a *App) handleMagnetSources(w http.ResponseWriter, r *http.Request) {
	cfg := a.store.Get()
	enabled := map[string]bool{}
	for _, k := range enabledMagnetSources(cfg) {
		enabled[k] = true
	}
	urls := map[string]string{
		magnetSourceJavBus: cfg.JavBusURL,
		magnetSourceJavDB:  cfg.JavDBURL,
	}
	cookies := map[string]string{
		magnetSourceJavBus: cfg.JavBusCookie,
		magnetSourceJavDB:  cfg.JavDBCookie,
	}
	out := make([]magnetSourceView, 0, len(magnetSourceKeys))
	for _, k := range magnetSourceKeys {
		out = append(out, magnetSourceView{
			Key: k, Name: magnetSourceLabel(k), URL: urls[k],
			HasCookie: strings.TrimSpace(cookies[k]) != "", Enabled: enabled[k],
		})
	}

	if r.URL.Query().Get("probe") == "1" {
		ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
		defer cancel()
		var wg sync.WaitGroup
		for i := range out {
			if !out[i].Enabled {
				continue
			}
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				var st MagnetSourceStatus
				if out[i].Key == magnetSourceJavDB {
					st = NewJavDB(cfg).Probe(ctx)
				} else {
					p := NewJavBus(cfg).Probe(ctx, "")
					st = MagnetSourceStatus{Key: magnetSourceJavBus, Name: magnetSourceLabel(magnetSourceJavBus),
						OK: p.OK, ElapsedMS: p.ElapsedMS, HTTPStatus: p.Status}
					if !p.OK {
						st.Error = p.Message
					}
				}
				out[i].Status = &st
			}(i)
		}
		wg.Wait()
	}
	writeOK(w, map[string]any{"sources": out})
}
