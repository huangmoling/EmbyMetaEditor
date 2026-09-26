"""用 CDP 驱动无头 Edge，验证「选择头像」弹窗里的候选缩略图**真的渲染出来**，
以及每张图下面那行「宽×高 · 体积」是不是真的填上了。

这是线上 bug「弹窗里搜出来的图全是破图」的界面级回归，补的是
`tools/verify_images.py` 漏掉的**第四个** `imgSrc()` 调用点：

  A. 番号补全的缺失封面   -> `#jbGrid .misscard img`   （verify_images.py）
  B. MetaTube 搜索结果封面 -> `#dvHits .hit img`        （verify_images.py）
  C. gfriends 头像库缩略图 -> `#gfResult img`           （verify_images.py）
  D. 选择头像弹窗的候选图  -> `#pkList .pk-item img`    <-- 本脚本

**这个 bug 用命令行查不出来**：弹窗里的图当时直接写的是 CDN 外链，
`curl` 那个外链是 200、服务端 `/api/img` 也是 200，只有浏览器因为 CSP
`img-src 'self'` 把它拦了。所以判据必须是渲染层的 `naturalWidth > 0`，
外加「每张都走 /api/img」和「没有 CSP 拦截报错」。
（另一个反向坑：`alt` 为空时破图会显示成一片空白，很容易被看成"图没出来但没报错"。）

尺寸 / 体积那行同样只能在浏览器里验：它是**异步**补上去的（服务端要去取原图），
所以顺序不能反 —— 先出图、后补小字。断言必须区分「还没探完（…）」和「探不回来（读不到）」，
否则一个卡住的探测会被当成"刚好没探完"混过去。

跑之前需要：
  1. `EMBYME_AUTH_PASSWORD=test-pass EmbyMetaEditor.exe -port 8097 -open=false`
     （用真实 config.json，演员列表要能连上 Emby）
  2. 无头 Edge 带 `--remote-debugging-port=9333`

只读：只查演员列表 + gfriends 搜索，**不点**弹窗里的图（不会往 Emby 写头像）。
"""
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from verify_javbus_probe import Page, http_json  # noqa: E402
from verify_images import safe_shot, settle_imgs  # noqa: E402
import wbauth  # noqa: E402

APP = "http://127.0.0.1:8097/"
# 固定关键词，不依赖列表里第一个演员叫什么；三上悠亜 在 gfriends 库里必定有候选。
STAR = os.environ.get("VERIFY_STAR", "三上悠亜")

# 候选图下面那行小字的读取。分三档统计，是为了能区分
# 「还没探完」「探了但取不到」「探到了」—— 只数「有几个带 ×」会把卡住的探测放过。
META_JS = """(() => {
  const items = [...document.querySelectorAll('#pkList .pk-item')];
  const meta = items.map((i) => ((i.querySelector('.pk-meta') || {}).textContent || '').trim());
  const dim = (s) => /\\d+×\\d+/.test(s);
  const size = (s) => /\\d+(\\.\\d+)?\\s*(B|KB|MB|GB)/.test(s);
  return {
    items: items.length,
    metaLines: meta.length,
    pending: meta.filter((s) => s === '…').length,
    sized: meta.filter(dim).length,
    withBytes: meta.filter((s) => dim(s) && size(s)).length,
    unreadable: meta.filter((s) => s.indexOf('读不到') >= 0).length,
    sample: meta.slice(0, 4),
  };
})()"""


def main():
    results = []

    def check(name, ok, detail=""):
        # detail 一律 str()：这个脚本里既会传字符串（"18/18"）也会传整个快照 dict，
        # 直接拼字符串会在「断言失败、正需要看快照」的那一刻抛 TypeError 把脚本炸掉。
        results.append((name, ok, detail))
        print(("PASS  " if ok else "FAIL  ") + name +
              (("  | " + str(detail)) if detail else ""))

    tgt = http_json("/json/new?about:blank", method="PUT")
    page = Page(tgt["webSocketDebuggerUrl"])
    for m in ("Runtime.enable", "Log.enable", "Network.enable", "Page.enable"):
        page.send(m)
    page.send("Emulation.setDeviceMetricsOverride", width=1440, height=1000,
              deviceScaleFactor=1, mobile=False)

    # 记下探测请求：尺寸/体积是走 /api/img/info 由服务端代取的，
    # 页面里没有别的接口能拿到这两个数。
    info_calls = []
    orig_keep = page._keep

    def keep(msg):
        if msg.get("method") == "Network.requestWillBeSent":
            u = msg["params"]["request"]["url"]
            if "/api/img/info" in u:
                info_calls.append(u)
        orig_keep(msg)

    page._keep = keep

    print("1) 打开应用并过访问认证")
    page.send("Page.navigate", url=APP)
    wbauth.login_in_browser(page)
    page.send("Page.navigate", url=APP)
    page.wait_for("!!document.querySelector('#lgSkip')", desc="登录页出现")
    page.pump(0.6)
    page.eval("document.querySelector('#lgSkip').click()")
    page.wait_for("!!document.querySelector('#jbStar')", desc="进入主界面")
    page.pump(0.5)

    print("\n2) 切到「演员头像」视图，等演员卡片")
    page.eval("""(() => {
      const b = [...document.querySelectorAll('button')].find(x => x.dataset.view === 'persons');
      if (b) b.click();
      return !!b;
    })()""")
    page.wait_for("getComputedStyle(document.querySelector('#view-persons')).display !== 'none'",
                  desc="演员视图可见")
    page.wait_for("document.querySelectorAll('#psList .pcard').length > 0",
                  timeout=180, desc="演员卡片出现（要能连上 Emby）")
    page.pump(0.5)

    print("\n3) 点第一张卡片的「选图」，弹出挑选弹窗")
    opened = page.eval("""(() => {
      const b = document.querySelector('#psList .pcard button[data-act="pick"]');
      if (b) b.click();
      return !!b;
    })()""")
    check("演员卡片上有「选图」按钮（data-act=pick）", bool(opened))
    if not opened:
        return 1
    page.wait_for("!!document.querySelector('#pkList')", timeout=60, desc="挑选弹窗打开")

    print("\n4) 用固定关键词搜一次（不依赖演员本名能不能搜到）")
    page.eval("""(() => {
      const i = document.querySelector('#pkQ');
      i.value = %s;
      i.dispatchEvent(new Event('input', {bubbles: true}));
      document.querySelector('#pkGo').click();
      return true;
    })()""" % repr(STAR))
    page.wait_for("document.querySelectorAll('#pkList img').length > 0",
                  timeout=180, desc="挑选弹窗出图")
    page.pump(0.5)

    print("\n5) 渲染层判据")
    info = settle_imgs(page, "#pkList .pk-item img")
    print("   分组 %d / <img> %d / 走代理 %d" % (info["cards"], info["imgs"], info["proxied"]))
    print("   加载成功 %d / 失败 %d / 仍在加载 %d"
          % (info["loaded"], info["broken"], info["pending"]))
    for s in info["sample"]:
        print("   样例：%dx%d  %s" % (s["w"], s["h"], s["src"]))

    check("弹窗里有候选图", info["imgs"] > 0, "img=%d" % info["imgs"])
    check("全部走 /api/img 同源代理", info["proxied"] == info["imgs"],
          "proxied=%d/%d" % (info["proxied"], info["imgs"]))
    check("全部加载成功（naturalWidth > 0）", info["loaded"] == info["imgs"],
          "loaded=%d/%d" % (info["loaded"], info["imgs"]))
    check("没有破图", info["broken"] == 0, "broken=%d" % info["broken"])
    check("没有卡住的图", info["pending"] == 0, "pending=%d" % info["pending"])
    check("样例有真实尺寸", all(s["w"] > 0 for s in info["sample"]),
          " / ".join("%dx%d" % (s["w"], s["h"]) for s in info["sample"]))

    print("\n5b) 每张候选图下面的「宽×高 · 体积」")
    page.wait_for("document.querySelectorAll('#pkList .pk-meta').length > 0"
                  " && [...document.querySelectorAll('#pkList .pk-meta')]"
                  ".every(e => e.textContent.trim() !== '…')",
                  timeout=180, desc="尺寸/体积探测返回（要能连上 gfriends CDN）")
    page.pump(0.5)
    meta = page.eval(META_JS)
    print("   候选 %d 张 / 带尺寸 %d / 带体积 %d / 读不到 %d"
          % (meta["metaLines"], meta["sized"], meta["withBytes"], meta["unreadable"]))
    print("   样例：%s" % " | ".join(meta["sample"]))
    check("探测请求确实发出去了（尺寸体积只能由服务端代取）", len(info_calls) >= 1,
          "%d 次" % len(info_calls))
    check("每张候选图都挂了那行小字", meta["metaLines"] == meta["items"] and meta["items"] > 0, meta)
    check("没有停在「还没探完」的状态", meta["pending"] == 0, meta["pending"])
    check("绝大多数候选都探出了像素尺寸", meta["sized"] >= max(1, meta["metaLines"] - 1),
          meta["sample"])
    check("尺寸和体积同时给出（用户就是靠这两个数挑）",
          meta["withBytes"] >= max(1, meta["metaLines"] - 1), meta["sample"])

    safe_shot(page, "15-选择头像弹窗.png")

    print("\n6) CSP / 接口报错检查")
    errs = page.errors()
    csp = [e for e in errs if "Content Security Policy" in e or "img-src" in e]
    img_errs = [e for e in errs if "/api/img" in e]
    for e in csp + img_errs:
        print("   ! " + e)
    check("没有 CSP 拦截图片的报错", not csp, "%d 条" % len(csp))
    check("封面接口无报错", not img_errs, "%d 条" % len(img_errs))

    bad = [r for r in results if not r[1]]
    print("\n" + "=" * 62)
    print("共 %d 项，通过 %d，失败 %d" % (len(results), len(results) - len(bad), len(bad)))
    for n, _, d in bad:
        print("  失败：" + n + "  " + d)
    print("=" * 62)
    return 1 if bad else 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as e:
        print("E2E 失败:", e)
        sys.exit(2)
