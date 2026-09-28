package main

// 人物归并：把 Emby 里「其实是同一个人」的多个条目合成一个。
//
// 为什么这个东西需要存在：Emby 的人物库是**按名字自动建条目**的，同一个演员
// 只要在不同媒体库里被写成「三上悠亜」和「三上悠亞」，或者某次刮削带了
// 「(中文)」后缀，就会各建一个条目。结果是演员页里出现两条同名人物，
// 各自挂着一半作品，头像也只补上了一个 —— 用户看到的现象是「这个演员有
// 一半片子没头像」，而原因在人物条目上，不在片子上。
//
// 归并做三件事：把 drop 名下所有作品的 People 引用改写成 keep、把 drop 的
// 头像补给 keep（keep 没有头像时）、删掉 drop 条目。三件事都不可逆，
// 所以强制走「预演 → 点两次 → 写入」，并且每次写条目 People 前都留快照。
//
// 已知边界（界面上也写了）：
//   - 回滚能还原「作品原本挂在谁名下」，但**不会重建被删掉的人物条目**。
//   - 简繁/异体字（亜 / 亚）**不自动算成同一个人**：日文名里两者都有人真在用，
//     并错的代价大于漏并。要抓这类把阈值调低，候选会带分数列出来由人确认。

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	dupDefaultMinScore = 86
	dupDefaultLimit    = 3000
	dupMaxLimit        = 20000
	dupPersonPageSize  = 500
	dupCountWorkers    = 6
	// dupItemPageLimit 是合并时单个 drop 条目一次翻页的作品数上限。
	// 一次翻完（而不是边翻边写）是**有意**的：写入会改变 People，
	// 而 People 正是筛选条件，边翻边写会让分页错位、漏掉作品。
	dupItemPageLimit = 200
	// dupMaxItemsPerDrop 是单个 drop 最多处理多少部作品，防止一次点歪把整库刷一遍。
	dupMaxItemsPerDrop = 2000
)

// dupPersonView 是候选列表里的一个人物。
type dupPersonView struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	ImageTag string `json:"image_tag,omitempty"`
	Works    int    `json:"works"`
}

// dupGroupView 是一组「可能是同一个人」的候选。
type dupGroupView struct {
	Key     string          `json:"key"`
	Reason  string          `json:"reason"`
	Persons []dupPersonView `json:"persons"`
}

// dupReportView 是一次查重的结果。
type dupReportView struct {
	Scanned   int            `json:"scanned"`
	Total     int            `json:"total"`
	Truncated bool           `json:"truncated"`
	Persons   int            `json:"persons"`
	MinScore  int            `json:"min_score"`
	Note      string         `json:"note,omitempty"`
	Groups    []dupGroupView `json:"groups"`
}

// PersonMergePlan 是某个 drop 条目在本次归并里的动作。
type PersonMergePlan struct {
	DropID   string `json:"drop_id"`
	DropName string `json:"drop_name"`
	Items    int    `json:"items"`
	Moved    int    `json:"moved"`
	Image    bool   `json:"image"`
	Deleted  bool   `json:"deleted"`
}

// PersonMergeResult 是一次归并（含预演）的结果。
type PersonMergeResult struct {
	KeepID     string            `json:"keep_id"`
	KeepName   string            `json:"keep_name"`
	DryRun     bool              `json:"dry_run"`
	Plan       []PersonMergePlan `json:"plan"`
	TotalItems int               `json:"total_items"`
	Deleted    int               `json:"deleted"`
	Snapshots  []string          `json:"snapshots,omitempty"`
	Errors     []string          `json:"errors,omitempty"`
}

// ---------- 名字相似度 ----------

// nameSimilarity 返回两个名字的相似度（0..100）。
//
// 分数是可解释的，界面会把理由写出来让用户自己确认：
//
//	100  归一化后完全相同（大小写 / 全半角 / 空格 / 标点差异都算同一个）
//	 90  一方是另一方的子串，且短的那方占比够（「三上悠亜」/「三上悠亜(中文)」）
//	其余  编辑距离相似度
//
// 别名记忆那条更强的信号（97）在 clusterDuplicates 里处理 —— 它需要查表，
// 不该塞进一个纯字符串比较函数里。
func nameSimilarity(a, b string) int {
	na, nb := normName(a), normName(b)
	if na == "" || nb == "" {
		return 0
	}
	if na == nb {
		return 100
	}
	if strings.Contains(na, nb) || strings.Contains(nb, na) {
		short, long := na, nb
		if len([]rune(short)) > len([]rune(long)) {
			short, long = long, short
		}
		if len([]rune(short)) >= 2 &&
			float64(len([]rune(short)))/float64(len([]rune(long))) >= 0.6 {
			return 90
		}
	}
	return editSimilarity(na, nb)
}

// editSimilarity 把编辑距离换算成 0..100 的相似度。
func editSimilarity(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 || len(rb) == 0 {
		return 0
	}
	d := runeDistance(ra, rb)
	max := len(ra)
	if len(rb) > max {
		max = len(rb)
	}
	sim := 100 * (max - d) / max
	if sim < 0 {
		return 0
	}
	return sim
}

// runeDistance 是 Levenshtein 距离（按 rune 算，中文名字按字符计才合理）。
func runeDistance(a, b []rune) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = minInt(prev[j]+1, minInt(cur[j-1]+1, prev[j-1]+cost))
		}
		copy(prev, cur)
	}
	return prev[len(b)]
}

// clusterDuplicates 把人物按「可能是同一个人」分组成簇。
//
// 三条产生候选的路径，按可靠程度从高到低：
//  1. 归一化后名字完全相同 —— 最常见，也最该归并的一类。
//  2. 别名记忆里指向同一组 —— 抓资料时人工确认过，可靠度最高。
//  3. 名字写法相近（首个字符相同、长度差不超过 1、相似度 ≥ minScore）——
//     用「首字符 + 长度」分桶把比较量压下来，否则万级人物做两两比较是 O(n²)。
func clusterDuplicates(items []dupPersonView, minScore int, canon func(string) string) []dupGroupView {
	n := len(items)
	if n < 2 {
		return nil
	}
	uf := newUnionFind(n)
	link := map[int]int{} // 找到的最强理由：0 归一化 / 1 别名 / 2 相似
	record := func(i, j, kind int) {
		uf.union(i, j)
		for _, k := range []int{i, j} {
			if old, ok := link[k]; !ok || kind < old {
				link[k] = kind
			}
		}
	}

	// 1) 归一化名字相同的分到一组
	byNorm := map[string][]int{}
	for i, it := range items {
		key := normName(it.Name)
		if key == "" {
			continue
		}
		byNorm[key] = append(byNorm[key], i)
	}
	for _, idxs := range byNorm {
		for k := 1; k < len(idxs); k++ {
			record(idxs[0], idxs[k], 0)
		}
	}

	// 2) 别名记忆
	aliasSeen := map[string][]int{}
	for i, it := range items {
		if c := canon(it.Name); c != "" {
			aliasSeen[c] = append(aliasSeen[c], i)
		}
	}
	for _, idxs := range aliasSeen {
		for k := 1; k < len(idxs); k++ {
			record(idxs[0], idxs[k], 1)
		}
	}

	// 3) 名字相近：按「首字符 + 长度」分桶
	type bucket struct {
		first  rune
		length int
	}
	buckets := map[bucket][]int{}
	for i, it := range items {
		name := normName(it.Name)
		r := []rune(name)
		if len(r) < 2 {
			continue
		}
		k := bucket{first: r[0], length: len(r)}
		buckets[k] = append(buckets[k], i)
	}
	for _, idxs := range buckets {
		if len(idxs) < 2 || len(idxs) > 80 {
			// 桶太大说明这一桶里全是「同一个首字母 + 同长度」的不同人，
			// 两两比较既慢又没什么价值，直接跳过。
			continue
		}
		for a := 0; a < len(idxs); a++ {
			for b := a + 1; b < len(idxs); b++ {
				na, nb := items[idxs[a]].Name, items[idxs[b]].Name
				if nameSimilarity(na, nb) >= minScore {
					record(idxs[a], idxs[b], 2)
				}
			}
		}
	}

	// 收集簇
	groups := map[int][]int{}
	for i := range items {
		root := uf.find(i)
		groups[root] = append(groups[root], i)
	}
	out := make([]dupGroupView, 0, len(groups))
	for _, idxs := range groups {
		if len(idxs) < 2 {
			continue
		}
		g := dupGroupView{Persons: make([]dupPersonView, 0, len(idxs))}
		kinds := map[int]bool{}
		canons := map[string]bool{}
		for _, i := range idxs {
			g.Persons = append(g.Persons, items[i])
			if k, ok := link[i]; ok {
				kinds[k] = true
			}
			if c := canon(items[i].Name); c != "" {
				canons[c] = true
			}
		}
		// 顺序稳定：作品多的在前（更可能是"正主"），作品数相同按名字。
		sortGroupPersons(&g)
		g.Key = g.Persons[0].Name
		switch {
		case len(canons) > 0:
			keys := make([]string, 0, len(canons))
			for c := range canons {
				keys = append(keys, c)
			}
			sort.Strings(keys)
			g.Key = keys[0]
			g.Reason = "别名记忆里指向同一个人（" + strings.Join(keys, " / ") + "）"
		case kinds[0]:
			g.Reason = "归一化后名字完全相同（大小写 / 全半角 / 空格 / 标点差异都算同一个）"
		default:
			g.Reason = "名字写法相近（相似度 ≥ " + itoa(minScore) + "%），请逐个确认"
		}
		out = append(out, g)
	}
	sort.SliceStable(out, func(a, b int) bool {
		if len(out[a].Persons) != len(out[b].Persons) {
			return len(out[a].Persons) > len(out[b].Persons)
		}
		return out[a].Key < out[b].Key
	})
	return out
}

// sortGroupPersons 把组内人物按「作品多的在前」重排。
//
// 抽出来是因为它必须能被**调两次**：作品数是分簇之后才逐个人物查出来的
// （要对每个人发一次 /Items 查询），而 clusterDuplicates 是纯函数、不联网、
// 拿不到作品数。只在分簇时排一次的话，那一刻所有人都是 0，等于按名字排 ——
// 界面上排在第一个、被默认选成「保留」的就不是作品最多的那个。这个 bug 真的
// 出现过：查重接口返回的顺序和「作品数」对不上，正是因为这个先后顺序。
func sortGroupPersons(g *dupGroupView) {
	sort.SliceStable(g.Persons, func(a, b int) bool {
		if g.Persons[a].Works != g.Persons[b].Works {
			return g.Persons[a].Works > g.Persons[b].Works
		}
		return g.Persons[a].Name < g.Persons[b].Name
	})
}

// unionFind 是路径压缩的并查集。
type unionFind struct{ parent []int }

func newUnionFind(n int) *unionFind {
	uf := &unionFind{parent: make([]int, n)}
	for i := range uf.parent {
		uf.parent[i] = i
	}
	return uf
}

func (u *unionFind) find(i int) int {
	for u.parent[i] != i {
		u.parent[i] = u.parent[u.parent[i]]
		i = u.parent[i]
	}
	return i
}

func (u *unionFind) union(a, b int) {
	ra, rb := u.find(a), u.find(b)
	if ra != rb {
		u.parent[rb] = ra
	}
}

// ---------- 接口 ----------

// handlePersonDuplicates 是只读查重：只列候选，不写任何东西。
func (a *App) handlePersonDuplicates(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	minScore := atoiSafe(q.Get("min_score"))
	if minScore <= 0 {
		minScore = dupDefaultMinScore
	}
	if minScore < 50 {
		minScore = 50
	}
	if minScore > 100 {
		minScore = 100
	}
	limit := atoiSafe(q.Get("limit"))
	if limit <= 0 {
		limit = dupDefaultLimit
	}
	if limit > dupMaxLimit {
		limit = dupMaxLimit
	}
	useAlias := q.Get("alias") != "false"

	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Minute)
	defer cancel()
	e := NewEmby(a.store.Get())

	// 翻页拉人物。人物库动辄上万条，必须分页；拉到 limit 就停。
	var items []dupPersonView
	total := 0
	seen := map[string]bool{}
	for start := 0; start < limit; start += dupPersonPageSize {
		pr, err := e.Persons(ctx, start, dupPersonPageSize, "", "", personTypesDefault)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		total = pr.TotalRecordCount
		for _, p := range pr.Items {
			if p.Id == "" || seen[p.Id] {
				continue
			}
			seen[p.Id] = true
			items = append(items, dupPersonView{
				ID: p.Id, Name: p.Name, ImageTag: p.ImageTags["Primary"],
			})
		}
		if len(pr.Items) == 0 || start+dupPersonPageSize >= pr.TotalRecordCount {
			break
		}
	}

	canon := func(string) string { return "" }
	if useAlias && a.aliases != nil {
		canon = a.aliases.CanonicalKey
	}
	groups := clusterDuplicates(items, minScore, canon)
	// 只给候选补作品数：全量统计要对每个人发一次查询，上万条不划算。
	a.countWorksForGroups(ctx, e, groups)
	// 补完作品数必须**重排一次**（原因见 sortGroupPersons 的注释）。
	for i := range groups {
		sortGroupPersons(&groups[i])
	}

	rep := dupReportView{
		Scanned:   len(items),
		Total:     total,
		Truncated: total > len(items),
		MinScore:  minScore,
		Groups:    groups,
	}
	for _, g := range groups {
		rep.Persons += len(g.Persons)
	}
	if !useAlias {
		rep.Note = "未参考别名记忆"
	}
	writeOK(w, rep)
}

// countWorksForGroups 给候选人物补「有多少部作品」。
//
// 这个数字是排序与判断的主要依据（作品多的那个通常是"正主"），
// 所以值得多发这些查询；但只对**进了候选的人**发，不是全库。
func (a *App) countWorksForGroups(ctx context.Context, e *Emby, groups []dupGroupView) {
	type job struct{ gi, pi int }
	sem := make(chan struct{}, dupCountWorkers)
	var wg sync.WaitGroup
	for gi := range groups {
		for pi := range groups[gi].Persons {
			wg.Add(1)
			go func(gi, pi int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				id := groups[gi].Persons[pi].ID
				res, err := e.Items(ctx, ItemQuery{
					PersonIDs: []string{id},
					Recursive: true,
					Limit:     1,
				})
				if err == nil {
					groups[gi].Persons[pi].Works = res.TotalRecordCount
				}
			}(gi, pi)
		}
	}
	wg.Wait()
}

// handlePersonMerge 执行（或预演）一次归并。
func (a *App) handlePersonMerge(w http.ResponseWriter, r *http.Request) {
	var in struct {
		KeepID  string   `json:"keep_id"`
		DropIDs []string `json:"drop_ids"`
		DryRun  bool     `json:"dry_run"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	keepID := strings.TrimSpace(in.KeepID)
	if keepID == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("缺少要保留的人物 ID"))
		return
	}
	drops := make([]string, 0, len(in.DropIDs))
	for _, d := range in.DropIDs {
		d = strings.TrimSpace(d)
		if d != "" && d != keepID {
			drops = append(drops, d)
		}
	}
	if len(drops) == 0 {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("至少要有一个要并入的条目"))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	e := NewEmby(a.store.Get())
	res, err := a.runPersonMerge(ctx, e, keepID, drops, in.DryRun)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeOK(w, res)
}

// runPersonMerge 是归并的实际流程。预演与真跑共用同一条路径 ——
// 分成两套实现的话，迟早会出现「预演说改 3 部，真跑改了 5 部」。
func (a *App) runPersonMerge(ctx context.Context, e *Emby, keepID string, dropIDs []string,
	dry bool) (*PersonMergeResult, error) {
	keep, err := readPersonBrief(ctx, e, keepID)
	if err != nil {
		return nil, fmt.Errorf("读不到要保留的人物 %s：%w", keepID, err)
	}
	res := &PersonMergeResult{KeepID: keepID, KeepName: keep.name, DryRun: dry}

	for _, dropID := range dropIDs {
		drop, err := readPersonBrief(ctx, e, dropID)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("读不到人物 %s：%v", dropID, err))
			continue
		}
		plan := PersonMergePlan{DropID: dropID, DropName: drop.name}

		items, err := itemsOfPerson(ctx, e, dropID, dupMaxItemsPerDrop)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("列出「%s」的作品失败：%v", drop.name, err))
			continue
		}
		plan.Items = len(items)
		for _, it := range items {
			itemID := itemStr(it, "Id")
			if itemID == "" {
				continue
			}
			newPeople, changed := repointPeople(it["People"], dropID, keepID, keep.name)
			if !changed {
				continue
			}
			if dry {
				plan.Moved++
				continue
			}
			// 快照在写入之前取：People 是列表字段，回滚时会被发成 `[]` 或原数组，
			// 走的是已经验证过的 itemsnapshot 那条路。
			if sid := a.recordItemWrite(it, map[string]any{"People": newPeople}, "人物归并"); sid != "" {
				res.Snapshots = append(res.Snapshots, sid)
			}
			if err := e.UpdateItemExact(ctx, itemID, map[string]any{"People": newPeople}); err != nil {
				res.Errors = append(res.Errors,
					fmt.Sprintf("改写作品 %s 的演员失败：%v", firstNonEmpty(itemStr(it, "Name"), itemID), err))
				continue
			}
			plan.Moved++
		}

		// 头像：keep 没有、drop 有，才转 —— 否则就是把好东西换成差东西。
		if keep.imageTag == "" && drop.imageTag != "" {
			plan.Image = true
			if !dry {
				if err := e.CopyImage(ctx, dropID, keepID, "Primary"); err != nil {
					res.Errors = append(res.Errors, fmt.Sprintf("转移头像失败：%v", err))
					plan.Image = false
				} else {
					keep.imageTag = drop.imageTag
				}
			}
		}

		if !dry {
			if err := e.DeleteItem(ctx, dropID); err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("删除人物「%s」失败：%v", drop.name, err))
			} else {
				plan.Deleted = true
				res.Deleted++
			}
		}
		res.Plan = append(res.Plan, plan)
		res.TotalItems += plan.Moved
	}
	return res, nil
}

// personBrief 是一个人物条目里我们用得到的两个字段。
type personBrief struct {
	id, name, imageTag string
}

// readPersonBrief 读一个人物的基本信息。
//
// 走 `/Items/{id}` 而不是 `/Persons?SearchTerm=`：后者按名字搜，同名的人
// 会互相串（这正是本功能要解决的问题），拿 ID 直接读才唯一。
func readPersonBrief(ctx context.Context, e *Emby, id string) (personBrief, error) {
	it, err := e.ItemDetail(ctx, id)
	if err != nil {
		return personBrief{}, err
	}
	p := personBrief{id: id, name: itemStr(it, "Name")}
	if tags, ok := it["ImageTags"].(map[string]any); ok {
		if v, ok := tags["Primary"].(string); ok {
			p.imageTag = v
		}
	}
	if p.name == "" {
		return p, fmt.Errorf("条目没有名字")
	}
	return p, nil
}

// itemsOfPerson 列出挂在某个人物名下的全部作品。
//
// 一次翻完再写（见 dupItemPageLimit 的注释）：写入会改 People，
// 而 People 就是筛选条件，边翻边写会让分页错位漏掉作品。
func itemsOfPerson(ctx context.Context, e *Emby, personID string, max int) ([]Item, error) {
	var out []Item
	for start := 0; start < max; start += dupItemPageLimit {
		res, err := e.Items(ctx, ItemQuery{
			PersonIDs:  []string{personID},
			Recursive:  true,
			Fields:     []string{"People"},
			StartIndex: start,
			Limit:      dupItemPageLimit,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, res.Items...)
		if len(res.Items) == 0 || start+dupItemPageLimit >= res.TotalRecordCount {
			break
		}
	}
	return out, nil
}

// repointPeople 把 People 数组里指向 dropID 的引用改成 keep。
//
// 返回新数组与「是否真的有改动」。保留原有的 Type/Role（用户可能把某个人
// 定位成导演或写了角色名），只换 Id 和 Name —— 整条替换会把这些信息抹掉。
func repointPeople(raw any, dropID, keepID, keepName string) ([]any, bool) {
	list, ok := raw.([]any)
	if !ok {
		return nil, false
	}
	out := make([]any, 0, len(list))
	changed := false
	for _, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			out = append(out, e)
			continue
		}
		if itemStr(m, "Id") != dropID {
			out = append(out, e)
			continue
		}
		cp := make(map[string]any, len(m))
		for k, v := range m {
			cp[k] = v
		}
		cp["Id"] = keepID
		cp["Name"] = keepName
		out = append(out, cp)
		changed = true
	}
	if !changed {
		return nil, false
	}
	// 同一个人在同一部片子里本来就出现两次时不合并 —— 那可能是两个角色，
	// 合并会丢掉一个。这里只做去重保护：完全相同的两条只留一条。
	seen := map[string]bool{}
	dedup := out[:0]
	for _, e := range out {
		if m, ok := e.(map[string]any); ok && itemStr(m, "Id") == keepID {
			key := keepID + "|" + itemStr(m, "Type") + "|" + itemStr(m, "Role")
			if seen[key] {
				changed = true
				continue
			}
			seen[key] = true
		}
		dedup = append(dedup, e)
	}
	return dedup, changed
}
