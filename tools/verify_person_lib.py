"""用 CDP 驱动无头 Edge，验证「演员头像」页的两个下拉**真的生效**。

为什么要验到渲染层：接口返回 200 不等于界面变了。中间任何一环断掉
（下拉没填选项、onchange 没绑、参数没拼进 URL、后端没透传），
用户看到的都是「选了但列表纹丝不动」。

验证内容：
  1. #psLib 有选项（全部媒体库 + 真实库名）
  2. 选中某个库后，分页说明里出现该库名（说明前端把库名和总数对上了）
  3. 该库的人物数与「全部媒体库」不同（说明过滤真的到了 Emby）
  4. 切换库后卡片确实换了（不是同一批人）
  5. #psType 默认是「演员 + 导演」而不是「全部」——Emby 的人物库里连片商名都算人物，
     默认全量列出来一半是杂物
  6. 切成「仅演员」「仅导演」「全部人物」后总数与卡片都跟着变
  7. 没有 console 报错

跑之前需要：
  1. EmbyMetaEditor.exe -port 8097 -open=false   （用真实 config.json）
  2. 无头 Edge 带 --remote-debugging-port=9333

只读：只查 /Persons 和 /api/libraries，不往 Emby 写任何东西。
"""
import os
import re
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from verify_javbus_probe import OUT, Page, http_json  # noqa: E402
import wbauth  # noqa: E402

APP = "http://127.0.0.1:8097/"
MAX_CANDIDATES = int(os.environ.get("VERIFY_LIB_TRIES", "4"))

# 抓一份页面快照：卡片数、前几个人名、分页说明、空态文案。
SNAP = """(() => {
  const cards = [...document.querySelectorAll('#psList .pcard')];
  const pager = document.querySelector('#psPager');
  const empty = document.querySelector('#psList .empty');
  return {
    cards: cards.length,
    names: cards.slice(0, 10).map(c => c.dataset.name),
    pager: pager ? pager.textContent : '',
    empty: empty ? empty.textContent : '',
    libValue: document.querySelector('#psLib').value,
    libCount: document.querySelector('#psLib').options.length,
    typeValue: document.querySelector('#psType').value,
  };
})()"""


def total_of(pager):
    """从分页说明里抠出「共 N 位人物」。"""
    m = re.search(r"共\s*([\d,]+)\s*位人物", pager)
    return int(m.group(1).replace(",", "")) if m else -1


def set_select(page, sel, value):
    """给下拉赋值并派发 change —— 直接改 value 不会触发 onchange。"""
    return page.eval("""(() => {
      const s = document.querySelector(%s);
      s.value = %s;
      s.dispatchEvent(new Event('change', {bubbles: true}));
      return s.value;
    })()""" % (repr(sel), repr(value)))


def wait_total(page, old, timeout=180):
    """等分页说明里的总数换成别的值（列表刷新完成的信号）。

    不拿「前几个字符」当条件：那种判据在两种状态下都成立，会立刻通过、
    读到还没刷新的旧值。总数变了才是真的刷新了。
    """
    deadline = time.time() + timeout
    while time.time() < deadline:
        page.pump(0.5)
        t = total_of(page.eval("document.querySelector('#psPager').textContent || ''"))
        if t > 0 and t != old:
            return t
    return total_of(page.eval("document.querySelector('#psPager').textContent || ''"))


def safe_shot(page, name):
    """截图是锦上添花，失败不该让整轮验证挂掉。"""
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


def main():
    results = []

    def check(name, ok, detail=""):
        results.append((name, ok, detail))
        print(("PASS  " if ok else "FAIL  ") + name +
              (("  | " + str(detail)) if detail else ""))

    tgt = http_json("/json/new?about:blank", method="PUT")
    page = Page(tgt["webSocketDebuggerUrl"])
    for m in ("Runtime.enable", "Log.enable", "Network.enable", "Page.enable"):
        page.send(m)
    page.send("Emulation.setDeviceMetricsOverride", width=1440, height=1000,
              deviceScaleFactor=1, mobile=False)

    print("1) 打开应用")
    page.send("Page.navigate", url=APP)
    # 先过访问认证（未登录时所有 /api/ 都是 401），再重载让 boot() 带着会话跑
    wbauth.login_in_browser(page)
    page.send("Page.navigate", url=APP)
    page.wait_for("!!document.querySelector('#lgSkip')", desc="登录页出现")
    page.pump(0.6)
    page.eval("document.querySelector('#lgSkip').click()")
    page.wait_for("!!document.querySelector('#jbStar')", desc="进入主界面")
    page.pump(0.5)

    print("\n2) 切到「演员头像」")
    page.eval("""(() => {
      const b = [...document.querySelectorAll('button')].find(x => x.dataset.view === 'persons');
      if (b) b.click();
      return !!b;
    })()""")
    page.wait_for("getComputedStyle(document.querySelector('#view-persons')).display !== 'none'",
                  desc="演员视图可见")
    page.wait_for("document.querySelectorAll('#psList .pcard').length > 0",
                  timeout=180, desc="演员卡片出现")
    page.pump(0.4)

    base = page.eval(SNAP)
    print("   全部媒体库：卡片 %d / 下拉 %d 项" % (base["cards"], base["libCount"]))
    print("   分页说明：%s" % base["pager"].strip())

    check("媒体库下拉有选项", base["libCount"] > 1,
          "%d 项（含「全部媒体库」）" % base["libCount"])
    base_total = total_of(base["pager"])
    check("全局视图能算出演员总数", base_total > 0, "共 %d 位" % base_total)
    check("默认视图有卡片", base["cards"] > 0, "%d 张" % base["cards"])

    # 下拉里的真实库（跳过第 0 项「全部媒体库」）
    opts = page.eval("""[...document.querySelectorAll('#psLib option')]
      .map(o => ({v: o.value, t: o.textContent}))""")
    libs = [o for o in opts if o["v"]]
    print("   可选媒体库：%s" % " / ".join(o["t"] for o in libs))

    picked, scoped, tries = None, None, 0
    for o in libs[:MAX_CANDIDATES]:
        tries += 1
        page.eval("""(() => {
          const s = document.querySelector('#psLib');
          s.value = %s;
          s.dispatchEvent(new Event('change', {bubbles: true}));
          return s.value;
        })()""" % repr(o["v"]))
        try:
            page.wait_for("(document.querySelector('#psPager').textContent || '').includes(%s)"
                          % repr(o["t"]), timeout=180, desc="分页说明带上库名 %s" % o["t"])
        except TimeoutError:
            continue
        snap = page.eval(SNAP)
        n = total_of(snap["pager"])
        print("   尝试「%s」：卡片 %d / %s" % (o["t"], snap["cards"], snap["pager"].strip()))
        if n > 0:
            picked, scoped = o, snap
            break

    if not picked:
        check("找到有演员的媒体库", False,
              "试了 %d 个库都没有演员（可能都没刮演员元数据）" % tries)
        safe_shot(page, "14-演员按库过滤.png")
        return finish(results, page)

    check("分页说明带上所选库名", picked["t"] in scoped["pager"],
          scoped["pager"].strip())
    check("该库演员数与全局不同", total_of(scoped["pager"]) != base_total,
          "%s 共 %d 位 / 全局 %d 位" % (picked["t"], total_of(scoped["pager"]), base_total))
    check("该库演员数小于全局", 0 < total_of(scoped["pager"]) < base_total,
          "%d < %d" % (total_of(scoped["pager"]), base_total))
    check("切换后卡片确实换了", scoped["names"] != base["names"],
          "库内前几位：%s" % "、".join(scoped["names"][:5]))
    check("下拉的当前值已更新", scoped["libValue"] == picked["v"],
          "value=%s" % scoped["libValue"])

    # 切回全部媒体库，确认能还原。
    # 注意别用 pager 的前几个字符当等待条件 —— 「上一页本页」在两种状态下都成立，
    # 会立刻通过、读到还没刷新的旧值。要等的是只在目标状态出现的那段文字。
    page.eval("""(() => {
      const s = document.querySelector('#psLib');
      s.value = '';
      s.dispatchEvent(new Event('change', {bubbles: true}));
      return true;
    })()""")
    page.wait_for("(document.querySelector('#psPager').textContent || '').includes('全部媒体库')",
                  timeout=180, desc="切回全部媒体库")
    page.pump(0.3)
    back = page.eval(SNAP)
    check("能切回全部媒体库", total_of(back["pager"]) == base_total,
          "共 %d 位（原 %d 位）" % (total_of(back["pager"]), base_total))

    # ---- 人物类型（Emby 的 PersonTypes）----
    # 这一节盯的是「默认不能是全部」和「三种取值真的分别生效」。
    # Emby 的人物库把片商名之类也当人物存，全量列出来一半是杂物。
    print("\n3) 人物类型筛选")
    check("默认类型是「演员 + 导演」而不是全部",
          page.eval("document.querySelector('#psType').value") == "Actor,Director",
          page.eval("document.querySelector('#psType').value"))

    both_total = total_of(back["pager"])
    by_type = {}
    for value, label in (("Actor", "仅演员"), ("Director", "仅导演"), ("all", "全部人物")):
        set_select(page, "#psType", value)
        t = wait_total(page, both_total)
        snap = page.eval(SNAP)
        by_type[value] = (t, snap)
        print("   %s（%s）：共 %d 位 / 卡片 %d / 首位 %s"
              % (label, value, t, snap["cards"],
                 snap["names"][0] if snap["names"] else "-"))
        check("切换成「%s」后下拉值已更新" % label, snap["typeValue"] == value, snap["typeValue"])

    actor_t = by_type["Actor"][0]
    director_t = by_type["Director"][0]
    all_t = by_type["all"][0]

    check("「仅演员」比「演员 + 导演」少", 0 < actor_t < both_total,
          "%d < %d" % (actor_t, both_total))
    check("「仅导演」比「演员 + 导演」少", 0 < director_t < both_total,
          "%d < %d" % (director_t, both_total))
    # 默认与「全部人物」的差值取决于这个库里有没有「既非演员也非导演」的人物，
    # 不能写成硬编码的绝对数，只能比大小。
    check("默认不多于「全部人物」", both_total <= all_t, "%d <= %d" % (both_total, all_t))
    if both_total == all_t:
        print("   注意：这个库里没有既非演员也非导演的人物，默认过滤在这台机器上看不出差别")
    check("三档类型给出三个不同总数（过滤作用在集合上，不只是换了参数）",
          len({actor_t, director_t, all_t}) == 3,
          "演员 %d / 导演 %d / 全部 %d" % (actor_t, director_t, all_t))
    # 这里**不能**断言「演员页与导演页的人名不一样」：Emby 里被刮错的条目常常同时挂着
    # Actor 和 Director 两个类型，而它们正好排在最前面（实测这台机器上前 48 条两边
    # 完全一致）。所以人名相同不代表过滤没生效 —— 判据只能是总数。
    if by_type["Actor"][1]["names"] == by_type["Director"][1]["names"]:
        print("   提示：首屏人名两边一致（这些条目同时标了演员和导演），以总数判断过滤是否生效")

    # 还原成默认，免得截图停在「全部人物」上
    set_select(page, "#psType", "Actor,Director")
    wait_total(page, all_t)
    check("能切回默认的「演员 + 导演」",
          total_of(page.eval("document.querySelector('#psPager').textContent || ''")) == both_total,
          "共 %d 位（原 %d 位）" % (total_of(page.eval("document.querySelector('#psPager').textContent || ''")), both_total))

    safe_shot(page, "14-演员按库过滤.png")
    return finish(results, page)


def finish(results, page):
    errs = []
    for e in page.events:
        m = e.get("method")
        if m == "Runtime.exceptionThrown":
            errs.append("未捕获异常: " + str(e["params"].get("exceptionDetails", {}).get("text")))
        elif m == "Log.entryAdded":
            en = e["params"]["entry"]
            if en.get("level") == "error":
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
