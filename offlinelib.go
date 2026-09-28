package main

// 离线演员资料库：一个 **本地只读** 的资料源。
//
// 三件事先说清楚，因为它们决定了这个文件为什么长这样：
//
//  1. **数据从哪来。** 原始资料库（`20260924资料库.db`）是 SQLCipher 加密的，
//     口令硬编码在另一个工具的打包 exe 里，静态提取不出来，而且它的表结构也只是
//     那个工具的实现细节。所以本项目**不解析那个 .db**，而是用
//     `tools/export_offline_db.py` 把它导出成 JSON，这里读的是那个 JSON。
//     原始 .db 我们一个字节都不写 —— 那个库归另一个工具所有，只读是硬约束。
//
//  2. **为什么不可能提供头像。** ActorFacts 里根本没有图片字段，也就是说这个源
//     在**类型上**就无法参与头像。头像继续走各自独立的头像源顺序
//     （gfriends 那一条链），这里不是靠「约定不用」而是靠「没法用」。
//
//  3. **懒加载 + 按 mtime 失效。** 导出文件可能几十万行，启动时不该读；但也不能
//     读一次就再也不看 —— 用户重新导出一份要能生效，所以每次都 stat 一下
//     mtime/size，变了才重读。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// offlineLibSourceKey 是这个源的稳定 key。它同时会写进 ProviderIds，
// 所以别随手改 —— 改过的话老条目里的键就对不上了。
const offlineLibSourceKey = "OfflineDB"

// offlineLibAliasScore 是靠**别名**命中时的姓名置信度。
//
// 与 nameMatchScore 里「命中别名记 95」保持一致，**不另设一套**：这里只是在如实
// 标注「是按哪个名字命中的」。按本名命中给 100（库里的键就是这个写法）。
const offlineLibAliasScore = 95

// offlineLibFile 是导出文件的顶层结构（见 tools/export_offline_db.py）。
type offlineLibFile struct {
	Version int            `json:"Version"`
	Source  string         `json:"Source"`
	Note    string         `json:"Note"`
	Entries []offlineEntry `json:"Entries"`
}

// offlineEntry 是一条演员资料。
//
// 字段名与 ActorFacts **逐字对齐**（`Summary` / `ProviderID` / `DebutDate`…），
// 这样导出脚本和这里不用维护第二套映射 —— 少一层映射就少一处「改了这边忘了那边」。
type offlineEntry struct {
	Name       string   `json:"Name"`
	Aliases    []string `json:"Aliases"`
	SourceURL  string   `json:"SourceURL"`
	Summary    string   `json:"Summary"`
	BirthDate  string   `json:"BirthDate"`
	BirthPlace string   `json:"BirthPlace"`
	Height     string   `json:"Height"`
	Bust       string   `json:"Bust"`
	Waist      string   `json:"Waist"`
	Hip        string   `json:"Hip"`
	Cup        string   `json:"Cup"`
	BloodType  string   `json:"BloodType"`
	DebutDate  string   `json:"DebutDate"`
	Agency     string   `json:"Agency"`
	Tags       []string `json:"Tags"`
	ProviderID string   `json:"ProviderID"`
}

// offlineLibrarySource 实现 ProfileSource。
type offlineLibrarySource struct {
	path string

	mu     sync.Mutex
	index  map[string]*offlineEntry // normName -> 条目（本名与别名都进这个表）
	names  int                      // 条目数（不是键数）
	mtime  time.Time
	size   int64
	errMsg string // 最近一次加载失败的原因，供界面显示
}

func newOfflineLibrarySource(path string) *offlineLibrarySource {
	return &offlineLibrarySource{path: path}
}

// offlineLib 返回配置里那个离线资料库源；没启用或没填路径时返回 nil。
//
// 实例缓存在 App 上，不是每次新建：源内部有「按 mtime 失效的索引」，
// 每次新建等于每次抓取都把整个导出文件重读一遍。路径变了就换一个实例。
func (a *App) offlineLib() *offlineLibrarySource {
	cfg := a.store.Get()
	if !cfg.OfflineDBEnabled || strings.TrimSpace(cfg.OfflineDBPath) == "" {
		return nil
	}
	a.offlineMu.Lock()
	defer a.offlineMu.Unlock()
	if a.offlineSrc == nil || a.offlineSrc.Path() != cfg.OfflineDBPath {
		a.offlineSrc = newOfflineLibrarySource(cfg.OfflineDBPath)
	}
	return a.offlineSrc
}

func (s *offlineLibrarySource) Key() string   { return offlineLibSourceKey }
func (s *offlineLibrarySource) Label() string { return "离线资料库" }

// Path 返回导出文件路径（界面显示用）。
func (s *offlineLibrarySource) Path() string { return s.path }

// Stats 返回「已载入多少条 / 最近一次失败原因」。
//
// 它会**顺带触发一次加载**：设置页想知道「这个文件到底读得出来吗」，
// 不实际读一次是答不出来的。加载失败不 panic，只把原因放进 errMsg 显示出来。
func (s *offlineLibrarySource) Stats() (int, string) {
	_ = s.ensureLoaded()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.names, s.errMsg
}

// ensureLoaded 按 mtime + size 决定要不要重新读文件。
func (s *offlineLibrarySource) ensureLoaded() error {
	fi, err := os.Stat(s.path)
	if err != nil {
		return s.loadErr("读不到导出文件 %s：%v（先用 tools/export_offline_db.py 导出）", s.path, err)
	}

	s.mu.Lock()
	upToDate := s.index != nil && fi.ModTime().Equal(s.mtime) && fi.Size() == s.size
	errMsg := s.errMsg
	s.mu.Unlock()
	if upToDate {
		if errMsg != "" {
			return errors.New(errMsg)
		}
		return nil
	}

	raw, err := os.ReadFile(s.path)
	if err != nil {
		return s.loadErr("读不到导出文件 %s：%v", s.path, err)
	}
	var f offlineLibFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return s.loadErr("导出文件 %s 不是有效的 JSON：%v", s.path, err)
	}

	idx := make(map[string]*offlineEntry, len(f.Entries)*2)
	for i := range f.Entries {
		e := &f.Entries[i]
		// 本名与别名都进索引：这张表**只用来查人**，不参与任何打分。
		// 「是不是同一个人」的门槛仍然由各源 + nameMatchScore 决定，这里不越权。
		for _, n := range append([]string{e.Name}, e.Aliases...) {
			k := normName(n)
			if k == "" {
				continue
			}
			if _, ok := idx[k]; !ok { // 同名冲突保留先出现的
				idx[k] = e
			}
		}
	}

	s.mu.Lock()
	s.index = idx
	s.names = len(f.Entries)
	s.mtime = fi.ModTime()
	s.size = fi.Size()
	s.errMsg = ""
	s.mu.Unlock()
	return nil
}

// loadErr 把「读不出来」的原因写成**同一句话**：既记进 errMsg（设置页那行状态要显示），
// 也作为 error 返回给调用方（抓取失败时进 Warnings）。
//
// 两处必须逐字一致。第一版是各写各的，结果设置页显示的是光秃秃的
// 「读不到导出文件：<系统错误>」，而抽屉里那条告警才带着
// 「先用 tools/export_offline_db.py 导出」—— 最管用的那句提示恰好只在用户看不到的地方出现。
// 用户对着一个「读不到」是没法自查的：他不知道这个文件得先由另一个脚本导出来。
func (s *offlineLibrarySource) loadErr(format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	s.setErr(msg)
	return errors.New(msg)
}

func (s *offlineLibrarySource) setErr(msg string) {
	s.mu.Lock()
	s.errMsg = msg
	s.mu.Unlock()
}

// Fetch 按姓名（含别名候选）在离线库里查一个人。
//
// 未命中返回 (nil, nil) —— 「这个库里没有他」不是错误，和别的源一样，
// 不该在面板上冒一条告警。只有「配了但读不出来」才是错误。
func (s *offlineLibrarySource) Fetch(_ context.Context, _ *http.Client, name string, aliases []string) (*ActorFacts, error) {
	if err := s.ensureLoaded(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	idx := s.index
	s.mu.Unlock()
	if idx == nil {
		return nil, nil
	}

	score := 100
	e := idx[normName(name)]
	if e == nil {
		for _, a := range aliases {
			if e = idx[normName(a)]; e != nil {
				score = offlineLibAliasScore
				break
			}
		}
	}
	if e == nil {
		return nil, nil
	}

	f := &ActorFacts{
		Source:      offlineLibSourceKey,
		SourceLabel: s.Label(),
		SourceURL:   e.SourceURL,
		MatchScore:  score,
		MatchedName: e.Name,
		Aliases:     append([]string(nil), e.Aliases...),
		BirthDate:   e.BirthDate,
		BirthPlace:  e.BirthPlace,
		Height:      e.Height,
		Bust:        e.Bust,
		Waist:       e.Waist,
		Hip:         e.Hip,
		Cup:         e.Cup,
		BloodType:   e.BloodType,
		DebutDate:   e.DebutDate,
		Agency:      e.Agency,
		Tags:        append([]string(nil), e.Tags...),
		Summary:     e.Summary,
		ProviderID:  e.ProviderID,
	}
	// 整条什么也没有（连别名都空）就别报成命中：那只会让「命中来源」里多一个
	// 什么都没提供的源，用户还得去猜它为什么在那儿。
	// isEmpty() 本身就把「只有别名」算作非空 —— 正是我们要的，因为别名也是资料。
	if f.isEmpty() {
		return nil, nil
	}
	normalizeOfflineFacts(f)
	return f, nil
}

// normalizeOfflineFacts 把导出文件里的空白收拾干净。
//
// 导出的是别人的库，值里带首尾空白、或者整行都是空白字符都很正常；
// 不收拾的话 mergeFacts 会把它当成「有值」而挡住后面在线源的真实值 ——
// 「先到先得」的优先级只在值非空时成立。
func normalizeOfflineFacts(f *ActorFacts) {
	f.Summary = strings.TrimSpace(f.Summary)
	f.BirthDate = normalizeOfflineDate(strings.TrimSpace(f.BirthDate))
	f.BirthPlace = strings.TrimSpace(f.BirthPlace)
	f.Height = strings.TrimSpace(f.Height)
	f.Bust = strings.TrimSpace(f.Bust)
	f.Waist = strings.TrimSpace(f.Waist)
	f.Hip = strings.TrimSpace(f.Hip)
	f.Cup = strings.TrimSpace(f.Cup)
	f.BloodType = strings.TrimSpace(f.BloodType)
	f.DebutDate = normalizeOfflineDate(strings.TrimSpace(f.DebutDate))
	f.Agency = strings.TrimSpace(f.Agency)
}

// normalizeOfflineDate 把常见写法收敛成 YYYY-MM-DD。
//
// 出生日期是要写回 Emby 的 PremiereDate 的，格式不对整个写入就白搭；
// 而导出文件来自别的工具，写法不受我们控制，所以这里兜一手。
// 认不出来就原样返回 —— 宁可让用户在上游改，也不在这里瞎猜一个日期。
func normalizeOfflineDate(s string) string {
	if s == "" {
		return ""
	}
	if len(s) >= 10 && s[4] == '-' && s[7] == '-' {
		return s[:10]
	}
	// 1991/04/19、1991.04.19、19910419
	r := strings.NewReplacer("/", "-", ".", "-")
	if t := r.Replace(s); len(t) >= 10 && t[4] == '-' && t[7] == '-' {
		return t[:10]
	}
	if len(s) == 8 {
		if _, err := time.Parse("20060102", s); err == nil {
			return s[:4] + "-" + s[4:6] + "-" + s[6:]
		}
	}
	return s
}
