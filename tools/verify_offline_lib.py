"""用 CDP 驱动无头 Edge，验证「离线演员资料库」**内嵌进程序**之后真的接通了。

为什么非要有这一层：后端单测能证明「内嵌数据解析得出来」「排在资料源最前面」
「类型上没有图片字段」，但它证明不了**跑起来的这个 exe 里确实带着那份数据**，
也证明不了界面这一层已经把那张作废的设置卡片收干净了。v1.10.0 把离线库从
「用户填路径」改成 `//go:embed` 之后，这条链上最危险的失效形态恰好是**静默的**：
`data/` 没被跟踪、或者 `.dockerignore` 把它排除了 → 产物里没有数据 → 界面一切正常，
只是永远查不到人。所以这里拿 `data/actresses_export.csv` 里的**真实记录**做一次
端到端命中，而不是喂一份自造夹具 —— 夹具能过、真数据不行，正是要防的那种情况。

验证内容：
  1. 设置页**没有**离线资料库卡片（开关 / 路径 / 状态行都不该在）—— 它已内嵌、不可配
  2. 人物归并已整页下线：侧栏没有入口、页面不在、`POST /api/persons/merge` 回 404
  3. /api/profile/sources 里 OfflineDB 排**第一个**（第一优先级），且不再下发 offline_db 状态
  4. 拿真实记录的本名查 → 命中，来源标注「离线资料库」，简介里带出结构化字段
  5. 一个**真实存在**的重名写法 → 面板上给出「对应多条记录」的告警
  6. 库里这个人的其它写法出现在结果的别名里（与别名记忆联动的「往外给」那一侧）
  7. 没有 console 报错 —— 两类预期报错例外、会打印但不算失败：脚本故意打的 404
     （验「归并接口真的没了」），以及**没配 Emby** 时 /api/libraries、/api/stats 的 502

跑之前需要：
  1. EmbyMetaEditor.exe -port 8097 -open=false
  2. 无头 Edge 带 --remote-debugging-port=9333

⚠️ 请在 `-dir .tmp-test-home` 的实例上跑：`/api/profile/preview` 会把这次解析出的
别名**落进别名记忆**（cache/actor_aliases.json）—— 那是「人工采用」的正常行为，
但会往真实数据里塞进探针名字。收尾不还原这一点，所以别对着真实 config 跑。

只读写本项目自己的接口，不碰 Emby，也不碰原始 .db。
"""
import collections
import csv
import io
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from verify_javbus_probe import Page, http_json  # noqa: E402
import wbauth  # noqa: E402

APP = "http://127.0.0.1:8097/"
HERE = os.path.dirname(os.path.abspath(__file__))
DATA = os.path.abspath(os.path.join(HERE, "..", "data", "actresses_export.csv"))

# 判「这条记录除了名字之外有没有内容」——和 offlinelib.go 的 hasContent() 同一个口径。
# 空壳记录按设计就是查不到的（否则「命中来源」里会多一个什么都没提供的源）。
CONTENT_COLUMNS = (
    "biography_zh_cn", "biography_original", "birthdate", "birthplace",
    "height_cm", "bust_cm", "agency", "retirement_date", "cup", "blood_type",
)


def load_rows():
    """读仓库里那份**真正被内嵌**的 CSV。

    用 utf-8-sig 打开：这份文件带 UTF-8 BOM（导出脚本就是这么写的），
    BOM 不剥掉的话第一列的列名会变成 "\\ufeffid"，所有字段都读成空。
    """
    with io.open(DATA, encoding="utf-8-sig", newline="") as f:
        return list(csv.DictReader(f))


def has_content(row):
    return any((row.get(c) or "").strip() for c in CONTENT_COLUMNS)


def pick_probes(rows):
    """挑两条真实探针：一条本名在全库里唯一的，一条真的重名的。

    都在真实数据上挑，而不是写死名字 —— 写死的话换一份导出就失效，
    而「脚本失效」往往表现为「测试莫名其妙红了」，没人会去更新它。
    """
    cnt = collections.Counter((r.get("name_original") or "").strip() for r in rows)
    uniq = next((r for r in rows
                 if cnt[(r.get("name_original") or "").strip()] == 1 and has_content(r)), None)
    dup = None
    for name, _n in cnt.most_common(80):  # most_common 是按次数倒序，遇到 1 次就可以停
        if cnt[name] < 2:
            break
        first = next(r for r in rows if (r.get("name_original") or "").strip() == name)
        if has_content(first):
            dup = first
            break
    return uniq, dup


def cm(s):
    """把 `163.0` 收敛成 `163`、保留 `85.5` —— 与 offlinelib.go 的 offlineCM() 同一口径。

    这份导出把身高/三围写成浮点，而简介版式里要的是 `163 cm`。
    别用 rstrip("0") 图省事：`160` 会被削成 `16`。
    """
    s = (s or "").strip()
    if not s:
        return ""
    try:
        v = float(s)
    except ValueError:
        return s
    return str(int(v)) if v == int(v) else repr(v)


def api(page, path, body=None):
    """在页面里打一次同源接口（借浏览器的会话 Cookie），拿回信封里的 data。

    注意**必须取 .data**：本项目所有 /api 都回 `{ok:true,data:…}`（见 app.js 的 api()）。
    这里若直接用整个信封，`d.sources` 全是 undefined —— 断言会一败涂地，
    而且看起来像是「后端没提供」，其实是脚本读错了层。第一版就踩了这个坑。
    """
    if body is None:
        js = ("(async () => { const r = await fetch(%s); const j = await r.json(); "
              "return j && j.ok ? j.data : j; })()" % json.dumps(path))
    else:
        js = ("(async () => { const r = await fetch(%s, {method: 'POST', "
              "headers: {'Content-Type': 'application/json'}, body: %s}); "
              "const j = await r.json(); return j && j.ok ? j.data : j; })()"
              % (json.dumps(path), json.dumps(json.dumps(body))))
    return page.eval(js, await_promise=True)


def status_of(page, path, method="POST", body=None):
    """只要 HTTP 状态码（用来断言「这条路由真的没了」）。"""
    js = ("(async () => { const r = await fetch(%s, {method: %s, "
          "headers: {'Content-Type': 'application/json'}, body: %s}); return r.status; })()"
          % (json.dumps(path), json.dumps(method), json.dumps(json.dumps(body or {}))))
    return page.eval(js, await_promise=True)


def enter_main(page):
    """过访问认证 + 跳过登录，进到主界面。"""
    page.send("Page.navigate", url=APP)
    wbauth.login_in_browser(page)
    page.send("Page.navigate", url=APP)
    page.wait_for("!!document.querySelector('#lgSkip')", desc="登录页出现")
    page.pump(0.6)
    page.eval("document.querySelector('#lgSkip').click()")
    page.wait_for("!!document.querySelector('#jbStar')", desc="进入主界面")
    page.pump(0.5)


def goto_settings(page):
    page.eval("""(() => {
      const b = [...document.querySelectorAll('button')].find(x => x.dataset.view === 'settings');
      if (b) b.click();
      return !!b;
    })()""")
    page.wait_for("getComputedStyle(document.querySelector('#view-settings')).display !== 'none'",
                  desc="设置视图可见")
    page.pump(0.4)


def main():
    results = []

    def check(name, ok, detail=""):
        results.append((name, ok, detail))
        print(("PASS  " if ok else "FAIL  ") + name +
              (("  | " + str(detail)) if detail else ""))

    rows = load_rows()
    uniq, dup = pick_probes(rows)
    if not uniq or not dup:
        print("从 %s 里挑不出探针（唯一名=%s 重名=%s）—— 这份数据不对？"
              % (DATA, bool(uniq), bool(dup)))
        return 2
    uniq_name = (uniq.get("name_original") or "").strip()
    dup_name = (dup.get("name_original") or "").strip()
    dup_times = sum(1 for r in rows if (r.get("name_original") or "").strip() == dup_name)
    print("内嵌数据 %s：%d 条" % (DATA, len(rows)))
    print("探针：唯一名 %r（id %s）/ 重名 %r（%d 次，id %s）"
          % (uniq_name, uniq["id"], dup_name, dup_times, dup["id"]))

    tgt = http_json("/json/new?about:blank", method="PUT")
    page = Page(tgt["webSocketDebuggerUrl"])
    for m in ("Runtime.enable", "Log.enable", "Network.enable", "Page.enable"):
        page.send(m)
    page.send("Emulation.setDeviceMetricsOverride", width=1440, height=1000,
              deviceScaleFactor=1, mobile=False)

    print("\n1) 打开应用并进设置")
    enter_main(page)
    goto_settings(page)

    print("\n2) 设置页不该再有离线资料库那张卡片（数据已内嵌，没有东西可配）")
    present = page.eval("""(() => ({
      on: !!document.querySelector('#stOffOn'),
      path: !!document.querySelector('#stOffPath'),
      stat: !!document.querySelector('#stOffStat'),
    }))()""")
    check("设置页没有启用开关 / 路径框 / 状态行",
          not (present["on"] or present["path"] or present["stat"]), present)
    st_txt = page.eval("(document.querySelector('#view-settings') || {}).innerText || ''") or ""
    check("设置页文案里没有「离线演员资料库」", "离线演员资料库" not in st_txt)
    # 顺带确认设置页本身还在（否则上面两条只是因为「页面没渲染」而假装通过）
    check("设置页本身渲染出来了", len(st_txt) > 200, len(st_txt))

    print("\n3) 人物归并已整页下线")
    nav = page.eval("""(() => ({
      btn: !!document.querySelector('[data-view=merge]'),
      view: !!document.querySelector('#view-merge'),
    }))()""")
    check("侧栏没有「人物归并」入口", not nav["btn"], nav)
    check("页面里没有 #view-merge", not nav["view"], nav)
    st = status_of(page, "/api/persons/merge", body={"keep_id": "1", "drop_ids": ["2"]})
    print("   POST /api/persons/merge -> HTTP %s" % st)
    check("归并接口已不存在（404）", st == 404, st)

    print("\n4) 资料源列表里它必须排第一，且不再下发 offline_db 状态")
    srcs = api(page, "/api/profile/sources")
    keys = [s["key"] for s in (srcs.get("sources") or [])]
    print("   资料源顺序：%s" % " > ".join(keys))
    check("离线源在列表里", "OfflineDB" in keys, keys[:3])
    check("离线源排在第一位（第一优先级）", keys and keys[0] == "OfflineDB", keys[:3])
    # 状态那份载荷是给那张卡片用的，卡片没了就该一起消失 —— 留着它等于
    # 「界面上看不到、但接口还承诺有一个可配置的离线库」，下次接手的人会被它带偏。
    check("不再下发 offline_db 状态字段", "offline_db" not in srcs, sorted(srcs.keys()))
    note = next((s.get("note") for s in (srcs.get("sources") or []) if s["key"] == "OfflineDB"), "")
    print("   源说明：%s" % note)
    check("源列表里带了说明文案", bool(note), note[:40])

    print("\n5) 拿内嵌数据里的**真实记录**端到端查一次")
    res = api(page, "/api/profile/preview",
              {"name": uniq_name, "person_id": "", "sources": ["OfflineDB"]})
    data = res or {}
    labels = data.get("sources") or []
    fields = {f.get("key"): f for f in (data.get("fields") or [])}
    ov = (fields.get("overview") or {}).get("value") or ""
    print("   sources=%s" % labels)
    print("   简介=%r" % ov[:160])
    check("真实记录命中，抓到了资料", bool(labels), labels)
    check("来源标注是「离线资料库」", bool(labels) and labels[0] == "离线资料库", labels)
    # 简介是**结构化版式**（一行一个字段），不是源站那段自由散文 ——
    # 用真实数据里的出生地/身高来核对，比核对夹具更硬：这条链只要 BOM 没剥、
    # 列名映射错一位、或者数据压根没物进产物，这里立刻就是空的。
    place = (uniq.get("birthplace") or "").strip()
    height = cm(uniq.get("height_cm"))
    if place:
        check("简介里的出生地与内嵌数据一致（%s）" % place, "出生地：" + place in ov, ov[:160])
    else:
        check("该记录没有出生地，跳过", True, "")
    if height:
        check("简介里的身高与内嵌数据一致（%s cm）" % height,
              "身高：" + height + " cm" in ov, ov[:160])
    # 别名（与别名记忆联动的「往外给」那一侧）：库里这个人的各种写法要暴露出去，
    # 采用 / 同步时才会落进 cache/actor_aliases.json，之后所有源都能共享。
    aliases = data.get("aliases") or []
    print("   aliases=%s" % aliases[:6])
    check("命中的记录把它的各种写法作为别名吐出来了", bool(aliases), aliases[:6])

    print("\n6) 一个真实的重名写法必须给出告警")
    # 内嵌库里这种写法很常见（name_original / kana 都有百分之十几的键撞车）。
    # 悄悄挑一条的后果是把另一个人的出生日期写进用户的 Emby，而界面上毫无异常。
    res_dup = api(page, "/api/profile/preview",
                  {"name": dup_name, "person_id": "", "sources": ["OfflineDB"]})
    d_dup = res_dup or {}
    warns = d_dup.get("warnings") or []
    print("   warnings=%s" % warns)
    check("重名时给出了告警", any("条记录" in w for w in warns), warns)

    print("\n7) 界面这一层也确认它不再是「可配置的源」")
    # 设置页拿不到那张卡片，但侧栏别的视图也不能冒出「离线资料库」的开关 ——
    # 一个能改却不生效的开关比没有开关更糟。
    all_switches = page.eval("""(() => {
      const ids = [...document.querySelectorAll('input[type=checkbox]')].map(x => x.id);
      return ids;
    })()""")
    check("页面上没有任何离线库开关残留",
          not any(("Off" in (i or "")) for i in all_switches),
          [i for i in all_switches if "Off" in (i or "")])

    return finish(results, page)


def finish(results, page):
    # 会被「合理地」忽略掉的报错，两类，都不算缺陷：
    #   1. 脚本故意打了一条不存在的路由（验「归并接口真的没了」）→ 404 是**被测行为**。
    #   2. 这个实例**没配 Emby**（离线库这条链根本不需要连 Emby），可主界面一进来就会
    #      打 /api/libraries 与 /api/stats —— 后端「未配置 Emby 地址」直接回 502。
    #      和 verify_javbus_probe.py 里「Emby 指向死地址」的预期清单是同一个口径。
    # 但**要把它打印出来**：静默吞掉风险太大，将来真出问题会被这条断言放过去。
    EXPECTED = ("/api/libraries", "/api/stats")
    errs, expected = [], []
    for e in page.events:
        m = e.get("method")
        if m == "Runtime.exceptionThrown":
            errs.append("未捕获异常: " + str(e["params"].get("exceptionDetails", {}).get("text")))
        elif m == "Log.entryAdded":
            en = e["params"]["entry"]
            if en.get("level") != "error":
                continue
            text = str(en.get("text"))
            url = str(en.get("url") or "")
            if "404" in text or any(k in url or k in text for k in EXPECTED):
                expected.append("[预期] " + text[:100] + (" <- " + url if url else ""))
                continue
            errs.append("console.error: " + text[:120] + (" <- " + url if url else ""))
        elif m == "Runtime.consoleAPICalled" and e["params"].get("type") == "error":
            errs.append("console.error: " + str(e["params"].get("args"))[:120])
    if expected:
        print("   以下报错是预期内的（没配 Emby / 故意打不存在的路由），忽略：")
        for line in expected:
            print("   ~ " + line)
    results.append(("没有 console 报错", not errs, "；".join(errs[:3])))
    print(("PASS  " if not errs else "FAIL  ") + "没有 console 报错" +
          ("  | " + "；".join(errs[:3]) if errs else ""))

    print("\n" + "=" * 70)
    bad = [r for r in results if not r[1]]
    print("共 %d 项，通过 %d，失败 %d" % (len(results), len(results) - len(bad), len(bad)))
    for n, _, d in bad:
        print("  失败：" + n + "  " + str(d))
    print("=" * 70)
    sys.exit(1 if bad else 0)


if __name__ == "__main__":
    main()
