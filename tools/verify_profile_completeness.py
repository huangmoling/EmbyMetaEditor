"""用 CDP 驱动无头 Edge，验证「演员头像页的资料完整度百分比」真的接通了。

为什么要这一层：后端单测能证明 profileCompleteness 算得对、接口把 profile_percent
下发了，但证明不了**界面真的把它画出来了** —— 本项目最典型的坏法是「算了、传了、
没画」：接口 200、单测全绿、用户却什么都看不到。所以这里在真实 Emby 上做一次
「界面显示 == Emby 原始数据」的核对。

核对为什么非要有：完整度的分母是 5 个资料字段，**任何一个没从 Emby 取回来都会让
百分比静默偏低**（实测 Emby 4.9：/Persons 不带 Fields 时一个资料字段都不返回，
连 ProviderIds 都没有）。那种情况下界面和接口是**一致地错**的 —— 只比 DOM 与
/api/persons 抓不到，必须拿 Emby 自己的条目做独立参照。

验证内容：
  1. 头像页渲染出人物卡片，且**每张**卡片都画了完整度区块
  2. 文案是「资料 N%」，档位只可能是 0/20/40/60/80/100（分母 5）
  3. 0% 的卡片带 .zero 类（整条转灰，而不是用紫色假装有进度）
  4. 抽查前几位，界面显示的百分比 == 直接查 Emby 条目数出来的百分比
  5. 没有 console 报错

跑之前需要：
  1. 配好了 Emby 的实例在 8097（脚本读 config.json 拿 Emby 地址与令牌）
  2. 无头 Edge 带 --remote-debugging-port=9333

用法：
    python tools/verify_profile_completeness.py [config.json 路径]
（用 `-dir .tmp-test-home` 起实例时，config 在 .tmp-test-home/config.json）

只读：只打本机接口与 Emby 的只读接口，不写任何东西。
"""
import json
import os
import sys
import urllib.parse
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from verify_javbus_probe import Page, http_json  # noqa: E402
import wbauth  # noqa: E402

APP = "http://127.0.0.1:8097/"
HERE = os.path.dirname(os.path.abspath(__file__))
DEFAULT_CFG = os.path.abspath(os.path.join(HERE, "..", "config.json"))

# 与后端 profileCompleteness 的分母逐字对应（简介 / 出生日期 / 出生年份 /
# 出生地 / 外部 ID）。后端加字段时这里要一起改，否则脚本会用旧分母去核对。
PROFILE_KEYS = ("Overview", "PremiereDate", "ProductionYear",
                "ProductionLocations", "ProviderIds")
STEP = 100 // len(PROFILE_KEYS)


def load_emby(cfg_path):
    with open(cfg_path, encoding="utf-8") as f:
        d = json.load(f)
    base = (d.get("emby_url") or "").strip().rstrip("/")
    tok = (d.get("token") or "").strip()
    uid = (d.get("user_id") or "").strip()
    if not base or not tok or not uid:
        raise SystemExit("config.json 里没配好 Emby（emby_url / token / user_id）——"
                         "本脚本要直连 Emby 做独立参照，缺一不可。")
    return base, tok, uid


def filled_count(item):
    """数 Emby 条目里这 5 个字段填了几个 —— 与后端 profileValuePresent 同一口径。"""
    n = 0
    for k in PROFILE_KEYS:
        v = item.get(k)
        if isinstance(v, str):
            if v.strip():
                n += 1
        elif isinstance(v, bool):
            continue
        elif isinstance(v, (int, float)):
            if v != 0:
                n += 1
        elif isinstance(v, (list, dict)):
            if len(v) > 0:
                n += 1
    return n


def emby_pick_profiled(base, tok, uid, scan=500, want=3):
    """直连 Emby 扫一批人物，挑出**资料最全**的几位，返回 (名字, 已填数, id)。

    为什么非要挑有资料的：抽样如果全是 0%，那「Fields 漏了字段 → 界面与 Emby
    都是 0%」这种一致地错也能蒙混过关，这条核对就废了。必须拿非零档位对一次。
    """
    url = "%s/Persons?%s" % (base, urllib.parse.urlencode(
        {"Limit": scan, "Fields": ",".join(PROFILE_KEYS)}))
    req = urllib.request.Request(url, headers={"X-Emby-Token": tok})
    with urllib.request.urlopen(req, timeout=30) as r:
        res = json.loads(r.read().decode())
    rows = []
    for it in res.get("Items", []):
        n = filled_count(it)
        if n > 0:
            rows.append(((it.get("Name") or "").strip(), n, it.get("Id") or ""))
    rows.sort(key=lambda x: -x[1])
    # 按档位去重后再取，而不是一律取最高的三个：只对 100% 的话，「出生年份 /
    # 出生地这类字段漏取」只会把 5/5 变成 4/5，而四档里其它组合根本对不到 ——
    # 抽样要盖住不同的字段组合，才谈得上验证分母是对的。
    seen_step, out = set(), []
    for name, n, iid in rows:
        if n in seen_step:
            continue
        seen_step.add(n)
        out.append((name, n, iid))
        if len(out) >= want:
            break
    return out


def ui_pct_of(page, name, item_id):
    """在界面上搜到这个演员，读回卡片显示的百分比；没搜到返回 None。

    按 **id** 认卡片而不是按名字：人物库里重名很常见，按名字会认错人，
    而按 id 认错的可能性是零（id 就是 Emby 的条目 id）。
    """
    ok = page.eval("""(() => {
      const q = document.querySelector('#psQ');
      if (!q) return false;
      // 「只看无头像」默认勾着，而取样挑的偏偏是资料最全的几位 —— 他们通常
      // 也有头像，不放开这个筛选就会一张卡片都搜不到（超时，看着像功能坏了）。
      const miss = document.querySelector('#psMissing');
      if (miss) miss.checked = false;
      q.value = %s;
      loadPersons(0);
      return true;
    })()""" % json.dumps(name))
    if not ok:
        return None
    hit = """([...document.querySelectorAll('#psList .pcard')].find(x => x.dataset.id === %s) || null)""" % json.dumps(item_id)
    page.wait_for("!!" + hit, timeout=40, desc="搜索结果出现")
    page.pump(0.6)
    txt = page.eval("""(() => {
      const c = %s;
      return c ? ((c.querySelector('.pfbar .txt') || {}).textContent || '').trim() : '';
    })()""" % hit) or ""
    if not txt.startswith("资料 ") or not txt.endswith("%"):
        return None
    try:
        return int(txt[len("资料 "):-1])
    except ValueError:
        return None


def main():
    cfg = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_CFG
    base, tok, uid = load_emby(cfg)
    print("Emby：%s\nconfig：%s" % (base, cfg))

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

    print("\n1) 打开应用并用「跳过登录」进主界面")
    page.send("Page.navigate", url=APP)
    wbauth.login_in_browser(page)
    page.send("Page.navigate", url=APP)
    page.wait_for("!!document.querySelector('#lgSkip')", desc="登录页出现")
    page.pump(0.6)
    page.eval("document.querySelector('#lgSkip').click()")
    page.wait_for("!!document.querySelector('#jbStar')", desc="进入主界面")
    page.pump(0.4)

    print("\n2) 切到「演员头像」并等卡片")
    clicked = page.eval("""(() => {
      const b = [...document.querySelectorAll('button')].find(x => x.dataset.view === 'persons');
      if (b) b.click();
      return !!b;
    })()""")
    check("侧栏能找到「演员头像」入口", clicked is True, clicked)
    page.wait_for("!!document.querySelector('#psList .pcard')", timeout=60,
                  desc="人物卡片出现")
    page.pump(1.5)

    cards = page.eval("""(() => [...document.querySelectorAll('#psList .pcard')].map(c => ({
      id: c.dataset.id,
      name: ((c.querySelector('.info b') || {}).textContent || '').trim(),
      pct: ((c.querySelector('.pfbar .txt') || {}).textContent || '').trim(),
      zero: !!c.querySelector('.pfbar.zero'),
      bar: !!c.querySelector('.pfbar .track i'),
    })))()""") or []
    print("   卡片数：%d" % len(cards))
    check("头像页渲染出了人物卡片", len(cards) > 0, len(cards))
    if not cards:
        return finish(results, page)

    print("\n3) 每张卡片都要有完整度区块")
    no_block = [c["name"] for c in cards if not c["bar"] or not c["pct"]]
    check("每张卡片都画了完整度区块", not no_block, no_block[:5])

    print("\n4) 文案与档位")
    bad_txt, bad_step = [], []
    parsed = []
    for c in cards:
        txt = c["pct"]
        if not txt.startswith("资料 ") or not txt.endswith("%"):
            bad_txt.append(txt)
            continue
        try:
            n = int(txt[len("资料 "):-1])
        except ValueError:
            bad_txt.append(txt)
            continue
        parsed.append((c, n))
        if n % STEP != 0 or not (0 <= n <= 100):
            bad_step.append(txt)
    check("百分比文案形如「资料 N%」", not bad_txt, bad_txt[:3])
    check("百分比都落在 %d 的档位上（分母 %d）" % (STEP, len(PROFILE_KEYS)),
          not bad_step, bad_step[:3])

    wrong_zero = [c["name"] for c, n in parsed if (n == 0) != c["zero"]]
    check("0% 与 .zero 类一一对应（0% 要转灰，别用紫色假装有进度）",
          not wrong_zero, wrong_zero[:3])

    print("\n5) 拿 Emby 自己的条目做独立参照，核对界面显示的百分比")
    # 先直连 Emby 挑出**有资料**的几位再来对 —— 全 0% 的抽样证明不了什么
    #（Fields 漏字段时两边一致地错）。这一步才是真正守「有没有把标志字段取回来」。
    try:
        probes = emby_pick_profiled(base, tok, uid)
    except Exception as e:  # noqa: BLE001
        probes = []
        print("   直连 Emby 取样失败：%s" % e)
    if not probes:
        check("能从 Emby 里找到有资料的演员来核对", False,
              "扫了 500 条都没一条有资料 —— 无法验证非零档位")
    else:
        print("   取样：" + "、".join("%s（%d/5）" % (n, f) for n, f, _ in probes))
        mismatch = []
        for name, filled, item_id in probes:
            got = ui_pct_of(page, name, item_id)
            want = filled * STEP
            if got is None:
                mismatch.append("%s：界面上没搜到这张卡片" % name)
            elif got != want:
                mismatch.append("%s：界面 %d%% vs Emby 直查 %d%%（已填 %d/5）"
                                % (name, got, want, filled))
            else:
                print("   ✓ %-18s 界面 %3d%% = Emby 直查（已填 %d/5）"
                      % (name, got, filled))
        check("有资料的演员：界面百分比 == Emby 原始数据", not mismatch, mismatch[:3])

    return finish(results, page)


def finish(results, page):
    # 预期内的报错，不算缺陷（与 verify_javbus_probe.py 同一口径）：
    # MetaTube 没起时的 502、以及脚本里故意打的 404。
    EXPECTED = ("/api/metatube", "/api/persons/merge")
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
        print("   以下报错是预期内的，忽略：")
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
