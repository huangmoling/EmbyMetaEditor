"""用 CDP 驱动无头 Edge，验证「选择头像」弹窗里的候选缩略图**真的渲染出来**。

这是线上 bug「弹窗里搜出来的图全是破图」的界面级回归，补的是
`tools/verify_images.py` 漏掉的**第四个** `imgSrc()` 调用点：

  A. 番号补全的缺失封面   -> `#jbGrid .misscard img`   （verify_images.py）
  B. MetaTube 搜索结果封面 -> `#dvHits .hit img`        （verify_images.py）
  C. gfriends 头像库缩略图 -> `#gfResult img`           （verify_images.py）
  D. 选择头像弹窗的候选图  -> `#pkList img`             <-- 本脚本

**这个 bug 用命令行查不出来**：弹窗里的图当时直接写的是 CDN 外链，
`curl` 那个外链是 200、服务端 `/api/img` 也是 200，只有浏览器因为 CSP
`img-src 'self'` 把它拦了。所以判据必须是渲染层的 `naturalWidth > 0`，
外加「每张都走 /api/img」和「没有 CSP 拦截报错」。
（另一个反向坑：`alt` 为空时破图会显示成一片空白，很容易被看成"图没出来但没报错"。）

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


def main():
    results = []

    def check(name, ok, detail=""):
        results.append((name, ok, detail))
        print(("PASS  " if ok else "FAIL  ") + name + (("  | " + detail) if detail else ""))

    tgt = http_json("/json/new?about:blank", method="PUT")
    page = Page(tgt["webSocketDebuggerUrl"])
    for m in ("Runtime.enable", "Log.enable", "Network.enable", "Page.enable"):
        page.send(m)
    page.send("Emulation.setDeviceMetricsOverride", width=1440, height=1000,
              deviceScaleFactor=1, mobile=False)

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

    print("\n3) 点第一张卡片的「选择」，弹出挑选弹窗")
    opened = page.eval("""(() => {
      const b = document.querySelector('#psList .pcard button[data-act="pick"]');
      if (b) b.click();
      return !!b;
    })()""")
    check("演员卡片上有「选择」按钮", bool(opened))
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
    info = settle_imgs(page, "#pkList img")
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
