package main

// 姓名映射表（内嵌）：把「同一个人的不同写法」连起来。
//
// 为什么需要它。三个在线资料源（AVデータバンク / AV-League / 日文 Wikipedia）全是
// 日文站，而 Emby 里的人物名多半是刮削器写的中文简体（「三上悠亚」）。拿中文名去
// 日文站搜就是一条都不中 —— 这不是网络问题，是查询词压根不是人家用的写法。
// 手工填「搜索用名字」能解决，但那要求用户自己知道日文名怎么写。
//
// 这张表就是那层「汉字圈常用写法 ↔ 日文写法」的对照：
//   zh_cn   简体中文名
//   zh_tw   繁体中文名
//   jp      日文名（**日文站的检索主键**，所以它排在首选搜索词第一位）
//   keyword 逗号分隔的其它别名（旧艺名、罗马音、假名、异体字）
//
// 它只解决「用哪个名字去搜」和「哪些写法算同一个人」，**不提供任何资料字段** ——
// 生日、出生地、简介仍然只来自各资料源。这一点是刻意的：这张表里没有日期与地点，
// 把它当资料源只会让面板多一行空来源。
//
// 数据来源：github.com/li-peifeng/Jav-Actors-Mapping 的 mapping_actor.xml
// （26,088 条，UTF-8，上游由 scripts/format_actor_mapping.py 自动生成精简表）。
// 更新走 `python tools/fetch_actor_mapping.py`，然后**必须重新构建**才生效。
//
// ⚠️ 上游 README 写明「本仓库内容仅适用于 AVdb，其他项目如需使用请自行评估兼容性
// 并自行维护」，且仓库没有 LICENSE。此处选择内嵌（与 data/actresses_export.csv
// 同一套做法），意味着这份数据会随 exe 与镜像**公开分发** —— 这是使用方明确接受的
// 取舍，写在这里是为了让后来的人知道边界，而不是假装它不存在。
//
// 几个实现上的取舍：
//
//  1. **键冲突的整条键丢弃，不是「先到先得」。** 离线资料库那边撞键是常态（15%/18%），
//     所以它认一条 + 告警；这里不一样：这张表唯一的职责就是身份映射，认错一条的后果是
//     把别人的生日写进你的 Emby，而映射表本身没有任何字段能让用户看出「这条不像」。
//     实测 62,098 个键里只有 3 个撞车，宁可丢掉这 3 个键（涉及 6 个名字），
//     也不猜。数量记在 ambiguous 里，诊断与单测都能看见。
//
//  2. **只读，运行期不写回。** 不写 actor_aliases.json（那是用户确认过的记忆，
//     和「编译进程序的静态表」是两种可信度，混在一起就分不清哪条是用户认的）。
//
//  3. **索引只建一次。** 数据是编译进来的，运行期不可能变，所以没有 mtime 那一套。

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// builtinActorMappingXML 是编译进程序的姓名映射表（约 3.0 MB，26,088 条）。
//
// 与 data/actresses_export.csv 一样靠 .gitattributes 里的 `data/** -text` 保住字节，
// 剥 BOM 与解析都按字节来 —— 换行被 checkout 改掉的话，这里会解析失败并告警
// （不会静默变成「一条都对不上」，那种坏法最难查）。
//
//go:embed data/actor_mapping.xml
var builtinActorMappingXML []byte

// actorMappingHint 是「读不懂」时统一给的那句出处说明。
//
// 指向源文件与更新脚本，而不是「用户该去改什么设置」—— 这里根本没有设置可改，
// 数据是构建进产物的。
const actorMappingHint = "数据来自 github.com/li-peifeng/Jav-Actors-Mapping 的 mapping_actor.xml（源文件 data/actor_mapping.xml，已内嵌进程序；用 tools/fetch_actor_mapping.py 更新后需重新构建）"

// 数据的两个来源。
//
// 内嵌那份是**出厂基线**：跟着版本走，永远可用、不需要联网 —— 这是这个项目
// 一贯的前提（单文件 exe，拷走就能跑）。
// 但上游是会变的（新演员、改名、补别名），所以再留一条 `cache/actor_mapping.xml`
// 覆盖通道：界面上一键从上游拉最新，落盘后**立即生效**，不用重新构建。
//
// 优先级：cache（用户主动更新过）> 内嵌基线。cache 坏了就回退基线并说明原因 ——
// 「更新失败导致整个功能失效」比「用旧数据」糟糕得多。
const (
	mappingSourceBuiltin = "builtin"
	mappingSourceCache   = "cache"
	mappingCacheName     = "actor_mapping.xml"

	// mappingMinEntries 是「这份数据看起来还像映射表」的下限。
	//
	// 光靠「能解析」不够：被截断的文件、上游改成返回错误页但碰巧是 HTML 标签树、
	// 甚至一个只含 `<actor></actor>` 的空壳，都能解析成功。真按那个覆盖下去，
	// 用户的映射表就变成 3 条，而界面上只会显示「已更新」。实测上游 26,088 条，
	// 一万条这条线拦的是「数量级不对」，不是「少了几条」。
	mappingMinEntries = 10000
)

// actorMappingCachePath 是「在线更新」落盘的位置（dataDir/cache/）。
func actorMappingCachePath() string {
	return filepath.Join(cacheDir(), mappingCacheName)
}

// actorMappingUpstreamURLs 是上游地址，按可用性排序（与 gfriends 同一套思路）：
// jsdelivr 是 CDN，国内多数网络可达；raw.githubusercontent.com 走 GitHub 自己的
// 基础设施，两个都挂的概率远小于其中任一个。
var actorMappingUpstreamURLs = []string{
	"https://cdn.jsdelivr.net/gh/li-peifeng/Jav-Actors-Mapping@main/mapping_actor.xml",
	"https://raw.githubusercontent.com/li-peifeng/Jav-Actors-Mapping/main/mapping_actor.xml",
}

// actorMappingEntry 是一条映射记录。
type actorMappingEntry struct {
	ZhCN     string
	ZhTW     string
	JP       string
	Keywords []string
}

// names 返回这条记录的全部写法，**日文名在最前**。
//
// 顺序有意义：`preferredSearchName` 直接取第一个非空值，而日文名是日文站唯一
// 真正搜得中的写法。中文名排在后面只是作为「认可的写法」。
func (e *actorMappingEntry) names() []string {
	if e == nil {
		return nil
	}
	out := make([]string, 0, 3+len(e.Keywords))
	for _, s := range []string{e.JP, e.ZhTW, e.ZhCN} {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	for _, k := range e.Keywords {
		if k = strings.TrimSpace(k); k != "" {
			out = append(out, k)
		}
	}
	return out
}

// preferredSearchName 返回「拿去日文站搜最可能命中」的写法。
//
// jp 缺失时才退到中文写法（实测 26,088 条里 jp 一条都不缺，但这属于数据质量，
// 不是格式保证 —— 上游允许字段缺失时提交 PR，所以这里不能假设它一定有）。
func (e *actorMappingEntry) preferredSearchName() string {
	if e == nil {
		return ""
	}
	for _, s := range []string{e.JP, e.ZhTW, e.ZhCN} {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	return ""
}

// aliasesFor 返回「同一个人的其它写法」，按归一化后的写法去重并排除 identity 本身。
//
// identity 是 Emby 里的名字：它已经在 `name` 参数里当成待匹配的基准传下去了，
// 再作为别名出现一次只会让告警与计数变噪。
func (e *actorMappingEntry) aliasesFor(identity string) []string {
	if e == nil {
		return nil
	}
	self := normName(identity)
	seen := map[string]bool{}
	if self != "" {
		seen[self] = true
	}
	out := make([]string, 0, len(e.Keywords)+2)
	for _, n := range e.names() {
		k := normName(n)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, n)
	}
	return out
}

// actorMappingIndex 是映射表的内存索引。
type actorMappingIndex struct {
	raw []byte // 内嵌基线（永远在，作为回退）

	// cachePath 是「在线更新」落盘的那份（可能不存在）。空串表示**不读缓存** ——
	// 单测夹具走的就是这条路，免得测试结果受本机 cache/ 目录影响。
	cachePath string

	mu        sync.Mutex
	loaded    bool
	entries   []actorMappingEntry
	idx       map[string]*actorMappingEntry // normName -> 记录
	keys      int                           // 入索引的键数
	ambiguous int                           // 撞键而被丢弃的键数
	source    string                        // builtin | cache
	bytes     int                           // 当前用的是多少字节
	updatedAt time.Time                     // cache 文件的 mtime（内嵌时为零值）
	fromURL   string                        // 这次更新是从哪个地址拉下来的
	note      string                        // 缓存坏掉回退时的说明（非致命）
	errMsg    string
	loadErr   error
}

// newActorMapping 返回生产用的索引：内嵌基线 + cache 覆盖通道。
func newActorMapping() *actorMappingIndex {
	return &actorMappingIndex{raw: builtinActorMappingXML, cachePath: actorMappingCachePath()}
}

// newActorMappingFromBytes 用给定字节建索引，单测夹具走这里。
func newActorMappingFromBytes(raw []byte) *actorMappingIndex {
	return &actorMappingIndex{raw: raw}
}

// adoptLocked 把一份**已经解析好**的数据换成当前数据（调用方需持锁）。
func (m *actorMappingIndex) adoptLocked(raw []byte, entries []actorMappingEntry, source string, at time.Time) {
	idx, ambiguous := buildActorMappingIndex(entries)
	m.entries = entries
	m.idx = idx
	m.keys = len(idx)
	m.ambiguous = ambiguous
	m.bytes = len(raw)
	m.source = source
	m.updatedAt = at
	m.errMsg = ""
	m.loadErr = nil
}

// install 换成一份新数据（在线更新成功后调用），**立即生效**，不等重启。
//
// 解析放在调用方、持锁只做替换：解析要几百毫秒，拿着锁解析会把正在跑的抓取全卡住。
func (m *actorMappingIndex) install(raw []byte, entries []actorMappingEntry, source string, at time.Time, fromURL string) error {
	if len(entries) < mappingMinEntries {
		return fmt.Errorf("只解析出 %d 条（少于 %d 条），看起来不像完整的映射表", len(entries), mappingMinEntries)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.adoptLocked(raw, entries, source, at)
	m.fromURL = fromURL
	m.note = ""
	m.loaded = true
	return nil
}

// actorMapping 返回内嵌的映射表索引（懒创建，进程内只有一个）。
//
// 解析一次要遍历 2.6 万个节点，每次抓取都重建等于白烧 CPU —— 数据是编译期定死的，
// 没有任何理由重建。
func (a *App) actorMapping() *actorMappingIndex {
	a.mappingMu.Lock()
	defer a.mappingMu.Unlock()
	if a.mappingSrc == nil {
		a.mappingSrc = newActorMapping()
	}
	return a.mappingSrc
}

// mappingLookup 在映射表里查一个写法，同时把「表本身坏了」和「表里没这个人」分开。
//
// 未命中返回 (nil, nil)：库里没这个人不是错误，不该在面板上冒告警。
// 解析失败返回 (nil, err)：那是产物坏了，必须说出来 —— 这类「数据编译进程序」
// 的设计最容易悄悄坏掉的地方就是界面一切正常、只是永远查不到人。
func (a *App) mappingLookup(name string) (*actorMappingEntry, error) {
	m := a.actorMapping()
	if err := m.ensureLoaded(); err != nil {
		return nil, err
	}
	k := normName(name)
	if k == "" {
		return nil, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.idx[k], nil
}

// mappingAliases 返回映射表认为「同一个人的其它写法」（含中文名、繁体名、旧艺名）。
func (a *App) mappingAliases(name string) []string {
	e, err := a.mappingLookup(name)
	if err != nil {
		return nil
	}
	return e.aliasesFor(name)
}

// mappingPreferredName 返回映射表建议的日文写法（未命中或读不出来时返回空串）。
func (a *App) mappingPreferredName(name string) string {
	e, err := a.mappingLookup(name)
	if err != nil {
		return ""
	}
	return e.preferredSearchName()
}

// mappingNameCandidates 返回「查头像索引时该试哪些写法」：原名在前，映射表补在后。
//
// 头像这条链上它救的是另一种情况：gfriends 索引里的文件名是日文写法，
// Emby 里是中文名 → Lookup 一个都命不中，卡片上写着「库中无记录」，
// 而库里其实有这个人。
func (a *App) mappingNameCandidates(name string) []string {
	out := []string{strings.TrimSpace(name)}
	seen := map[string]bool{}
	if k := normName(name); k != "" {
		seen[k] = true
	}
	e, err := a.mappingLookup(name)
	if err != nil {
		return out
	}
	for _, n := range e.names() {
		k := normName(n)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, n)
	}
	return out
}

// Stats 返回「载入了多少条 / 多少个键 / 几个歧义键 / 解析失败原因」。
// 诊断与单测用 —— 生产路径上界面不显示它，但没有它就没法一眼确认内嵌数据是活的。
func (m *actorMappingIndex) Stats() (entries, keys, ambiguous int, errMsg string) {
	_ = m.ensureLoaded()
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.entries), m.keys, m.ambiguous, m.errMsg
}

// actorMappingStatus 是给界面 / 诊断看的一份快照。
type actorMappingStatus struct {
	// Source 说明现在用的是哪一份：builtin（内嵌基线）或 cache（在线更新过）。
	Source    string `json:"source"`
	Entries   int    `json:"entries"`
	Keys      int    `json:"keys"`
	Ambiguous int    `json:"ambiguous"`
	Bytes     int    `json:"bytes"`
	// UpdatedAt 只在 cache 时才有值（文件的 mtime）。
	UpdatedAt string `json:"updated_at,omitempty"`
	FromURL   string `json:"from_url,omitempty"`
	CachePath string `json:"cache_path"`
	// Note 是「缓存坏了已回退」这类**不致命**的说明；Error 是内嵌基线都读不出来。
	Note  string `json:"note,omitempty"`
	Error string `json:"error,omitempty"`
	// EmbeddedBytes 是出厂基线的体积（不解析也能给），用来一眼看出「更新后的表
	// 跟基线差多少」。
	EmbeddedBytes int      `json:"embedded_bytes"`
	Upstream      []string `json:"upstream"`
}

// mappingStatus 返回当前映射表状态。
func (a *App) mappingStatus() *actorMappingStatus {
	m := a.actorMapping()
	_ = m.ensureLoaded()
	m.mu.Lock()
	defer m.mu.Unlock()
	st := &actorMappingStatus{
		Source:        m.source,
		Entries:       len(m.entries),
		Keys:          m.keys,
		Ambiguous:     m.ambiguous,
		Bytes:         m.bytes,
		FromURL:       m.fromURL,
		CachePath:     m.cachePath,
		Note:          m.note,
		Error:         m.errMsg,
		EmbeddedBytes: len(builtinActorMappingXML),
		Upstream:      append([]string(nil), actorMappingUpstreamURLs...),
	}
	if !m.updatedAt.IsZero() {
		st.UpdatedAt = m.updatedAt.Format(time.RFC3339)
	}
	return st
}

// refreshActorMapping 从上游拉一份最新的映射表，校验通过后落盘并**立即生效**。
//
// 三条安全线，缺一条都会让「更新」变成破坏动作：
//  1. 拉下来的东西必须**解析得出来**（错误页 / 半截文件挡在外面）；
//  2. 条数不能少于 mappingMinEntries（挡住「解析成功但只有 3 条」的截断文件）；
//  3. 先落盘再换内存 —— 写盘失败就别声称更新成功，否则重启后又变回去。
//
// 全部失败时保留现状：继续用内存里那一份（cache 或基线），只把原因返回给界面。
func (a *App) refreshActorMapping(ctx context.Context) (*actorMappingStatus, error) {
	client := newHTTPClient(a.store.Get())
	var lastErr error
	for _, u := range actorMappingUpstreamURLs {
		raw, err := fetchMappingBytes(ctx, client, u)
		if err != nil {
			lastErr = fmt.Errorf("%s：%w", u, err)
			continue
		}
		entries, perr := parseActorMappingXML(raw)
		if perr != nil {
			lastErr = fmt.Errorf("%s 返回的内容不是有效的映射表：%w", u, perr)
			continue
		}
		if len(entries) < mappingMinEntries {
			lastErr = fmt.Errorf("%s 返回的映射表只有 %d 条，拒绝用它覆盖（阈值 %d）", u, len(entries), mappingMinEntries)
			continue
		}
		cachePath := actorMappingCachePath()
		tmp := cachePath + ".tmp"
		if err := os.WriteFile(tmp, raw, 0o644); err != nil {
			return nil, fmt.Errorf("写入 %s 失败：%w", cachePath, err)
		}
		if err := os.Rename(tmp, cachePath); err != nil {
			return nil, fmt.Errorf("替换 %s 失败：%w", cachePath, err)
		}
		if err := a.actorMapping().install(raw, entries, mappingSourceCache, time.Now(), u); err != nil {
			return nil, err
		}
		return a.mappingStatus(), nil
	}
	if lastErr == nil {
		lastErr = errors.New("没有可用的上游地址")
	}
	return nil, fmt.Errorf("更新失败，仍在用当前这份数据：%w", lastErr)
}

// fetchMappingBytes 取一份映射表（带浏览器 UA，上限 8 MB）。
func fetchMappingBytes(ctx context.Context, client *http.Client, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Accept", "application/xml,text/xml,*/*")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	raw, err := readAllLimit(resp.Body, 8<<20)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, errors.New("响应为空")
	}
	return raw, nil
}

// ensureLoaded 解析数据并建索引，**只做一次**。
//
// 顺序：cache（用户点过「从上游更新」）→ 内嵌基线。
// cache 坏掉时**不让功能整体失效**：回退基线，把原因记进 note，等界面显示/诊断拿走。
// 「更新失败导致映射表全没了」比「继续用旧数据」糟糕得多。
func (m *actorMappingIndex) ensureLoaded() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.loaded {
		return m.loadErr
	}
	m.loaded = true

	if m.cachePath != "" {
		if fi, err := os.Stat(m.cachePath); err == nil && fi.Size() > 0 {
			raw, rerr := os.ReadFile(m.cachePath)
			if rerr != nil {
				m.note = fmt.Sprintf("cache 里的映射表读不出来（%v），已回退到内嵌基线", rerr)
			} else if entries, perr := parseActorMappingXML(raw); perr != nil {
				m.note = fmt.Sprintf("cache 里的映射表解析失败（%v），已回退到内嵌基线", perr)
			} else if len(entries) < mappingMinEntries {
				m.note = fmt.Sprintf("cache 里的映射表只有 %d 条（少于 %d 条），已回退到内嵌基线", len(entries), mappingMinEntries)
			} else {
				m.adoptLocked(raw, entries, mappingSourceCache, fi.ModTime())
				return nil
			}
		}
	}

	entries, err := parseActorMappingXML(m.raw)
	if err != nil {
		m.errMsg = fmt.Sprintf("内嵌的姓名映射表读不出来：%v。%s。这份数据是**编译进程序**的，不是外部文件 —— 出现这个说明产物本身有问题", err, actorMappingHint)
		m.loadErr = errors.New(m.errMsg)
		return m.loadErr
	}
	m.adoptLocked(m.raw, entries, mappingSourceBuiltin, time.Time{})
	return nil
}

// buildActorMappingIndex 建「写法 -> 记录」的索引。
//
// 先把每个键指向的所有记录收集齐，再**只保留唯一指向的键** —— 一个键对应两条记录时
// 无法判断该认谁，而认错就是「把别人的资料写进你的 Emby」，界面上还看不出异常。
// 详见文件头第 1 点。
func buildActorMappingIndex(entries []actorMappingEntry) (map[string]*actorMappingEntry, int) {
	owners := make(map[string][]int, len(entries)*3)
	for i := range entries {
		seen := map[string]bool{}
		for _, n := range entries[i].names() {
			k := normName(n)
			if k == "" || seen[k] {
				continue
			}
			seen[k] = true
			owners[k] = append(owners[k], i)
		}
	}
	idx := make(map[string]*actorMappingEntry, len(owners))
	ambiguous := 0
	for k, ids := range owners {
		if len(ids) != 1 {
			ambiguous++
			continue
		}
		idx[k] = &entries[ids[0]]
	}
	return idx, ambiguous
}

// parseActorMappingXML 解析映射表。
//
// 用流式 Decoder 而不是一次性 Unmarshal：文档里 2.6 万个 `<a>` 节点，
// 流式处理不需要先把整棵树建成结构体。同时**只按元素名 `a` 取**，
// 于是精简表（根 `<actor>`）与上游源文件（根 `<actor-mapping>`）都能直接吃。
//
// 属性值里的实体由 encoding/xml 解码（上游文件里 `&amp;` 这类转义多处存在），
// 我们只取名字，别的属性（tmdb_id / bio_graphy / verified）一概不碰。
func parseActorMappingXML(raw []byte) ([]actorMappingEntry, error) {
	// BOM 必须先剥：带 BOM 时第一个 Token 是字符数据而不是 `<?xml`，
	// 严格意义上 XMLDecl 必须出现在最前面，某些解析器会直接报错。
	// （实测精简表不带 BOM，但「实测」不是格式保证 —— 上游重新生成时带上就有了。）
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, errors.New("文件是空的")
	}

	dec := xml.NewDecoder(bytes.NewReader(raw))
	// 名字里可能出现日文生僻字与汉字异体字，不必按 UTF-8 以外的编码猜。
	dec.Strict = true

	var out []actorMappingEntry
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			line, col := dec.InputPos()
			return nil, fmt.Errorf("XML 解析失败（第 %d 行第 %d 列附近）：%w", line, col, err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "a" {
			continue
		}
		var node struct {
			ZhCN    string `xml:"zh_cn,attr"`
			ZhTW    string `xml:"zh_tw,attr"`
			JP      string `xml:"jp,attr"`
			Keyword string `xml:"keyword,attr"`
		}
		if err := dec.DecodeElement(&node, &se); err != nil {
			return nil, fmt.Errorf("读取第 %d 条记录失败：%w", len(out)+1, err)
		}
		e := actorMappingEntry{
			ZhCN: strings.TrimSpace(node.ZhCN),
			ZhTW: strings.TrimSpace(node.ZhTW),
			JP:   strings.TrimSpace(node.JP),
		}
		// keyword 用英文逗号分隔且**不带空格**（上游格式要求）。这里仍逐个 trim，
		// 因为手改过的条目未必守规矩，而多一个空别名就会污染索引。
		for _, k := range strings.Split(node.Keyword, ",") {
			if k = strings.TrimSpace(k); k != "" {
				e.Keywords = append(e.Keywords, k)
			}
		}
		if len(e.names()) == 0 {
			continue // 空壳条目（四个属性全空）不该进索引
		}
		out = append(out, e)
	}
	if len(out) == 0 {
		return nil, errors.New("没有解析出任何 <a> 记录（元素名或属性名可能变了）")
	}
	return out, nil
}
