# Emby 元数据编辑器

一个用 Go 写的 Emby 媒体库管理工具。单文件 exe，双击即用，界面跑在本机浏览器里（默认只监听 `127.0.0.1`，不会暴露到局域网）。

围绕五件事：

| 模块 | 能力 |
|---|---|
| **Emby 登录** | 用户名 / 密码 或 API Key 两种方式，凭据本地保存 |
| **MetaTube 刮削** | 单个 / 批量刮削元数据与图片，**服务地址自行配置**（公共后端已下线，建议自建） |
| **gfriends 头像库** | 10 万+ 张头像索引，演员头像单个挑、批量刮 |
| **javbus 番号统计** | 按演员抓取全部番号，与本地媒体库比对找出缺失，抓取磁力列表；**带连通性诊断** |
| **媒体库统计** | 各媒体库条目数、电影 / 剧集 / 集数、可选统计占用体积 |

---

## 快速开始

### 方式一：双击

直接双击 `EmbyMetaEditor.exe`，会自动打开浏览器进入控制台。

### 方式二：命令行

```bash
EmbyMetaEditor.exe                 # 默认 127.0.0.1:8097，自动开浏览器
EmbyMetaEditor.exe -port 8098      # 换端口
EmbyMetaEditor.exe -open=false     # 不自动开浏览器
EmbyMetaEditor.exe -dir D:\data    # 指定数据目录
EmbyMetaEditor.exe -host 0.0.0.0   # 允许局域网访问（默认不开，注意凭据安全）
```

首次启动会在程序目录生成 `config.json`（配置）和 `cache/`（gfriends 索引缓存，约 10 MB，下载一次可离线复用）。

### 使用顺序

1. **登录** —— 填 Emby 地址 + 用户名密码，或切到 API Key 模式。
   API Key 在 Emby 后台「高级 → API 密钥」里生成。
2. **概览统计** —— 看媒体库数量分布、缺头像演员数；勾「统计体积」会扫描文件大小（较慢）。
3. **媒体库刮削** —— 选媒体库、筛「只看无海报」、批量刮削；点单个卡片的「刮削」或「详情」可手动搜索匹配。
4. **演员头像** —— 默认列出无头像演员，点「刮削」单个处理，「批量刮削头像」按数量批量跑。
5. **番号补全** —— 填演员名（或 javbus 演员页地址），统计后得到缺失番号网格，勾选后抓磁力。

---

## 配置项

全部可在界面「设置」里改，也可以直接编辑 `config.json`。

| 配置 | 默认值 | 说明 |
|---|---|---|
| `emby_url` | `http://127.0.0.1:8096` | Emby 服务器地址 |
| `metatube_url` | `http://127.0.0.1:8080` | MetaTube Server 地址，**必填自建实例** |
| `metatube_token` | 空 | MetaTube 实例开了鉴权时填写 |
| `gfriends_tree_url` | jsdelivr 上的 Filetree.json | 头像索引地址 |
| `gfriends_cdn` | jsdelivr 上的仓库根 | 头像图片前缀 |
| `javbus_url` | `https://www.javbus.com` | 可换成镜像站 |
| `javbus_cookie` | `age=verified; dv=1; existmag=mag` | javbus 的年龄验证 + 磁力开关 |
| `proxy` | 空 | HTTP 代理，留空则读系统环境变量 |
| `concurrency` | 4 | 批量任务并发数 |
| `javbus_interval_ms` | 1500 | javbus 请求间隔，别调太小 |
| `insecure_tls` | false | 自签证书的 Emby 勾上 |

### 关于 MetaTube

官方公共后端已下线，需要自己跑一个：

```bash
docker run -d --name metatube -p 8080:8080 metatube/metatube-server:latest
```

装好后把 `metatube_url` 填成 `http://你的IP:8080`，在「设置 → MetaTube → 获取 provider 列表」点一下能列出 `FANZA / MGStage / DUMMY` 之类就算通了。

搜影片时**不指定 provider 更稳**：指定了会强制走那一个站点的抓取器，某个抓取器挂掉就是 500。不指定则由服务端并发查所有站点再聚合，实测同一个番号能同时拿到 `JavBus` / `JAV321` 两家的结果，按番号精确匹配取最合适的一条。

### 关于 Emby 的 API Key 登录

API Key 在 Emby 后台「高级 → API 密钥」生成，直接填进「设置 → API Key」即可。

注意：**API Key 是服务器级的，不绑定具体用户**。Emby 4.9.x 上用 API Key 请求 `/Users/Me` 会返回 500（`Unrecognized Guid format`），所以工具做了多级回退——先试 `/Users/Me`，不行就用已保存的 `user_id` 查 `/Users/{id}`，再不行列 `/Users` 挑一个。你只要在界面上正常登录过一次，身份就存下来了，之后一直能用。

### 关于 gfriends

`raw.githubusercontent.com` 在国内基本直连不了，所以默认走 jsdelivr CDN。索引文件有 6.5 MB，代码里带了三重备用地址（jsdelivr / gcore / fastly），换 CDN 不用重新下索引。

### 关于 javbus

- 站点需要 `age=verified` 之类的 Cookie 才给看内容，默认值已内置。
- 如果被 Cloudflare 拦（返回 403 + "Just a moment"），从浏览器 F12 里复制完整 Cookie（含 `cf_clearance`）粘进设置。
- 抓磁力要请求作品详情页 + 一个 ajax 接口，所以每个番号约 2 次请求，代码里默认限速 1.5 秒 / 次、并发 2。
- **抓不到东西时，先点「连通性诊断」。** 它会在输入框有内容时顺便测一次演员搜索接口。

#### 连通性诊断

javbus 这类站点出问题的原因有好几种，光看「抓取失败」分不出来。点一下诊断，它会把结论直接摊开：

| 现象 | 诊断给出的结论 |
|---|---|
| 域名解析不了 / 连接被拒 / 超时 | 无法连接 —— 检查网络或代理，也可以换个镜像地址 |
| HTTP 200 但**响应体是空的** | 被本机网络或运营商拦截了（这是国内最常见的形态） |
| 页面含 `Just a moment` / `cf-browser-verification` | 被 Cloudflare 拦了 —— 去浏览器复制含 `cf_clearance` 的完整 Cookie |
| 能连上但页面里没有影片列表标记 | 可能返回的是验证页或公告页，**原始页面已存到 `cache/debug/`** |
| 演员搜索接口返回空数组 | 站内没有这个演员名，换个写法或直接填演员页地址 |

诊断结果里还会列出 HTTP 状态、响应大小、耗时、以及页面上各结构标记（`movie-box` / `photo-frame` / `pics/cover` …）的出现次数。判断不了的时候，去 `cache/debug/` 打开落盘的原始 HTML 看看到底返回了什么 —— 比猜快得多。

也可以用命令行直接调：

```bash
curl "http://127.0.0.1:8097/api/javbus/probe"
curl "http://127.0.0.1:8097/api/javbus/probe?q=三上悠亜"
```

#### 封面图片为什么要走服务端

javbus 的图片按 **Referer 防盗链**：同一张封面，不带 Referer 返回 403，带 `Referer: https://www.javbus.com/` 返回 200，带 `Referer: http://127.0.0.1:8097/` 同样 403。所以浏览器从本机页面直接 `<img src="https://www.javbus.com/pics/...">` 必然拿不到图，页面上就是一片空白。

页面里所有外部图片都走 `/api/img?u=<原始地址>`：

| 目标主机 | 行为 |
|---|---|
| 白名单（当前配置的 javbus / gfriends 主机 + `pics.dmm.co.jp`、`cdn.jsdelivr.net` 等公开图床） | 服务端带正确 Referer 代取，内存缓存 6 小时 |
| 其他公网图床 | 302 回原地址，浏览器直连 —— 效果与直连完全一致 |
| 内网地址、非 http(s) | 502 拒绝，避免这个接口变成 SSRF 跳板 |

「哪些图需要代取」的策略只写在服务端，所以你把 javbus 换成镜像域名时不用动前端。

```bash
curl -I "http://127.0.0.1:8097/api/img?u=https%3A%2F%2Fwww.javbus.com%2Fpics%2Fcover%2F83hf_b.jpg"
# HTTP/1.1 200 OK
# Content-Type: image/jpeg
# Cache-Control: public, max-age=21600
```

---

## 界面

| 登录 | 概览统计 |
|---|---|
| ![登录](screenshots/01-登录.png) | ![概览](screenshots/02-概览统计.png) |

| 媒体库刮削 | 详情与手动匹配 |
|---|---|
| ![媒体库](screenshots/03-媒体库刮削.png) | ![详情](screenshots/04-详情与手动匹配.png) |

| 演员头像 | 番号补全 |
|---|---|
| ![演员](screenshots/05-演员头像.png) | ![番号](screenshots/06-番号补全.png) |

| 连通性诊断 · 正常 | 连通性诊断 · 失败 |
|---|---|
| ![诊断正常](screenshots/08-连通性诊断-正常.png) | ![诊断失败](screenshots/09-连通性诊断-失败.png) |

（截图里的数据来自本地 mock Emby / mock javbus，仅用于展示界面。）

---

## 从源码构建

```bash
go build -trimpath -ldflags "-s -w" -o EmbyMetaEditor.exe .
```

单文件 exe，前端资源（`web/`）和图标（`rsrc_windows_amd64.syso`）都通过 `go:embed` / PE 资源嵌进去了，拷走 exe 就能跑。

跑测试：

```bash
go test ./...
```

90 个用例，覆盖番号归一化、javbus 页面解析（含备用结构回退、裸 `<tr>` 片段、真实详情页片段）、连通性诊断的五种失败形态、MetaTube 字段兼容与 provider 结构兼容、Emby 用户身份解析的多级回退、以及用 mock Emby + mock MetaTube 跑通的完整刮削链路。

其中 mock Emby 是**按真实 4.9 构建的行为建模**的，不是「理想 Emby」：读详情只认用户作用域路由（全局路径返回 404）、写操作只认全局路径、`POST /Items/{id}` 是整对象替换、图片上传只收 base64 文本、列表接口不返回 `SortName`。线上踩过的坑因此都能在单元测试里复现，而不是等上线才发现。

界面回归（可选）：起一个模拟 javbus 站点，用无头浏览器真实点击验证诊断按钮的渲染。

```bash
python tools/mock_javbus.py 9500 &          # 模拟 javbus
EmbyMetaEditor.exe -port 8097 -open=false & # 起应用
python tools/verify_javbus_probe.py         # 走 CDP 点击 + 截图 + 断言
```

实机冒烟（可选）：对真实 Emby / MetaTube / gfriends / javbus 跑一轮**只读**验证，逐项打印实测数字。

```bash
EmbyMetaEditor.exe -port 8097 -open=false &
python tools/smoke_real.py
```

前端静态自检（不打开浏览器，比对 HTML / JS / 后端路由）：

```bash
python tools/check_frontend.py
```

页面图片的**渲染**回归（需要无头 Edge + 真实 config.json）：

```bash
# 1) 起应用（真实配置）  2) 起无头 Edge
EmbyMetaEditor.exe -port 8097 -open=false &
"C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe" \
  --headless=new --disable-gpu --remote-debugging-port=9333 \
  --remote-allow-origins=* --user-data-dir=C:/Users/xiao/AppData/Local/Temp/edgeimg about:blank &
# 3) 跑验证
python tools/verify_images.py
```

它会真的点「统计番号」、等结果、**逐个 `scrollIntoView` 触发懒加载**，然后断言每张图的
`<img>` 都走了服务端代理且 `naturalWidth > 0`，最后截图到 `screenshots/`。
覆盖三处：番号补全的缺失番号封面、MetaTube 搜索结果的封面、gfriends 头像库的缩略图。

> 「接口返回了 JPEG」和「页面上看得到图」是两件事，这个脚本验证的是后者。

---

## 目录结构

```
main.go              启动、参数、控制台 UTF-8、自动开浏览器
config.go            配置结构与持久化
api.go               HTTP 路由与处理函数
emby.go              Emby REST 客户端
imageproxy.go        图片代理：Referer 防盗链、白名单、6 小时缓存
metatube.go          MetaTube v1 客户端
gfriends.go          gfriends 索引下载 / 缓存 / 查询
javbus.go            javbus 抓取、HTML 解析、连通性诊断
scrape.go            刮削编排、番号比对
jobs.go              后台任务与进度
util.go              番号归一化、HTML 辅助、HTTP 客户端
console_windows.go   Windows 控制台切 UTF-8
web/                 前端（原生 JS，无构建步骤）
tools/               验证脚本：模拟站点 / CDP 界面回归 / 前端自检 / 封面渲染 / 实机冒烟
app.ico              图标源文件
```

验证脚本一览：

| 脚本 | 验什么 | 需要什么 |
|---|---|---|
| `check_frontend.py` | HTML / JS / 后端路由静态对照 | 无 |
| `smoke_real.py` | 真实环境只读冒烟 23 项 | 起 exe + 真实 config |
| `verify_images.py` | 页面图片**真的渲染出来**（番号补全 / MetaTube 搜索 / gfriends 三处） | 起 exe + 无头 Edge |
| `mock_javbus.py` + `verify_javbus_probe.py` | 模拟站点 + 诊断按钮的界面交互 | 起 exe + 无头 Edge |
| `live_test.go`（`EMBY_LIVE=1 go test -run TestLive`） | 实机**写**路径，幂等不改变数据 | 真实 config |

---

## 已知边界

- 刮削只处理 `Movie` 类型条目；剧集 / 分集不在范围内。
- 番号识别靠正则从片名和路径里提取，路径里带番号是最稳的。
- javbus 页面结构如果改版，解析可能失效 —— `parseStarPage` / `parseMagnets` 已有单元测试夹具，改起来很快。真改版了先跑「连通性诊断」，看 `cache/debug/` 里的原始页面就知道新结构长什么样。
- javbus 有反爬。限速默认 1.5 秒 / 次、并发 2，别调太激进。
- 工具默认只监听本机。用 `-host 0.0.0.0` 开放出去等于把 Emby 凭据摊在网上，自己掂量。
- `SortName` / `ForcedSortName` 在部分 Emby 构建上设不进去，详见下面「一个改不回来的字段」。
- 封面代理只放行白名单主机。要加自己的图床，改 `imageproxy.go` 里的 `imageCDNHosts`。

---

## 踩过的坑（都已在代码里修掉）

记在这里，是因为它们都属于「照文档写就会错」的类型：

| 现象 | 根因 | 处理 |
|---|---|---|
| Emby 报 `token_valid: false`，但媒体库明明能列出 | `/Users/Me` 在 Emby 4.9.x 上用 API Key 调会 500（`Unrecognized Guid format`）——该令牌没有关联用户 | `Emby.Me` 改为多级回退：`/Users/Me` → `/Users/{已知id}` → `/Users` 列表 |
| 媒体库刮削 / 点详情报 `404 找不到文件 "/Items/513232"` | 这个构建**只注册了用户作用域的详情路由**，`GET /Items/{id}` 会落到静态文件处理器。反过来写操作（`POST /Items/{id}`、`/Refresh`、`DELETE …/Images/…`）**只有全局路径存在** | `ItemDetail` 先试 `/Users/{uid}/Items/{id}` 再退回全局；写操作保持全局路径 |
| 刮削后条目简介、年份、评分全没了 | `POST /Items/{id}` 是**整对象替换**而不是部分更新——body 里没带的字段会被清空 | `UpdateItem` 以**当前完整 DTO** 为底再叠加 patch；只排除 `Etag`/`MediaSources`/`MediaStreams`/`Chapters` 这类服务端派生的大字段 |
| 更新报 `400 Value cannot be null. (Parameter 'source')` | body 里缺 `ProviderIds`（或为 `null`）时服务端直接拒绝 | `UpdateItem` 始终回填一个非 nil 的 `ProviderIds`，并与已有外部 ID 合并 |
| 上传头像报 `500 The input is not a valid Base-64 string…` | 这个构建的 `POST /Items/{id}/Images/{Primary}` 要的是 **base64 文本** body，标准 Emby 要的是原始字节 | 先发原始字节，命中 base64 报错再换格式重试。**顺序不能反**——标准服务器上 base64 文本会被当成图片数据静默存进去 |
| 上传图片报 `400 Unable to determine image file extension from mime type` | 服务端用 Content-Type 决定存盘扩展名 | `normalizeImageType` 归一化 MIME，缺失时按文件头猜；认不出是图片就本地报错，不发请求 |
| 缺失番号列表里封面全是空白 | javbus 图片的 Referer 防盗链（详见上一节） | 服务端 `/api/img` 代理 + 6 小时缓存 |
| 媒体库卡片的角标显示的是年份，不是番号 | **Emby 的 `/Items` 列表接口不返回 `SortName`**（实测全是 `null`，详情接口才返回），前端拿它当番号永远拿不到值 | 番号改由服务端算：`itemNumber()` 复用刮削那套归一化，`/api/items` 与 `/api/items/detail` 都回填 `Number` |
| javbus 磁力永远「暂无链接」，但接口明明有数据 | ajax 返回的是**裸 `<tr>` 片段**，HTML5 树构造会把游离的 `<tr>` 直接丢弃 | `parseMagnets` 先套一层 `<table><tbody>` 再解析 |
| MetaTube 报「解析 providers 失败」 | v1 实际返回 `{"data":{"movie_providers":{…}}}`，不是文档里的扁平数组 | 三种结构都兼容 |
| 刮削后制作商 / 简介为空 | 实际字段名是 `maker` / `summary`，代码里写的是 `studio` / `plot` | 两套都留，取值用 `firstNonEmpty` 兜底 |
| 番号统计偶发抓不到演员 | 搜索接口有时返回 JSON、有时返回 HTML | 两种都解析，且失败时落盘原始响应 |

### 一个改不回来的字段

`SortName` / `ForcedSortName` 在这个 Emby 构建（4.9.0.42）上**无法通过 API 设置**。实测三种 payload 形态、只发 `ForcedSortName`、以及 `/Items/{id}/Metadata` 端点，全部无效（POST 返回 204 但服务端总是按 `Name` 重新计算）。条目本身没有锁定（`LockedFields` 为空）。

好在原始日文标题同时存在于 `OriginalTitle` 字段里，没有真的丢。刮削时也就没必要再往 patch 里塞这两个字段了。

MIT License.
