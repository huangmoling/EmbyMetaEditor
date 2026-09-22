"""对真实 Emby / MetaTube / gfriends / javbus 做一轮只读冒烟验证。

只调只读接口，不会往用户的 Emby 里写任何东西。
用法：python tools/smoke_real.py [base_url]
"""
import json
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

BASE = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:8097"


def get(path, timeout=90):
    with urllib.request.urlopen(BASE + path, timeout=timeout) as r:
        return json.loads(r.read().decode())


def fetch_raw(path, timeout=60):
    """取原始响应，返回 (状态码, Content-Type, 字节)。不解析 JSON。"""
    try:
        with urllib.request.urlopen(BASE + path, timeout=timeout) as r:
            return r.status, r.headers.get("Content-Type", ""), r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.headers.get("Content-Type", ""), e.read()


def q(s):
    return urllib.parse.quote(s, safe="")


def post(path, payload, timeout=30):
    req = urllib.request.Request(
        BASE + path, method="POST",
        data=json.dumps(payload).encode(),
        headers={"Content-Type": "application/json"})
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
    st = get("/api/stats?size=false", timeout=120)["data"]
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

print("\n" + "=" * 70)
bad = [r for r in results if not r[1]]
print("共 %d 项，通过 %d，失败 %d" % (len(results), len(results) - len(bad), len(bad)))
if bad:
    for n, _, d in bad:
        print("  失败：" + n + "  " + d)
print("=" * 70)
sys.exit(1 if bad else 0)
