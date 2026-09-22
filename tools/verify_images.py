"""用 CDP 驱动无头 Edge，验证页面上的外部图片**真的渲染出来**。

这是线上 bug「缺失番号没有图片」的界面级回归，同时覆盖 imgSrc() 的全部三个调用点：

  A. 番号补全页的缺失番号封面   -> #jbGrid .misscard img
  B. MetaTube 搜索结果的封面     -> #dvHits .hit img
  C. gfriends 头像库的缩略图     -> #gfResult img

接口能返回 JPEG 不等于页面上看得到图 —— 可能是 <img> 的 src 拼错、
被 referrerpolicy 掐掉、或者 loading="lazy" 根本没触发。

跑之前需要：
  1. EmbyMetaEditor.exe -port 8097 -open=false   （用真实 config.json）
  2. 无头 Edge 带 --remote-debugging-port=9333

只读：扫描走 javbus 公开页面，不往 Emby 写任何东西。
"""
import base64
import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from verify_javbus_probe import OUT, Page, http_json  # noqa: E402

APP = "http://127.0.0.1:8097/"
STAR = os.environ.get("VERIFY_STAR", "三上悠亜")
PAGES = os.environ.get("VERIFY_PAGES", "1")

# 用 scrollIntoView 逐个滚动，而不是手动滚 window：
# 抽屉（.drawer）是 position:fixed + overflow:auto 的独立滚动容器，
# 滚 window 对它完全无效，里面的懒加载图永远不触发。
IMG_STATS = """(async () => {
  const imgs = [...document.querySelectorAll(%s)];
  for (const i of imgs) {
    i.scrollIntoView({block: 'center'});
    await new Promise(r => setTimeout(r, 90));
  }
  if (imgs.length) { imgs[0].scrollIntoView({block: 'start'}); }
  await new Promise(r => setTimeout(r, 150));
  return {
    cards: document.querySelectorAll(%s.replace(/ img$/, '')).length,
    imgs: imgs.length,
    loaded:  imgs.filter(i => i.complete && i.naturalWidth > 0).length,
    broken:  imgs.filter(i => i.complete && i.naturalWidth === 0).length,
    pending: imgs.filter(i => !i.complete).length,
    proxied: imgs.filter(i => (i.currentSrc || i.src).includes('/api/img?u=')).length,
    sample: imgs.slice(0, 3).map(i => ({
      src: (i.currentSrc || i.src).slice(0, 110),
      w: i.naturalWidth, h: i.naturalHeight,
    })),
  };
})()"""


def safe_shot(page, name):
    """截图是锦上添花，失败不该让整轮验证挂掉。

    页面塞满懒加载图片时 Page.captureScreenshot 容易超时，
    所以不用 captureBeyondViewport（视口截图快得多），并且吞掉异常。
    """
    try:
        page.ws.settimeout(60)
        r = page.send("Page.captureScreenshot", format="png")
        path = os.path.join(OUT, name)
        with open(path, "wb") as f:
            f.write(base64.b64decode(r["data"]))
        print("   screenshot -> %s (%d bytes)" % (os.path.abspath(path), os.path.getsize(path)))
    except Exception as e:
        print("   [提示] 截图失败（不影响结论）：%s" % e)
    finally:
        page.ws.settimeout(30)


def img_stats(page, sel):
    return page.eval(IMG_STATS % (repr(sel), repr(sel)), await_promise=True)


def settle_imgs(page, sel, timeout=60):
    """等懒加载收敛：反复统计直到没有 pending，或超时。"""
    info = img_stats(page, sel)
    deadline = time.time() + timeout
    while time.time() < deadline and info["pending"] > 0:
        page.pump(1.0)
        info = img_stats(page, sel)
    return info


def main():
    results = []

    def check(name, ok, detail=""):
        results.append((name, ok, detail))
        print(("PASS  " if ok else "FAIL  ") + name + (("  | " + detail) if detail else ""))

    def report(label, info):
        print("   %s：卡片 %d / <img> %d / 走代理 %d" %
              (label, info["cards"], info["imgs"], info["proxied"]))
        print("   加载成功 %d / 失败 %d / 仍在加载 %d" %
              (info["loaded"], info["broken"], info["pending"]))
        for s in info["sample"]:
            print("   样例：%dx%d  %s" % (s["w"], s["h"], s["src"]))

    def assert_ok(prefix, info, min_imgs=1):
        check("%s：有图片元素" % prefix, info["imgs"] >= min_imgs,
              "img=%d（卡片 %d）" % (info["imgs"], info["cards"]))
        if info["imgs"] == 0:
            return
        check("%s：全部走服务端代理" % prefix, info["proxied"] == info["imgs"],
              "proxied=%d/%d" % (info["proxied"], info["imgs"]))
        check("%s：全部加载成功" % prefix, info["loaded"] == info["imgs"],
              "loaded=%d/%d" % (info["loaded"], info["imgs"]))
        check("%s：没有加载失败" % prefix, info["broken"] == 0, "broken=%d" % info["broken"])
        check("%s：没有卡住的图" % prefix, info["pending"] == 0, "pending=%d" % info["pending"])
        check("%s：样例有真实尺寸" % prefix, all(s["w"] > 0 for s in info["sample"]),
              " / ".join("%dx%d" % (s["w"], s["h"]) for s in info["sample"]))

    tgt = http_json("/json/new?about:blank", method="PUT")
    page = Page(tgt["webSocketDebuggerUrl"])
    for m in ("Runtime.enable", "Log.enable", "Network.enable", "Page.enable"):
        page.send(m)
    page.send("Emulation.setDeviceMetricsOverride", width=1440, height=1000,
              deviceScaleFactor=1, mobile=False)

    print("1) 打开应用")
    page.send("Page.navigate", url=APP)
    page.wait_for("!!document.querySelector('#lgSkip')", desc="登录页出现")
    page.pump(0.6)
    page.eval("document.querySelector('#lgSkip').click()")
    page.wait_for("!!document.querySelector('#jbStar')", desc="进入主界面")
    page.pump(0.5)

    def goto(view):
        page.eval("""(() => {
          const b = [...document.querySelectorAll('button')].find(x => x.dataset.view === %s);
          if (b) b.click();
          return !!b;
        })()""" % repr(view))
        page.wait_for("getComputedStyle(document.querySelector('#view-%s')).display !== 'none'" % view,
                      desc="%s 视图可见" % view)
        page.pump(0.4)

    # ---------- A. 番号补全 ----------
    print("\n[A] 番号补全页的缺失番号封面")
    goto("javbus")
    page.eval("""(() => {
      const p = document.querySelector('#jbPages');
      if (p) { p.value = '%s'; p.dispatchEvent(new Event('change', {bubbles: true})); }
      const i = document.querySelector('#jbStar');
      i.value = %s;
      i.dispatchEvent(new Event('input', {bubbles: true}));
      return true;
    })()""" % (PAGES, repr(STAR)))
    page.eval("document.querySelector('#jbScan').click()")
    t0 = time.time()
    page.wait_for("document.querySelectorAll('#jbGrid .misscard').length > 0",
                  timeout=300, desc="缺失番号卡片出现")
    print("   扫描用时 %.1f 秒" % (time.time() - t0))
    page.pump(0.5)
    infoA = settle_imgs(page, "#jbGrid .misscard img")
    report("番号补全", infoA)
    safe_shot(page, "10-缺失番号封面.png")
    assert_ok("番号补全", infoA)

    # ---------- B. MetaTube 搜索结果封面 ----------
    print("\n[B] MetaTube 搜索结果的封面")
    goto("library")
    page.wait_for("document.querySelectorAll('#lbGrid .mcard').length > 0",
                  timeout=120, desc="影片卡片出现")
    page.pump(0.5)
    # 先看列表本身的海报（走 Emby 直连，不经代理）
    infoLib = settle_imgs(page, "#lbGrid .mcard img")
    report("媒体库列表", infoLib)
    check("媒体库列表：海报全部加载", infoLib["imgs"] > 0 and infoLib["loaded"] == infoLib["imgs"],
          "loaded=%d/%d" % (infoLib["loaded"], infoLib["imgs"]))
    safe_shot(page, "11-媒体库列表.png")

    print("   打开第一条的详情抽屉")
    page.eval("""(() => {
      const b = document.querySelector('#lbGrid .mcard button[data-act="detail"]');
      if (b) b.click();
      return !!b;
    })()""")
    page.wait_for("!!document.querySelector('#dvSearch')", timeout=60, desc="详情抽屉打开")
    page.pump(0.5)
    # 抽屉里预填的是条目自己的 SortName/Name，未必能搜到东西。
    # 这里换成一个确定有结果的番号，专测「搜索结果封面能不能渲染」。
    QUERY = os.environ.get("VERIFY_QUERY", "SSNI-989")
    page.eval("""(() => {
      const i = document.querySelector('#dvQ');
      i.value = %s;
      i.dispatchEvent(new Event('input', {bubbles: true}));
      return i.value;
    })()""" % repr(QUERY))
    print("   搜索关键词 =", page.eval("document.querySelector('#dvQ').value"))
    page.eval("document.querySelector('#dvSearch').click()")
    page.wait_for("document.querySelectorAll('#dvHits .hit').length > 0",
                  timeout=180, desc="MetaTube 搜索结果")
    page.pump(0.5)
    infoB = settle_imgs(page, "#dvHits .hit img")
    report("MetaTube 搜索", infoB)
    safe_shot(page, "12-MetaTube搜索结果.png")
    assert_ok("MetaTube 搜索", infoB)

    # ---------- C. gfriends 头像库 ----------
    print("\n[C] gfriends 头像库的缩略图")
    page.eval("document.querySelector('#drawerHost').innerHTML = ''")  # 关掉抽屉
    page.pump(0.3)
    goto("persons")
    page.eval("""(() => {
      const i = document.querySelector('#gfQ');
      i.value = %s;
      i.dispatchEvent(new Event('input', {bubbles: true}));
      return true;
    })()""" % repr(STAR))
    page.eval("document.querySelector('#gfSearch').click()")
    page.wait_for("document.querySelectorAll('#gfResult img').length > 0",
                  timeout=120, desc="gfriends 结果")
    page.pump(0.5)
    infoC = settle_imgs(page, "#gfResult img")
    report("gfriends", infoC)
    safe_shot(page, "13-gfriends头像库.png")
    assert_ok("gfriends", infoC)

    # ---------- 页面报错 ----------
    print("\n[D] 页面报错检查")
    errs = page.errors()
    # /api/img 出现 4xx/5xx 就是真问题，不能混进"预期内"
    img_errs = [e for e in errs if "/api/img" in e]
    other = [e for e in errs if "/api/img" not in e]
    if img_errs:
        for e in img_errs:
            print("   ! " + e)
        check("封面接口无报错", False, "%d 条" % len(img_errs))
    else:
        check("封面接口无报错", True)
    if other:
        print("   其他报错（多为登录页调用，非本次范围）：")
        for e in other[:8]:
            print("   ~ " + e)

    bad = [r for r in results if not r[1]]
    print("\n" + "=" * 62)
    print("共 %d 项，通过 %d，失败 %d" % (len(results), len(results) - len(bad), len(bad)))
    if bad:
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
