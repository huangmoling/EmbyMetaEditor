"""对真实 Emby / MetaTube / gfriends / javbus 做一轮只读冒烟验证。

只调只读接口，不会往用户的 Emby 里写任何东西。
用法：python tools/smoke_real.py [base_url]
"""
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

BASE = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:8097"

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import wbauth  # noqa: E402  （访问认证辅助，见 wbauth.py）

# 界面有进程自己的登录，未登录时 /api/ 一律 401 —— 先换一个会话 cookie，
# 后面所有请求都带上它（密码从 EMBYME_AUTH_PASSWORD 读，默认 test-pass）。
COOKIE_HEADER = {"Cookie": wbauth.cookie_header(BASE)}


def get(path, timeout=90):
    req = urllib.request.Request(BASE + path, headers=dict(COOKIE_HEADER))
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode())


def fetch_raw(path, timeout=60, headers=None):
    """取原始响应，返回 (状态码, Content-Type, 字节)。不解析 JSON。"""
    h = dict(COOKIE_HEADER)
    h.update(headers or {})
    req = urllib.request.Request(BASE + path, headers=h)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, r.headers.get("Content-Type", ""), r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.headers.get("Content-Type", ""), e.read()


# 浏览器 UA：国产传媒几家图床会按 UA 拦非浏览器请求（实测 upload.xchina.io
# 对 Python-urllib 直接 403，换浏览器 UA 就 200）。验「浏览器能不能拿到图」
# 就得用浏览器的身份去要。
BROWSER_HEADERS = {
    "User-Agent": ("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
                   "(KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"),
    "Accept": "image/avif,image/webp,image/*,*/*;q=0.8",
}


def q(s):
    return urllib.parse.quote(s, safe="")


def post(path, payload, timeout=30):
    h = {"Content-Type": "application/json"}
    h.update(COOKIE_HEADER)
    req = urllib.request.Request(
        BASE + path, method="POST",
        data=json.dumps(payload).encode(), headers=h)
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode())


def wait_job(jid, timeout=180):
    end = time.time() + timeout
    while time.time() < end:
        j = get("/api/jobs/" + jid, timeout=20)["data"]
        if j["status"] != "running":
            return j
        time.sleep(1.5)
    raise TimeoutError("任务超时：" + jid)


results = []


def check(name, ok, detail=""):
    results.append((name, ok, detail))
    print(("PASS  " if ok else "FAIL  ") + name + (("  | " + detail) if detail else ""))


print("=" * 70)
print("对真实环境的只读冒烟验证")
print("=" * 70)

# --- 1. Emby 登录态 ---
print("\n[1] Emby")
try:
    st = get("/api/emby/status", timeout=30)["data"]
    check("服务在线", st.get("online") is True, st.get("server", {}).get("ServerName", ""))
    check("令牌有效", st.get("token_valid") is True,
          "用户=%s" % st.get("user"))
    libs = get("/api/libraries", timeout=30)["data"]
    check("媒体库可列出", len(libs) > 0, "%d 个库" % len(libs))
except Exception as e:
    check("Emby 接口", False, str(e))

# --- 2. 媒体库统计 ---
print("\n[2] 媒体库统计")
try:
    st = get("/api/stats", timeout=120)["data"]
    c = st["counts"]
    check("总量统计", c.get("MovieCount", 0) > 0, "影片 %d 部" % c.get("MovieCount", 0))
    check("分库统计", len(st.get("libraries", [])) > 0,
          " / ".join("%s=%d" % (l["name"], l["item_count"]) for l in st["libraries"][:4]))
    g = st.get("gfriends", {})
    check("gfriends 索引", g.get("loaded") is True,
          "%d 位演员 / %d 张图" % (g.get("names", 0), g.get("total", 0)))
except Exception as e:
    check("统计接口", False, str(e))

# --- 2b. 条目详情（线上「GET /Items/{id} 404」的回归）---
print("\n[2b] 条目详情")
try:
    libs0 = get("/api/libraries", timeout=30)["data"]
    pid0 = next((l["Id"] for l in libs0 if "骑兵" in (l.get("Name") or "")), libs0[0]["Id"])
    lst = get("/api/items?parent=%s&limit=3" % pid0, timeout=90)["data"]
    items = lst.get("items") or []
    check("能列出条目", len(items) > 0, "共 %d 条" % lst.get("total", 0))
    if items:
        iid = items[0]["Id"]
        d = get("/api/items/detail?id=" + q(iid), timeout=90)["data"]
        check("条目详情可读", bool(d.get("Id")),
              "%s / %s" % (d.get("Name", ""), iid))
except Exception as e:
    check("条目详情", False, str(e))

# --- 3. javbus 连通性诊断 ---
print("\n[3] javbus 诊断")
try:
    p = get("/api/javbus/probe?q=" + urllib.parse.quote("三上悠亜"), timeout=90)["data"]
    check("站点可达", p.get("ok") is True,
          "HTTP %s / %d 字节 / %d ms" % (p.get("status"), p.get("size"), p.get("elapsed_ms")))
    check("页面结构符合预期", p.get("looks_like_home") is True,
          "movie-box × %d" % (p.get("markers") or {}).get("movie-box", 0))
    s = p.get("search") or {}
    check("演员搜索接口", s.get("ok") is True,
          "%s 形式，命中 %d 个" % (s.get("kind"), s.get("found", 0)))
except Exception as e:
    check("javbus 诊断", False, str(e))

# --- 4. javbus 番号统计（真实抓取）---
print("\n[4] javbus 番号统计（真实抓取 3 页）")
miss = []
try:
    libs = get("/api/libraries", timeout=30)["data"]
    pid = next((l["Id"] for l in libs if "骑兵" in (l.get("Name") or "")), libs[0]["Id"])
    jid = post("/api/javbus/scan",
               {"star": "三上悠亜", "parent_id": pid, "max_pages": 3})["data"]["job_id"]
    j = wait_job(jid)
    r = j.get("result") or {}
    check("任务完成", j["status"] == "done", "进度 %s/%s" % (j.get("done"), j.get("total")))
    check("抓到作品", r.get("total", 0) > 0,
          "javbus %d 部 / 本地命中 %d / 缺失 %d" % (r.get("total", 0), r.get("matched", 0), len(r.get("missing") or [])))
    miss = r.get("missing") or []
    if miss:
        jid2 = post("/api/javbus/magnets", {"targets": miss[:2]})["data"]["job_id"]
        j2 = wait_job(jid2)
        res = j2.get("result") or []
        tot = sum(len(x.get("magnets") or []) for x in res)
        check("磁力抓取", tot > 0, " / ".join("%s=%d 条" % (x["number"], len(x.get("magnets") or [])) for x in res))
        if res and res[0].get("magnets"):
            m = res[0]["magnets"][0]
            check("磁力字段完整", bool(m.get("link") and m.get("size")),
                  "%s %s" % (m.get("size"), m.get("link")[:50]))
except Exception as e:
    check("javbus 抓取", False, str(e))

# --- 4b. 封面代理（线上「缺失番号没有图片」的回归）---
print("\n[4b] 封面代理")
cover = (miss[0].get("cover") or "") if miss else ""
if not cover:
    check("拿到待测封面", False, "上一节没抓到缺失番号，跳过（不算接口问题）")
else:
    st, ct, body = fetch_raw("/api/img?u=" + q(cover), timeout=90)
    check("白名单封面代取", st == 200 and ct.startswith("image/"),
          "HTTP %d / %s / %d 字节" % (st, ct, len(body)))
    # 内网地址必须被拒，否则这个接口就成了 SSRF 跳板
    st2, _, _ = fetch_raw("/api/img?u=" + q("http://192.168.1.1/x.jpg"), timeout=30)
    check("内网地址被拒", st2 == 502, "HTTP %d" % st2)
    st3, _, _ = fetch_raw("/api/img", timeout=30)
    check("缺参数报 400", st3 == 400, "HTTP %d" % st3)

# --- 5. MetaTube ---
print("\n[5] MetaTube")
try:
    pv = get("/api/metatube/providers", timeout=60)["data"]
    check("provider 列表", len(pv) > 0, "%d 个：%s…" % (len(pv), " / ".join(pv[:5])))
    sr = get("/api/metatube/search?q=SSNI-989", timeout=120)["data"]
    hit = [m for m in sr if (m.get("number") or "").upper() == "SSNI-989"]
    check("按番号搜索命中", len(hit) > 0,
          "%d 条结果，其中 %d 条番号完全匹配" % (len(sr), len(hit)))
    if hit:
        check("封面可用", bool(hit[0].get("cover_url")), hit[0].get("cover_url", "")[:60])
except Exception as e:
    check("MetaTube", False, str(e))

# --- 6. 演员 ---
print("\n[6] 演员与头像索引")
try:
    ps = get("/api/persons?limit=3", timeout=180)["data"]
    check("演员列表", ps.get("total", 0) > 0, "共 %d 位演员" % ps.get("total", 0))
    gf = get("/api/gfriends/search?q=" + urllib.parse.quote("三上悠亜"), timeout=60)["data"]
    check("gfriends 命中", len(gf) > 0,
          "%d 个条目" % (len(gf[0]["entries"]) if gf else 0))
except Exception as e:
    check("演员接口", False, str(e))

# --- 6b. 演员按媒体库过滤（验证真实 Emby 的 /Persons 支持 ParentId）---
print("\n[6b] 演员按媒体库过滤")
try:
    libs = get("/api/libraries")["data"]
    check("媒体库列表", len(libs) > 0, "%d 个库" % len(libs))

    g_all = get("/api/persons?limit=1", timeout=180)["data"].get("total", 0)
    check("全局演员数", g_all > 0, "%d 位" % g_all)

    sample = libs[:4]
    counts = []
    for lib in sample:
        d = get("/api/persons?limit=1&parent_id=" + urllib.parse.quote(lib["Id"]), timeout=180)["data"]
        counts.append((lib.get("Name", lib["Id"]), d.get("total", 0)))

    non_zero = [c for c in counts if c[1] > 0]
    check("按库过滤生效", len(non_zero) >= 1,
          " / ".join("%s=%d" % c for c in counts))

    # 任何一个库的演员数都不该等于全局数，否则说明 ParentId 根本没被服务端采纳
    same_as_global = [c for c in counts if c[1] == g_all and g_all > 0]
    check("过滤确实缩小了范围", not same_as_global,
          ("以下库返回了与全局相同的数量：" + str(same_as_global)) if same_as_global
          else "全局 %d，抽样库均小于全局" % g_all)

    # 真实行为（实测）：ParentId 格式非法（含非十六进制字符）→ Emby 直接
    # 500 "Unrecognized Guid format."；格式合法但库不存在 → 200 且 0 人。
    # 前端只会传 "" 或真实库 Id，两种都安全，所以这里验后者。
    bogus = get("/api/persons?limit=1&parent_id=" + "0" * 32, timeout=180)["data"].get("total", 0)
    check("不存在的库返回 0", bogus == 0, "实际 %d" % bogus)
except Exception as e:
    check("演员按库过滤", False, str(e))

# --- 7. 国产传媒专项刮削 ---
#
# 这一段全是**只读**的：搜索接口不写 Emby，批量接口一律带 dry_run=true。
# 用来确认四家站点真能连上、番号真能精确命中、模糊命中真被丢掉。
print("\n[7] 国产传媒专项刮削")
cn_lib = None
cn_cover = ""
try:
    sites = get("/api/cn/sites", timeout=30)["data"]
    check("站点配置", len(sites) == 4,
          " / ".join("%s=%s" % (s["name"], s["url"]) for s in sites))

    d = get("/api/cn/search?q=" + q("91CM-014"), timeout=180)["data"]
    ok_sites = [s for s in d["sites"] if s.get("ok")]
    check("四家站点都能抓", len(ok_sites) == 4,
          " / ".join("%s=%d 条" % (s["site"], len(s.get("hits") or [])) for s in d["sites"]))

    # xChina 的搜索页是服务端渲染的，91CM-014 在它那里有唯一精确命中
    xc = next((s for s in d["sites"] if s["site"] == "xchina"), {})
    xc_exact = [h for h in (xc.get("hits") or []) if h.get("exact")]
    check("xChina 精确命中 91CM-014", len(xc_exact) == 1,
          (xc_exact[0].get("title") if xc_exact else "没命中"))

    # 麻豆区的搜索是**模糊**的：搜 91CM-014 会返回 91CM074/084/094。
    # 这些一条都不能被判成精确命中，否则会把别的作品的封面写进去。
    mq = next((s for s in d["sites"] if s["site"] == "madouqu"), {})
    mq_hits = mq.get("hits") or []
    mq_exact = [h for h in mq_hits if h.get("exact")]
    check("麻豆区模糊结果被排除", len(mq_exact) == 0,
          "共 %d 条模糊结果（%s），精确 0 条" % (
              len(mq_hits), " / ".join((h.get("number") or "?") for h in mq_hits[:3])))

    check("合并结果只取精确命中", d["matched"] == ["xchina"], "matched=%s" % d["matched"])
    p = d["picked"]
    cn_cover = p.get("cover") or ""
    check("合并出标题与封面", bool(p.get("title")) and bool(cn_cover),
          "%s | %s" % (p.get("title"), cn_cover[:60]))

    # 麻豆社自己的番号（xChina / 麻豆区 / 麻豆社 三家都该命中）
    d2 = get("/api/cn/search?q=" + q("MDHG0010"), timeout=180)["data"]
    check("多站点命中同一番号", len(d2["matched"]) >= 2,
          "matched=%s，标题=%s" % (d2["matched"], d2["picked"].get("title")))

    # 条目上的番号要能正确识别「数字开头的番号」
    libs = get("/api/libraries", timeout=30)["data"]
    cn_lib = next((l for l in libs if "国产" in (l.get("Name") or "")), None)
    if cn_lib:
        lst = get("/api/items?parent=%s&limit=40&sort=SortName" % q(cn_lib["Id"]), timeout=120)["data"]
        nums = [it.get("Number") or "" for it in (lst.get("items") or [])]
        bad = [n for n in nums if n and n.startswith("CM-")]
        check("数字开头的番号没被砍前缀", not bad,
              "抽样 %d 条，形如 %s" % (len(nums), " / ".join([n for n in nums if n][:4])))
        # 扩展名误报：`.mp4` 会被番号正则拆成 "MP"+"4"，`.CD1.mkv` 会变成 "CD-1"。
        # 出过一次 —— 修之前 200 条里有 196 条「有番号」，相当一部分是这种。
        ext = [n for n in nums if n in ("MP-4", "MP-3", "MP-2", "CD-1", "CD-2")]
        check("文件扩展名没被当成番号", not ext,
              "抽样 %d 条，误报 %s" % (len(nums), ext[:5] or "无"))
except Exception as e:
    check("国产传媒接口", False, str(e))

# --- 7b. 国产传媒试运行批量（dry_run，绝不写数据）---
print("\n[7b] 国产传媒批量刮削（dry_run）")
if not cn_lib:
    check("找到国产传媒库", False, "上一节没定位到库，跳过")
else:
    try:
        jid = post("/api/cn/scrape-batch", {
            "parent_id": cn_lib["Id"], "limit": 3, "only_missing_cover": False,
            "fields": ["cover", "title", "tags", "date"], "dry_run": True,
        }, timeout=60)["data"]["job_id"]
        j = wait_job(jid, timeout=300)
        check("任务完成", j["status"] == "done", "进度 %s/%s" % (j.get("done"), j.get("total")))
        res = j.get("result") or []
        check("返回逐条结果", len(res) > 0, "%d 条" % len(res))
        wrote = [r for r in res if r.get("applied")]
        check("试运行没有写入任何条目", not wrote, "已写入 %d 条" % len(wrote))
        if res:
            r0 = res[0]
            check("结果带番号与站点明细", bool(r0.get("number")) and len(r0.get("sites") or []) == 4,
                  "%s [%s] %s" % (r0.get("item_name", "")[:24], r0.get("number"), r0.get("message")))
    except Exception as e:
        check("国产传媒批量", False, str(e))

# --- 7c. 国产传媒封面（这些主机在白名单里：对浏览器是 Cloudflare 挑战页，
#          服务端带浏览器 UA 才能取到，所以必须走 /api/img 代取）---
print("\n[7c] 国产传媒封面")
if not cn_cover:
    check("拿到待测封面", False, "上一节没合并出封面，跳过")
else:
    st, ct, body = fetch_raw("/api/img?u=" + q(cn_cover), timeout=90, headers=BROWSER_HEADERS)
    check("封面可渲染", st == 200 and ct.startswith("image/"),
          "HTTP %d / %s / %d 字节" % (st, ct, len(body)))

# --- 7d. 按勾选的 id 批量：界面「刮削选中」走的就是这条路。
#          仍然带 dry_run，绝不能因为改了接口就把冒烟脚本变成写操作。---
print("\n[7d] 国产传媒批量（按勾选的 id）")
if not cn_lib:
    check("找到国产传媒库", False, "前面没定位到库，跳过")
else:
    try:
        lst = get("/api/items?parent=%s&limit=40" % q(cn_lib["Id"]), timeout=120)["data"]
        pick = [it for it in (lst.get("items") or []) if it.get("Number")][:2]
        if not pick:
            check("库里有带番号的条目", False, "抽样 40 条都没有番号")
        else:
            ids = [str(it["Id"]) for it in pick]
            jid = post("/api/cn/scrape-batch", {"ids": ids, "dry_run": True}, timeout=60)["data"]["job_id"]
            j = wait_job(jid, timeout=300)
            check("任务完成", j["status"] == "done", "进度 %s/%s" % (j.get("done"), j.get("total")))
            res = j.get("result") or []
            got = sorted(str(r.get("item_id")) for r in res)
            check("只处理勾选的那几条", got == sorted(ids),
                  "勾选 %s，实际处理 %s" % (ids, got))
            wrote = [r for r in res if r.get("applied")]
            check("dry_run 没有写入任何条目", not wrote, "已写入 %d 条" % len(wrote))
    except Exception as e:
        check("国产传媒按 id 批量", False, str(e))

print("\n" + "=" * 70)
bad = [r for r in results if not r[1]]
print("共 %d 项，通过 %d，失败 %d" % (len(results), len(results) - len(bad), len(bad)))
if bad:
    for n, _, d in bad:
        print("  失败：" + n + "  " + d)
print("=" * 70)
sys.exit(1 if bad else 0)
