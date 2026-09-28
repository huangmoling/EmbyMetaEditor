package main

// 离线演员资料库：一个 **本地只读** 的资料源。
//
// 五件事先说清楚，因为它们决定了这个文件为什么长这样：
//
//  1. **数据从哪来。** 原始资料库（`20260924资料库.db`）是 SQLCipher 4 加密的，
//     归另一个工具所有。本项目**既不解析那个 .db、也不写它一个字节**：
//     数据由 `tools/sqlcipher_dump.py` 以只读方式导出成 CSV（脚本对 .db 全程
//     `SQLITE_OPEN_READONLY`），这里读的是那份 CSV。
//     直接读 CSV 而不是先转成中间格式，是为了**少一层「导出了但忘了再转一次」的陈旧态**：
//     用户重新导出一次就立刻生效（靠 mtime/size 判定）。
//
//  2. **为什么不可能提供头像。** ActorFacts 里根本没有图片字段，也就是说这个源
//     在**类型上**就无法参与头像。头像继续走各自独立的头像源顺序
//     （gfriends 那一条链），这里不是靠「约定不用」而是靠「没法用」。
//     导出文件里那列 `profile_image_url` 因此**有意不入库**。
//
//  3. **同一个写法指向多条记录是常态，不是异常。** 实测这份库里
//     `name_original` 有 15% 的键、`kana` 有 18% 的键同时属于两条以上记录
//     （重名的不同演员，或者同一人的新旧两条记录）。所以索引不假装唯一：
//     一个键只认一条（按可靠度分两轮取先到者），但同时**记下还指向谁**，
//     抓取时把这件事作为告警说出来 —— 悄悄挑一条会让人把别人的生日写进自己的 Emby。
//
//  4. **哪些列有意不映射。** `tags_json`（是「日本艺人/30代/美魔女」这类派生分类，
//     不是源站题材标签，并进简介只会把简介变脏，而简介是整字段写入、脏了就没法只用干净那半）、
//     `social_links_json` / `awards_json` / `timeline_json` / `public_roles_json` /
//     `data_conflicts_json`（结构复杂，没有对应的 Emby 字段）、
//     `shoe_cm` / `body_type` / `nationality` / `occupation` / `career_status` /
//     `favorite_count`（同上）。
//
//  5. **懒加载 + 按 mtime 失效。** 导出文件近 10 MB，启动时不该读；但也不能读一次
//     就再也不看 —— 用户重新导出一份要能生效，所以每次都 stat 一下 mtime/size，
//     变了才重读。索引常驻，抓取是纯内存查表。

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// offlineLibSourceKey 是这个源的稳定 key。
const offlineLibSourceKey = "OfflineDB"

// offlineLibAliasScore 是靠**别名**命中时的姓名置信度。
//
// 与 nameMatchScore 里「命中别名记 95」保持一致，**不另设一套**：这里只是在如实
// 标注「是按哪个名字命中的」。按本名命中给 100（库里的键就是这个写法）。
//
// 注意这是「按哪个写法搜到的」，不是「这个人是谁」的判定 —— **身份阈值一个都没放宽**。
const offlineLibAliasScore = 95

// offlineCSVHint 是「读不到 / 认不出」时统一给的那句下一步。
//
// 两个出口（设置页那行状态、抽屉里的告警）必须逐字用同一句：第一版是各写各的，
// 设置页只显示光秃秃的「读不到：<系统错误>」，而最管用的「这文件得先导出来」
// 恰好只在用户看不到的地方出现。
const offlineCSVHint = "先用 tools/sqlcipher_dump.py --csv <输出.csv> 从资料库导出"

// offlineEntry 是一条演员资料。
//
// 保留 5 个名字列而不是合成一个「名字」，是因为它们的可靠度不一样：
// `name_zh_cn` 在实测数据里 674 条**零冲突**，`kana` 却有 18% 的键撞车。
// 建索引时按可靠度分两轮，靠的就是这个区分。
type offlineEntry struct {
	ID string

	NameOriginal string
	NameJA       string
	NameZH       string
	NameRoman    string
	Kana         string

	Aliases []string // aliases_json + nicknames_json（已按顿号等再拆一层）

	Summary        string
	SourceURL      string
	BirthDate      string
	BirthPlace     string
	Height         string
	Bust           string
	Waist          string
	Hip            string
	Cup            string
	BloodType      string
	DebutDate      string
	RetirementDate string
	Hobby          string
	Agency         string
}

// name 是展示用的本名。库里 `name_original` 是它自己的主写法，缺了才回退日文写法。
func (e *offlineEntry) name() string {
	if s := strings.TrimSpace(e.NameOriginal); s != "" {
		return s
	}
	return strings.TrimSpace(e.NameJA)
}

// strongKeys 是「比较可信」的写法，第一轮进索引。
func (e *offlineEntry) strongKeys() []string {
	out := make([]string, 0, 3+len(e.Aliases))
	out = append(out, e.NameZH, e.NameOriginal, e.NameJA)
	return append(out, e.Aliases...)
}

// weakKeys 是「读音/罗马音」这类撞车率高的写法，第二轮才进索引 ——
// 这样一个键同时被「某人的本名」和「另一个人的读音」占用时，本名赢。
func (e *offlineEntry) weakKeys() []string {
	return []string{e.NameRoman, e.Kana}
}

// variants 是同一个人的全部写法（含主写法）。它会作为**别名**暴露出去，
// 于是「采用 / 同步」时会被记进本地别名记忆，之后所有资料源都能共享这些搜索词 ——
// 这正是这个库与别名记忆联动的地方。
func (e *offlineEntry) variants() []string {
	return []string{e.NameOriginal, e.NameJA, e.NameZH, e.NameRoman, e.Kana}
}

// profileAliases 汇总这个人的别名（数据自带的 + 各种写法），去重后按原样返回。
//
// 顺序有意是「先数据自带的别名、后写法」：界面上一眼能看到的是这个人在别处的艺名，
// 而不是一串读音。
func (e *offlineEntry) profileAliases() []string {
	seen := make(map[string]bool, len(e.Aliases)+5)
	out := make([]string, 0, len(e.Aliases)+5)
	add := func(s string) {
		s = strings.TrimSpace(s)
		n := normName(s)
		if s == "" || n == "" || seen[n] {
			return
		}
		seen[n] = true
		out = append(out, s)
	}
	for _, a := range e.Aliases {
		add(a)
	}
	for _, v := range e.variants() {
		add(v)
	}
	return out
}

// offlineSlot 是索引里的一格：认下的那条记录，加上**同一个键还指向谁**。
//
// dupes 是刻意留着的：这份库重名率不低，而「悄悄挑一条」的后果是把别人的
// 出生日期写进用户的 Emby。宁可多一条告警，也不能少一次提醒。
type offlineSlot struct {
	entry *offlineEntry
	dupes []string // 同键的其它记录 id，按出现顺序
}

// offlineLibrarySource 实现 ProfileSource。
type offlineLibrarySource struct {
	path string

	mu     sync.Mutex
	index  map[string]*offlineSlot // normName -> 那一格
	names  int                     // 记录数（不是键数）
	keys   int                     // 索引键数（界面/诊断用）
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

// Stats 返回「已载入多少条记录 / 最近一次失败原因」。
//
// 它会**顺带触发一次加载**：设置页想知道「这个文件到底读得出来吗」，
// 不实际读一次是答不出来的。加载失败不 panic，只把原因放进 errMsg 显示出来。
func (s *offlineLibrarySource) Stats() (int, string) {
	_ = s.ensureLoaded()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.names, s.errMsg
}

// KeyCount 返回索引里的键数。只在诊断/单测里用得上。
func (s *offlineLibrarySource) KeyCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keys
}

// ensureLoaded 按 mtime + size 决定要不要重新读文件。
func (s *offlineLibrarySource) ensureLoaded() error {
	fi, err := os.Stat(s.path)
	if err != nil {
		return s.loadErr("读不到资料库导出文件 %s：%v（%s）", s.path, err, offlineCSVHint)
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
		return s.loadErr("读不到资料库导出文件 %s：%v", s.path, err)
	}
	entries, err := parseOfflineCSV(raw)
	if err != nil {
		return s.loadErr("读不懂资料库导出文件 %s：%v", s.path, err)
	}
	idx := buildOfflineIndex(entries)

	s.mu.Lock()
	s.index = idx
	s.names = len(entries)
	s.keys = len(idx)
	s.mtime = fi.ModTime()
	s.size = fi.Size()
	s.errMsg = ""
	s.mu.Unlock()
	return nil
}

// loadErr 把「读不出来」的原因写成**同一句话**：既记进 errMsg（设置页那行状态要显示），
// 也作为 error 返回给调用方（抓取失败时进 Warnings）。
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

// buildOfflineIndex 建「写法 -> 记录」的索引。
//
// 分两轮是为了让**更可靠的写法先占坑**：第一轮只放本名（中文名/主写法/日文名）与
// 数据自带的别名，第二轮才放罗马音与假名。同一个键被两条记录同时占用时，
// 先到者被认下，后到者记进 dupes 由界面去提醒。
//
// 这里**不做任何打分**：索引只回答「这个写法对应库里的哪一条」，
// 「是不是同一个人」的门槛仍然由各源 + nameMatchScore 决定，这里不越权。
func buildOfflineIndex(entries []offlineEntry) map[string]*offlineSlot {
	idx := make(map[string]*offlineSlot, len(entries)*3)
	for _, weak := range []bool{false, true} {
		for i := range entries {
			e := &entries[i]
			keys := e.strongKeys()
			if weak {
				keys = e.weakKeys()
			}
			for _, k := range keys {
				n := normName(k)
				if n == "" {
					continue
				}
				slot, ok := idx[n]
				if !ok {
					idx[n] = &offlineSlot{entry: e}
					continue
				}
				// 同一条记录的两种写法撞在同一个键上（比如日文名与罗马音归一化后相同），
				// 那不是「重名」，不该报冲突。
				if slot.entry == e || slices.Contains(slot.dupes, e.ID) {
					continue
				}
				slot.dupes = append(slot.dupes, e.ID)
			}
		}
	}
	return idx
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

	// 先用本名查，再用别名候选查 —— 别名候选里就有本地别名记忆**共享出来**的旧艺名，
	// 所以「上次确认过的写法」这次就能直接命中，不用用户再手输一遍。
	score := 100
	slot := idx[normName(name)]
	if slot == nil {
		for _, a := range aliases {
			if slot = idx[normName(a)]; slot != nil {
				score = offlineLibAliasScore
				break
			}
		}
	}
	if slot == nil {
		return nil, nil
	}
	e := slot.entry

	f := &ActorFacts{
		Source:         offlineLibSourceKey,
		SourceLabel:    s.Label(),
		SourceURL:      e.SourceURL,
		MatchScore:     score,
		MatchedName:    e.name(),
		Aliases:        e.profileAliases(),
		BirthDate:      e.BirthDate,
		BirthPlace:     e.BirthPlace,
		Height:         e.Height,
		Bust:           e.Bust,
		Waist:          e.Waist,
		Hip:            e.Hip,
		Cup:            e.Cup,
		BloodType:      e.BloodType,
		DebutDate:      e.DebutDate,
		RetirementDate: e.RetirementDate,
		Hobby:          e.Hobby,
		Agency:         e.Agency,
		Summary:        e.Summary,
	}
	// 一个写法对应多条记录时说清楚，别让用户以为「库里就是这么写的」。
	if len(slot.dupes) > 0 {
		f.Note = fmt.Sprintf("这个写法在资料库里对应多条记录：用了 id %s，另有 %s。若人不对，请换更精确的写法再查",
			e.ID, strings.Join(slot.dupes, "、"))
	}
	// 只报「真带了东西」的命中。
	//
	// 判据**不能**用 f.isEmpty()：这个源现在一定会带出别名（各种写法都算别名），
	// 于是 isEmpty() 永远为假 —— 一条只有名字的空壳记录会变成「命中了一个
	// 什么都没提供的源」，用户还得去猜它为什么在那儿。
	// 所以这里问的是那个更准的问题：**除了名字之外，它到底有没有内容？**
	// 别名算内容（「这个人在别处叫什么」本身就有用），名字的其它写法不算。
	if !e.hasContent() {
		return nil, nil
	}
	normalizeOfflineFacts(f)
	return f, nil
}

// hasContent 判断这条记录除了名字之外是否还带了可用信息。
//
// 特意把 SourceURL 也算在内是**错**的（第一版就这么写过）：一个只有官网链接、
// 没有任何资料字段的记录，命中了也只是给面板添一个空链接。
func (e *offlineEntry) hasContent() bool {
	if len(e.Aliases) > 0 {
		return true
	}
	return e.Summary != "" || e.BirthDate != "" || e.BirthPlace != "" ||
		e.Height != "" || e.Bust != "" || e.Waist != "" || e.Hip != "" ||
		e.Cup != "" || e.BloodType != "" || e.DebutDate != "" ||
		e.RetirementDate != "" || e.Hobby != "" || e.Agency != ""
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
	f.RetirementDate = normalizeOfflineDate(strings.TrimSpace(f.RetirementDate))
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

// ---------- CSV 解析 ----------

// offlineCSVColumns 是我们真正要用的列。名字写在这里而不是按位置取，
// 是为了让「上游多一列/换顺序」不至于让所有字段串位。
var offlineCSVColumns = []string{
	"id",
	"name_original", "name_ja", "name_zh_cn", "name_romanized", "kana",
	"birthplace", "birthdate", "blood_type",
	"height_cm", "bust_cm", "waist_cm", "hip_cm", "cup",
	"debut_date", "retirement_date", "hobbies", "agency",
	"biography_original", "biography_zh_cn", "official_site",
	"aliases_json", "nicknames_json",
}

// parseOfflineCSV 把导出文件解析成条目。
//
// 容错是刻意的，但**不猜**：认不出的值原样留着，缺列就明确报错（而不是当成空）。
// 这份数据来自第三方工具，我们只读它，出了问题要能一眼看出是数据的问题还是我们的问题。
func parseOfflineCSV(raw []byte) ([]offlineEntry, error) {
	// 导出文件带 UTF-8 BOM。不剥掉的话第一列的名字会变成 "\ufeffid"，
	// 之后按列名取值全部落空 —— 症状是「一条记录的所有字段都是空的」，
	// 而文件看起来完全正常，很难一眼看出是 BOM 的问题。
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})

	rd := csv.NewReader(bytes.NewReader(raw))
	rd.FieldsPerRecord = -1 // 列数不齐的行走自己的路，不因此让整份文件失败
	rd.LazyQuotes = true    // 第三方导出的引号写法不完全规范

	head, err := rd.Read()
	if err != nil {
		return nil, fmt.Errorf("读表头失败：%v（%s）", err, offlineCSVHint)
	}
	col := make(map[string]int, len(head))
	for i, h := range head {
		h = strings.TrimSpace(strings.TrimPrefix(h, "\ufeff"))
		if h != "" {
			col[h] = i
		}
	}
	if _, ok := col["name_original"]; !ok {
		return nil, fmt.Errorf("表头里没有 name_original 列 —— 看着不像资料库导出的 CSV（%s）", offlineCSVHint)
	}

	get := func(rec []string, name string) string {
		i, ok := col[name]
		if !ok || i >= len(rec) {
			return ""
		}
		return strings.TrimSpace(rec[i])
	}

	var out []offlineEntry
	for line := 2; ; line++ {
		rec, err := rd.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("第 %d 行读不出来：%v", line, err)
		}
		e := offlineEntry{
			ID:             get(rec, "id"),
			NameOriginal:   get(rec, "name_original"),
			NameJA:         get(rec, "name_ja"),
			NameZH:         get(rec, "name_zh_cn"),
			NameRoman:      get(rec, "name_romanized"),
			Kana:           get(rec, "kana"),
			Aliases:        append(offlineNameList(get(rec, "aliases_json")), offlineNameList(get(rec, "nicknames_json"))...),
			BirthPlace:     get(rec, "birthplace"),
			BirthDate:      get(rec, "birthdate"),
			BloodType:      get(rec, "blood_type"),
			Height:         offlineCM(get(rec, "height_cm")),
			Bust:           offlineCM(get(rec, "bust_cm")),
			Waist:          offlineCM(get(rec, "waist_cm")),
			Hip:            offlineCM(get(rec, "hip_cm")),
			Cup:            get(rec, "cup"),
			DebutDate:      get(rec, "debut_date"),
			RetirementDate: get(rec, "retirement_date"),
			Hobby:          get(rec, "hobbies"),
			Agency:         get(rec, "agency"),
			SourceURL:      get(rec, "official_site"),
			Summary:        offlineBiography(get(rec, "biography_zh_cn"), get(rec, "biography_original")),
		}
		if e.name() == "" {
			continue // 连名字都没有的行不是资料
		}
		out = append(out, e)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("一个有效条目都没有（%s）", offlineCSVHint)
	}
	return out, nil
}

// offlineBiography 优先用中文简介，没有才回退原文。
//
// 截断到 600 字：导出库里有些简介是整段长文，而它会作为「没有结构化字段时」的
// 简介正文落进 Emby，不设上限是给用户的库塞一篇论文。
func offlineBiography(zh, orig string) string {
	if strings.TrimSpace(zh) != "" {
		return clipRunes(strings.TrimSpace(zh), 600)
	}
	return clipRunes(strings.TrimSpace(orig), 600)
}

// offlineNameList 解析 `aliases_json` / `nicknames_json` 这类「字符串数组」列。
//
// 里面经常一条里塞了多个名字（实测样本里就有 `"本多翼、白瀬真希"`），所以再按
// 顿号/逗号/斜杠拆一层 —— 不拆的话「白瀬真希」这个写法永远进不了索引，
// 用户拿它查就是查不到，而库里其实有。
func offlineNameList(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" || s == "[]" || s == "null" {
		return nil
	}
	var arr []string
	if err := json.Unmarshal([]byte(s), &arr); err != nil {
		return nil // 不是数组就当没有：不在这里发明解析规则
	}
	var out []string
	for _, a := range arr {
		for _, part := range strings.FieldsFunc(a, isOfflineNameSep) {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

func isOfflineNameSep(r rune) bool {
	switch r {
	case '、', ',', '，', '/', '／', ';', '；', '|':
		return true
	}
	return false
}

// offlineCM 把 `163.0` 这种数值列写成 `163`。
//
// 导出脚本把身高/三围写成了浮点，直接用会在简介里出现「身高：163.0 cm」，
// 和别的源（`163`）摆在一起就是两种格式。认不出的值原样返回，不猜。
func offlineCM(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return s
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}
