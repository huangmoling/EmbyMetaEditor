"""用 CDP 驱动无头 Edge，验证「离线演员资料库」在界面上真的接通了。

为什么非要有这一层：后端单测能证明「文件读得出」「排在资料源最前面」「类型上没有
图片字段」，但它证明不了「用户在设置页勾了开关、填了路径、点了保存之后，重开设置页
开关还是勾着的、状态行写着载入了几条、抽屉里的源列表里它排在最前面」。这条链上
任何一环断掉，用户看到的都是「设置了但没生效」——而且**往往只在保存后重开一次才暴露**。
所以这里必须「保存 → 重新加载页面 → 再读」，不能读内存里的值。

验证内容：
  1. 设置页有离线资料库卡片：#stOffOn / #stOffPath / #stOffStat
  2. 填路径 + 勾开关 + 保存 → 状态行显示「已载入 N 条…」
  3. **重新加载页面**后开关仍勾着、路径仍在 → 说明真的落进了 config.json
  4. /api/profile/sources 里 OfflineDB 排**第一个**（第一优先级），entries == N
  5. 只勾离线源抓一个「只有别名在库里」的名字 → 命中，且来源标注是「离线资料库」
     （别名命中也要算命中 —— 用户确认过的旧艺名就是靠这条链串起来的）
  6. 关掉开关 → 该源从资料源列表里**消失**（而不是留在列表里永远返回空）
  7. 路径指向不存在的文件 → 状态行原样报出原因（这是个加密导出的库，读不到的原因值得看）
  8. 保存时只带这两个键，别处的设置不能被清空（后端「键出现过才覆盖」）
  9. 收尾把开关/路径还原成跑之前的值
 10. 没有 console 报错

跑之前需要：
  1. EmbyMetaEditor.exe -port 8097 -open=false
     （**建议加 -dir .tmp-test-home**：这个脚本会开开关、填路径，别在真实 config 上试）
  2. 无头 Edge 带 --remote-debugging-port=9333

只读写本项目自己的配置文件和 /api，不碰 Emby，也不碰原始 .db。
"""
import csv
import io
import json
import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from verify_javbus_probe import OUT, Page, http_json  # noqa: E402
import wbauth  # noqa: E402

APP = "http://127.0.0.1:8097/"
HERE = os.path.dirname(os.path.abspath(__file__))

# 列名与列序照抄**真实导出文件**（tools/sqlcipher_dump.py 导出的 41 列）。
# 夹具如果自己发明一套列名，「列名对不上」这类问题在这个脚本里就永远暴露不出来 ——
# 而它恰恰是这条链路上最容易犯的错（真实的坑：文件带 BOM，第一列会变成 "\ufeffid"）。
CSV_COLUMNS = [
    "id", "name_original", "name_ja", "name_zh_cn", "name_romanized", "kana",
    "nationality", "birthplace", "birthdate", "blood_type",
    "height_cm", "bust_cm", "waist_cm", "hip_cm", "cup", "shoe_cm",
    "body_type", "occupation", "agency", "debut_date", "debut_year",
    "retirement_date", "retirement_year", "career_status", "hobbies", "specialties",
    "biography_original", "biography_zh_cn", "profile_image_url", "official_site",
    "favorite_count", "aliases_json", "nicknames_json", "tags_json",
    "social_links_json", "awards_json", "timeline_json", "public_roles_json",
    "data_conflicts_json", "created_at", "updated_at",
]

# 夹具里放四个人。第一个的关键点是「本名查不到、只有别名查得到」——
# 这正对应真实用法：Emby 里的人物名是中文，而库里记的是旧艺名，
# 能不能对上全靠别名索引 + 别名记忆。
# 第四个人**故意和第一个重名**：真实库里 15% 的名字都对应多条记录，
# 这条用来验「一个写法命中多条时会不会提醒」。
FIXTURE_ROWS = [
    {
        "id": "1",
        "name_original": "离线検証 ひとり",
        "name_ja": "离线検証 ひとり",
        "name_romanized": "Offline Hitori",
        "biography_zh_cn": "离线库夹具简介：第一位。",
        "birthdate": "1991/04/19",
        "birthplace": "東京都",
        "blood_type": "A",
        "height_cm": "163.0",
        "debut_date": "2013-03-09",
        "retirement_date": "2018-12-01",
        "agency": "夹具事务所",
        "aliases_json": '["离线别名検証、夹具旧名"]',
    },
    {
        "id": "2",
        "name_original": "离线検証 ふたり",
        "name_ja": "离线検証 ふたり",
        "biography_zh_cn": "离线库夹具简介：第二位。",
        "birthdate": "1993-07-02",
        "birthplace": "大阪府",
    },
    # 整条只有名字：没有内容就不该被当成命中（否则「命中来源」里会多一个
    # 什么都没提供的源，用户还得猜它为什么在那儿）。
    {"id": "3", "name_original": "空条目検証", "name_ja": "空条目検証"},
    {
        "id": "4",
        "name_original": "离线検証 ひとり",
        "name_ja": "离线検証 ひとり",
        "biography_zh_cn": "同名但其实是另一个人。",
        "birthdate": "1975-11-02",
    },
]


def fixture_path():
    return os.path.join(HERE, "..", ".tmp-offlib-fixture.csv")


def write_fixture():
    """按真实导出文件的形态写夹具（含 UTF-8 BOM、41 列）。"""
    p = os.path.abspath(fixture_path())
    with io.open(p, "w", encoding="utf-8-sig", newline="") as f:
        w = csv.writer(f)
        w.writerow(CSV_COLUMNS)
        for row in FIXTURE_ROWS:
            w.writerow([row.get(c, "") for c in CSV_COLUMNS])
    return p


def api(page, path, body=None):
    """在页面里打一次同源接口（借浏览器的会话 Cookie），拿回信封里的 data。

    注意**必须取 .data**：本项目所有 /api 都回 `{ok:true,data:…}`（见 app.js 的 api()）。
    这里若直接用整个信封，`d.offline_db` / `d.sources` 全是 undefined —— 断言会一败涂地，
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


def set_form(page, path, enabled):
    """把设置页上的开关与路径写进表单（不保存）。"""
    return page.eval("""(() => {
      const on = document.querySelector('#stOffOn');
      const p = document.querySelector('#stOffPath');
      on.checked = %s;
      p.value = %s;
      p.dispatchEvent(new Event('input', {bubbles: true}));
      return {on: on.checked, path: p.value};
    })()""" % ("true" if enabled else "false", json.dumps(path)))


def save_settings(page):
    page.eval("document.querySelector('#stSave').click()")
    # toast 是保存完成的信号；比 sleep 稳，且能顺带发现「按钮点了没反应」。
    page.wait_for("!!document.querySelector('.toast')", timeout=30, desc="保存 toast")


def stat_text(page):
    return (page.eval("(document.querySelector('#stOffStat') || {}).textContent") or "").strip()


def wait_stat(page, want, timeout=60):
    """等状态行出现某段文字（refreshOfflineStat 是异步的）。"""
    deadline = time.time() + timeout
    last = ""
    while time.time() < deadline:
        page.pump(0.4)
        last = stat_text(page)
        if want in last:
            return last
    return last


def safe_shot(page, name):
    try:
        page.send("Emulation.setDeviceMetricsOverride", width=1440, height=1000,
                  deviceScaleFactor=1, mobile=False)
        r = page.send("Page.captureScreenshot", format="png")
        import base64
        path = os.path.join(OUT, name)
        with open(path, "wb") as f:
            f.write(base64.b64decode(r["data"]))
        print("   screenshot -> %s (%d bytes)" % (os.path.abspath(path), os.path.getsize(path)))
    except Exception as e:
        print("   screenshot 跳过：%s" % e)


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
    page.pump(0.5)


def main():
    results = []

    def check(name, ok, detail=""):
        results.append((name, ok, detail))
        print(("PASS  " if ok else "FAIL  ") + name +
              (("  | " + str(detail)) if detail else ""))

    fp = write_fixture()
    print("夹具已写入 %s（%d 条）" % (fp, len(FIXTURE_ROWS)))

    tgt = http_json("/json/new?about:blank", method="PUT")
    page = Page(tgt["webSocketDebuggerUrl"])
    for m in ("Runtime.enable", "Log.enable", "Network.enable", "Page.enable"):
        page.send(m)
    page.send("Emulation.setDeviceMetricsOverride", width=1440, height=1000,
              deviceScaleFactor=1, mobile=False)

    print("1) 打开应用并进设置")
    enter_main(page)
    goto_settings(page)

    # 先记下跑之前的值，收尾要还原 —— 这个脚本会在配置里写东西，
    # 万一有人直接对着真实 config.json 跑，也不能留下痕迹。
    cfg0 = api(page, "/api/config")
    orig = {"offline_db_enabled": bool(cfg0.get("offline_db_enabled")),
            "offline_db_path": cfg0.get("offline_db_path") or ""}
    orig_url = cfg0.get("emby_url") or ""
    print("   原值：enabled=%s path=%r" % (orig["offline_db_enabled"], orig["offline_db_path"]))

    check("设置页有离线资料库开关/路径/状态行",
          page.eval("!!document.querySelector('#stOffOn') && "
                    "!!document.querySelector('#stOffPath') && "
                    "!!document.querySelector('#stOffStat')"))

    print("\n2) 填路径 + 勾开关 + 保存")
    set_form(page, fp, True)
    save_settings(page)
    txt = wait_stat(page, "已载入")
    print("   状态行：%s" % txt)
    check("保存后状态行报出载入条数", "已载入" in txt, txt)
    check("条数与夹具一致", ("%d 条" % len(FIXTURE_ROWS)) in txt, txt)
    check("状态行说明了它排在最前面", "前面" in txt, txt)

    print("\n3) 重新加载页面，确认真的落盘了（不是只活在内存里）")
    page.send("Page.navigate", url=APP)
    page.wait_for("!!document.querySelector('#lgSkip')", desc="重新加载完成")
    page.pump(0.6)
    page.eval("document.querySelector('#lgSkip').click()")
    page.wait_for("!!document.querySelector('#jbStar')", desc="进入主界面")
    page.pump(0.4)
    goto_settings(page)
    page.pump(0.6)
    back = page.eval("""(() => ({
      on: document.querySelector('#stOffOn').checked,
      path: document.querySelector('#stOffPath').value,
      stat: document.querySelector('#stOffStat').textContent,
    }))()""")
    print("   重载后：on=%s path=%r" % (back["on"], back["path"]))
    check("重载后开关仍是开的", back["on"] is True)
    check("重载后路径还在", back["path"] == fp, back["path"])
    check("重载后状态行仍能报出条数", "已载入" in (back["stat"] or ""), (back["stat"] or "").strip())

    print("\n4) 资料源列表里它必须排第一")
    srcs = api(page, "/api/profile/sources")
    keys = [s["key"] for s in (srcs.get("sources") or [])]
    off = srcs.get("offline_db") or {}
    print("   资料源顺序：%s" % " > ".join(keys))
    print("   offline_db 状态：%s" % json.dumps(off, ensure_ascii=False))
    check("离线源在列表里", "OfflineDB" in keys, keys[:3])
    check("离线源排在第一位（第一优先级）", keys and keys[0] == "OfflineDB", keys[:3])
    check("offline_db.enabled 为真", off.get("enabled") is True, off)
    check("offline_db.entries 与夹具一致", off.get("entries") == len(FIXTURE_ROWS), off.get("entries"))
    check("offline_db 没有报错", not off.get("error"), off.get("error"))
    note = next((s.get("note") for s in (srcs.get("sources") or []) if s["key"] == "OfflineDB"), "")
    check("源列表里带了说明文案", bool(note), note[:40])

    # 截图挑在「开着、已载入、状态行有数」的这一刻 —— 关掉之后再拍只剩一个「未启用。」，
    # 作为 README 的配图说明不了任何事。拍之前还有两件必做的事：
    #  1. 侧栏那行「在线 · <user>」显示的是当前 Emby 用户名，公开仓库里不能出现 → 藏掉；
    #  2. 这张卡片在设置页很靠下，不滚过去拍的是一屏跟离线库无关的内容。
    page.eval("""(() => {
      const s = document.querySelector('#sbState');
      if (s && s.parentElement) s.parentElement.style.visibility = 'hidden';
      const p = document.querySelector('#stOffPath');
      if (p) p.scrollIntoView({block: 'center'});
      return true;
    })()""")
    try:  # toast 浮在右上角、会压住卡片标题，等它自己消失
        page.wait_for("!document.querySelector('.toast')", timeout=10, desc="toast 消失")
    except TimeoutError:
        pass
    page.pump(0.3)
    safe_shot(page, "17-离线资料库设置.png")

    print("\n5) 只用离线源抓一个「只有别名在库里」的名字")
    # "离线别名検証" 只是第 1 条里的别名，不是本名 —— 走的正是别名索引那条路。
    res = api(page, "/api/profile/preview",
              {"name": "离线别名検証", "person_id": "", "sources": ["OfflineDB"]})
    data = res or {}
    labels = data.get("sources") or []
    fields = {f.get("key"): f for f in (data.get("fields") or [])}
    ov = (fields.get("overview") or {}).get("value") or ""
    print("   sources=%s" % labels)
    print("   简介=%r 出生地=%r 日期=%r" % (ov, (fields.get("production_locations") or {}).get("value"),
                                        (fields.get("premiere_date") or {}).get("value")))
    check("别名命中：抓到了资料", bool(labels), labels)
    check("来源标注是「离线资料库」", labels and labels[0] == "离线资料库", labels)
    # 简介是**结构化版式**，不是源站那段自由文本：这一点拿真实数据核对过 ——
    # 原版「Emby演员扩展器」写进人物 Overview 的就是「罗马音: …<br/>出生日期: …」
    # 这种一行一字段的样子，没有散文段落。所以这里断言的是夹具里的**结构化字段**
    # 有没有落进简介，而不是夹具里那句 Summary。
    check("简介里的结构化行来自夹具",
          "血型：A" in ov and "事务所：夹具事务所" in ov, ov)
    # 退役日期是我们为这个资料库新加进版式的一行：这些库里退隐的演员不少，
    # 只有出道日期的话读起来像是还在活跃。
    check("退役日期进了简介版式", "退役：2018-12-01" in ov, ov)
    check("斜杠日期被收敛成 YYYY-MM-DD",
          (fields.get("premiere_date") or {}).get("value") == "1991-04-19",
          (fields.get("premiere_date") or {}).get("value"))
    check("出生地来自夹具", (fields.get("production_locations") or {}).get("value") == "東京都",
          (fields.get("production_locations") or {}).get("value"))

    print("\n6) 整条空白的条目不该被当成命中")
    res2 = api(page, "/api/profile/preview",
               {"name": "空条目検証", "person_id": "", "sources": ["OfflineDB"]})
    d2 = res2 or {}
    check("空条目不产生 facts", not (d2.get("facts") or []), len(d2.get("facts") or []))

    print("\n6.5) 一个写法对应多条记录时必须提醒（真实库里 15% 的名字是这样）")
    # 夹具里第 1 条与第 4 条同名。悄悄挑一条的后果是把另一个人的出生日期
    # 写进用户的 Emby，而界面上看起来毫无异常 —— 所以这里要的是**有告警**。
    res_dup = api(page, "/api/profile/preview",
                  {"name": "离线検証 ひとり", "person_id": "", "sources": ["OfflineDB"]})
    d_dup = res_dup or {}
    warns = d_dup.get("warnings") or []
    print("   warnings=%s" % warns)
    check("重名时给出了告警", any("条记录" in w for w in warns), warns)
    check("告警里点出了另一条记录的 id", any("另有 4" in w for w in warns), warns)

    print("\n7) 路径指向不存在的文件时，状态行要原样报出原因")
    set_form(page, os.path.join(HERE, "..", ".tmp-offlib-nope.csv"), True)
    save_settings(page)
    txt = wait_stat(page, "读不到")
    print("   状态行：%s" % txt)
    check("报出了「读不到」", "读不到" in txt, txt)
    check("原因里提到导出脚本", "sqlcipher_dump" in txt, txt[:120])

    print("\n8) 关掉开关 → 该源从列表里消失")
    set_form(page, fp, False)
    save_settings(page)
    page.pump(0.6)
    srcs_off = api(page, "/api/profile/sources")
    keys_off = [s["key"] for s in (srcs_off.get("sources") or [])]
    off_off = srcs_off.get("offline_db") or {}
    check("关掉后离线源不在列表里", "OfflineDB" not in keys_off, keys_off[:3])
    check("关掉后 offline_db.enabled 为假", off_off.get("enabled") is False, off_off)
    check("关掉后状态行回到「未启用」", wait_stat(page, "未启用").startswith("未启用"), stat_text(page))

    print("\n9) 只带这两个键保存，别处的设置不能被清掉")
    # 这条守的是后端「请求里出现过这个键才覆盖」：登录页的高级配置只发一部分键，
    # 曾经的写法是直接赋值，于是每次从登录页连接都会把布尔开关静默关掉。
    api(page, "/api/config", {"offline_db_enabled": False, "offline_db_path": fp})
    cfg1 = api(page, "/api/config")
    check("emby_url 没被清空", (cfg1.get("emby_url") or "") == orig_url,
          "%r -> %r" % (orig_url, cfg1.get("emby_url")))
    check("只带两个键也能写进路径", (cfg1.get("offline_db_path") or "") == fp,
          cfg1.get("offline_db_path"))

    # 收尾还原
    print("\n10) 还原跑之前的配置")
    api(page, "/api/config", orig)
    cfg2 = api(page, "/api/config")
    check("开关已还原", bool(cfg2.get("offline_db_enabled")) == orig["offline_db_enabled"],
          cfg2.get("offline_db_enabled"))
    check("路径已还原", (cfg2.get("offline_db_path") or "") == orig["offline_db_path"],
          cfg2.get("offline_db_path"))

    return finish(results, page)


def finish(results, page):
    errs = []
    for e in page.events:
        m = e.get("method")
        if m == "Runtime.exceptionThrown":
            errs.append("未捕获异常: " + str(e["params"].get("exceptionDetails", {}).get("text")))
        elif m == "Log.entryAdded":
            en = e["params"]["entry"]
            # 夹具故意指向一个不存在的文件，服务端会回 4xx/5xx；
            # 那是**被测行为**，不是脚本的错，别算进 console 报错里。
            if en.get("level") == "error" and "读不到导出文件" not in str(en.get("text")):
                errs.append("console.error: " + str(en.get("text"))[:120])
        elif m == "Runtime.consoleAPICalled" and e["params"].get("type") == "error":
            errs.append("console.error: " + str(e["params"].get("args"))[:120])
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
