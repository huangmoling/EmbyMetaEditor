package main

// 媒体库体检：只读扫描一个库，按「问题类型」把条目分组列出来。
//
// 为什么值得做：这个工具的核心动作（刮削）依赖元数据本身的质量 ——
// 没有番号就搜不了、标题还是文件名就永远匹配不上、没有年份/海报就分不清是哪一部。
// 但在此之前用户只能靠「凭感觉翻列表」。体检把「哪些条目还没被收拾过」
// 变成一份可数的清单。
//
// 这里刻意**只读**：扫描不发任何写请求，所有修复动作都由用户点进具体条目去做。
// 一个「自动修复 N 条」的按钮配上一个不可逆的写接口，出事只是时间问题。

import (
	"net/http"
	"strings"
)

// 严重程度：err = 会直接挡住刮削；warn = 影响展示/筛选。
const (
	healthErr  = "err"
	healthWarn = "warn"
)

// healthIssueDef 是一类问题的定义。顺序即界面上分组的展示顺序（先严重的）。
type healthIssueDef struct {
	Key   string
	Label string
	Hint  string
	Level string
	Has   func(Item) bool
}

// mediaExts 是共享里的常见容器扩展名。标题以它们结尾，说明这条从下载站
// 进来之后就没被刮过 —— 名字还是文件名。
var mediaExts = []string{
	".mp4", ".mkv", ".avi", ".wmv", ".mov", ".ts", ".m2ts", ".strm",
	".rmvb", ".rm", ".flv", ".webm", ".iso", ".mpg", ".mpeg", ".m4v", ".f4v",
}

// looksLikeFilename 判断标题是不是「原始文件名 / 下载站水印名」。
//
// 判据刻意保守：只认**扩展名结尾**和**@下载站域名**两种最常见的形态。
// 宽了会把正经片名误报（比如片名里带点的），那比漏报更烦人。
func looksLikeFilename(name string) bool {
	s := strings.ToLower(strings.TrimSpace(name))
	if s == "" {
		return false
	}
	for _, e := range mediaExts {
		if strings.HasSuffix(s, e) {
			return true
		}
	}
	// hhd800.com@91CM014 / xxx.net@SSIS-001 这类水印前缀
	if at := strings.Index(s, "@"); at > 0 {
		head := s[:at]
		if strings.Contains(head, ".com") || strings.Contains(head, ".net") ||
			strings.Contains(head, ".org") || strings.Contains(head, ".cc") {
			return true
		}
	}
	return false
}

// itemTagsOf 读条目上的标签。这个 Emby 构建的详情接口不返回 Tags，
// 标签只体现在 TagItems 里 —— 两边都看一眼才准。
func itemTagsOf(it Item) []string {
	out := []string{}
	if raw, ok := it["Tags"].([]any); ok {
		for _, v := range raw {
			if s, _ := v.(string); strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
	}
	if raw, ok := it["TagItems"].([]any); ok {
		for _, v := range raw {
			if m, ok := v.(map[string]any); ok {
				if s, _ := m["Name"].(string); strings.TrimSpace(s) != "" {
					out = append(out, strings.TrimSpace(s))
				}
			}
		}
	}
	return dedupeStrings(out)
}

// healthIssueDefs 是体检的全部规则。加规则只改这里，报告与界面自动跟着走。
var healthIssueDefs = []healthIssueDef{
	{
		Key: "no_number", Label: "认不出番号", Level: healthErr,
		Hint: "刮削要靠番号去搜。这类条目名称里没有「字母+数字」的番号，自动刮削会直接跳过，只能手动指定。",
		Has:  func(it Item) bool { return itemNumber(it) == "" },
	},
	{
		Key: "title_is_filename", Label: "标题像文件名", Level: healthErr,
		Hint: "标题还是下载时的文件名（带 .mp4 / @下载站域名）。这类标题既不像片名，也常常认不出番号。",
		Has: func(it Item) bool {
			n, _ := it["Name"].(string)
			return looksLikeFilename(n)
		},
	},
	{
		Key: "missing_poster", Label: "缺海报", Level: healthWarn,
		Hint: "没有主图（海报）。列表和详情页都会显示成灰块，也不利于人工核对刮削结果。",
		Has:  func(it Item) bool { return !imageTagExists(it, "Primary") },
	},
	{
		Key: "missing_overview", Label: "缺简介", Level: healthWarn,
		Hint: "简介为空。多数刮削源都带简介，通常是这条从没刮过或被清空过。",
		Has: func(it Item) bool {
			s, _ := it["Overview"].(string)
			return strings.TrimSpace(s) == ""
		},
	},
	{
		Key: "no_year", Label: "缺年份", Level: healthWarn,
		Hint: "ProductionYear 为 0 且没有发行日期。年份是列表里最主要的区分依据之一。",
		Has: func(it Item) bool {
			if y, ok := it["ProductionYear"].(float64); ok && int(y) > 0 {
				return false
			}
			s, _ := it["PremiereDate"].(string)
			return strings.TrimSpace(s) == ""
		},
	},
	{
		Key: "no_tags", Label: "无标签", Level: healthWarn,
		Hint: "标签为空。国产传媒那条链路会把番号写进标签，无标签通常表示没走过刮削。",
		Has:  func(it Item) bool { return len(itemTagsOf(it)) == 0 },
	},
}

// healthItem 是报告里的一条条目（只带界面需要的字段，别把整个 DTO 塞进去）。
type healthItem struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Number string   `json:"number"`
	Year   int      `json:"year"`
	Path   string   `json:"path"`
	Issues []string `json:"issues"`
}

// healthGroup 是一类问题的分组结果。
type healthGroup struct {
	Key   string       `json:"key"`
	Label string       `json:"label"`
	Hint  string       `json:"hint"`
	Level string       `json:"level"`
	Count int          `json:"count"` // 命中总数
	More  int          `json:"more"`  // 因为分组上限而没列出来的条数
	Items []healthItem `json:"items"`
}

// healthReport 是一次体检的结果。
type healthReport struct {
	Parent    string        `json:"parent"`
	Scanned   int           `json:"scanned"`   // 实际检查了几条
	Total     int           `json:"total"`     // 库里一共几条（Emby 报的）
	Truncated bool          `json:"truncated"` // 是否因为上限只检查了一部分
	Clean     int           `json:"clean"`     // 一条问题都没有的条数
	Problem   int           `json:"problem"`   // 至少有一个问题的条数
	Groups    []healthGroup `json:"groups"`
}

// healthGroupItemCap 每个分组最多列多少条。
//
// 一个万条的大库可能整库都缺海报，全塞进 JSON 会让接口变成几 MB，
// 界面也没人会翻到第 3000 条 —— 计数给全，明细给前若干条就够了。
const healthGroupItemCap = 100

// buildHealthReport 从条目列表算出报告。纯函数，方便单测。
func buildHealthReport(parent string, items []Item, total int) healthReport {
	rep := healthReport{Parent: parent, Scanned: len(items), Total: total, Truncated: total > len(items)}
	rep.Groups = make([]healthGroup, 0, len(healthIssueDefs))

	// 先把每条的问题算出来（条目会同时落进多个分组）
	perItem := make([]healthItem, 0, len(items))
	for _, it := range items {
		id, _ := it["Id"].(string)
		name, _ := it["Name"].(string)
		path, _ := it["Path"].(string)
		year := 0
		if y, ok := it["ProductionYear"].(float64); ok {
			year = int(y)
		}
		hi := healthItem{ID: id, Name: name, Number: itemNumber(it), Year: year, Path: path}
		for _, def := range healthIssueDefs {
			if def.Has(it) {
				hi.Issues = append(hi.Issues, def.Key)
			}
		}
		if len(hi.Issues) == 0 {
			rep.Clean++
		} else {
			rep.Problem++
		}
		perItem = append(perItem, hi)
	}

	for _, def := range healthIssueDefs {
		g := healthGroup{Key: def.Key, Label: def.Label, Hint: def.Hint, Level: def.Level,
			Items: []healthItem{}}
		for _, hi := range perItem {
			if !cnHasString(hi.Issues, def.Key) {
				continue
			}
			g.Count++
			if len(g.Items) < healthGroupItemCap {
				g.Items = append(g.Items, hi)
			}
		}
		g.More = g.Count - len(g.Items)
		rep.Groups = append(rep.Groups, g)
	}
	return rep
}

// ---------- 接口 ----------

// healthDefaultLimit / healthMaxLimit 是一次体检检查的条目数上限。
// 只读扫描，但仍然要封顶：一个万条的大库全量拉回来既慢又占内存，
// 而体检本来就是「先看一批、修一批」的用法。
const (
	healthDefaultLimit = 500
	healthMaxLimit     = 2000
)

// handleHealth 对某个媒体库做一次只读体检。
//
// 不传 parent 就是全库（跨所有媒体库）—— 允许，因为「我整个库里哪些条目没收拾过」
// 本身就是一个合理的问题。但 UI 默认要求先选库，免得用户无意中扫出一个巨大的报告。
func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	parent := strings.TrimSpace(q.Get("parent"))
	limit := atoiSafe(q.Get("limit"))
	if limit <= 0 {
		limit = healthDefaultLimit
	}
	if limit > healthMaxLimit {
		limit = healthMaxLimit
	}
	itemType := firstNonEmpty(strings.TrimSpace(q.Get("type")), "Movie")

	res, err := NewEmby(a.store.Get()).Items(r.Context(), ItemQuery{
		ParentID:         parent,
		Recursive:        true,
		IncludeItemTypes: itemType,
		Fields:           []string{"Path,ImageTags,ProductionYear,PremiereDate,Overview,ProviderIds,Tags,TagItems"},
		StartIndex:       0,
		Limit:            limit,
		SortBy:           "SortName",
		SortOrder:        "Ascending",
	})
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeOK(w, buildHealthReport(parent, res.Items, res.TotalRecordCount))
}
