package main

// 演员资料管理：抓取 → 与 Emby 现有值比对 → **只填空白** → 写回 → 留快照可回滚。
//
// 这是 actorprofile.go（抓取/解析）的上一层，负责所有「会改数据」的动作。
// 两条铁律（都是用户明确选的策略）：
//  1. **只填空白。** Emby 里已有值的字段一律不动 —— 库里 1400 个演员已有头像/资料，
//     全量覆盖会毁数据。是否写入的判定完全在服务端做，不信前端传来的值。
//  2. **先快照再写。** 每次写之前落一份 Before/After，出问题能一键还原。

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------- 别名记忆 ----------

// AliasGroup 是一组「同一个人」的写法。字段名与文件结构与原版
// 「Emby演员扩展器」的 data/演员别名记忆.json **保持一致**，两边可以互相搬用。
type AliasGroup struct {
	Id            string   `json:"Id"`
	CanonicalName string   `json:"CanonicalName"`
	Names         []string `json:"Names"`
	Sources       []string `json:"Sources"`
	UpdatedUtc    string   `json:"UpdatedUtc"`
}

// AliasStore 管理别名记忆的读写与查询。
type AliasStore struct {
	Version int          `json:"Version"`
	Groups  []AliasGroup `json:"Groups"`

	mu     sync.RWMutex
	byName map[string]int // normName -> Groups 下标
	path   string
}

func NewAliasStore() *AliasStore {
	a := &AliasStore{Version: 2, path: filepath.Join(cacheDir(), "actor_aliases.json")}
	a.reindex()
	return a
}

func (a *AliasStore) reindex() {
	a.byName = make(map[string]int, len(a.Groups)*4)
	for i, g := range a.Groups {
		for _, n := range g.Names {
			if k := normName(n); k != "" {
				a.byName[k] = i
			}
		}
		if k := normName(g.CanonicalName); k != "" {
			a.byName[k] = i
		}
	}
}

// LoadFromCache 读本地别名记忆；文件不存在或损坏都不算错误。
func (a *AliasStore) LoadFromCache() bool {
	var tmp AliasStore
	if err := readJSONFile(a.path, &tmp); err != nil || len(tmp.Groups) == 0 {
		return false
	}
	a.mu.Lock()
	a.Version = tmp.Version
	if a.Version == 0 {
		a.Version = 2
	}
	a.Groups = tmp.Groups
	a.reindex()
	a.mu.Unlock()
	return true
}

// NamesFor 返回某人已知的全部写法（含本人名字），用于提高各源的命中率。
func (a *AliasStore) NamesFor(name string) []string {
	out := []string{strings.TrimSpace(name)}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if i, ok := a.byName[normName(name)]; ok {
		for _, n := range a.Groups[i].Names {
			if n = strings.TrimSpace(n); n != "" {
				out = append(out, n)
			}
		}
	}
	return dedupeStrings(out)
}

// Remember 把「本人名字 + 本次抓到的别名」记成一组，已存在的组则合并。
// source 用来记「这批别名是哪来的」（如 已同步:AVデータバンク）。
func (a *AliasStore) Remember(canonical string, names []string, source string) {
	canonical = strings.TrimSpace(canonical)
	if canonical == "" {
		return
	}
	all := dedupeStrings(append([]string{canonical}, names...))
	if len(all) <= 1 {
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	idx, ok := a.byName[normName(canonical)]
	if !ok {
		a.Groups = append(a.Groups, AliasGroup{
			Id:            randHex(16),
			CanonicalName: canonical,
			Names:         all,
			Sources:       []string{source},
			UpdatedUtc:    time.Now().UTC().Format(time.RFC3339Nano),
		})
		idx = len(a.Groups) - 1
	} else {
		g := &a.Groups[idx]
		g.Names = dedupeStrings(append(g.Names, all...))
		g.Sources = dedupeStrings(append(g.Sources, source))
		g.UpdatedUtc = time.Now().UTC().Format(time.RFC3339Nano)
	}
	a.reindex()
	_ = writeJSONFile(a.path, map[string]any{
		"Version": a.Version,
		"Groups":  a.Groups,
	})
}

// Count 返回已记忆的组数。
func (a *AliasStore) Count() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.Groups)
}

// ---------- 同步历史 ----------

// SyncRecord 是一次资料写入的快照。Before/After 只存我们管内的那几个字段
// （整份 Person DTO 没必要，而且回滚也不需要）。
type SyncRecord struct {
	ID         string         `json:"id"`
	PersonID   string         `json:"person_id"`
	Name       string         `json:"name"`
	CreatedAt  time.Time      `json:"created_at"`
	Sources    []string       `json:"sources"`
	Changed    []string       `json:"changed"` // 人类可读的「改了哪些字段」
	Before     map[string]any `json:"before"`
	After      map[string]any `json:"after"`
	RolledBack bool           `json:"rolled_back"`
	RollbackAt time.Time      `json:"rollback_at,omitempty"`
}

// SyncStore 保存最近的写入历史，容量满了丢最旧的。
type SyncStore struct {
	Records []SyncRecord `json:"Records"`

	mu   sync.Mutex
	path string
}

// syncHistoryCap 保留多少条历史。演员资料不大，留多一点方便追溯。
const syncHistoryCap = 1000

func NewSyncStore() *SyncStore {
	return &SyncStore{path: filepath.Join(cacheDir(), "sync_history.json")}
}

func (s *SyncStore) LoadFromCache() bool {
	var tmp SyncStore
	if err := readJSONFile(s.path, &tmp); err != nil || len(tmp.Records) == 0 {
		return false
	}
	s.mu.Lock()
	s.Records = tmp.Records
	s.mu.Unlock()
	return true
}

func (s *SyncStore) save() {
	_ = writeJSONFile(s.path, map[string]any{"Records": s.Records})
}

// Add 追加一条历史，并**返回它的 ID**。
//
// 必须把 ID 返回给调用方：rec 是按值传进来的，在这里补的 ID 传不回去 ——
// 早先这里没返回值，`res.RecordID = rec.ID` 拿到的一直是空串，界面上
// 「刚写入的这条」就少了 ID（单测当场抓到）。
func (s *SyncStore) Add(rec SyncRecord) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec.ID == "" {
		rec.ID = randHex(8)
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now()
	}
	// 新记录放最前，方便界面倒序展示
	s.Records = append([]SyncRecord{rec}, s.Records...)
	if len(s.Records) > syncHistoryCap {
		s.Records = s.Records[:syncHistoryCap]
	}
	s.save()
	return rec.ID
}

// List 返回最近的历史（不含快照本体，省流量）。
func (s *SyncStore) List(limit int) []SyncRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > len(s.Records) {
		limit = len(s.Records)
	}
	out := make([]SyncRecord, 0, limit)
	for _, r := range s.Records[:limit] {
		r.Before, r.After = nil, nil
		out = append(out, r)
	}
	return out
}

// Get 按 ID 取完整记录。
func (s *SyncStore) Get(id string) (SyncRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.Records {
		if r.ID == id {
			return r, true
		}
	}
	return SyncRecord{}, false
}

// MarkRolledBack 打上「已回滚」标记。
func (s *SyncStore) MarkRolledBack(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.Records {
		if s.Records[i].ID == id {
			s.Records[i].RolledBack = true
			s.Records[i].RollbackAt = time.Now()
			break
		}
	}
	s.save()
}

// ---------- Emby 字段读写 ----------

// profileFieldKeys 是我们管的全部字段，顺序即界面展示顺序。
//
// **没有 tags**：实测这个 Emby 构建对 Person 不保存 Tags（POST 返回 204，但详情、
// 列表、TagItems、全局 /Tags 字典全都读不回来），写它等于每次运行都重复写、
// 界面上还谎报成功。详见 buildActorProfile 里「把标签并进简介」那段。
var profileFieldKeys = []string{
	"overview", "premiere_date", "production_year",
	"production_locations", "provider_ids",
}

// embyProfileSnapshot 从 Person DTO 里抽出我们管的字段（回滚快照用）。
func embyProfileSnapshot(item Item) map[string]any {
	return map[string]any{
		"overview":             itemStr(item, "Overview"),
		"premiere_date":        itemStr(item, "PremiereDate"),
		"production_year":      itemInt(item, "ProductionYear"),
		"production_locations": itemStrings(item, "ProductionLocations"),
		"provider_ids":         itemStrMap(item, "ProviderIds"),
	}
}

func itemStr(m Item, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func itemInt(m Item, key string) int {
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		n, _ := strconv.Atoi(v)
		return n
	}
	return 0
}

func itemStrings(m Item, key string) []string {
	raw, ok := m[key].([]any)
	if !ok {
		if ss, ok := m[key].([]string); ok {
			return append([]string(nil), ss...)
		}
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

func itemStrMap(m Item, key string) map[string]string {
	out := map[string]string{}
	switch raw := m[key].(type) {
	case map[string]any:
		for k, v := range raw {
			if s, ok := v.(string); ok && s != "" {
				out[k] = s
			}
		}
	case map[string]string:
		for k, v := range raw {
			if v != "" {
				out[k] = v
			}
		}
	}
	return out
}

// ---------- 抓取 ----------

// FetchOptions 控制一次资料抓取。
type FetchOptions struct {
	Sources      []string // 源 key；空 = 全部启用的源
	UseAliasMemo bool     // 是否用别名记忆扩展搜索词
}

// fetchActorProfile 并发跑各源，合并成一份可供界面比对的资料。
//
// 全程**只读**：不改 Emby、不落盘。界面点「抓取」走的也是这里。
func (a *App) fetchActorProfile(ctx context.Context, personID, name string, opts FetchOptions) (*ActorProfile, error) {
	cfg := a.store.Get()
	client := newHTTPClient(cfg)

	// Emby 现有值（也是只读）
	ex := embyExisting{}
	if personID != "" {
		if e := NewEmby(cfg); e != nil {
			if item, err := e.ItemDetail(ctx, personID); err == nil {
				snap := embyProfileSnapshot(item)
				ex.Overview = strOr(snap["overview"], "")
				ex.Year = strOr(snap["production_year"], "")
				ex.Birth = strOr(snap["premiere_date"], "")
				ex.Locations = strings.Join(toStringSlice(snap["production_locations"]), ", ")
				prov := snap["provider_ids"]
				ex.ProviderID = providerSummary(toStrMap(prov))
			}
		}
	}

	// 搜索名候选：本人名字 + 别名记忆里的写法
	names := []string{name}
	var aliases []string
	if opts.UseAliasMemo && a.aliases != nil {
		names = append(names, a.aliases.NamesFor(name)...)
	}
	for _, n := range names[1:] {
		if strings.TrimSpace(n) != "" {
			aliases = append(aliases, strings.TrimSpace(n))
		}
	}

	// 选源
	srcs := actorSources()
	if len(opts.Sources) > 0 {
		want := map[string]bool{}
		for _, k := range opts.Sources {
			want[k] = true
		}
		filtered := srcs[:0:0]
		for _, s := range srcs {
			if want[s.Key()] {
				filtered = append(filtered, s)
			}
		}
		srcs = filtered
	}

	// 并发抓（每源独立超时，一个源挂了不影响其他）
	type res struct {
		f   *ActorFacts
		err error
	}
	results := make([]res, len(srcs))
	var wg sync.WaitGroup
	for i, s := range srcs {
		wg.Add(1)
		go func(i int, s ProfileSource) {
			defer wg.Done()
			sctx, cancel := context.WithTimeout(ctx, 25*time.Second)
			defer cancel()
			f, err := s.Fetch(sctx, client, name, aliases)
			results[i] = res{f: f, err: err}
		}(i, s)
	}
	wg.Wait()

	// **顺序即优先级**：按 actorSources() 的注册顺序（AVデータバンク 最优先）
	var facts []ActorFacts
	prof := &ActorProfile{Name: name, PersonID: personID}
	for i, r := range results {
		if r.err != nil {
			prof.Warnings = append(prof.Warnings, fmt.Sprintf("%s：%v", srcs[i].Label(), r.err))
			continue
		}
		if r.f == nil {
			continue
		}
		facts = append(facts, *r.f)
	}
	built := buildActorProfile(name, personID, facts, ex)
	built.Warnings = append(prof.Warnings, built.Warnings...)
	return built, nil
}

func strOr(v any, def string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	if n, ok := v.(int); ok && n != 0 {
		return strconv.Itoa(n)
	}
	return def
}

func toStringSlice(v any) []string {
	switch raw := v.(type) {
	case []string:
		return raw
	case []any:
		out := make([]string, 0, len(raw))
		for _, x := range raw {
			if s, ok := x.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func toStrMap(v any) map[string]string {
	out := map[string]string{}
	switch raw := v.(type) {
	case map[string]any:
		for k, x := range raw {
			if s, ok := x.(string); ok && s != "" {
				out[k] = s
			}
		}
	case map[string]string:
		return raw
	}
	return out
}

// ---------- 写入 ----------

// ApplyResult 是一次写入的结果。
type ApplyResult struct {
	PersonID  string   `json:"person_id"`
	Name      string   `json:"name"`
	Written   []string `json:"written"`
	Skipped   []string `json:"skipped"`
	RecordID  string   `json:"record_id"`
	Message   string   `json:"message"`
	Sources   []string `json:"sources"`
	AliasMemo int      `json:"alias_memo"`
}

// applyActorProfile 抓取并写入（对外入口：先抓网络，再写）。
//
// keys 为空表示「写入所有判定为 WillWrite 的字段」；非空时只写其中的子集
// （界面上的逐字段勾选）。**无论哪种，最终写什么由服务端重新抓取后判定**，
// 不接受前端传来的值 —— 避免把任意内容写进用户的 Emby。
func (a *App) applyActorProfile(ctx context.Context, personID, name string, keys []string, opts FetchOptions) (*ApplyResult, error) {
	if personID == "" {
		return nil, fmt.Errorf("缺少演员 ID")
	}
	prof, err := a.fetchActorProfile(ctx, personID, name, opts)
	if err != nil {
		return nil, err
	}
	return a.applyProfileFacts(ctx, prof, keys)
}

// applyProfileFacts 把一份**已经抓好的**资料写进 Emby。
//
// 和 applyActorProfile 分开是为了可测：这条路径会改用户的媒体库，是整套功能里
// 唯一「出错就没法挽回」的地方（除了回滚），必须能脱离外网单独测
// （fetchActorProfile 要连 av-db.net，单测里没法稳定重放）。
func (a *App) applyProfileFacts(ctx context.Context, prof *ActorProfile, keys []string) (*ApplyResult, error) {
	personID, name := prof.PersonID, prof.Name
	if personID == "" {
		return nil, fmt.Errorf("缺少演员 ID")
	}
	res := &ApplyResult{PersonID: personID, Name: name, Sources: prof.Sources}

	want := map[string]bool{}
	for _, k := range keys {
		want[k] = true
	}
	picked := map[string]bool{}
	for _, f := range prof.Fields {
		if !f.WillWrite {
			res.Skipped = append(res.Skipped, f.Label+"（"+f.Note+"）")
			continue
		}
		if len(want) > 0 && !want[f.Key] {
			res.Skipped = append(res.Skipped, f.Label+"（未勾选）")
			continue
		}
		picked[f.Key] = true
	}
	if len(picked) == 0 {
		res.Message = "没有需要写入的字段（Emby 里该有的都有了）"
		return res, nil
	}

	cfg := a.store.Get()
	e := NewEmby(cfg)

	// 先读完整快照，写历史备忘
	item, err := e.ItemDetail(ctx, personID)
	if err != nil {
		return nil, fmt.Errorf("读取演员条目失败：%w", err)
	}
	before := embyProfileSnapshot(item)

	// 组装 patch（值全部来自服务端本次抓取的结果）
	patch := map[string]any{}
	m := mergeFacts(prof.Facts)
	if picked["overview"] {
		patch["Overview"] = prof.Overview
	}
	if picked["premiere_date"] {
		patch["PremiereDate"] = m.fields["birth_date"]
	}
	if picked["production_year"] {
		if y, err := strconv.Atoi(yearOf(m.fields["birth_date"])); err == nil && y > 0 {
			patch["ProductionYear"] = y
		}
	}
	if picked["production_locations"] {
		if bp := m.fields["birth_place"]; bp != "" {
			patch["ProductionLocations"] = []string{bp}
		}
	}
	if picked["provider_ids"] {
		pv := map[string]any{}
		for i := range prof.Facts {
			if f := prof.Facts[i]; f.ProviderID != "" {
				pv[f.Source] = f.ProviderID
			}
		}
		if len(pv) > 0 {
			patch["ProviderIds"] = pv
		}
	}
	if len(patch) == 0 {
		res.Message = "没有可写入的字段"
		return res, nil
	}

	if err := e.UpdateItem(ctx, personID, patch); err != nil {
		return nil, fmt.Errorf("写回 Emby 失败：%w", err)
	}

	// 历史在写成功之后落（失败就不该留记录）
	after := map[string]any{}
	for k, v := range before {
		after[k] = v
	}
	for k := range picked {
		switch k {
		case "overview":
			after["overview"] = patch["Overview"]
		case "premiere_date":
			after["premiere_date"] = patch["PremiereDate"]
		case "production_year":
			after["production_year"] = patch["ProductionYear"]
		case "production_locations":
			after["production_locations"] = patch["ProductionLocations"]
		case "provider_ids":
			after["provider_ids"] = toStrMap(patch["ProviderIds"])
		}
	}
	rec := SyncRecord{
		PersonID: personID,
		Name:     name,
		Sources:  prof.Sources,
		Before:   before,
		After:    after,
	}
	for _, f := range prof.Fields {
		if picked[f.Key] {
			res.Written = append(res.Written, f.Label)
			rec.Changed = append(rec.Changed, f.Label)
		}
	}
	res.RecordID = a.sync.Add(rec)
	// 抓到的别名进别名记忆，下次搜索命中率更高
	if a.aliases != nil && len(prof.Aliases) > 0 {
		src := "已同步"
		if len(prof.Sources) > 0 {
			src = "已同步:" + prof.Sources[0]
		}
		a.aliases.Remember(name, prof.Aliases, src)
		res.AliasMemo = len(prof.Aliases)
	}

	res.Message = fmt.Sprintf("已写入 %d 个字段", len(res.Written))
	return res, nil
}

// rollbackSync 把某次写入还原成写入前的样子。
//
// 用 UpdateItemExact 而不是 UpdateItem —— 后者对 ProviderIds 是**只增不减**的
// 合并语义，回滚不掉外部 ID。
func (a *App) rollbackSync(ctx context.Context, recordID string) (*ApplyResult, error) {
	rec, ok := a.sync.Get(recordID)
	if !ok {
		return nil, fmt.Errorf("找不到这条同步记录")
	}
	if rec.RolledBack {
		return nil, fmt.Errorf("这条记录已经回滚过了")
	}
	// 组装「写入前」的 patch。
	//
	// 下面三处兜底必须用 isNilVal 而不是 `== nil`：`toStringSlice` 在值缺失时
	// 返回的是**类型化的 nil**（`[]string(nil)`），装进 any 之后接口不等于 nil，
	// `patch[k] == nil` 抓不到它 → 兜底不生效 → 这个键被 updateItem 的 nil 防护
	// 直接跳过 → 于是 Emby 里那个字段**根本没被还原**。
	//
	// 而 null 也不是正确答案：要清空一个列表字段，得明确发 `[]`。
	patch := map[string]any{
		"Overview":            strOr(rec.Before["overview"], ""),
		"PremiereDate":        strOr(rec.Before["premiere_date"], ""),
		"ProductionYear":      rec.Before["production_year"],
		"ProductionLocations": toStringSlice(rec.Before["production_locations"]),
		"ProviderIds":         rec.Before["provider_ids"],
	}
	if isNilVal(patch["ProductionLocations"]) {
		patch["ProductionLocations"] = []string{}
	}
	if isNilVal(patch["ProviderIds"]) {
		patch["ProviderIds"] = map[string]any{}
	}
	if isNilVal(patch["ProductionYear"]) {
		patch["ProductionYear"] = 0
	}

	e := NewEmby(a.store.Get())
	if err := e.UpdateItemExact(ctx, rec.PersonID, patch); err != nil {
		return nil, fmt.Errorf("回滚失败：%w", err)
	}
	a.sync.MarkRolledBack(recordID)
	return &ApplyResult{
		PersonID: rec.PersonID,
		Name:     rec.Name,
		Message:  "已还原到写入前的状态",
	}, nil
}

// sortSourceKeys 让界面上的源列表顺序稳定。
func sortSourceKeys(keys []string) []string {
	out := append([]string(nil), keys...)
	sort.Strings(out)
	return out
}
