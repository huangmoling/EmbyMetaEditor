"""用 CDP 驱动无头 Edge，验证「演员头像」页的媒体库下拉**真的生效**。

为什么要验到渲染层：接口返回 200 不等于界面变了。中间任何一环断掉
（下拉没填选项、onchange 没绑、参数没拼进 URL、后端没透传），
用户看到的都是「选了库但列表纹丝不动」。

验证内容：
  1. #psLib 有选项（全部媒体库 + 真实库名）
  2. 选中某个库后，分页说明里出现该库名（说明前端把库名和总数对上了）
  3. 该库的演员数与「全部媒体库」不同（说明过滤真的到了 Emby）
  4. 切换库后卡片确实换了（不是同一批人）
  5. 没有 console 报错

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
  };
})()"""


def total_of(pager):
    """从分页说明里抠出「共 N 位演员」。"""
    m = re.search(r"共\s*([\d,]+)\s*位演员", pager)
    return int(m.group(1).replace(",", "")) if m else -1


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
        print("  失败：" + n + "  " + d)
    print("=" * 70)
    sys.exit(1 if bad else 0)


if __name__ == "__main__":
    main()
