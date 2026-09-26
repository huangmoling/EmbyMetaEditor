"""用 CDP 驱动无头 Edge，验证「演员资料」面板的渲染与勾选状态。

为什么需要界面层回归（静态检查覆盖不到的部分）：

  「只填空白」这条策略在界面上的表现形式是**勾选框的可用性** —— Emby 已有值的字段
  必须是 disabled + 不勾选，可写的才勾上。这条如果错了，用户点一下「写入勾选字段」
  就可能覆盖掉已有资料，而服务端那层虽然也拦得住，界面上看起来却是「让你选」。

  同样地，「回滚」是破坏性操作，脚本要确认它**不是点一下就执行**（需要二次确认）；
  再点一次「资料」按钮、关掉抽屉这些流程也要确认没报错、没 CSP 拦截。

跑之前：
  1. `EMBYME_AUTH_PASSWORD=test-pass EmbyMetaEditor.exe -port 8097 -open=false`
     （用真实 config.json，演员列表要能连上 Emby）
  2. 无头 Edge 带 `--remote-debugging-port=9333`

**只读**：只点「资料」打开预览面板（预览不写任何东西），以及打开「同步历史」。
回滚按钮只点一次（停在「确认回滚？」那步），**不会真的回滚** —— 因此第 9 步只有在
历史里存在**尚未回滚**的记录时才会真正执行；跑完 `smoke_profile.py`（它会 apply 后
立刻 rollback）之后通常全是被回滚的记录，这步会跳过。这是有意的，不是漏测。
"""
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from verify_javbus_probe import Page, http_json  # noqa: E402
from verify_images import safe_shot  # noqa: E402
import wbauth  # noqa: E402

APP = "http://127.0.0.1:8097/"

# 候选演员名，按顺序试，挑第一个「预览表里**既有 Emby 已有值、又有可写空白**」的。
#
# 为什么必须是这种候选：默认列表里排前面的是「プレミアムビデオ」这类工作室名，抓不到
# 任何资料，用它验「只填空白」等于什么都没验；而资料已被旧扩展器填满的演员
# write_count 是 0，就只有「跳过」没有「写入」那一面。两边都得有样本，两条断言才不是空过。
#
# 又因为界面上「只看无头像」默认勾着，而有头像的演员才更可能「填了一半」，
# 所以第 7 步会先把这个过滤取消掉，再按名字搜。
STARS = [s.strip() for s in os.environ.get("VERIFY_STARS", "").split(",") if s.strip()] or [
    "玉木くるみ", "心花ゆら", "広瀬みやび", "月見若葉", "江上しほ", "神ユキ"]

# 读「现有值 vs 抓取值」对照表。抽成常量是因为要在一个候选循环里反复读。
TABLE_JS = """(() => {
  const rows = [...document.querySelectorAll('.pf-tbl tbody tr')];
  const cb = (r) => r.querySelector('input[data-key]');
  const old = (r) => (r.querySelector('.pf-old').textContent || '').trim();
  const blank = (r) => { const t = old(r); return !t || t === '（空）'; };
  return {
    keys: rows.map(r => cb(r).dataset.key),
    total: rows.length,
    // 「只填空白」在界面上的形态：Emby 已有值的字段勾选框必须 disabled
    badEnabled: rows.filter(r => !blank(r) && !cb(r).disabled).map(r => cb(r).dataset.key),
    // 反过来：可写的字段必须勾上且没被禁用
    badChecked: rows.filter(r => r.classList.contains('pf-will') && (!cb(r).checked || cb(r).disabled))
                    .map(r => cb(r).dataset.key),
    willRows: rows.filter(r => r.classList.contains('pf-will')).length,
    // 界面判定为「已有值，跳过」的行
    skipRows: rows.filter(r => r.querySelector('.pf-n').textContent.indexOf('跳过') >= 0).length,
    badCheckedSkip: rows.filter(r => r.querySelector('.pf-n').textContent.indexOf('跳过') >= 0
                                    && cb(r).checked).map(r => cb(r).dataset.key),
    embyRows: rows.filter(r => !blank(r)).length,
    facts: document.querySelectorAll('.pf-fact').length,
    srcTags: document.querySelectorAll('.pf-srcbar .tag').length,
    kind: rows.map(r => (r.querySelector('.pf-n').textContent || '').trim()),
  };
})()"""


def main():
    results = []

    def check(name, ok, detail=""):
        results.append((name, ok, detail))
        print(("PASS  " if ok else "FAIL  ") + name + (("  | " + str(detail)) if detail else ""))

    tgt = http_json("/json/new?about:blank", method="PUT")
    page = Page(tgt["webSocketDebuggerUrl"])
    for m in ("Runtime.enable", "Log.enable", "Network.enable", "Page.enable"):
        page.send(m)
    page.send("Emulation.setDeviceMetricsOverride", width=1440, height=1100,
              deviceScaleFactor=1, mobile=False)

    # 记录所有 /api/profile/rollback 请求：用来证明「第一次点回滚不会真的执行」
    rollback_calls = []
    page.rollback_tap = None
    orig_keep = page._keep

    def keep(msg):
        m = msg.get("method")
        if m == "Network.requestWillBeSent":
            u = msg["params"]["request"]["url"]
            if "/api/profile/rollback" in u:
                rollback_calls.append(u)
        orig_keep(msg)

    page._keep = keep

    print("1) 打开应用并过访问认证")
    page.send("Page.navigate", url=APP)
    wbauth.login_in_browser(page)
    page.send("Page.navigate", url=APP)
    page.wait_for("!!document.querySelector('#lgSkip')", desc="登录页出现")
    page.pump(0.6)
    page.eval("document.querySelector('#lgSkip').click()")
    page.wait_for("!!document.querySelector('#psQ')", desc="进入主界面")
    page.pump(0.5)

    print("\n2) 切到「演员头像」视图")
    page.eval("""(() => {
      const b = [...document.querySelectorAll('#nav button')].find(x => x.dataset.view === 'persons');
      if (b) b.click();
      return !!b;
    })()""")
    page.wait_for("getComputedStyle(document.querySelector('#view-persons')).display !== 'none'",
                  desc="演员视图可见")
    page.wait_for("document.querySelectorAll('#psList .pcard').length > 0",
                  timeout=180, desc="演员卡片出现（要能连上 Emby）")
    page.pump(0.5)

    print("\n3) 「演员资料」卡片：资料源勾选项与状态标签")
    page.wait_for("document.querySelectorAll('#pfSources input').length > 0",
                  timeout=60, desc="资料源复选框出现（要能调 /api/profile/sources）")
    src = page.eval("""(() => {
      const boxes = [...document.querySelectorAll('#pfSources input')];
      return {
        n: boxes.length,
        keys: boxes.map(b => b.dataset.key),
        checked: boxes.filter(b => b.checked).length,
        state: (document.querySelector('#pfState') || {}).textContent || '',
        aliasN: (document.querySelector('#pfAliasN') || {}).textContent || '',
        hasAlias: !!document.querySelector('#pfAlias'),
        hasBatchPage: !!document.querySelector('#pfBatchPage'),
        hasBatchLimit: !!document.querySelector('#pfBatchLimit'),
        hasHistory: !!document.querySelector('#pfHistory'),
        hasLimit: !!document.querySelector('#pfLimit'),
      };
    })()""")
    print("     源：%s" % src["keys"])
    check("至少 3 个资料源复选框", src["n"] >= 3, src)
    check("默认全选", src["checked"] == src["n"], "%s/%s" % (src["checked"], src["n"]))
    check("状态标签显示「已启用 n/n 个源」", src["state"].startswith("已启用"), src["state"])
    check("别名记忆组数已回填", src["aliasN"].strip().isdigit(), repr(src["aliasN"]))
    for k in ("hasAlias", "hasBatchPage", "hasBatchLimit", "hasHistory", "hasLimit"):
        check("控件存在：%s" % k, src[k])

    print("\n4) 取消一个源 → 状态标签跟着变（勾选状态要能持久到重渲染）")
    page.eval("""(() => {
      const b = document.querySelector('#pfSources input');
      b.checked = false; b.dispatchEvent(new Event('change', {bubbles: true}));
      return true;
    })()""")
    page.pump(0.3)
    st2 = page.eval("document.querySelector('#pfState').textContent")
    check("取消勾选后状态标签更新", st2.startswith("已启用") and st2 != src["state"], st2)
    page.eval("""(() => {
      const b = document.querySelector('#pfSources input');
      b.checked = true; b.dispatchEvent(new Event('change', {bubbles: true}));
      return true;
    })()""")

    print("\n5) 头像强制覆盖的入口")
    ow = page.eval("""(() => {
      const l = document.querySelector('label.switch:has(#psSource), #psOverwrite') ;
      return {
        hasSwitch: !!document.querySelector('#psOverwrite'),
        label: (document.querySelector('#psOverwrite') || {}).parentElement ?
               document.querySelector('#psOverwrite').parentElement.textContent.trim() : '',
      };
    })()""")
    print("     工具栏开关：%r" % ow["label"])
    check("工具栏有「强制覆盖已有头像」开关", ow["hasSwitch"], ow)
    check("开关文案写清了是强制覆盖", "强制覆盖" in ow["label"], ow["label"])

    print("\n6) 演员卡片上的操作按钮（按有无头像分别断言）")
    btns = page.eval("""(() => {
      const cards = [...document.querySelectorAll('#psList .pcard')];
      const read = (c) => {
        const b = c.querySelector('button[data-act="av"]');
        return { acts: [...c.querySelectorAll('button[data-act]')].map(x => x.dataset.act),
                 text: b ? b.textContent.trim() : '', title: b ? b.title : '' };
      };
      const withImg = cards.filter(c => c.querySelector('.av img')).map(read);
      const noImg = cards.filter(c => !c.querySelector('.av img')).map(read);
      return { n: cards.length, withImg: withImg, noImg: noImg };
    })()""")
    print("     共 %d 张卡：有头像 %d 张、无头像 %d 张"
          % (btns["n"], len(btns["withImg"]), len(btns["noImg"])))
    acts_ok = all(b["acts"] == ["av", "pick", "prof"]
                  for b in btns["withImg"] + btns["noImg"])
    check("每张卡都有三个操作：头像 / 选图 / 资料", acts_ok,
          (btns["withImg"] + btns["noImg"])[:2])
    bad_ov = [b for b in btns["withImg"] if b["text"] != "重写头像"]
    check("已有头像的卡片按钮是「重写头像」（点了就走强制覆盖）", not bad_ov, bad_ov[:2])
    bad_no = [b for b in btns["noImg"] if b["text"] != "刮削头像"]
    check("无头像的卡片按钮是「刮削头像」", not bad_no, bad_no[:2])

    print("\n7) 搜一个真实演员，点「资料」预览（只读，不写任何东西）")
    # 「只看无头像」默认勾着，而有头像的演员才更可能「填了一半」——
    # 要同时验到「Emby 已有值 → 禁用」和「空白 → 可写」两面，先把它取消掉。
    page.eval("""(() => {
      const m = document.querySelector('#psMissing');
      if (m && m.checked) { m.checked = false; m.dispatchEvent(new Event('change', {bubbles: true})); }
      return true;
    })()""")
    page.pump(0.6)

    def close_drawer():
        """关掉资料抽屉，好在循环里重开下一个候选。"""
        page.eval("""(() => {
          const m = document.querySelector('.drawer-mask');
          if (m) m.click();
          return true;
        })()""")
        page.pump(0.4)

    tbl, star, tried = None, None, []
    for name in STARS:
        js_name = json.dumps(name, ensure_ascii=False)
        has_card = ("[...document.querySelectorAll('#psList .pcard')]"
                    ".some(c => c.dataset.name === %s)" % js_name)
        page.eval("""(() => {
          const i = document.querySelector('#psQ');
          i.value = %s;
          i.dispatchEvent(new Event('input', {bubbles: true}));
          document.querySelector('#psSearch').click();
          return true;
        })()""" % js_name)
        try:
            page.wait_for(has_card, timeout=40, desc="卡片出现：" + name)
        except TimeoutError:
            tried.append((name, "库里没有"))
            print("     %-10s 库里没有，换下一个" % name)
            continue
        page.pump(0.3)
        page.eval("""(() => {
          const c = [...document.querySelectorAll('#psList .pcard')]
                      .find(x => x.dataset.name === %s);
          c.querySelector('button[data-act="prof"]').click();
          return true;
        })()""" % js_name)
        page.wait_for("!!document.querySelector('#pfBody')", timeout=20, desc="资料抽屉打开")
        try:
            page.wait_for("!!document.querySelector('#pfApply')", timeout=180,
                          desc="预览返回并渲染出对照表（要能连上 av-db.net）")
        except TimeoutError:
            tried.append((name, "预览没出结果"))
            print("     %-10s 预览没出结果，换下一个" % name)
            close_drawer()
            continue
        page.pump(0.4)
        snap = page.eval(TABLE_JS)
        tried.append((name, "emby=%d will=%d" % (snap["embyRows"], snap["willRows"])))
        if snap["embyRows"] >= 1 and snap["willRows"] >= 1:
            tbl, star = snap, name
            print("     %-10s 命中：Emby 已有值 %d 行、将写入 %d 行"
                  % (name, snap["embyRows"], snap["willRows"]))
            break
        print("     %-10s 样本不够（已有值 %d、可写 %d），换下一个"
              % (name, snap["embyRows"], snap["willRows"]))
        close_drawer()

    check("找到一个「已有值 + 有空白」都有样本的演员", tbl is not None, tried)
    if tbl is None:
        # 前面已经把抽屉都关掉了，后面的步骤还要接着跑，但表相关的断言无从谈起
        tbl = page.eval(TABLE_JS)
    else:
        print("     字段：%s" % tbl["keys"])
        print("     每行判定：%s" % tbl["kind"])
        check("对照表渲染出全部受管字段", tbl["total"] >= 4, tbl)
        check("表格里没有写不进去的 tags 字段", "tags" not in (tbl["keys"] or []), tbl["keys"])
        # 样本必须都要有，否则下面两条断言是空过的
        check("样本里有「Emby 已有值」的行（只填空白的对照面）", tbl["embyRows"] >= 1, tbl)
        check("样本里有「将写入」的行（可写的那一面）", tbl["willRows"] >= 1, tbl)
        check("已有值的字段勾选框是禁用的（界面上就不给选）",
              not tbl["badEnabled"], tbl["badEnabled"])
        check("判定为可写的字段已勾选且可操作", not tbl["badChecked"], tbl["badChecked"])
        check("判定为跳过的字段勾选框是禁用的", not tbl["badCheckedSkip"], tbl["badCheckedSkip"])
        check("各资料源明细渲染出来了", tbl["facts"] >= 1, tbl["facts"])
        check("命中来源标签已展示", tbl["srcTags"] >= 1, tbl["srcTags"])

    page.pump(0.4)
    # 这张图要进 README（公开仓库），先把侧栏那行藏掉 —— 它会显示当前 Emby 用户名
    # （「在线 · <user>」），属于不该公开的信息。
    page.eval("""(() => {
      const s = document.querySelector('#sbState');
      if (s && s.parentElement) s.parentElement.style.visibility = 'hidden';
      return true;
    })()""")
    page.pump(0.3)
    safe_shot(page, "16-演员资料面板.png")

    print("\n8) 关闭抽屉 → 打开「同步历史」")
    close_drawer()
    page.wait_for("!document.querySelector('.drawer-mask')", desc="抽屉关闭")
    page.eval("document.querySelector('#pfHistory').click()")
    page.wait_for("!!document.querySelector('#shBody')", desc="同步历史抽屉打开")
    page.pump(1.0)
    hist = page.eval("""(() => {
      const rows = [...document.querySelectorAll('.sh-row')];
      return {
        rows: rows.length,
        empty: !!document.querySelector('#shBody .empty'),
        rollbackBtns: document.querySelectorAll('#shBody button[data-rid]').length,
        rolledTags: [...document.querySelectorAll('#shBody .tag')].map(t => t.textContent.trim()),
        doneRows: document.querySelectorAll('.sh-row.sh-done').length,
      };
    })()""")
    print("     历史 %d 条：待回滚 %d 条、已回滚 %d 条"
          % (hist["rows"], hist["rollbackBtns"], len(hist["rolledTags"])))
    check("历史抽屉渲染出了内容（记录或空态）", hist["rows"] > 0 or hist["empty"], hist)
    if hist["rows"] > 0:
        # 每条记录都必须落到「可回滚」或「已回滚」之一 —— 两个都不给说明状态渲染漏了，
        # 用户会看到一条既点不动、也不知道回没回滚的记录。
        check("每条历史记录都有明确状态（回滚按钮 或 已回滚标签）",
              hist["rollbackBtns"] + len(hist["rolledTags"]) == hist["rows"], hist)
        check("已回滚的记录带 sh-done 标记",
              hist["doneRows"] == len(hist["rolledTags"]), hist)

    if hist["rollbackBtns"] > 0:
        print("\n9) 回滚必须点两次（第一次只进入确认态，不能真的发请求）")
        page.eval("""(() => {
          document.querySelector('#shBody button[data-rid]').click();
          return true;
        })()""")
        page.pump(0.8)
        armed = page.eval("""(() => {
          const b = document.querySelector('#shBody button[data-rid]');
          return b.textContent.trim();
        })()""")
        check("第一次点击变成「确认回滚？」", armed.startswith("确认回滚"), armed)
        check("第一次点击没有发出回滚请求", len(rollback_calls) == 0, rollback_calls)
        # 把确认态撤销掉，别在只读脚本里留下一个「已武装」的按钮
        page.eval("""(() => {
          const b = document.querySelector('#shBody button[data-rid]');
          b.dataset.armed = '0'; b.textContent = '回滚'; b.classList.remove('btn-danger');
          return true;
        })()""")
    else:
        print("\n9) 回滚二次确认  —— 跳过（当前没有未回滚的历史记录）")

    print("\n10) 页面错误与控制台")
    errs = page.errors()
    hard = [e for e in errs if e.get("method") != "__http__"]
    http_bad = [e for e in errs if e.get("method") == "__http__" and "favicon" not in (e.get("url") or "")]
    check("没有未捕获异常 / console 报错 / CSP 拦截", not hard, hard[:3])
    check("没有 4xx/5xx 响应", not http_bad, http_bad[:3])

    print("\n" + ("全部通过" if not [r for r in results if not r[1]]
                  else "有 %d 项失败" % len([r for r in results if not r[1]])))
    if safe_shot.__doc__:
        pass
    return 1 if [r for r in results if not r[1]] else 0


if __name__ == "__main__":
    sys.exit(main())
