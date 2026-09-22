package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// 为什么需要图片代理：
//
// javbus 的图片有**基于 Referer 的防盗链**。实测同一张封面：
//   不带 Referer                        -> 403
//   Referer: https://www.javbus.com/    -> 200（200KB JPEG）
//   Referer: http://127.0.0.1:8097/     -> 403
// 浏览器从本机页面直接 <img src="https://www.javbus.com/pics/..."> 必然拿不到图。
//
// MetaTube 的搜索结果里，JavBus provider 给的 cover_url 也是 javbus 域名，
// 所以同一个代理也服务那边。
//
// 安全：只代理**白名单主机**上的图片，避免这个接口变成任意 URL 代理（SSRF）。
// 白名单 = 当前配置里的 javbus / gfriends 主机 + 一组公开图床常量。

const (
	imageCacheTTL     = 6 * time.Hour
	imageCacheMaxKeep = 400
	imageMaxBytes     = 8 << 20
)

// imageCDNHosts 是允许代理的公开图床（都是只读的公开图片服务）。
var imageCDNHosts = []string{
	"javbus.com", "www.javbus.com",
	"javcdn.com", "www.javcdn.com",
	"pics.dmm.co.jp", "awsimgsrc.dmm.co.jp",
	"cdn.jsdelivr.net",
}

type imageEntry struct {
	data  []byte
	ctype string
	at    time.Time
}

// ImageProxy 是带容量上限和 TTL 的图片缓存。
// 一屏缺失番号可能有几十上百张图，不缓存会对上游造成不必要的压力。
type ImageProxy struct {
	mu    sync.Mutex
	items map[string]*imageEntry
	order []string
	http  *http.Client
}

// NewImageProxy 构造图片代理。
func NewImageProxy(client *http.Client) *ImageProxy {
	if client == nil {
		client = http.DefaultClient
	}
	return &ImageProxy{items: map[string]*imageEntry{}, http: client}
}

func (p *ImageProxy) lookup(key string) (*imageEntry, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.items[key]
	if !ok {
		return nil, false
	}
	if time.Since(e.at) > imageCacheTTL {
		delete(p.items, key)
		return nil, false
	}
	return e, true
}

func (p *ImageProxy) store(key string, e *imageEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.items[key]; !exists {
		p.order = append(p.order, key)
	}
	p.items[key] = e
	for len(p.order) > imageCacheMaxKeep {
		oldest := p.order[0]
		p.order = p.order[1:]
		delete(p.items, oldest)
	}
}

// allowedImageHosts 汇总允许服务端代取的主机名（不带端口）。
func allowedImageHosts(cfg Config) map[string]bool {
	hosts := map[string]bool{}
	for _, raw := range []string{cfg.JavBusURL, cfg.GfriendsCDN} {
		if u, err := url.Parse(strings.TrimSpace(raw)); err == nil && u.Hostname() != "" {
			hosts[strings.ToLower(u.Hostname())] = true
		}
	}
	for _, h := range imageCDNHosts {
		hosts[h] = true
	}
	return hosts
}

// isPrivateHost 判断主机是否是本机 / 内网地址。
func isPrivateHost(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

// ClassifyImage 解析目标地址，并判断该由谁去取。
//
// 返回 proxy=true：目标在白名单内（javbus 等有 Referer 防盗链的图床），服务端代取。
// 返回 proxy=false：公网普通图床，让浏览器直连——服务端中转没有意义，
// 也免得这个接口变成开放中继。
// 非 http(s) 或内网地址一律拒绝（SSRF）。
//
// 注意白名单判定要放在内网检查之前：用户可能把 gfriends 镜像配在内网地址上，
// 那是他自己的配置，代取没问题。
func ClassifyImage(raw string, cfg Config) (*url.URL, bool, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, false, fmt.Errorf("图片地址无法解析")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, false, fmt.Errorf("只支持 http/https 图片地址")
	}
	if u.Hostname() == "" {
		return nil, false, fmt.Errorf("图片地址缺少主机名")
	}
	host := strings.ToLower(u.Hostname())
	if allowedImageHosts(cfg)[host] {
		return u, true, nil
	}
	if isPrivateHost(host) {
		return nil, false, fmt.Errorf("不允许代理内网地址：%s", u.Host)
	}
	return u, false, nil
}

// Fetch 是「必须由服务端代取」的便捷入口；目标不在白名单时直接报错。
func (p *ImageProxy) Fetch(ctx context.Context, raw string, cfg Config) ([]byte, string, error) {
	u, proxy, err := ClassifyImage(raw, cfg)
	if err != nil {
		return nil, "", err
	}
	if !proxy {
		return nil, "", fmt.Errorf("该主机不在代理白名单内：%s", u.Host)
	}
	return p.fetchResolved(ctx, u)
}

// fetchResolved 取回已解析的图片数据（带缓存）。
func (p *ImageProxy) fetchResolved(ctx context.Context, target *url.URL) ([]byte, string, error) {
	key := target.String()
	if e, ok := p.lookup(key); ok {
		return e.data, e.ctype, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, key, nil)
	if err != nil {
		return nil, "", err
	}
	// 防盗链的关键。用「目标自己的 origin」当 Referer：
	// javbus 认自家域名，其他图床本来就不校验，这个值两边都合适。
	req.Header.Set("Referer", target.Scheme+"://"+target.Host+"/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "image/avif,image/webp,image/*,*/*;q=0.8")

	resp, err := p.http.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("下载图片失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, "", fmt.Errorf("图片返回 HTTP %d（javbus 有 Referer 防盗链，若持续失败可换镜像地址）", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, imageMaxBytes))
	if err != nil {
		return nil, "", err
	}
	if len(data) == 0 {
		return nil, "", fmt.Errorf("图片响应体为空")
	}
	ctype := resp.Header.Get("Content-Type")
	if ctype == "" || strings.Contains(ctype, "octet-stream") {
		ctype = http.DetectContentType(data)
	}
	if !strings.HasPrefix(ctype, "image/") {
		return nil, "", fmt.Errorf("上游返回的不是图片（%s）", ctype)
	}
	p.store(key, &imageEntry{data: data, ctype: ctype, at: time.Now()})
	return data, ctype, nil
}
