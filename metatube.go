package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// MetaTube 是 MetaTube Server (v1 API) 的客户端。
// 地址完全可配置，支持自建实例；如实例开启了鉴权则填 Token。
type MetaTube struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// NewMetaTube 依据配置构造客户端。
func NewMetaTube(cfg Config) *MetaTube {
	return &MetaTube{
		BaseURL: strings.TrimRight(strings.TrimSpace(cfg.MetaTubeURL), "/"),
		Token:   strings.TrimSpace(cfg.MetaTubeToken),
		HTTP:    newHTTPClient(cfg),
	}
}

// MTMovie 对应 MetaTube 的影片信息（搜索与详情共用，字段缺失时为零值）。
//
// 注意：字段名有两套。v1 实际返回的是 maker / summary / actors 为字符串数组；
// studio / plot 是早期文档里的写法。两套都留着，取值时用 firstNonEmpty 兜底，
// 否则换了服务端版本就会静默刮成空值。
type MTMovie struct {
	ID            string   `json:"id"`
	Number        string   `json:"number"`
	Title         string   `json:"title"`
	TitleZh       string   `json:"title_zh"`
	TitleJa       string   `json:"title_ja"`
	Actors        []string `json:"actors"`
	Genres        []string `json:"genres"`
	CoverURL      string   `json:"cover_url"`
	BigCoverURL   string   `json:"big_cover_url"`
	PosterURL     string   `json:"poster_url"`
	ThumbURL      string   `json:"thumb_url"`
	PreviewImages []string `json:"preview_images"`
	PreviewVideo  string   `json:"preview_video_url"`
	TrailerURL    string   `json:"trailer_url"`
	ReleaseDate   string   `json:"release_date"`
	Runtime       int      `json:"runtime"`
	Score         float64  `json:"score"`
	Director      string   `json:"director"`
	Series        string   `json:"series"`
	Label         string   `json:"label"`
	Provider      string   `json:"provider"`
	Homepage      string   `json:"homepage"`
	// 以下两对是同一含义的不同写法，取 firstNonEmpty
	Studio  string `json:"studio"`
	Maker   string `json:"maker"`
	Plot    string `json:"plot"`
	Summary string `json:"summary"`
}

// plot 返回简介，兼容 plot / summary 两种字段名。
func (m *MTMovie) plot() string { return firstNonEmpty(m.Plot, m.Summary) }

// studio 返回制作商，兼容 studio / maker 两种字段名。
func (m *MTMovie) studio() string { return firstNonEmpty(m.Studio, m.Maker) }

// MTActor 对应 MetaTube 的人物信息。
type MTActor struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Aliases  []string `json:"aliases"`
	Images   []string `json:"images"`
	Provider string   `json:"provider"`
}

// mtEnvelope 兼容 {"data": ...} 包裹形式。
type mtEnvelope struct {
	Data   json.RawMessage `json:"data"`
	Error  string          `json:"error"`
	Detail string          `json:"detail"`
}

func (m *MetaTube) do(ctx context.Context, path string, q url.Values) ([]byte, error) {
	if m.BaseURL == "" {
		return nil, fmt.Errorf("未配置 MetaTube 地址")
	}
	u := m.BaseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", embyClientName+"/"+embyClientVersion)
	if m.Token != "" {
		req.Header.Set("Authorization", "Bearer "+m.Token)
	}
	resp, err := m.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连接 MetaTube 失败：%w", err)
	}
	defer resp.Body.Close()
	data, err := readAllLimit(resp.Body, 32<<20)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("MetaTube %s 返回 %d：%s", path, resp.StatusCode, strings.TrimSpace(string(truncateBytes(data, 300))))
	}
	return data, nil
}

// unwrap 去掉可能的 data 包裹并返回内部 JSON。
func unwrap(data []byte) json.RawMessage {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil
	}
	if trimmed[0] == '{' {
		var env mtEnvelope
		if err := json.Unmarshal(trimmed, &env); err == nil && len(env.Data) > 0 {
			if env.Error != "" {
				return nil
			}
			return env.Data
		}
	}
	return trimmed
}

// Providers 列出实例支持的元数据提供方。
//
// v1 的实际返回是 {"data":{"movie_providers":{名称:主页},"actor_providers":{...}}}，
// 早期文档里的扁平数组形式也一并兼容。返回影片提供方名称（前端就是拿来展示的）。
func (m *MetaTube) Providers(ctx context.Context) ([]string, error) {
	data, err := m.do(ctx, "/v1/providers", nil)
	if err != nil {
		return nil, err
	}
	inner := unwrap(data)

	// 形式一：["FANZA","MGStage",...]
	var list []string
	if err := json.Unmarshal(inner, &list); err == nil {
		return list, nil
	}

	// 形式二：{"movie_providers":{...},"actor_providers":{...}}
	var obj struct {
		MovieProviders map[string]string `json:"movie_providers"`
		ActorProviders map[string]string `json:"actor_providers"`
	}
	if err := json.Unmarshal(inner, &obj); err == nil && len(obj.MovieProviders) > 0 {
		names := make([]string, 0, len(obj.MovieProviders))
		for k := range obj.MovieProviders {
			names = append(names, k)
		}
		sort.Strings(names)
		return names, nil
	}

	// 形式三：{"providers":{...}} / {"providers":[...]}
	var alt struct {
		Providers json.RawMessage `json:"providers"`
	}
	if err := json.Unmarshal(inner, &alt); err == nil && len(alt.Providers) > 0 {
		if err := json.Unmarshal(alt.Providers, &list); err == nil {
			return list, nil
		}
		var m2 map[string]string
		if err := json.Unmarshal(alt.Providers, &m2); err == nil {
			names := make([]string, 0, len(m2))
			for k := range m2 {
				names = append(names, k)
			}
			sort.Strings(names)
			return names, nil
		}
	}

	return nil, fmt.Errorf("解析 providers 失败：无法识别的响应结构：%s", string(truncateBytes(inner, 200)))
}

// Search 按关键词（通常填番号）搜索影片。
func (m *MetaTube) Search(ctx context.Context, keyword, provider string, fallback bool) ([]MTMovie, error) {
	q := url.Values{}
	q.Set("q", keyword)
	if provider != "" {
		q.Set("provider", provider)
	}
	if fallback {
		q.Set("fallback", "true")
	}
	data, err := m.do(ctx, "/v1/movies/search", q)
	if err != nil {
		return nil, err
	}
	var list []MTMovie
	if err := json.Unmarshal(unwrap(data), &list); err != nil {
		return nil, fmt.Errorf("解析搜索结果失败：%w", err)
	}
	return list, nil
}

// Movie 按 provider + id 取影片详情。
func (m *MetaTube) Movie(ctx context.Context, provider, id string) (*MTMovie, error) {
	if provider == "" || id == "" {
		return nil, fmt.Errorf("provider 与 id 不能为空")
	}
	data, err := m.do(ctx, "/v1/movies/"+url.PathEscape(provider)+"/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, err
	}
	var mv MTMovie
	if err := json.Unmarshal(unwrap(data), &mv); err != nil {
		return nil, fmt.Errorf("解析影片详情失败：%w", err)
	}
	if mv.Provider == "" {
		mv.Provider = provider
	}
	if mv.ID == "" {
		mv.ID = id
	}
	return &mv, nil
}

// SearchActor 搜索人物，用于 gfriends 未命中时的兜底头像来源。
func (m *MetaTube) SearchActor(ctx context.Context, keyword, provider string) ([]MTActor, error) {
	q := url.Values{}
	q.Set("q", keyword)
	if provider != "" {
		q.Set("provider", provider)
	}
	q.Set("fallback", "true")
	data, err := m.do(ctx, "/v1/actors/search", q)
	if err != nil {
		return nil, err
	}
	var list []MTActor
	if err := json.Unmarshal(unwrap(data), &list); err != nil {
		return nil, fmt.Errorf("解析人物搜索失败：%w", err)
	}
	return list, nil
}

// ImageURL 返回经由 MetaTube 代理的图片地址（kind: primary/thumb/backdrop）。
func (m *MetaTube) ImageURL(kind, provider, id string) string {
	if kind == "" {
		kind = "primary"
	}
	return fmt.Sprintf("%s/v1/images/%s/%s/%s", m.BaseURL,
		url.PathEscape(kind), url.PathEscape(provider), url.PathEscape(id))
}

// PreviewImageURL 返回剧照地址。
func (m *MetaTube) PreviewImageURL(provider, id string, index int) string {
	return fmt.Sprintf("%s/v1/images/preview/%s/%s?index=%d", m.BaseURL,
		url.PathEscape(provider), url.PathEscape(id), index)
}

// FetchImage 下载图片字节，优先走 MetaTube 代理，失败则回退到直链。
func (m *MetaTube) FetchImage(ctx context.Context, kind, provider, id string, fallbackURLs ...string) ([]byte, string, error) {
	candidates := []string{m.ImageURL(kind, provider, id)}
	for _, u := range fallbackURLs {
		if strings.TrimSpace(u) != "" {
			candidates = append(candidates, u)
		}
	}
	var lastErr error
	for _, u := range candidates {
		data, ct, err := fetchImageBytes(ctx, m.HTTP, u, m.Token)
		if err == nil && len(data) > 0 {
			return data, ct, nil
		}
		if err != nil {
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("没有可用的图片地址")
	}
	return nil, "", lastErr
}

// fetchImageBytes 下载图片并返回内容类型。
func fetchImageBytes(ctx context.Context, client *http.Client, rawURL, bearer string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) "+embyClientName)
	req.Header.Set("Accept", "image/*,*/*")
	if bearer != "" && strings.Contains(rawURL, "/v1/images/") {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, "", fmt.Errorf("下载图片 %s 返回 %d", rawURL, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, "", err
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" || strings.Contains(ct, "octet-stream") {
		ct = http.DetectContentType(data)
	}
	return data, ct, nil
}
