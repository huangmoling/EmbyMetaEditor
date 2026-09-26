"""用 CDP 驱动无头 Edge，验证「演员资料」面板的渲染与勾选状态。

为什么需要界面层回归（静态检查覆盖不到的部分）：

  「只填空白」这条策略在界面上的表现形式是**勾选框的勾选状态**：默认只勾 Emby 里
  空着的字段，已有值的字段**可勾但默认不勾**（勾上就是覆盖，这是新加的能力）。
  这条如果错了 —— 比如把已有值的字段预先勾上 —— 用户点一下「写入勾选字段」
  就会覆盖掉自己攒的资料，而服务端那层虽然也拦不住（勾选的语义就是「以勾选为准」），
  界面上看起来却像是「默认帮你选好了」。

  同样地，「回滚」是破坏性操作，脚本要确认它**不是点一下就执行**（需要二次确认）；
  再点一次「资料」按钮、关掉抽屉这些流程也要确认没报错、没 CSP 拦截。
  「媒体库作品」的封面必须走同源代取（CSP 是 `img-src 'self'`），
  所以这里不只看有没有 `<img>`，还要看 `naturalWidth` —— 外链的图在命令行是 200、
  在浏览器里是全白，只看 src 永远发现不了。

跑之前：
  1. `EMBYME_AUTH_PASSWORD=test-pass EmbyMetaEditor.exe -port 8097 -open=false`
     （用真实 config.json，演员列表与作品列表都要能连上 Emby）
  2. 无头 Edge 带 `--remote-debugging-port=9333`

**只读**：只点「资料」打开预览面板（预览不写任何东西）、打开「同步历史」，
以及勾选/取消一个「覆盖」复选框（只改界面状态，脚本会断言它**没有**发出写入请求）。
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
  const note = (r) => (r.querySelector('.pf-n').textContent || '').trim();
  const over = (r) => r.dataset.over === '1';
  return {
    keys: rows.map(r => cb(r).dataset.key),
    total: rows.length,
    embyRows: rows.filter(r => !blank(r)).length,
    // 「可覆盖」的行：Emby 已有值、这次也抓到了值 → 勾选框必须**可勾**（新需求）
    overRows: rows.filter(over).length,
    badOverDisabled: rows.filter(r => over(r) && cb(r).disabled).map(r => cb(r).dataset.key),
    // 但默认不能预先勾上 —— 默认策略仍是「只填空白」
    badOverPrechecked: rows.filter(r => over(r) && cb(r).checked).map(r => cb(r).dataset.key),
    willRows: rows.filter(r => r.classList.contains('pf-will')).length,
    // 判定为「将写入」的行必须勾上且可用
    badChecked: rows.filter(r => r.classList.contains('pf-will') && (!cb(r).checked || cb(r).disabled))
                    .map(r => cb(r).dataset.key),
    // 判定为「跳过」的行默认不能是勾着的
    badCheckedSkip: rows.filter(r => note(r).indexOf('跳过') >= 0 && cb(r).checked)
                        .map(r => cb(r).dataset.key),
    // 没抓到值的字段勾了也没用 → 必须禁用
    badEmptyEnabled: rows.filter(r => (r.querySelector('.pf-new').textContent || '').trim() === '—'
                                      && !cb(r).disabled).map(r => cb(r).dataset.key),
    facts: document.querySelectorAll('.pf-fact').length,
    srcTags: document.querySelectorAll('.pf-srcbar .tag').length,
    kind: rows.map(note),
  };
})()"""

# 资料面板底部的「媒体库作品」：单独接口、单独加载，所以单独取一次快照。
WORKS_JS = """(() => {
  const box = document.querySelector('#pfWorks');
  if (!box) return { missing: true };
  const head = box.querySelector('.pf-wh');
  const tag = head ? head.querySelector('.tag') : null;
  const imgs = [...box.querySelectorAll('.pf-wcard .wc img')];
  return {
    missing: false,
    head: head ? head.textContent.trim() : '',
    total: tag ? tag.textContent.trim() : '',
    cards: box.querySelectorAll('.pf-wcard').length,
    named: [...box.querySelectorAll('.pf-wcard .wt b')].filter(b => (b.textContent || '').trim()).length,
    meta: [...box.querySelectorAll('.pf-wcard .wt span')].filter(s => (s.textContent || '').trim()).length,
    imgs: imgs.length,
    // CSP 是 img-src 'self'，封面走错地址就是「有 img 但加载失败」，必须查 naturalWidth
    broken: imgs.filter(i => i.complete && i.naturalWidth === 0).length,
    external: imgs.filter(i => !i.getAttribute('src').startsWith('/')).length,
    hint: (box.querySelector('.cnhint') || {}).textContent || '',
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
    # 同理记录 /api/profile/apply：证明「勾选覆盖」只是改界面状态，不写任何东西
    apply_calls = []
    # 作品列表的请求 URL：用来断言分页时确实带了更大的 limit
    works_calls = []
    page.rollback_tap = None
    orig_keep = page._keep

    def keep(msg):
        m = msg.get("method")
        if m == "Network.requestWillBeSent":
            u = msg["params"]["request"]["url"]
            if "/api/profile/rollback" in u:
                rollback_calls.append(u)
            if "/api/profile/apply" in u:
                apply_calls.append(u)
            if "/api/profile/works" in u:
                works_calls.append(u)
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
    wk_total = -1  # 「媒体库作品」报出来的真实总数，7d 用它验证分页后能回到真实值
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
        tried.append((name, "emby=%d will=%d over=%d"
                      % (snap["embyRows"], snap["willRows"], snap["overRows"])))
        # 样本要求三面都有：Emby 已有值（含可覆盖的）、有空白可写。
        # 少任何一面，下面那几条断言就是空过的。
        if snap["embyRows"] >= 1 and snap["willRows"] >= 1 and snap["overRows"] >= 1:
            tbl, star = snap, name
            print("     %-10s 命中：已有值 %d 行（可覆盖 %d 行）、将写入 %d 行"
                  % (name, snap["embyRows"], snap["overRows"], snap["willRows"]))
            break
        print("     %-10s 样本不够（已有值 %d、可覆盖 %d、可写 %d），换下一个"
              % (name, snap["embyRows"], snap["overRows"], snap["willRows"]))
        close_drawer()

    check("找到一个「已有值（含可覆盖）+ 有空白」都有样本的演员", tbl is not None, tried)
    if tbl is None:
        # 前面已经把抽屉都关掉了，后面的步骤还要接着跑，但表相关的断言无从谈起
        tbl = page.eval(TABLE_JS)
    else:
        print("     字段：%s" % tbl["keys"])
        print("     每行判定：%s" % tbl["kind"])
        check("对照表渲染出全部受管字段", tbl["total"] >= 4, tbl)
        check("表格里没有写不进去的 tags 字段", "tags" not in (tbl["keys"] or []), tbl["keys"])
        # 样本必须都要有，否则下面几条断言是空过的
        check("样本里有「Emby 已有值」的行", tbl["embyRows"] >= 1, tbl)
        check("样本里有「可覆盖」的行（已有值 + 本次抓到）", tbl["overRows"] >= 1, tbl)
        check("样本里有「将写入」的行（可写的那一面）", tbl["willRows"] >= 1, tbl)
        check("判定为可写的字段已勾选且可操作", not tbl["badChecked"], tbl["badChecked"])
        check("判定为跳过的字段默认不勾选", not tbl["badCheckedSkip"], tbl["badCheckedSkip"])
        check("没抓到值的字段勾选框是禁用的", not tbl["badEmptyEnabled"], tbl["badEmptyEnabled"])
        # 新需求：已有值的字段要能勾选覆盖（但默认不勾）
        check("已有值的字段勾选框是**可勾选**的（支持覆盖）",
              not tbl["badOverDisabled"], tbl["badOverDisabled"])
        check("已有值的字段默认不预先勾选（默认仍是只填空白）",
              not tbl["badOverPrechecked"], tbl["badOverPrechecked"])
        check("各资料源明细渲染出来了", tbl["facts"] >= 1, tbl["facts"])
        check("命中来源标签已展示", tbl["srcTags"] >= 1, tbl["srcTags"])

        print("\n7b) 「媒体库作品」：单独接口、单独加载")
        try:
            page.wait_for("document.querySelectorAll('#pfWorks .pf-wcard').length > 0",
                          timeout=90, desc="媒体库作品卡片出现（要能查本地 Emby）")
        except TimeoutError:
            pass
        page.pump(0.6)
        wk = page.eval(WORKS_JS)
        try:
            wk_total = int(str(wk.get("total", "")).strip())
        except ValueError:
            wk_total = -1
        print("     %s / 卡片 %d 张 / 封面 %d 张（破图 %d）"
              % (wk.get("head", ""), wk.get("cards", 0), wk.get("imgs", 0), wk.get("broken", 0)))
        check("资料面板里有「媒体库作品」区块", not wk.get("missing"), wk)
        check("作品列表拿到了数据", wk.get("cards", 0) >= 1, wk)
        check("作品卡片都带番号/标题", wk.get("named", 0) == wk.get("cards", 0), wk)
        check("作品卡片带年份/所属库的副标题", wk.get("meta", 0) >= 1, wk)
        # 封面必须走同源代取（CSP 是 img-src 'self'），且要真的能显示出来
        check("作品封面全部走同源地址", wk.get("external", 0) == 0, wk.get("external"))
        check("作品封面没有破图", wk.get("broken", 0) == 0, wk.get("broken"))

        print("\n7c) 勾选「覆盖」：只改界面判定，不发任何请求")
        before_calls = len(apply_calls)
        over = page.eval("""(() => {
          const tr = [...document.querySelectorAll('.pf-tbl tbody tr')].find(r => r.dataset.over === '1');
          if (!tr) return null;
          const before = tr.querySelector('.pf-n').textContent.trim();
          const cb = tr.querySelector('input[data-key]');
          cb.checked = true; cb.dispatchEvent(new Event('change', {bubbles: true}));
          const btn = document.querySelector('#pfApply');
          return {
            key: cb.dataset.key, before: before,
            after: tr.querySelector('.pf-n').textContent.trim(),
            cls: tr.className,
            btn: btn.textContent.trim(),
            danger: btn.classList.contains('btn-danger'),
            count: (document.querySelector('#pfCount') || {}).textContent || '',
          };
        })()""")
        page.pump(0.5)
        if over is None:
            check("找到可覆盖的行用于交互验证", False, "没有 dataset.over=1 的行")
        else:
            print("     %s：%r → %r / 按钮 %r" % (over["key"], over["before"], over["after"], over["btn"]))
            check("勾上后判定变成「将覆盖原值」", "覆盖" in over["after"], over["after"])
            check("覆盖行有醒目的样式类（pf-over）", "pf-over" in over["cls"], over["cls"])
            check("主按钮文案写明含几项覆盖", "覆盖" in over["btn"], over["btn"])
            check("有覆盖时主按钮变危险色", over["danger"], over["danger"])
            check("按钮旁的数量提示也点出覆盖", "覆盖" in over["count"], over["count"])
            check("勾选覆盖不发写入请求（只是界面状态）",
                  len(apply_calls) == before_calls, apply_calls[before_calls:])
            # 还原成默认状态，后面的截图与步骤都按「未勾选」呈现
            page.eval("""(() => {
              const tr = [...document.querySelectorAll('.pf-tbl tbody tr')].find(r => r.dataset.over === '1');
              const cb = tr.querySelector('input[data-key]');
              cb.checked = false; cb.dispatchEvent(new Event('change', {bubbles: true}));
              return true;
            })()""")
            page.pump(0.3)
            back = page.eval("document.querySelector('#pfApply').textContent.trim()")
            check("取消勾选后按钮文案回到只有「写入」", "覆盖" not in back, back)

        print("\n7d) 作品多于一屏时的「再加载」分页")
        # 库里这位演员只有十几部，凑不出「超过 60 部」的真实样本，
        # 所以直接把总数改大再重渲染 —— 验的是**渲染分支与请求参数**，
        # 不需要真有一个作品过百的演员（那种演员随库变化，拿它当样本迟早失效）。
        more = page.eval("""(() => {
          const b0 = document.querySelector('#pfWMore');
          if (b0) return { text: b0.textContent.trim(), preset: true };
          S.prof.works.total = 200;
          renderProfileWorks();
          const b = document.querySelector('#pfWMore');
          return b ? { text: b.textContent.trim(), preset: false } : null;
        })()""")
        if more is None:
            check("作品数超过已加载量时出现「再加载」按钮", False, "按钮没渲染出来")
        else:
            print("     %r" % more["text"])
            check("作品数超过已加载量时出现「再加载」按钮", "再加载" in more["text"], more)
            before = len(works_calls)
            page.eval("document.querySelector('#pfWMore').click();")
            page.pump(2.0)
            last = works_calls[-1] if len(works_calls) > before else ""
            print("     点后请求：%s" % (last or "(没有请求)"))
            check("点「再加载」会带更大的 limit 重新查询", "limit=180" in last, last)
            after = page.eval("""({
              btn: !!document.querySelector('#pfWMore'),
              cards: document.querySelectorAll('#pfWorks .pf-wcard').length,
              total: S.prof.works.total,
            })""")
            check("重新拉取后回到真实总数、按钮消失",
                  after["total"] == wk_total and not after["btn"], after)

    page.pump(0.4)
    # 这张图要进 README（公开仓库），先把侧栏那行藏掉 —— 它会显示当前 Emby 用户名
    # （「在线 · <user>」），属于不该公开的信息。
    page.eval("""(() => {
      const s = document.querySelector('#sbState');
      if (s && s.parentElement) s.parentElement.style.visibility = 'hidden';
      return true;
    })()""")
    # 作品封面是用户媒体库里的真实内容（成人向），也不该进公开仓库。
    # 不去掉图片而是**打码**：布局与「有封面」这个事实照样能看出来，
    # 只是看不出封面画的是什么。直接隐藏图片会让人以为这个功能没封面。
    page.eval("""(() => {
      document.querySelectorAll('#pfWorks .pf-wcard .wc img').forEach((i) => {
        i.style.filter = 'blur(9px)';
        i.style.transform = 'scale(1.08)';
      });
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
