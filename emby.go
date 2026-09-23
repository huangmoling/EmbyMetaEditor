package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

const (
	embyClientName    = "EmbyMetaEditor"
	embyClientVersion = "1.0.0"
)

// Emby 是 Emby Server REST API 的轻量客户端。
type Emby struct {
	BaseURL  string
	Token    string
	UserID   string
	UserName string
	DeviceID string
	HTTP     *http.Client

	// userMu 保护 UserID / UserName —— userID() 会惰性解析并回填
	userMu sync.Mutex
}

// NewEmby 依据配置构造客户端。
func NewEmby(cfg Config) *Emby {
	return &Emby{
		BaseURL:  strings.TrimRight(strings.TrimSpace(cfg.EmbyURL), "/"),
		Token:    strings.TrimSpace(cfg.Token),
		UserID:   strings.TrimSpace(cfg.UserID),
		UserName: cfg.UserName,
		DeviceID: cfg.DeviceID,
		HTTP:     newHTTPClient(cfg),
	}
}

func (e *Emby) authHeader() string {
	return fmt.Sprintf(`MediaBrowser Client="%s", Device="WebUI", DeviceId="%s", Version="%s"`,
		embyClientName, e.DeviceID, embyClientVersion)
}

// do 发起请求；body 为 []byte 时按原始字节发送，否则按 JSON 序列化。
func (e *Emby) do(ctx context.Context, method, path string, q url.Values, body any, hdr map[string]string) ([]byte, int, error) {
	if e.BaseURL == "" {
		return nil, 0, fmt.Errorf("未配置 Emby 地址")
	}
	u := e.BaseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rdr io.Reader
	rawBody, isRaw := body.([]byte)
	if body != nil {
		if isRaw {
			rdr = bytes.NewReader(rawBody)
		} else {
			buf, err := json.Marshal(body)
			if err != nil {
				return nil, 0, err
			}
			rdr = bytes.NewReader(buf)
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("X-Emby-Authorization", e.authHeader())
	req.Header.Set("Accept", "application/json")
	if e.Token != "" {
		req.Header.Set("X-Emby-Token", e.Token)
	}
	if body != nil && !isRaw {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := e.HTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := readAllLimit(resp.Body, 128<<20)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode >= 400 {
		return data, resp.StatusCode, fmt.Errorf("Emby %s %s 返回 %d：%s",
			method, path, resp.StatusCode, strings.TrimSpace(string(truncateBytes(data, 300))))
	}
	return data, resp.StatusCode, nil
}

func (e *Emby) getJSON(ctx context.Context, path string, q url.Values, out any) error {
	data, _, err := e.do(ctx, http.MethodGet, path, q, nil, nil)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

// ---------- 认证 ----------

// LoginResult 是登录成功后需要保存的凭据。
type LoginResult struct {
	Token    string `json:"token"`
	UserID   string `json:"user_id"`
	UserName string `json:"user_name"`
	ServerID string `json:"server_id"`
}

// AuthenticateByName 用用户名 + 密码换取访问令牌。
func (e *Emby) AuthenticateByName(ctx context.Context, username, password string) (*LoginResult, error) {
	body := map[string]string{"Username": username, "Pw": password}
	data, _, err := e.do(ctx, http.MethodPost, "/Users/AuthenticateByName", nil, body, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		AccessToken string `json:"AccessToken"`
		ServerID    string `json:"ServerId"`
		User        struct {
			Id   string `json:"Id"`
			Name string `json:"Name"`
		} `json:"User"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("解析登录响应失败：%w", err)
	}
	if resp.AccessToken == "" {
		return nil, fmt.Errorf("登录失败：服务端未返回 AccessToken")
	}
	e.Token = resp.AccessToken
	e.UserID = resp.User.Id
	e.UserName = resp.User.Name
	return &LoginResult{
		Token:    resp.AccessToken,
		UserID:   resp.User.Id,
		UserName: resp.User.Name,
		ServerID: resp.ServerID,
	}, nil
}

// embyUser 是 Emby 的用户对象（只取我们关心的字段）。
type embyUser struct {
	Id   string `json:"Id"`
	Name string `json:"Name"`
}

// Me 校验当前令牌并取回用户信息（API Key 登录走这里）。
//
// Emby 各版本行为不一致：4.9.x 上如果用的是 API Key（而不是用户会话令牌），
// 调 /Users/Me 会因为「这个令牌没有关联用户」而 500（Unrecognized Guid format）——
// 但同一个 Key 调 /Users/{id}、/Items 之类完全正常。所以不能只看 /Users/Me。
//
// 回退顺序：/Users/Me → /Users/{已知 id} → /Users 列表里挑一个。
func (e *Emby) Me(ctx context.Context) (*LoginResult, error) {
	var me embyUser
	meErr := e.getJSON(ctx, "/Users/Me", nil, &me)
	if meErr == nil && me.Id != "" {
		e.UserID, e.UserName = me.Id, me.Name
		return &LoginResult{Token: e.Token, UserID: me.Id, UserName: me.Name}, nil
	}

	// 回退 1：配置里已经存了 userId，直接查
	if e.UserID != "" {
		var u embyUser
		if err := e.getJSON(ctx, "/Users/"+e.UserID, nil, &u); err == nil && u.Id != "" {
			e.UserName = u.Name
			return &LoginResult{Token: e.Token, UserID: u.Id, UserName: u.Name}, nil
		}
	}

	// 回退 2：列出所有用户，优先按已知用户名匹配，否则取第一个
	var users []embyUser
	if err := e.getJSON(ctx, "/Users", nil, &users); err == nil && len(users) > 0 {
		pick := users[0]
		for _, u := range users {
			if e.UserName != "" && strings.EqualFold(u.Name, e.UserName) {
				pick = u
				break
			}
		}
		e.UserID, e.UserName = pick.Id, pick.Name
		return &LoginResult{Token: e.Token, UserID: pick.Id, UserName: pick.Name}, nil
	}

	if meErr != nil {
		return nil, meErr
	}
	return nil, fmt.Errorf("令牌有效，但这个 Emby 上取不到任何用户（/Users/Me 与 /Users 都没返回用户）")
}

// PublicInfo 探测服务端信息，用于展示版本。
func (e *Emby) PublicInfo(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	if err := e.getJSON(ctx, "/System/Info/Public", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ---------- 媒体库 ----------

// Library 是一个媒体库（视图）。
type Library struct {
	Id             string `json:"Id"`
	Name           string `json:"Name"`
	CollectionType string `json:"CollectionType"`
}

// Views 列出当前用户可见的媒体库。
func (e *Emby) Views(ctx context.Context) ([]Library, error) {
	uid := e.UserID
	if uid == "" {
		me, err := e.Me(ctx)
		if err != nil {
			return nil, err
		}
		uid = me.UserID
	}
	var resp struct {
		Items []Library `json:"Items"`
	}
	if err := e.getJSON(ctx, "/Users/"+uid+"/Views", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

// Counts 是全局媒体数量统计。
type Counts struct {
	MovieCount   int `json:"MovieCount"`
	SeriesCount  int `json:"SeriesCount"`
	EpisodeCount int `json:"EpisodeCount"`
	ArtistCount  int `json:"ArtistCount"`
	AlbumCount   int `json:"AlbumCount"`
	SongCount    int `json:"SongCount"`
	BoxSetCount  int `json:"BoxSetCount"`
	BookCount    int `json:"BookCount"`
	TrailerCount int `json:"TrailerCount"`
}

// ItemCounts 取全局数量统计。
func (e *Emby) ItemCounts(ctx context.Context) (*Counts, error) {
	var c Counts
	if err := e.getJSON(ctx, "/Items/Counts", nil, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// ItemQuery 是 /Items 的查询参数。
type ItemQuery struct {
	ParentID         string
	Recursive        bool
	IncludeItemTypes string
	Fields           []string
	SearchTerm       string
	StartIndex       int
	Limit            int
	SortBy           string
	SortOrder        string
	Filters          string
	IsMissing        *bool
	ImageTypes       string
}

func (q ItemQuery) values() url.Values {
	v := url.Values{}
	if q.ParentID != "" {
		v.Set("ParentId", q.ParentID)
	}
	v.Set("Recursive", fmt.Sprintf("%t", q.Recursive))
	if q.IncludeItemTypes != "" {
		v.Set("IncludeItemTypes", q.IncludeItemTypes)
	}
	if len(q.Fields) > 0 {
		v.Set("Fields", strings.Join(q.Fields, ","))
	}
	if q.SearchTerm != "" {
		v.Set("SearchTerm", q.SearchTerm)
	}
	if q.Limit > 0 {
		v.Set("Limit", itoa(q.Limit))
	}
	if q.StartIndex > 0 {
		v.Set("StartIndex", itoa(q.StartIndex))
	}
	if q.SortBy != "" {
		v.Set("SortBy", q.SortBy)
	}
	if q.SortOrder != "" {
		v.Set("SortOrder", q.SortOrder)
	}
	if q.Filters != "" {
		v.Set("Filters", q.Filters)
	}
	if q.ImageTypes != "" {
		v.Set("ImageTypes", q.ImageTypes)
	}
	if q.IsMissing != nil {
		v.Set("IsMissing", fmt.Sprintf("%t", *q.IsMissing))
	}
	return v
}

// Item 用 map 承载，避免跟随服务端版本频繁变动结构。
type Item = map[string]any

// ItemsResult 是分页结果。
type ItemsResult struct {
	Items            []Item `json:"Items"`
	TotalRecordCount int    `json:"TotalRecordCount"`
}

// Items 分页查询条目。
func (e *Emby) Items(ctx context.Context, q ItemQuery) (*ItemsResult, error) {
	var res ItemsResult
	if err := e.getJSON(ctx, "/Items", q.values(), &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// userID 返回当前令牌对应的用户 ID，必要时解析一次并缓存。
//
// 为什么要缓存：ItemDetail 每次都要用 userId 拼路径，而解析要走网络。
func (e *Emby) userID(ctx context.Context) string {
	e.userMu.Lock()
	if e.UserID != "" {
		uid := e.UserID
		e.userMu.Unlock()
		return uid
	}
	e.userMu.Unlock()

	me, err := e.Me(ctx)
	if err != nil || me == nil {
		return ""
	}
	e.userMu.Lock()
	e.UserID = me.UserID
	e.UserName = me.UserName
	e.userMu.Unlock()
	return me.UserID
}

// ItemDetail 取单个条目完整信息。
//
// 路径有讲究：实测部分 Emby 构建（4.9.0.42 的一个分支）只注册了
// /Users/{userId}/Items/{id}，而 /Items/{id} 会落到静态文件处理器，
// 返回 404「找不到文件 "/Items/xxx"」。标准 Emby 两条路径都在。
// 所以先试用户作用域，失败再退回全局路径。
func (e *Emby) ItemDetail(ctx context.Context, id string) (Item, error) {
	var firstErr error
	if uid := e.userID(ctx); uid != "" {
		var it Item
		if err := e.getJSON(ctx, "/Users/"+uid+"/Items/"+id, nil, &it); err == nil {
			return it, nil
		} else {
			firstErr = err
		}
	}
	var it Item
	if err := e.getJSON(ctx, "/Items/"+id, nil, &it); err != nil {
		if firstErr != nil {
			return nil, firstErr
		}
		return nil, err
	}
	return it, nil
}

// embyUnsafeWriteFields 是不回传到 POST /Items/{id} 的字段。
//
// 原因：这个接口是**整对象替换**（见 UpdateItem 注释），所以 body 必须以当前 DTO 为底。
// 但下面这些要么是服务端派生的只读值，要么体积很大且只在显式请求 Fields 时才返回，
// 回传没有意义、还可能被服务端拒绝或拖慢请求。
var embyUnsafeWriteFields = map[string]bool{
	"Etag":         true,
	"MediaSources": true,
	"MediaStreams": true,
	"Chapters":     true,
}

// UpdateItem 更新条目元数据。
//
// 关键：Emby 的 POST /Items/{id} **不是部分更新，而是整对象替换**——
// body 里没带的字段会被直接清空。实测只发 {"Id":..., "Name":...} 会连带把
// Overview / PremiereDate / ProductionYear / 各类评分 / OriginalTitle 全部抹掉。
// 所以这里必须以**当前完整 DTO** 为底，再把 patch 叠上去。
//
// 另外 ProviderIds 不能缺、也不能是 null，否则服务端直接 400
// （Value cannot be null. (Parameter 'source')）。
func (e *Emby) UpdateItem(ctx context.Context, id string, patch map[string]any) error {
	if len(patch) == 0 {
		return nil
	}
	cur, err := e.ItemDetail(ctx, id)
	if err != nil {
		return err
	}

	body := map[string]any{}
	for k, v := range cur {
		if embyUnsafeWriteFields[k] {
			continue
		}
		body[k] = v
	}
	body["Id"] = id
	for k, v := range patch {
		if v == nil {
			continue
		}
		body[k] = v
	}

	// ProviderIds：合并已有外部 ID，且保证非 nil
	merged := map[string]any{}
	if exist, ok := cur["ProviderIds"].(map[string]any); ok {
		for k, v := range exist {
			merged[k] = v
		}
	}
	if pv, ok := patch["ProviderIds"].(map[string]any); ok {
		for k, v := range pv {
			if s, ok := v.(string); ok && strings.TrimSpace(s) == "" {
				continue
			}
			merged[k] = v
		}
	}
	body["ProviderIds"] = merged

	_, _, err = e.do(ctx, http.MethodPost, "/Items/"+id, nil, body, nil)
	return err
}

// ---------- 图片 ----------

// normalizeImageType 归一化图片 MIME 类型。
//
// Emby 用 Content-Type 决定存盘的扩展名，非 image/* 会直接 400
// 「Unable to determine image file extension from mime type xxx」。
// 下载源偶尔不给 Content-Type（或给 application/octet-stream），
// 这时按文件头猜；实在认不出来就报错，别把非图片数据写进去。
func normalizeImageType(data []byte, ct string) string {
	ct = strings.ToLower(strings.TrimSpace(ct))
	if i := strings.Index(ct, ";"); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	if strings.HasPrefix(ct, "image/") {
		return ct
	}
	switch {
	case bytes.HasPrefix(data, []byte{0xFF, 0xD8, 0xFF}):
		return "image/jpeg"
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(data, []byte("GIF87a")), bytes.HasPrefix(data, []byte("GIF89a")):
		return "image/gif"
	case len(data) >= 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp"
	case len(data) >= 12 && string(data[4:8]) == "ftyp" &&
		(string(data[8:12]) == "avif" || string(data[8:12]) == "avis"):
		return "image/avif"
	}
	if d := http.DetectContentType(data); strings.HasPrefix(d, "image/") {
		return d
	}
	return ""
}

// UploadImage 上传图片到指定条目的指定图片类型。
//
// 两种 body 形态都得支持：
//   - 标准 Emby：原始字节 + Content-Type: image/xxx
//   - 部分构建（实测 4.9.0.42 的一个分支）：body 是 **base64 文本**，
//     发原始字节会 500「The input is not a valid Base-64 string…」
//
// 先按标准形态发，认到那个 base64 报错再换格式重试。
// 顺序不能反：标准服务器上 base64 文本会被当成图片数据存进去（静默损坏）。
func (e *Emby) UploadImage(ctx context.Context, itemID, imgType string, index int, data []byte, contentType string) error {
	if len(data) == 0 {
		return fmt.Errorf("图片数据为空")
	}
	ct := normalizeImageType(data, contentType)
	if ct == "" {
		head := data
		if len(head) > 16 {
			head = head[:16]
		}
		return fmt.Errorf("拿到的数据不是图片（Content-Type=%q，前 16 字节 % x）", contentType, head)
	}

	path := "/Items/" + itemID + "/Images/" + imgType
	if index >= 0 {
		path += "/" + itoa(index)
	}
	hdr := map[string]string{"Content-Type": ct}

	_, status, err := e.do(ctx, http.MethodPost, path, nil, data, hdr)
	if err == nil {
		return nil
	}
	if status == http.StatusInternalServerError &&
		strings.Contains(strings.ToLower(err.Error()), "base-64") {
		encoded := []byte(base64.StdEncoding.EncodeToString(data))
		if _, _, err2 := e.do(ctx, http.MethodPost, path, nil, encoded, hdr); err2 == nil {
			return nil
		} else {
			return err2
		}
	}
	return err
}

// DeleteImage 删除条目的指定图片。
func (e *Emby) DeleteImage(ctx context.Context, itemID, imgType string, index int) error {
	path := "/Items/" + itemID + "/Images/" + imgType
	if index >= 0 {
		path += "/" + itoa(index)
	}
	_, _, err := e.do(ctx, http.MethodDelete, path, nil, nil, nil)
	return err
}

// Refresh 触发条目元数据刷新。
func (e *Emby) Refresh(ctx context.Context, itemID string, recursive bool) error {
	q := url.Values{}
	q.Set("Recursive", fmt.Sprintf("%t", recursive))
	q.Set("MetadataRefreshMode", "Default")
	q.Set("ImageRefreshMode", "Default")
	q.Set("ReplaceAllImages", "false")
	_, _, err := e.do(ctx, http.MethodPost, "/Items/"+itemID+"/Refresh", q, nil, nil)
	return err
}

// Person 是 Emby 中的人物（演员/导演等）。
type Person struct {
	Id           string            `json:"Id"`
	Name         string            `json:"Name"`
	ImageTags    map[string]string `json:"ImageTags"`
	ProviderIds  map[string]string `json:"ProviderIds"`
	PremiereDate string            `json:"PremiereDate"`
}

// PersonsResult 是人物分页结果。
type PersonsResult struct {
	Items            []Person `json:"Items"`
	TotalRecordCount int      `json:"TotalRecordCount"`
}

// Persons 分页查询人物库。
//
// parentID 非空时只返回该媒体库下出现过的演员（实测 Emby 4.9 支持 `ParentId`：
// 全局 10592 人 → 按库过滤后 464 / 2664 / 594 人）。传空串即全局。
func (e *Emby) Persons(ctx context.Context, start, limit int, search, parentID string) (*PersonsResult, error) {
	q := url.Values{}
	q.Set("StartIndex", itoa(start))
	if limit > 0 {
		q.Set("Limit", itoa(limit))
	}
	q.Set("Fields", "ImageTags,ProviderIds")
	if search != "" {
		q.Set("SearchTerm", search)
	}
	if parentID != "" {
		q.Set("ParentId", parentID)
	}
	if e.UserID != "" {
		q.Set("UserId", e.UserID)
	}
	var res PersonsResult
	if err := e.getJSON(ctx, "/Persons", q, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// PersonByName 按名字精确查询人物。
func (e *Emby) PersonByName(ctx context.Context, name string) (*Person, error) {
	res, err := e.Persons(ctx, 0, 50, name, "")
	if err != nil {
		return nil, err
	}
	for i := range res.Items {
		if strings.EqualFold(strings.TrimSpace(res.Items[i].Name), strings.TrimSpace(name)) {
			return &res.Items[i], nil
		}
	}
	if len(res.Items) == 1 {
		return &res.Items[0], nil
	}
	return nil, fmt.Errorf("未找到演员：%s", name)
}

// ---------- 统计 ----------

// LibraryStat 是单个媒体库的统计信息。
type LibraryStat struct {
	Id             string `json:"id"`
	Name           string `json:"name"`
	CollectionType string `json:"collection_type"`
	ItemCount      int    `json:"item_count"`
	MovieCount     int    `json:"movie_count"`
	SeriesCount    int    `json:"series_count"`
	EpisodeCount   int    `json:"episode_count"`
}

// LibraryStats 汇总各媒体库条目数量。
func (e *Emby) LibraryStats(ctx context.Context) ([]LibraryStat, error) {
	libs, err := e.Views(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]LibraryStat, 0, len(libs))
	for _, lb := range libs {
		st := LibraryStat{Id: lb.Id, Name: lb.Name, CollectionType: lb.CollectionType}
		var res ItemsResult
		q := ItemQuery{ParentID: lb.Id, Recursive: true, Limit: 1}
		if err := e.getJSON(ctx, "/Items", q.values(), &res); err == nil {
			st.ItemCount = res.TotalRecordCount
		}
		countByType := func(t string) int {
			qq := ItemQuery{ParentID: lb.Id, Recursive: true, IncludeItemTypes: t, Limit: 1}
			var r ItemsResult
			if err := e.getJSON(ctx, "/Items", qq.values(), &r); err == nil {
				return r.TotalRecordCount
			}
			return 0
		}
		st.MovieCount = countByType("Movie")
		st.SeriesCount = countByType("Series")
		st.EpisodeCount = countByType("Episode")
		out = append(out, st)
	}
	return out, nil
}
