package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// App 汇总服务端依赖。
type App struct {
	store  *Store
	gf     *Gfriends
	jobs   *JobRegistry
	images *ImageProxy
	web    fs.FS

	// 国产传媒客户端按站点地址缓存：同一个批次里必须复用同一个实例，
	// 否则每次请求都新建，限速器（按站点共享）就成了摆设。
	cnMu sync.Mutex
	cn   *CNMedia
}

// NewApp 构造应用。
func NewApp(store *Store, web fs.FS) *App {
	gf := NewGfriends(store.Get)
	gf.LoadFromCache()
	return &App{
		store:  store,
		gf:     gf,
		jobs:   NewJobRegistry(),
		images: NewImageProxy(newHTTPClient(store.Get())),
		web:    web,
	}
}

// cnMedia 返回国产传媒客户端；站点地址改了会重建（同时重置限速器）。
func (a *App) cnMedia() *CNMedia {
	cfg := a.store.Get()
	a.cnMu.Lock()
	defer a.cnMu.Unlock()
	if a.cn != nil && a.cn.sig == cnSitesSig(cfg.CNSites) {
		return a.cn
	}
	a.cn = NewCNMedia(cfg)
	return a.cn
}

// ---------- HTTP 辅助 ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]any{"ok": false, "error": err.Error()})
}

func writeOK(w http.ResponseWriter, v any) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": v})
}

func decodeBody(r *http.Request, out any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	return dec.Decode(out)
}

// route 注册全部 API 与静态资源路由。
func (a *App) route() *http.ServeMux {
	mux := http.NewServeMux()

	// ---- 配置 ----
	mux.HandleFunc("GET /api/config", a.handleGetConfig)
	mux.HandleFunc("POST /api/config", a.handleSaveConfig)

	// ---- Emby 登录与状态 ----
	mux.HandleFunc("POST /api/emby/login", a.handleLogin)
	mux.HandleFunc("POST /api/emby/logout", a.handleLogout)
	mux.HandleFunc("GET /api/emby/status", a.handleEmbyStatus)
	mux.HandleFunc("GET /api/libraries", a.handleLibraries)
	mux.HandleFunc("GET /api/stats", a.handleStats)

	// ---- 媒体库 ----
	mux.HandleFunc("GET /api/items", a.handleItems)
	mux.HandleFunc("GET /api/items/detail", a.handleItemDetail)
	mux.HandleFunc("POST /api/items/update", a.handleItemUpdate)
	mux.HandleFunc("POST /api/items/scrape", a.handleScrapeItem)
	mux.HandleFunc("POST /api/items/scrape-batch", a.handleScrapeBatch)

	// ---- 演员 ----
	mux.HandleFunc("GET /api/persons", a.handlePersons)
	mux.HandleFunc("POST /api/persons/avatar", a.handlePersonAvatar)
	mux.HandleFunc("POST /api/persons/avatars", a.handlePersonAvatarBatch)

	// ---- MetaTube ----
	mux.HandleFunc("GET /api/metatube/providers", a.handleMTProviders)
	mux.HandleFunc("GET /api/metatube/search", a.handleMTSearch)
	mux.HandleFunc("GET /api/metatube/actors", a.handleMTActorSearch)

	// ---- gfriends ----
	mux.HandleFunc("GET /api/gfriends/status", a.handleGfStatus)
	mux.HandleFunc("POST /api/gfriends/reload", a.handleGfReload)
	mux.HandleFunc("GET /api/gfriends/search", a.handleGfSearch)

	// ---- javbus ----
	mux.HandleFunc("POST /api/javbus/scan", a.handleJavbusScan)
	mux.HandleFunc("POST /api/javbus/magnets", a.handleJavbusMagnets)
	mux.HandleFunc("GET /api/javbus/probe", a.handleJavbusProbe)

	// ---- 国产传媒专项刮削 ----
	mux.HandleFunc("GET /api/cn/sites", a.handleCNSites)
	mux.HandleFunc("GET /api/cn/search", a.handleCNSearch)
	mux.HandleFunc("POST /api/cn/scrape", a.handleCNScrape)
	mux.HandleFunc("POST /api/cn/scrape-batch", a.handleCNScrapeBatch)

	// ---- 图片代理（javbus 等有 Referer 防盗链）----
	mux.HandleFunc("GET /api/img", a.handleImageProxy)

	// ---- 任务 ----
	mux.HandleFunc("GET /api/jobs", a.handleJobs)
	mux.HandleFunc("GET /api/jobs/{id}", a.handleJobGet)
	mux.HandleFunc("POST /api/jobs/{id}/cancel", a.handleJobCancel)

	// ---- 静态资源 ----
	mux.Handle("/", http.FileServer(http.FS(a.web)))
	return mux
}

// ---------- 配置 ----------

func (a *App) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	cfg := a.store.Get()
	writeOK(w, publicConfig{
		Config:      cfg,
		ConfigPath:  a.store.Path(),
		LoggedIn:    cfg.Token != "",
		DataDirPath: dataDir(),
	})
}

func (a *App) handleSaveConfig(w http.ResponseWriter, r *http.Request) {
	var in Config
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	err := a.store.Update(func(c *Config) {
		if in.EmbyURL != "" {
			c.EmbyURL = strings.TrimRight(in.EmbyURL, "/")
		}
		c.Username = in.Username
		if in.Password != "" {
			c.Password = in.Password
		}
		c.APIKey = in.APIKey
		c.MetaTubeURL = in.MetaTubeURL
		c.MetaTubeToken = in.MetaTubeToken
		c.GfriendsTreeURL = in.GfriendsTreeURL
		c.GfriendsCDN = in.GfriendsCDN
		c.JavBusURL = in.JavBusURL
		c.JavBusCookie = in.JavBusCookie
		if in.CNSites != nil {
			if c.CNSites == nil {
				c.CNSites = map[string]string{}
			}
			for k, v := range in.CNSites {
				c.CNSites[k] = v
			}
		}
		c.Proxy = in.Proxy
		c.InsecureTLS = in.InsecureTLS
		c.AutoRefresh = in.AutoRefresh
		c.OverwriteImages = in.OverwriteImages
		if in.Concurrency > 0 {
			c.Concurrency = in.Concurrency
		}
		if in.JavBusInterval > 0 {
			c.JavBusInterval = in.JavBusInterval
		}
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeOK(w, a.store.Get())
}

// ---------- 登录 ----------

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL      string `json:"url"`
		Username string `json:"username"`
		Password string `json:"password"`
		APIKey   string `json:"api_key"`
		Mode     string `json:"mode"` // password | apikey
	}
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	cfg := a.store.Get()
	if in.URL != "" {
		cfg.EmbyURL = strings.TrimRight(in.URL, "/")
	}
	cfg.Username = in.Username
	cfg.Password = in.Password
	cfg.APIKey = in.APIKey

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	e := NewEmby(cfg)
	mode := in.Mode
	if mode == "" {
		if strings.TrimSpace(in.APIKey) != "" {
			mode = "apikey"
		} else {
			mode = "password"
		}
	}

	var lr *LoginResult
	var err error
	if mode == "apikey" {
		if strings.TrimSpace(in.APIKey) == "" {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("请填写 API Key"))
			return
		}
		e.Token = strings.TrimSpace(in.APIKey)
		lr, err = e.Me(ctx)
	} else {
		if strings.TrimSpace(in.Username) == "" {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("请填写用户名"))
			return
		}
		lr, err = e.AuthenticateByName(ctx, in.Username, in.Password)
	}
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err)
		return
	}

	info, _ := e.PublicInfo(ctx)
	_ = a.store.Update(func(c *Config) {
		c.EmbyURL = strings.TrimRight(in.URL, "/")
		c.Username = in.Username
		if in.Password != "" {
			c.Password = in.Password
		}
		c.APIKey = in.APIKey
		c.Token = lr.Token
		c.UserID = lr.UserID
		c.UserName = lr.UserName
	})
	writeOK(w, map[string]any{
		"token":     lr.Token,
		"user_id":   lr.UserID,
		"user_name": lr.UserName,
		"server":    info,
		"mode":      mode,
	})
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	_ = a.store.Update(func(c *Config) {
		c.Token = ""
		c.UserID = ""
	})
	writeOK(w, true)
}

func (a *App) handleEmbyStatus(w http.ResponseWriter, r *http.Request) {
	cfg := a.store.Get()
	e := NewEmby(cfg)
	out := map[string]any{"url": cfg.EmbyURL, "logged_in": cfg.Token != ""}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if info, err := e.PublicInfo(ctx); err == nil {
		out["server"] = info
		out["online"] = true
	} else {
		out["online"] = false
		out["error"] = err.Error()
	}
	if cfg.Token != "" {
		// 注意：/Users/Me 在部分 Emby 版本上用 API Key 会 500，
		// Emby.Me 内部有多级回退，这里只认最终结果。
		if me, err := e.Me(ctx); err == nil {
			out["token_valid"] = true
			out["user"] = me.UserName
			out["user_id"] = me.UserID
			if cfg.UserID != me.UserID || cfg.UserName != me.UserName {
				// 回退路径解析出来的身份，顺手补回配置，下次就不用再回退了
				_ = a.store.Update(func(c *Config) {
					c.UserID = me.UserID
					c.UserName = me.UserName
				})
			}
		} else {
			out["token_valid"] = false
			out["error"] = err.Error()
		}
	}
	writeOK(w, out)
}

func (a *App) handleLibraries(w http.ResponseWriter, r *http.Request) {
	cfg := a.store.Get()
	e := NewEmby(cfg)
	libs, err := e.Views(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeOK(w, libs)
}

// ---------- 统计 ----------

func (a *App) handleStats(w http.ResponseWriter, r *http.Request) {
	cfg := a.store.Get()
	e := NewEmby(cfg)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	out := map[string]any{}
	counts, err := e.ItemCounts(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	out["counts"] = counts

	if libs, err := e.LibraryStats(ctx); err == nil {
		out["libraries"] = libs
	}

	if r.URL.Query().Get("persons") != "false" {
		ps := a.personStats(ctx, e, 20000)
		out["persons"] = ps
	}
	out["gfriends"] = a.gf.Status()
	writeOK(w, out)
}

// personStats 统计演员总数与缺头像数量。
func (a *App) personStats(ctx context.Context, e *Emby, maxScan int) map[string]any {
	res := map[string]any{"total": 0, "with_image": 0, "missing_image": 0, "scanned": 0}
	start := 0
	const pageSize = 500
	total, withImg, missing, scanned := 0, 0, 0, 0
	for {
		pr, err := e.Persons(ctx, start, pageSize, "", "")
		if err != nil {
			res["error"] = err.Error()
			break
		}
		if start == 0 {
			total = pr.TotalRecordCount
		}
		for _, p := range pr.Items {
			scanned++
			if _, ok := p.ImageTags["Primary"]; ok {
				withImg++
			} else {
				missing++
			}
		}
		start += pageSize
		if len(pr.Items) == 0 || start >= pr.TotalRecordCount || scanned >= maxScan {
			break
		}
	}
	res["total"] = total
	res["with_image"] = withImg
	res["missing_image"] = missing
	res["scanned"] = scanned
	return res
}

// ---------- 媒体库条目 ----------

func (a *App) handleItems(w http.ResponseWriter, r *http.Request) {
	cfg := a.store.Get()
	e := NewEmby(cfg)
	q := r.URL.Query()
	start := atoiSafe(q.Get("start"))
	limit := atoiSafe(q.Get("limit"))
	if limit <= 0 {
		limit = 60
	}
	itemType := q.Get("type")
	if itemType == "" {
		itemType = "Movie"
	}
	query := ItemQuery{
		ParentID:         q.Get("parent"),
		Recursive:        true,
		IncludeItemTypes: itemType,
		Fields:           []string{"Path,ProviderIds,ImageTags,OriginalTitle,ProductionYear,CommunityRating,RunTimeTicks,Overview,Genres,DateCreated,People"},
		SearchTerm:       q.Get("q"),
		StartIndex:       start,
		Limit:            limit,
		SortBy:           firstNonEmpty(q.Get("sort"), "SortName"),
		SortOrder:        firstNonEmpty(q.Get("order"), "Ascending"),
	}
	if q.Get("missing_image") == "true" {
		query.ImageTypes = ""
	}
	res, err := e.Items(r.Context(), query)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	items := res.Items
	if q.Get("missing_image") == "true" {
		items = filterMissingImage(items)
	}
	// 顺带把番号算好给前端当卡片角标用：Emby 的列表接口不返回 SortName，
	// 前端拿不到番号就只能显示年份。
	for _, it := range items {
		it["Number"] = itemNumber(it)
	}
	writeOK(w, map[string]any{
		"items": items,
		"total": res.TotalRecordCount,
		"start": start,
		"limit": limit,
	})
}

func filterMissingImage(items []Item) []Item {
	out := items[:0:0]
	for _, it := range items {
		if !imageTagExists(it, "Primary") {
			out = append(out, it)
		}
	}
	return out
}

func (a *App) handleItemDetail(w http.ResponseWriter, r *http.Request) {
	cfg := a.store.Get()
	e := NewEmby(cfg)
	id := r.URL.Query().Get("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("缺少 id"))
		return
	}
	it, err := e.ItemDetail(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	// 同 /api/items：把番号算好。Emby 返回的 SortName 是它按片名转的拼音，
	// 拿来当番号显示/预填搜索框都是错的。
	it["Number"] = itemNumber(it)
	writeOK(w, it)
}

// handleItemUpdate 手动编辑条目元数据。
//
// 这里最容易踩的坑是**「没填」和「清空」分不清**：
// Emby 的 POST /Items/{id} 是整对象替换（见 Emby.UpdateItem），
// 如果前端把空表单原样提交，用户没碰过的字段就会被一起抹掉。
// 所以入参一律用指针：nil = 这次没提交这一项，不动它。
//
// 字段取舍：
//   - 文本类（名称/原始标题/简介/分级）允许清空 —— 空串是合法值；
//   - 发行日期只接受 `YYYY-MM-DD`，**留空表示不修改**（不给 Emby 发空日期，
//     免得它 400 或者把日期抹掉）；年份同理，0 或留空 = 不修改；
//   - 标签/类型是数组，提交空数组等于清空。
func (a *App) handleItemUpdate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID             string    `json:"id"`
		Name           *string   `json:"name"`
		OriginalTitle  *string   `json:"original_title"`
		Overview       *string   `json:"overview"`
		OfficialRating *string   `json:"official_rating"`
		PremiereDate   *string   `json:"premiere_date"`
		ProductionYear *int      `json:"production_year"`
		Tags           *[]string `json:"tags"`
		Genres         *[]string `json:"genres"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	id := strings.TrimSpace(in.ID)
	if id == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("缺少条目 id"))
		return
	}

	patch := map[string]any{}
	if in.Name != nil {
		n := strings.TrimSpace(*in.Name)
		if n == "" {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("名称不能为空"))
			return
		}
		patch["Name"] = n
	}
	if in.OriginalTitle != nil {
		patch["OriginalTitle"] = strings.TrimSpace(*in.OriginalTitle)
	}
	if in.Overview != nil {
		patch["Overview"] = strings.TrimSpace(*in.Overview)
	}
	if in.OfficialRating != nil {
		patch["OfficialRating"] = strings.TrimSpace(*in.OfficialRating)
	}
	if in.PremiereDate != nil {
		raw := strings.TrimSpace(*in.PremiereDate)
		if raw != "" {
			if !isDateOnly(raw) {
				writeErr(w, http.StatusBadRequest, fmt.Errorf("发行日期要写成 YYYY-MM-DD"))
				return
			}
			d := normalizeDate(raw)
			patch["PremiereDate"] = d
			// 日期和年份是配套的：只改日期不改年份，列表里会出现年份对不上的条目。
			if in.ProductionYear == nil {
				patch["ProductionYear"] = atoiSafe(d[:4])
			}
		}
	}
	if in.ProductionYear != nil && *in.ProductionYear > 0 {
		patch["ProductionYear"] = *in.ProductionYear
	}
	if in.Tags != nil {
		patch["Tags"] = dedupeStrings(trimAll(*in.Tags))
	}
	if in.Genres != nil {
		patch["Genres"] = dedupeStrings(trimAll(*in.Genres))
	}
	if len(patch) == 0 {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("没有要修改的字段"))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if err := NewEmby(a.store.Get()).UpdateItem(ctx, id, patch); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	// 回读一次，让前端拿到服务端归一化之后的真实值（顺带带回算好的番号）。
	it, err := NewEmby(a.store.Get()).ItemDetail(ctx, id)
	if err != nil {
		writeOK(w, map[string]any{"id": id, "updated": sortedKeys(patch)})
		return
	}
	it["Number"] = itemNumber(it)
	writeOK(w, map[string]any{"id": id, "updated": sortedKeys(patch), "item": it})
}

// isDateOnly 判断是不是严格的 YYYY-MM-DD（normalizeDate 只检查了短横线的位置，
// 不检查数字，所以这里补一道）。
func isDateOnly(s string) bool {
	if len(s) != 10 || s[4] != '-' || s[7] != '-' {
		return false
	}
	for i, r := range s {
		if i == 4 || i == 7 {
			continue
		}
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// trimAll 去掉每项首尾空白并丢掉空项。
func trimAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (a *App) handleScrapeItem(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID              string `json:"id"`
		Provider        string `json:"provider"`
		MovieID         string `json:"movie_id"`
		OverwriteImages bool   `json:"overwrite_images"`
		Refresh         bool   `json:"refresh"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if in.ID == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("缺少条目 id"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	res, err := a.ScrapeMovie(ctx, in.ID, ScrapeOptions{
		Provider:        in.Provider,
		MovieID:         in.MovieID,
		OverwriteImages: in.OverwriteImages,
		Refresh:         in.Refresh,
	})
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeOK(w, res)
}

// handleScrapeBatch 批量刮削影片，异步执行并返回任务 id。
func (a *App) handleScrapeBatch(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs             []string `json:"ids"`
		ParentID        string   `json:"parent_id"`
		OnlyNoPoster    bool     `json:"only_no_poster"`
		Limit           int      `json:"limit"`
		Provider        string   `json:"provider"`
		OverwriteImages bool     `json:"overwrite_images"`
		Refresh         bool     `json:"refresh"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	cfg := a.store.Get()
	e := NewEmby(cfg)
	ctx := r.Context()

	targets := in.IDs
	if len(targets) == 0 {
		limit := in.Limit
		if limit <= 0 {
			limit = 100
		}
		q := ItemQuery{
			ParentID:         in.ParentID,
			Recursive:        true,
			IncludeItemTypes: "Movie",
			Fields:           []string{"Path,ImageTags,OriginalTitle,Name"},
			Limit:            limit,
			SortBy:           "SortName",
			SortOrder:        "Ascending",
		}
		res, err := e.Items(ctx, q)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		for _, it := range res.Items {
			if in.OnlyNoPoster && imageTagExists(it, "Primary") {
				continue
			}
			if id, ok := it["Id"].(string); ok {
				targets = append(targets, id)
			}
		}
	}
	if len(targets) == 0 {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("没有需要刮削的条目"))
		return
	}

	job, jobCtx := a.jobs.New("movie-scrape", fmt.Sprintf("批量刮削影片（%d 个）", len(targets)), len(targets))
	job.addLog("info", fmt.Sprintf("开始刮削 %d 个条目", len(targets)))
	go func() {
		sem := make(chan struct{}, maxInt(1, cfg.Concurrency))
		var wg sync.WaitGroup
		for _, id := range targets {
			if jobCtx.Err() != nil {
				break
			}
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				octx, cancel := context.WithTimeout(jobCtx, 5*time.Minute)
				defer cancel()
				res, err := a.ScrapeMovie(octx, id, ScrapeOptions{
					Provider:        in.Provider,
					OverwriteImages: in.OverwriteImages,
					Refresh:         in.Refresh,
				})
				job.mu.Lock()
				if err != nil {
					job.Failed++
					job.mu.Unlock()
					job.addLog("error", fmt.Sprintf("%s：%v", id, err))
					return
				}
				if res.Skipped {
					job.Skipped++
				} else {
					job.Done++
				}
				job.mu.Unlock()
				job.addLog("ok", fmt.Sprintf("%s → %s（%s）", res.ItemName, res.Title, res.Number))
			}(id)
		}
		wg.Wait()
		if jobCtx.Err() != nil {
			job.setStatus("canceled")
		} else {
			job.setStatus("done")
		}
	}()
	writeOK(w, map[string]any{"job_id": job.ID})
}

// ---------- 演员 ----------

func (a *App) handlePersons(w http.ResponseWriter, r *http.Request) {
	cfg := a.store.Get()
	e := NewEmby(cfg)
	q := r.URL.Query()
	start := atoiSafe(q.Get("start"))
	limit := atoiSafe(q.Get("limit"))
	if limit <= 0 {
		limit = 60
	}
	pr, err := e.Persons(r.Context(), start, limit, q.Get("q"), q.Get("parent_id"))
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	type personView struct {
		Person
		HasImage bool `json:"has_image"`
		GFriends int  `json:"gfriends"`
	}
	out := make([]personView, 0, len(pr.Items))
	for _, p := range pr.Items {
		_, has := p.ImageTags["Primary"]
		pv := personView{Person: p, HasImage: has}
		if !has {
			pv.GFriends = len(a.gf.Lookup(p.Name))
		}
		out = append(out, pv)
	}
	if q.Get("missing_image") == "true" {
		filtered := out[:0:0]
		for _, p := range out {
			if !p.HasImage {
				filtered = append(filtered, p)
			}
		}
		out = filtered
	}
	writeOK(w, map[string]any{
		"items": out,
		"total": pr.TotalRecordCount,
		"start": start,
		"limit": limit,
	})
}

func (a *App) handlePersonAvatar(w http.ResponseWriter, r *http.Request) {
	var in struct {
		PersonID  string `json:"person_id"`
		Name      string `json:"name"`
		Source    string `json:"source"`
		Group     string `json:"group"`
		File      string `json:"file"`
		Overwrite bool   `json:"overwrite"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	res, err := a.ScrapePersonAvatar(ctx, in.PersonID, in.Name, AvatarOptions{
		Source: in.Source, Group: in.Group, File: in.File, Overwrite: in.Overwrite,
	})
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeOK(w, res)
}

// handlePersonAvatarBatch 批量刮削演员头像。
func (a *App) handlePersonAvatarBatch(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Mode      string   `json:"mode"` // missing | list
		Names     []string `json:"names"`
		Limit     int      `json:"limit"`
		Source    string   `json:"source"`
		Overwrite bool     `json:"overwrite"`
		ParentID  string   `json:"parent_id"` // 限定媒体库，空串为全部
	}
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	cfg := a.store.Get()
	e := NewEmby(cfg)
	ctx := r.Context()

	type target struct{ ID, Name string }
	var targets []target
	if in.Mode == "list" && len(in.Names) > 0 {
		for _, n := range in.Names {
			n = strings.TrimSpace(n)
			if n == "" {
				continue
			}
			targets = append(targets, target{Name: n})
		}
	} else {
		limit := in.Limit
		if limit <= 0 {
			limit = 200
		}
		start := 0
		for len(targets) < limit {
			pr, err := e.Persons(ctx, start, 500, "", in.ParentID)
			if err != nil {
				writeErr(w, http.StatusBadGateway, err)
				return
			}
			for _, p := range pr.Items {
				_, has := p.ImageTags["Primary"]
				if has && !in.Overwrite {
					continue
				}
				targets = append(targets, target{ID: p.Id, Name: p.Name})
				if len(targets) >= limit {
					break
				}
			}
			start += 500
			if start >= pr.TotalRecordCount || len(pr.Items) == 0 {
				break
			}
		}
	}
	if len(targets) == 0 {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("没有需要处理的演员"))
		return
	}

	job, jobCtx := a.jobs.New("avatar-scrape", fmt.Sprintf("批量刮削演员头像（%d 位）", len(targets)), len(targets))
	job.addLog("info", fmt.Sprintf("开始处理 %d 位演员", len(targets)))
	go func() {
		// 先确保头像索引可用
		if err := a.gf.EnsureLoaded(jobCtx, false); err != nil {
			job.addLog("warn", "gfriends 索引不可用："+err.Error())
		} else {
			job.addLog("info", fmt.Sprintf("gfriends 索引就绪，共 %d 位演员", a.gf.Count()))
		}
		sem := make(chan struct{}, maxInt(1, cfg.Concurrency))
		var wg sync.WaitGroup
		for _, t := range targets {
			if jobCtx.Err() != nil {
				break
			}
			wg.Add(1)
			go func(t target) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				octx, cancel := context.WithTimeout(jobCtx, 2*time.Minute)
				defer cancel()
				res, err := a.ScrapePersonAvatar(octx, t.ID, t.Name, AvatarOptions{
					Source: in.Source, Overwrite: in.Overwrite,
				})
				job.mu.Lock()
				if err != nil {
					job.Failed++
					job.mu.Unlock()
					job.addLog("error", fmt.Sprintf("%s：%v", t.Name, err))
					return
				}
				if res.Skipped {
					job.Skipped++
				} else {
					job.Done++
				}
				job.mu.Unlock()
				job.addLog("ok", fmt.Sprintf("%s → %s", res.Name, res.Message))
			}(t)
		}
		wg.Wait()
		if jobCtx.Err() != nil {
			job.setStatus("canceled")
		} else {
			job.setStatus("done")
		}
	}()
	writeOK(w, map[string]any{"job_id": job.ID})
}

// ---------- MetaTube ----------

func (a *App) handleMTProviders(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	list, err := NewMetaTube(a.store.Get()).Providers(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeOK(w, list)
}

func (a *App) handleMTSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	keyword := strings.TrimSpace(q.Get("q"))
	if keyword == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("缺少搜索关键词"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	list, err := NewMetaTube(a.store.Get()).Search(ctx, keyword, q.Get("provider"), true)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeOK(w, list)
}

func (a *App) handleMTActorSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	keyword := strings.TrimSpace(q.Get("q"))
	if keyword == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("缺少搜索关键词"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	list, err := NewMetaTube(a.store.Get()).SearchActor(ctx, keyword, q.Get("provider"))
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeOK(w, list)
}

// ---------- gfriends ----------

func (a *App) handleGfStatus(w http.ResponseWriter, r *http.Request) {
	writeOK(w, a.gf.Status())
}

func (a *App) handleGfReload(w http.ResponseWriter, r *http.Request) {
	job, jobCtx := a.jobs.New("gfriends-index", "重新下载 gfriends 头像索引", 1)
	go func() {
		job.addLog("info", "开始下载 Filetree.json…")
		if err := a.gf.Download(jobCtx); err != nil {
			job.Failed++
			job.setStatus("failed")
			job.Err = err.Error()
			job.addLog("error", err.Error())
			return
		}
		job.Done++
		job.setStatus("done")
		job.addLog("ok", fmt.Sprintf("索引就绪，共 %d 位演员", a.gf.Count()))
	}()
	writeOK(w, map[string]any{"job_id": job.ID})
}

func (a *App) handleGfSearch(w http.ResponseWriter, r *http.Request) {
	keyword := strings.TrimSpace(r.URL.Query().Get("q"))
	if keyword == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("缺少关键词"))
		return
	}
	if err := a.gf.EnsureLoaded(r.Context(), false); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	found := a.gf.Fuzzy(keyword, 30)
	type hit struct {
		Name    string         `json:"name"`
		Entries []GfriendEntry `json:"entries"`
	}
	out := make([]hit, 0, len(found))
	cfg := a.store.Get()
	for name, entries := range found {
		view := make([]GfriendEntry, 0, len(entries))
		for _, en := range entries {
			en.File = en.URL(cfg.GfriendsCDN)
			view = append(view, en)
		}
		out = append(out, hit{Name: name, Entries: view})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeOK(w, out)
}

// ---------- javbus ----------

func (a *App) handleJavbusScan(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Star     string `json:"star"`
		ParentID string `json:"parent_id"`
		MaxPages int    `json:"max_pages"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(in.Star) == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("请填写演员名或演员页地址"))
		return
	}
	job, jobCtx := a.jobs.New("javbus-scan", "统计番号并比对缺失："+in.Star, 0)
	go func() {
		ctx, cancel := context.WithTimeout(jobCtx, 30*time.Minute)
		defer cancel()
		res, err := a.ScanActorNumbers(ctx, in.Star, in.ParentID, in.MaxPages, job)
		if err != nil {
			job.Failed++
			job.setStatus("failed")
			job.Err = err.Error()
			job.addLog("error", err.Error())
			return
		}
		job.mu.Lock()
		job.Result = res
		job.Total = res.Total
		job.Done = res.Total
		job.mu.Unlock()
		job.setStatus("done")
	}()
	writeOK(w, map[string]any{"job_id": job.ID})
}

func (a *App) handleJavbusMagnets(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Targets []ScanMovie `json:"targets"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if len(in.Targets) == 0 {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("没有需要抓取的番号"))
		return
	}
	job, jobCtx := a.jobs.New("javbus-magnets", fmt.Sprintf("抓取磁力列表（%d 个番号）", len(in.Targets)), len(in.Targets))
	go func() {
		ctx, cancel := context.WithTimeout(jobCtx, 30*time.Minute)
		defer cancel()
		results := a.FetchMagnetsFor(ctx, in.Targets, 2, job)
		job.mu.Lock()
		job.Result = results
		job.mu.Unlock()
		if jobCtx.Err() != nil {
			job.setStatus("canceled")
		} else {
			job.setStatus("done")
		}
	}()
	writeOK(w, map[string]any{"job_id": job.ID})
}

// handleJavbusProbe 是「连通性诊断」接口：抓不到番号时先跑它，
// 能直接区分网络不通 / 被验证码拦 / 页面改版三种情况。
func (a *App) handleJavbusProbe(w http.ResponseWriter, r *http.Request) {
	keyword := strings.TrimSpace(r.URL.Query().Get("q"))
	cfg := a.store.Get()
	jb := NewJavBus(cfg)
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	writeOK(w, jb.Probe(ctx, keyword))
}

// ---------- 国产传媒专项刮削 ----------

// handleCNSites 返回站点列表（含当前配置的地址），前端用来展示与排障。
func (a *App) handleCNSites(w http.ResponseWriter, r *http.Request) {
	cn := a.cnMedia()
	out := make([]map[string]any, 0, len(cnSiteOrder))
	for _, m := range cnSiteOrder {
		out = append(out, map[string]any{"key": m.Key, "name": m.Name, "url": cn.sites[m.Key]})
	}
	writeOK(w, out)
}

// handleCNSearch 是只读预览：给一个番号，返回四个站点的原始搜索结果。
// 不碰 Emby，也不写任何数据。
func (a *App) handleCNSearch(w http.ResponseWriter, r *http.Request) {
	number := strings.TrimSpace(r.URL.Query().Get("q"))
	if number == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("缺少番号参数 q"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	hits := a.cnMedia().SearchAll(ctx, number)
	pick, matched := pickCN(hits, number)
	writeOK(w, map[string]any{
		"number":  number,
		"sites":   hits,
		"matched": matched,
		"picked":  pick,
	})
}

// handleCNScrape 对单个条目刮削一次并写入。
//
// 字段固定全开（封面/标题/标签/日期），界面上不再给勾选；
// `dry_run` 只为只读的冒烟脚本保留，界面不传，默认即写入。
func (a *App) handleCNScrape(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID              string   `json:"id"`
		Number          string   `json:"number"`
		Fields          []string `json:"fields"`
		OverwriteImages bool     `json:"overwrite_images"`
		OverwriteTitle  bool     `json:"overwrite_title"`
		DryRun          bool     `json:"dry_run"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(in.ID) == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("缺少条目 id"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	res, err := a.ScrapeCN(ctx, in.ID, in.Number, CNOptions{
		Fields:          cnFieldsFrom(in.Fields),
		OverwriteImages: in.OverwriteImages,
		OverwriteTitle:  in.OverwriteTitle,
		DryRun:          in.DryRun,
	})
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeOK(w, res)
}

// handleCNScrapeBatch 批量刮削国产传媒条目，异步执行。
//
// 目标有两种给法：
//   - `ids`：界面里勾选的那些条目，按用户选的精确处理（最常用）；
//   - `parent_id`：整个媒体库按条件扫一批（配合 only_missing_cover / limit）。
//
// 范围必须落到某个媒体库或明确的 id 上 —— 国产传媒番号只在这个库里成立，
// 拿全库跑会把「91CM-014」这种番号往日本片库里乱套。
func (a *App) handleCNScrapeBatch(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs              []string `json:"ids"`
		ParentID         string   `json:"parent_id"`
		Limit            int      `json:"limit"`
		OnlyMissingCover bool     `json:"only_missing_cover"`
		Fields           []string `json:"fields"`
		OverwriteImages  bool     `json:"overwrite_images"`
		OverwriteTitle   bool     `json:"overwrite_title"`
		// DryRun 界面已不再暴露（国产传媒页直接写入），保留是为了让
		// 只读的冒烟脚本还能安全地跑一遍完整链路。
		DryRun bool `json:"dry_run"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	cfg := a.store.Get()
	e := NewEmby(cfg)
	ctx := r.Context()

	type cnTarget struct{ ID, Name, Number string }
	var targets []cnTarget
	noNumber := 0
	var skipped []string

	if ids := in.IDs; len(ids) > 0 {
		// 勾选路径：逐个取详情。勾选数量由界面控制，这里再兜一道上限，
		// 免得手搓请求一次塞几千个 id 把站点打挂。
		for _, id := range ids {
			if strings.TrimSpace(id) == "" {
				continue
			}
			if len(targets) >= maxCNBatchTargets {
				skipped = append(skipped, fmt.Sprintf("超过 %d 条上限，其余已忽略", maxCNBatchTargets))
				break
			}
			it, err := e.ItemDetail(ctx, id)
			if err != nil {
				skipped = append(skipped, fmt.Sprintf("%s：读取条目失败（%v）", id, err))
				continue
			}
			name, _ := it["Name"].(string)
			num := cnItemNumber(it)
			if num == "" {
				noNumber++
				skipped = append(skipped, fmt.Sprintf("「%s」推断不出番号", name))
				continue
			}
			targets = append(targets, cnTarget{ID: id, Name: name, Number: num})
		}
	} else {
		limit := in.Limit
		if limit <= 0 {
			limit = 30
		}
		if limit > maxCNBatchTargets {
			limit = maxCNBatchTargets
		}
		// 「只看缺封面」要在服务端筛，所以一次多拉一些再截断，
		// 否则前 N 条都有封面时会白跑一趟。
		scan := limit
		if in.OnlyMissingCover {
			scan = maxInt(limit*4, 200)
			if scan > 1000 {
				scan = 1000
			}
		}
		res, err := e.Items(ctx, ItemQuery{
			ParentID:         in.ParentID,
			Recursive:        true,
			IncludeItemTypes: "Movie",
			Fields:           []string{"Path,ImageTags,OriginalTitle,SortName,ProductionYear"},
			Limit:            scan,
			SortBy:           "SortName",
			SortOrder:        "Ascending",
		})
		if err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		for _, it := range res.Items {
			if in.OnlyMissingCover && imageTagExists(it, "Primary") {
				continue
			}
			id, _ := it["Id"].(string)
			if id == "" {
				continue
			}
			name, _ := it["Name"].(string)
			// 用国产传媒的提取规则：itemNumber() 会把 91CM-014 压成 CM-014，
			// 拿去搜索必然一无所获。
			num := cnItemNumber(it)
			if num == "" {
				noNumber++
				continue // 推断不出番号就没法搜，直接跳过
			}
			targets = append(targets, cnTarget{ID: id, Name: name, Number: num})
			if len(targets) >= limit {
				break
			}
		}
	}

	if len(targets) == 0 {
		if len(in.IDs) > 0 {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("选中的条目里没有能刮削的（%d 条推断不出番号）", noNumber))
			return
		}
		writeErr(w, http.StatusBadRequest, fmt.Errorf("没有可刮削的条目（该范围内推断不出番号的条目有 %d 个）", noNumber))
		return
	}

	opts := CNOptions{
		Fields:          cnFieldsFrom(in.Fields),
		OverwriteImages: in.OverwriteImages,
		OverwriteTitle:  in.OverwriteTitle,
		DryRun:          in.DryRun,
	}
	cn := a.cnMedia()

	job, jobCtx := a.jobs.New("cn-scrape", fmt.Sprintf("国产传媒刮削（%d 个条目）", len(targets)), len(targets))
	if in.DryRun {
		job.addLog("info", "试运行：只搜索并展示结果，不会写入 Emby")
	}
	job.addLog("info", fmt.Sprintf("共 %d 个条目待处理，跳过 %d 个推断不出番号的条目", len(targets), noNumber))
	for _, s := range skipped {
		job.addLog("warn", s)
	}

	go func() {
		var rmu sync.Mutex
		results := make([]*CNScrape, 0, len(targets))
		// 站点是小站，并发压低一点，配合每站点的请求间隔
		sem := make(chan struct{}, maxInt(1, minInt(cfg.Concurrency, 4)))
		var wg sync.WaitGroup
		for _, t := range targets {
			if jobCtx.Err() != nil {
				break
			}
			wg.Add(1)
			go func(t cnTarget) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				octx, cancel := context.WithTimeout(jobCtx, 3*time.Minute)
				defer cancel()
				res, err := a.scrapeCNWith(octx, cn, e, t.ID, t.Number, opts)
				job.mu.Lock()
				if err != nil {
					job.Failed++
					job.mu.Unlock()
					job.addLog("error", fmt.Sprintf("%s：%v", t.Name, err))
					return
				}
				if res.Applied {
					job.Done++
				} else {
					job.Skipped++
				}
				job.mu.Unlock()
				job.addLog(cnLogLevel(res), fmt.Sprintf("%s [%s] %s", t.Name, res.Number, res.Message))
				rmu.Lock()
				results = append(results, res)
				rmu.Unlock()
			}(t)
		}
		wg.Wait()
		job.mu.Lock()
		job.Result = results
		job.mu.Unlock()
		if jobCtx.Err() != nil {
			job.setStatus("canceled")
		} else {
			job.setStatus("done")
		}
	}()
	writeOK(w, map[string]any{"job_id": job.ID})
}

// cnLogLevel 让日志一眼能看出「命中了」和「没命中」。
func cnLogLevel(res *CNScrape) string {
	switch {
	case res.Applied:
		return "ok"
	case len(res.Matched) > 0:
		return "info"
	default:
		return "warn"
	}
}

// handleImageProxy 统一处理页面里的外部图片。
//
// javbus 的图片按 Referer 防盗链，浏览器直接引用会 403，
// 所以前端把图片地址都丢给这里：
//   - 白名单主机（javbus / gfriends 等）：服务端带着正确 Referer 代取；
//   - 其他公网图床：302 回原地址让浏览器直连，行为与直连完全一致。
//
// 这样「哪些图需要代取」的策略只存在于服务端，用户把 JavBus 换成镜像域名
// 也不用改前端。
func (a *App) handleImageProxy(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(r.URL.Query().Get("u"))
	if raw == "" {
		http.Error(w, "缺少参数 u", http.StatusBadRequest)
		return
	}
	cfg := a.store.Get()
	u, proxy, err := ClassifyImage(raw, cfg)
	if err != nil {
		// 用 502 而不是 404：这是上游取不到，不是本机没这个资源
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if !proxy {
		http.Redirect(w, r, u.String(), http.StatusFound)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	data, ctype, err := a.images.fetchResolved(ctx, u)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	w.Header().Set("Cache-Control", "public, max-age=21600") // 6h，与内存缓存一致
	_, _ = w.Write(data)
}

// ---------- 任务 ----------

func (a *App) handleJobs(w http.ResponseWriter, r *http.Request) {
	writeOK(w, a.jobs.List(50))
}

func (a *App) handleJobGet(w http.ResponseWriter, r *http.Request) {
	job := a.jobs.Get(r.PathValue("id"))
	if job == nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("任务不存在"))
		return
	}
	writeOK(w, job.Snapshot())
}

func (a *App) handleJobCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !a.jobs.Cancel(id) {
		writeErr(w, http.StatusNotFound, fmt.Errorf("任务不存在"))
		return
	}
	writeOK(w, true)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
