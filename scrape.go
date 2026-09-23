package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // 注册解码器，供 image.DecodeConfig 读取头像尺寸
	_ "image/png"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"
)

// ---------- 影片刮削 ----------

// ScrapeOptions 是影片刮削的选项。
type ScrapeOptions struct {
	Provider        string `json:"provider"`
	MovieID         string `json:"movie_id"`
	OverwriteImages bool   `json:"overwrite_images"`
	Refresh         bool   `json:"refresh"`
}

// ScrapeResult 描述一次刮削的结果。
type ScrapeResult struct {
	ItemID   string   `json:"item_id"`
	ItemName string   `json:"item_name"`
	Provider string   `json:"provider"`
	MovieID  string   `json:"movie_id"`
	Number   string   `json:"number"`
	Title    string   `json:"title"`
	Fields   []string `json:"fields"`
	Images   []string `json:"images"`
	Skipped  bool     `json:"skipped"`
	Message  string   `json:"message"`
}

// ScrapeMovie 对单个条目执行一次 MetaTube 刮削（元数据 + 图片）。
func (a *App) ScrapeMovie(ctx context.Context, itemID string, opts ScrapeOptions) (*ScrapeResult, error) {
	cfg := a.store.Get()
	e := NewEmby(cfg)
	mt := NewMetaTube(cfg)
	res := &ScrapeResult{ItemID: itemID}

	item, err := e.ItemDetail(ctx, itemID)
	if err != nil {
		return nil, fmt.Errorf("读取条目失败：%w", err)
	}
	res.ItemName, _ = item["Name"].(string)

	provider, movieID := opts.Provider, opts.MovieID
	if provider == "" || movieID == "" {
		keyword := searchKeyword(item)
		if keyword == "" {
			return nil, fmt.Errorf("条目「%s」无法推断番号，请手动指定 provider:id", res.ItemName)
		}
		list, err := mt.Search(ctx, keyword, "", true)
		if err != nil {
			return nil, fmt.Errorf("MetaTube 搜索失败：%w", err)
		}
		if len(list) == 0 {
			return nil, fmt.Errorf("MetaTube 未搜到「%s」的结果", keyword)
		}
		best := pickBestMatch(list, keyword)
		provider, movieID = best.Provider, best.ID
	}
	mv, err := mt.Movie(ctx, provider, movieID)
	if err != nil {
		return nil, fmt.Errorf("获取 MetaTube 详情失败：%w", err)
	}
	res.Provider, res.MovieID = provider, movieID
	res.Number = firstNonEmpty(mv.Number, canonNumber(searchKeyword(item)))
	res.Title = firstNonEmpty(mv.TitleZh, mv.Title, mv.Number)

	patch := buildItemPatch(item, mv, res.Number)

	// 翻译：把非中文的标题、简介翻成中文。错误降级为原文，翻译失败绝不放慢 / 阻断刮削。
	if cfg.OpenAI.Ready() {
		name, _ := patch["Name"].(string)
		ov, _ := patch["Overview"].(string)
		if name != "" || ov != "" {
			// 日 / 韩原标题先存进 OriginalTitle：翻译后 Name 变中文，原文不丢。
			//（buildItemPatch 只在「原标题 != 展示标题」时才设 OriginalTitle，
			//  纯日文源两个值相同会漏设，翻译一开原文就丢了，这里补上。）
			if isJapaneseOrKorean(name) {
				patch["OriginalTitle"] = name
			}
			if nt, no, _ := a.translateMeta(ctx, name, ov); nt != "" || no != "" {
				if name != "" {
					patch["Name"] = nt
				}
				if ov != "" {
					patch["Overview"] = no
				}
			}
		}
	}

	// 标题保证以番号开头：MetaTube / 站点的源标题经常不带番号（纯日文标题、
	// 站点自己的文案标题），v1.0.5 的翻译修复只能「保留」已有番号，
	// 保不了本来就没有的 —— 用户看到的就是刮完标题里没有番号。
	// 这里在写入前主动把已知番号补到最前面；标题里已有该番号（任意常见写法）则不动。
	if n, ok := patch["Name"].(string); ok {
		patch["Name"] = ensureNumberPrefix(n, res.Number)
	}

	if err := e.UpdateItem(ctx, itemID, patch); err != nil {
		return nil, fmt.Errorf("写入元数据失败：%w", err)
	}
	res.Fields = sortedKeys(patch)

	// 图片：海报 + 缩略图 + 剧照
	hasPrimary := imageTagExists(item, "Primary")
	hasBackdrop := imageTagExists(item, "Backdrop")
	if opts.OverwriteImages || !hasPrimary {
		data, ct, err := mt.FetchImage(ctx, "primary", provider, movieID, mv.BigCoverURL, mv.CoverURL, mv.PosterURL)
		if err != nil {
			res.Message = "海报下载失败：" + err.Error()
		} else if err := e.UploadImage(ctx, itemID, "Primary", -1, data, ct); err != nil {
			res.Message = "海报上传失败：" + err.Error()
		} else {
			res.Images = append(res.Images, "Primary")
		}
	}
	// 缩略图（Thumb）：Emby 列表 / 横版视图用的那张。优先横版剧照，没有就用封面。
	if opts.OverwriteImages || !imageTagExists(item, "Thumb") {
		thumbURL := previewURLAt(mv, 0)
		if thumbURL == "" {
			thumbURL = firstNonEmpty(mv.BigCoverURL, mv.CoverURL, mv.PosterURL)
		}
		if thumbURL != "" {
			data, ct, err := mt.FetchImage(ctx, "preview", provider, movieID, thumbURL)
			switch {
			case err != nil:
				res.Message = appendScrapeNote(res.Message, "缩略图下载失败："+err.Error())
			case len(data) == 0:
				res.Message = appendScrapeNote(res.Message, "缩略图响应为空")
			default:
				if err := e.UploadImage(ctx, itemID, "Thumb", -1, data, ct); err != nil {
					res.Message = appendScrapeNote(res.Message, "缩略图上传失败："+err.Error())
				} else {
					res.Images = append(res.Images, "Thumb")
				}
			}
		}
	}
	if opts.OverwriteImages || !hasBackdrop {
		for i := 0; i < 3; i++ {
			data, ct, err := mt.FetchImage(ctx, "preview", provider, movieID, previewURLAt(mv, i))
			if err != nil {
				break
			}
			if err := e.UploadImage(ctx, itemID, "Backdrop", i, data, ct); err != nil {
				break
			}
			res.Images = append(res.Images, "Backdrop/"+itoa(i))
		}
	}
	if opts.Refresh {
		_ = e.Refresh(ctx, itemID, false)
	}
	if res.Message == "" {
		res.Message = "刮削完成"
	}
	return res, nil
}

// appendScrapeNote 往结果消息上追加一条提示（首条直接赋值，后续用「；」连接）。
func appendScrapeNote(msg, note string) string {
	if msg == "" {
		return note
	}
	return msg + "；" + note
}

// ensureNumberPrefix 保证标题以番号开头，没有就补上，已有则原样返回。
//
// 番号匹配用 numKeys 的规范形式（SSIS-001 / SSIS001 都算已有），
// 纯数字番号（010115-001 这类规范不出前缀的）退化为子串判断。
func ensureNumberPrefix(title, number string) string {
	title = strings.TrimSpace(title)
	number = strings.TrimSpace(number)
	if title == "" || number == "" {
		return title
	}
	want := canonNumber(number)
	if want == "" {
		// 纯数字番号（010115-001 这类）规范不出来，退化为子串判断
		if strings.Contains(title, number) {
			return title
		}
		return number + " " + title
	}
	for _, k := range numKeys(title) {
		if k == want {
			return title
		}
	}
	// 标题最前面的番号允许 1 位数字的压缩写法（ssis-1）：
	// numKeys 的正则要求 2 位以上数字兜不住，用 reCanon 再比一次。
	if head, _ := splitLeadingNumber(title); head != "" {
		if m := reCanon.FindStringSubmatch(strings.ToUpper(strings.TrimSpace(head))); m != nil {
			n := strings.TrimLeft(m[2], "0")
			if n == "" {
				n = "0"
			}
			if m[1]+"-"+n == want {
				return title
			}
		}
	}
	return number + " " + title
}

// previewURLAt 取第 i 张剧照的直链（用于 MetaTube 代理失败时回退）。
func previewURLAt(mv *MTMovie, i int) string {
	if i < len(mv.PreviewImages) {
		return mv.PreviewImages[i]
	}
	return ""
}

// buildItemPatch 把 MetaTube 影片信息转成 Emby 更新字段。
func buildItemPatch(item Item, mv *MTMovie, number string) map[string]any {
	patch := map[string]any{}
	title := firstNonEmpty(mv.TitleZh, mv.Title, mv.Number, number)
	if title != "" {
		patch["Name"] = title
	}
	if orig := firstNonEmpty(mv.Title, mv.TitleJa); orig != "" && orig != title {
		patch["OriginalTitle"] = orig
	}
	if p := mv.plot(); p != "" {
		patch["Overview"] = p
	}
	if number != "" {
		// 注意：实测 Emby 4.9.0.42 会**忽略** POST 里的 SortName / ForcedSortName，
		// 一律按 Name 重新计算，所以这一条在这个构建上是空操作。
		// 留着是因为标准 Emby 上它是有效的（按番号排序），删掉反而会改变那边的行为。
		patch["SortName"] = number
	}
	if d := normalizeDate(mv.ReleaseDate); d != "" {
		patch["PremiereDate"] = d
		if len(d) >= 4 {
			patch["ProductionYear"] = atoiSafe(d[:4])
		}
	}
	if len(mv.Genres) > 0 {
		patch["Genres"] = mv.Genres
	}
	tags := []string{}
	if number != "" {
		tags = append(tags, number)
	}
	if mv.Label != "" && mv.Label != mv.studio() {
		tags = append(tags, mv.Label)
	}
	if len(tags) > 0 {
		patch["Tags"] = tags
	}
	studios := []map[string]any{}
	for _, s := range []string{mv.studio(), mv.Label, mv.Series} {
		if strings.TrimSpace(s) != "" {
			studios = append(studios, map[string]any{"Name": strings.TrimSpace(s)})
		}
	}
	if len(studios) > 0 {
		patch["Studios"] = studios
	}
	people := []map[string]any{}
	for _, act := range mv.Actors {
		act = strings.TrimSpace(act)
		if act == "" {
			continue
		}
		people = append(people, map[string]any{"Name": act, "Type": "Actor", "Role": ""})
	}
	if d := strings.TrimSpace(mv.Director); d != "" {
		people = append(people, map[string]any{"Name": d, "Type": "Director", "Role": ""})
	}
	if len(people) > 0 {
		patch["People"] = people
	}
	if mv.Score > 0 {
		patch["CommunityRating"] = mv.Score
	}
	if mv.Runtime > 0 {
		patch["RunTimeTicks"] = int64(mv.Runtime) * 60 * 10_000_000
	}
	if mv.Provider != "" && mv.ID != "" {
		patch["ProviderIds"] = map[string]any{"MetaTube": mv.Provider + ":" + mv.ID}
	}
	return patch
}

// itemNumber 从条目推断番号，推断不出返回空串。
//
// 为什么要在服务端算：Emby 的 **/Items 列表不返回 SortName**（实测全是 null），
// 前端原本拿 SortName 当番号，于是卡片角标永远拿不到值、退化成显示年份。
// 用和刮削同一套归一化逻辑算好，前端直接用。
//
// 另外要处理「数字开头的番号」（91CM-014 / 91BCM-002）：
// 通用正则要求前缀是纯字母，会把前缀数字砍掉，详见下面实现里的说明。
func itemNumber(item Item) string {
	// numberSourceFields 会先把文件扩展名去掉 —— 不去的话 `.mp4` 会被当成
	// 番号 "MP-4"，一个误报就能让整库的角标和搜索关键词全错。
	joined := strings.Join(numberSourceFields(item), " ")

	generic := ""
	if ks := numKeys(joined); len(ks) > 0 {
		generic = displayNumber(ks[0])
		// 「HEVC10 1080P」这种压制组标记会被通用正则当成 HEVC-010。
		// 展示用不上这种「番号」，直接丢掉（不影响 javbus 那套匹配逻辑）。
		if cnNoisePrefix[strings.SplitN(generic, "-", 2)[0]] {
			generic = ""
		}
	}

	// 通用规则的正则要求前缀是**纯字母**，所以「数字开头的番号」会被砍掉前缀数字：
	// 91CM-014 → CM-014、91BCM-002 → BCM-002（国产传媒库里有 1400 多条这种）。
	// 只在能确认是「前缀被砍」时才改用国产传媒的提取结果 ——
	// 否则会把 HEVC10 / WEB-DL 这类压制组标记当番号。
	if cn := cnExtractNumber(joined); cn != "" {
		genPrefix := strings.SplitN(generic, "-", 2)[0]
		cnPrefix := strings.SplitN(cn, "-", 2)[0]
		if len(cnPrefix) > len(genPrefix) && strings.HasSuffix(cnPrefix, genPrefix) {
			return cn
		}
		if generic == "" {
			return cn
		}
	}
	return generic
}

// searchKeyword 从条目推断搜索关键词（优先番号，没有就用名字）。
func searchKeyword(item Item) string {
	if n := itemNumber(item); n != "" {
		return n
	}
	if s, ok := item["Name"].(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

// pickBestMatch 在搜索结果里挑最匹配的一条（优先番号一致）。
func pickBestMatch(list []MTMovie, keyword string) MTMovie {
	want := canonNumber(keyword)
	for _, m := range list {
		if canonNumber(m.Number) == want && want != "" {
			return m
		}
	}
	best := list[0]
	for _, m := range list {
		if m.Score > best.Score {
			best = m
		}
	}
	return best
}

func imageTagExists(item Item, kind string) bool {
	tags, ok := item["ImageTags"].(map[string]any)
	if !ok {
		return false
	}
	v, ok := tags[kind].(string)
	return ok && strings.TrimSpace(v) != ""
}

func normalizeDate(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) >= 10 && s[4] == '-' && s[7] == '-' {
		return s[:10] + "T00:00:00.0000000Z"
	}
	return ""
}

func atoiSafe(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return n
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------- 演员头像刮削 ----------

// matchGfriendEntry 在候选里找用户选中的那张头像。
//
// 前端传回的 file 是搜索接口给的**完整 URL**（handleGfSearch 把 File 覆写成 URL 后下发），
// 而索引里的 File 是裸文件名（可能带 ?t= 时间戳）—— 直接 EqualFold 永远不相等。
// 这里同时比对：裸文件名、完整 URL、URL 路径解码出来的文件名。
func matchGfriendEntry(entries []GfriendEntry, file, cdn string) (GfriendEntry, bool) {
	want := strings.TrimSpace(file)
	if want == "" {
		return GfriendEntry{}, false
	}
	wantBase := want
	if u, err := url.Parse(want); err == nil && u.Path != "" {
		p := u.Path
		if un, err := url.PathUnescape(p); err == nil {
			p = un
		}
		wantBase = path.Base(p)
	}
	if i := strings.Index(wantBase, "?"); i >= 0 {
		wantBase = wantBase[:i]
	}
	for _, en := range entries {
		if strings.EqualFold(en.File, want) ||
			strings.EqualFold(en.File, wantBase) ||
			strings.EqualFold(en.URL(cdn), want) {
			return en, true
		}
	}
	return GfriendEntry{}, false
}

// pickBestGfriends 并发下载全部候选头像，选**分辨率最高**的一张（对齐 Emby 官方
// gfriends 插件「优先高清」的行为）。面积并列取字节数大的；读不出尺寸的按字节数比。
// 下载 / 解析失败的候选直接跳过；全部失败才报错。手动指定 file 时候选只剩一张，行为不变。
func pickBestGfriends(ctx context.Context, client *http.Client, entries []GfriendEntry, cdn string) ([]byte, string, string, error) {
	type cand struct {
		data []byte
		ct   string
		area int
		en   GfriendEntry
	}
	sem := make(chan struct{}, 6) // 并发上限：一个演员的候选通常就几张
	var wg sync.WaitGroup
	ch := make(chan cand, len(entries))
	for _, en := range entries {
		wg.Add(1)
		go func(en GfriendEntry) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			d, c, err := fetchImageBytes(ctx, client, en.URL(cdn), "")
			if err != nil || len(d) == 0 {
				return
			}
			area := 0
			if cfg, _, derr := image.DecodeConfig(bytes.NewReader(d)); derr == nil {
				area = cfg.Width * cfg.Height
			}
			ch <- cand{data: d, ct: c, area: area, en: en}
		}(en)
	}
	wg.Wait()
	close(ch)

	all := make([]cand, 0, len(entries))
	for c := range ch {
		all = append(all, c)
	}
	if len(all) == 0 {
		return nil, "", "", errors.New("gfriends 候选头像全部下载失败")
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].area != all[j].area {
			return all[i].area > all[j].area
		}
		return len(all[i].data) > len(all[j].data)
	})
	best := all[0]
	return best.data, best.ct, best.en.Group + "/" + best.en.File, nil
}

// AvatarOptions 是头像刮削选项。
type AvatarOptions struct {
	Source    string `json:"source"` // auto / gfriends / metatube
	Group     string `json:"group"`  // 指定 gfriends 分组
	File      string `json:"file"`   // 指定 gfriends 文件
	Overwrite bool   `json:"overwrite"`
}

// AvatarResult 描述一次头像刮削结果。
type AvatarResult struct {
	PersonID string `json:"person_id"`
	Name     string `json:"name"`
	Source   string `json:"source"`
	Detail   string `json:"detail"`
	Skipped  bool   `json:"skipped"`
	Message  string `json:"message"`
}

// ScrapePersonAvatar 为单个演员抓取并上传头像。
func (a *App) ScrapePersonAvatar(ctx context.Context, personID, name string, opts AvatarOptions) (*AvatarResult, error) {
	cfg := a.store.Get()
	e := NewEmby(cfg)
	res := &AvatarResult{PersonID: personID, Name: name}

	if personID == "" {
		p, err := e.PersonByName(ctx, name)
		if err != nil {
			return nil, err
		}
		personID = p.Id
		res.PersonID = p.Id
		res.Name = p.Name
	}
	if !opts.Overwrite {
		if p, err := e.ItemDetail(ctx, personID); err == nil {
			if imageTagExists(p, "Primary") {
				res.Skipped = true
				res.Message = "已有头像，跳过"
				return res, nil
			}
		}
	}

	source := opts.Source
	if source == "" {
		source = "auto"
	}
	var imgData []byte
	var contentType string
	var detail string
	var lastErr error

	tryGfriends := source == "auto" || source == "gfriends"
	tryMetaTube := source == "auto" || source == "metatube"

	if tryGfriends {
		if err := a.gf.EnsureLoaded(ctx, false); err != nil {
			lastErr = err
		} else {
			entries := a.gf.Lookup(res.Name)
			if opts.File != "" {
				// 用户手动选中的那张必须精确命中。前端传回的 file 是搜索接口给的
				// **完整 URL**（handleGfSearch 会把 File 覆写成 URL），而索引里的
				// File 是裸文件名 —— 之前直接 EqualFold 两者永远不等，静默回落成
				// 第一张，就是「点谁都换不上去」的根因。现在匹配不到直接报错。
				picked, ok := matchGfriendEntry(entries, opts.File, cfg.GfriendsCDN)
				if !ok {
					return nil, fmt.Errorf("gfriends 中没有找到所选头像 %q（演员 %q），请重新搜索后再选", opts.File, res.Name)
				}
				entries = []GfriendEntry{picked}
			} else if opts.Group != "" {
				filtered := entries[:0:0]
				for _, en := range entries {
					if en.Group == opts.Group {
						filtered = append(filtered, en)
					}
				}
				if len(filtered) > 0 {
					entries = filtered
				}
			}
			if len(entries) > 0 {
				data, ct, det, err := pickBestGfriends(ctx, e.HTTP, entries, cfg.GfriendsCDN)
				if err != nil {
					lastErr = err
				} else {
					imgData, contentType, detail = data, ct, det
				}
			}
		}
	}

	if imgData == nil && tryMetaTube {
		mt := NewMetaTube(cfg)
		if actors, err := mt.SearchActor(ctx, res.Name, ""); err == nil {
			for _, ac := range actors {
				if normName(ac.Name) != normName(res.Name) {
					continue
				}
				for _, iu := range ac.Images {
					data, ct, err := fetchImageBytes(ctx, e.HTTP, iu, "")
					if err == nil && len(data) > 0 {
						imgData, contentType = data, ct
						detail = "MetaTube/" + ac.Provider + ":" + ac.ID
						break
					}
				}
				if imgData != nil {
					break
				}
			}
		} else {
			lastErr = err
		}
	}

	if imgData == nil {
		msg := "未找到可用头像"
		if lastErr != nil {
			msg += "：" + lastErr.Error()
		}
		return nil, fmt.Errorf("%s（演员：%s）", msg, res.Name)
	}

	if err := e.UploadImage(ctx, res.PersonID, "Primary", -1, imgData, contentType); err != nil {
		return nil, fmt.Errorf("上传头像失败：%w", err)
	}
	res.Source = source
	res.Detail = detail
	res.Message = fmt.Sprintf("已更新头像（%s，%d KB）", detail, len(imgData)/1024)
	return res, nil
}

// ---------- 番号统计与缺失比对 ----------

// LocalItemRef 是本地媒体库中的条目摘要。
type LocalItemRef struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Number     string `json:"number"`
	Path       string `json:"path"`
	HasPrimary bool   `json:"has_primary"`
	Year       int    `json:"year"`
}

// LocalIndex 是本地番号索引。
type LocalIndex struct {
	Items   []LocalItemRef
	ByKey   map[string]LocalItemRef
	Scanned int
}

// BuildLocalIndex 扫描媒体库，建立「番号 -> 条目」索引。
func (a *App) BuildLocalIndex(ctx context.Context, parentID string) (*LocalIndex, error) {
	cfg := a.store.Get()
	e := NewEmby(cfg)
	idx := &LocalIndex{ByKey: map[string]LocalItemRef{}}
	start := 0
	const pageSize = 500
	for {
		q := ItemQuery{
			ParentID:         parentID,
			Recursive:        true,
			IncludeItemTypes: "Movie",
			Fields:           []string{"Path,ProviderIds,ImageTags,OriginalTitle,ProductionYear,SortName"},
			StartIndex:       start,
			Limit:            pageSize,
		}
		res, err := e.Items(ctx, q)
		if err != nil {
			return nil, fmt.Errorf("扫描媒体库失败：%w", err)
		}
		for _, it := range res.Items {
			ref := LocalItemRef{}
			ref.ID, _ = it["Id"].(string)
			ref.Name, _ = it["Name"].(string)
			ref.Path, _ = it["Path"].(string)
			ref.HasPrimary = imageTagExists(it, "Primary")
			if y, ok := it["ProductionYear"].(float64); ok {
				ref.Year = int(y)
			}
			joined := ref.Name + " " + ref.Path
			if o, ok := it["OriginalTitle"].(string); ok {
				joined += " " + o
			}
			if s, ok := it["SortName"].(string); ok {
				joined += " " + s
			}
			keys := numKeys(joined)
			if len(keys) > 0 {
				ref.Number = displayNumber(keys[0])
			}
			idx.Items = append(idx.Items, ref)
			for _, k := range keys {
				if _, exists := idx.ByKey[k]; !exists {
					idx.ByKey[k] = ref
				}
			}
		}
		start += pageSize
		if start >= res.TotalRecordCount || len(res.Items) == 0 {
			break
		}
	}
	idx.Scanned = len(idx.Items)
	return idx, nil
}

// ScanMovie 是番号统计里的一条作品。
type ScanMovie struct {
	Number string `json:"number"`
	Title  string `json:"title"`
	Date   string `json:"date"`
	Cover  string `json:"cover"`
	URL    string `json:"url"`
	Local  bool   `json:"local"`
	ItemID string `json:"item_id"`
}

// ScanResult 是一次番号统计的结果。
type ScanResult struct {
	Star           JBStar      `json:"star"`
	Candidates     []JBStar    `json:"candidates"`
	Total          int         `json:"total"`
	Matched        int         `json:"matched"`
	Movies         []ScanMovie `json:"movies"`
	Missing        []ScanMovie `json:"missing"`
	LocalCount     int         `json:"local_count"`
	LocalUnmatched []string    `json:"local_unmatched"`
	Pages          int         `json:"pages"`
}

// ScanActorNumbers 统计某演员的全部番号，并与本地媒体库比对找出缺失。
func (a *App) ScanActorNumbers(ctx context.Context, starInput, parentID string, maxPages int, job *Job) (*ScanResult, error) {
	cfg := a.store.Get()
	jb := NewJavBus(cfg)

	logf := func(level, msg string) {
		if job != nil {
			job.addLog(level, msg)
		}
	}

	logf("info", "解析演员："+starInput)
	stars, err := jb.ResolveStar(ctx, starInput)
	if err != nil {
		return nil, err
	}
	res := &ScanResult{Candidates: stars, Star: stars[0]}
	logf("info", fmt.Sprintf("命中演员：%s（star id: %s）", res.Star.Name, res.Star.ID))

	logf("info", "抓取 javbus 演员页作品列表…")
	movies, err := jb.StarMovies(ctx, res.Star.ID, maxPages)
	if err != nil {
		return nil, err
	}
	res.Total = len(movies)
	logf("info", fmt.Sprintf("javbus 共 %d 部作品", len(movies)))

	logf("info", "扫描本地媒体库…")
	idx, err := a.BuildLocalIndex(ctx, parentID)
	if err != nil {
		return nil, err
	}
	res.LocalCount = idx.Scanned
	logf("info", fmt.Sprintf("本地媒体库共 %d 个影片条目", idx.Scanned))

	localUsed := map[string]bool{}
	for _, mv := range movies {
		sm := ScanMovie{Number: mv.Number, Title: mv.Title, Date: mv.Date, Cover: mv.Cover, URL: mv.URL}
		if ref, ok := idx.ByKey[canonNumber(mv.Number)]; ok {
			sm.Local = true
			sm.ItemID = ref.ID
			localUsed[ref.ID] = true
		}
		res.Movies = append(res.Movies, sm)
	}
	for _, sm := range res.Movies {
		if sm.Local {
			res.Matched++
		} else {
			res.Missing = append(res.Missing, sm)
		}
	}
	for _, it := range idx.Items {
		if !localUsed[it.ID] && it.Number != "" {
			res.LocalUnmatched = append(res.LocalUnmatched, it.Number)
		}
	}
	sort.Slice(res.Missing, func(i, j int) bool { return res.Missing[i].Date > res.Missing[j].Date })
	sort.Strings(res.LocalUnmatched)
	logf("info", fmt.Sprintf("已收录 %d 部，缺失 %d 部", res.Matched, len(res.Missing)))
	return res, nil
}

// MagnetResult 是某个番号的磁力抓取结果。
type MagnetResult struct {
	Number  string     `json:"number"`
	URL     string     `json:"url"`
	Title   string     `json:"title"`
	Magnets []JBMagnet `json:"magnets"`
	Error   string     `json:"error,omitempty"`
}

// FetchMagnetsFor 并发抓取一批番号的磁力列表。
func (a *App) FetchMagnetsFor(ctx context.Context, targets []ScanMovie, concurrency int, job *Job) []MagnetResult {
	cfg := a.store.Get()
	if concurrency <= 0 {
		concurrency = cfg.Concurrency
	}
	if concurrency <= 0 {
		concurrency = 4
	}
	// javbus 需要限速，这里限制并发为 2 以避免被封
	if concurrency > 2 {
		concurrency = 2
	}
	results := make([]MagnetResult, len(targets))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(i int, t ScanMovie) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			jb := NewJavBus(cfg)
			out := MagnetResult{Number: t.Number, URL: t.URL, Title: t.Title}
			mv, err := jb.MovieDetail(ctx, firstNonEmpty(t.URL, t.Number))
			if err != nil {
				out.Error = err.Error()
			} else {
				out.Magnets = mv.Magnets
				if mv.Title != "" {
					out.Title = mv.Title
				}
				if len(mv.Magnets) == 0 {
					out.Error = "该作品暂无磁力链接"
				}
			}
			results[i] = out
			if job != nil {
				job.mu.Lock()
				job.Done++
				job.mu.Unlock()
				if out.Error != "" {
					job.addLog("warn", fmt.Sprintf("%s：%s", t.Number, out.Error))
				} else {
					job.addLog("ok", fmt.Sprintf("%s：%d 条磁力", t.Number, len(out.Magnets)))
				}
			}
		}(i, t)
	}
	wg.Wait()
	return results
}
