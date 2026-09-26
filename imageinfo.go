package main

// 图片信息探测：给「选择头像」弹窗里的候选图配一行「宽×高 · 体积」。
//
// 为什么要有这个接口：同一个演员在 gfriends 库里常有多张候选（不同分组目录），
// 缩略图都缩到 76×104 显示，肉眼分不出哪张是原图、哪张是压缩过的小图。
// 像素尺寸和文件体积是最直接的判断依据 —— 但它们**都拿不到于前端**：
//   - 体积：浏览器没有 API 能读一张 <img> 的字节数；用 fetch 拿 blob 会被
//     CSP（connect-src 'self'）拦在跨域之外，而这个项目**不给 CSP 开口子**。
//   - 像素：naturalWidth 只有等图加载完才有，且要先让浏览器把整张图下下来。
//
// 所以由服务端代取（和 /api/img 同一套白名单判断）。顺带一个好处：
// 探测过程会把这些图写进 ImageProxy 的缓存，弹窗里的缩略图随后是秒开。

import (
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/gif"  // 注册解码器，否则 DecodeConfig 认不出格式
	_ "image/jpeg" // 同上
	_ "image/png"  // 同上
	"net/http"
	"sync"
	"time"
)

const (
	// maxImageProbe 一次最多探这么多张。上限是必须的：这个接口会真的去上游
	// 下载每一张图，不设限就等于对外开放了一个批量下载器。
	maxImageProbe = 80
	// imageProbeTimeout 单张图的超时。头像是几十 KB 的东西，15 秒已经很宽。
	imageProbeTimeout = 15 * time.Second
	// imageProbeWorkers 探测并发。别调大：上游是 jsdelivr 这类免费 CDN，
	// 一个搜索页几十张图，8 路并发已经能在两三秒内跑完。
	imageProbeWorkers = 8
)

// imageInfo 是一张图的探测结果。OK=false 时 Error 说明原因（不在白名单 / 取不到）。
type imageInfo struct {
	URL    string `json:"url"`
	OK     bool   `json:"ok"`
	Bytes  int    `json:"bytes,omitempty"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
	Format string `json:"format,omitempty"`
	Error  string `json:"error,omitempty"`
}

// probeImageConfig 从图片字节里读出像素尺寸。
//
// 用 DecodeConfig 而不是 Decode：前者只解析文件头，后者要把整张图解出来。
// 一张 2000px 的图全解码要几十毫秒，而这里只关心「有多大」。
// 解析失败（WebP 等没注册解码器的格式）返回全零，由调用方决定怎么显示。
func probeImageConfig(data []byte) (int, int, string) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, ""
	}
	return cfg.Width, cfg.Height, format
}

// handleImageInfo 批量探测图片尺寸与体积，请求体 {"urls": ["https://…", …]}。
//
// 只探白名单主机（复用 ClassifyImage 那套判断）。非白名单主机不回 5xx 而是
// 逐条给 ok=false —— 一次搜索里混进一两个不认识的域名不该让整批结果作废。
func (a *App) handleImageInfo(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URLs []string `json:"urls"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if len(in.URLs) == 0 {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("缺少 urls"))
		return
	}
	// 超上限就截断并如实告诉前端，而不是报错 —— 界面上「后面的图没有大小」
	// 比「整批都探测失败」好得多。
	truncated := false
	if len(in.URLs) > maxImageProbe {
		in.URLs = in.URLs[:maxImageProbe]
		truncated = true
	}

	cfg := a.store.Get()
	out := make([]imageInfo, len(in.URLs))
	sem := make(chan struct{}, imageProbeWorkers)
	var wg sync.WaitGroup
	for i, raw := range in.URLs {
		wg.Add(1)
		go func(i int, raw string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = a.probeOneImage(r.Context(), cfg, raw)
		}(i, raw)
	}
	wg.Wait() // 等全部收工再返回，否则 r.Context() 会先被取消

	writeOK(w, map[string]any{"items": out, "truncated": truncated})
}

// probeOneImage 探测一张图。白名单外的地址由 Fetch 直接拒绝。
func (a *App) probeOneImage(ctx context.Context, cfg Config, raw string) imageInfo {
	info := imageInfo{URL: raw}
	ctx, cancel := context.WithTimeout(ctx, imageProbeTimeout)
	defer cancel()

	data, _, err := a.images.Fetch(ctx, raw, cfg)
	if err != nil {
		info.Error = err.Error()
		return info
	}
	info.OK = true
	info.Bytes = len(data)
	info.Width, info.Height, info.Format = probeImageConfig(data)
	return info
}
