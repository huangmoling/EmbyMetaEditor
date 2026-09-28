"""前端一致性自检：不打开浏览器，静态比对 HTML / JS / 后端路由。

检查这几件事：
  1. web/app.js 语法（交给 node --check，本脚本只做正则层）
  2. app.js 里 $() 引用的元素 id 是否真的存在（静态 + JS 动态生成的都算）
  3. app.js 调用的 /api/... 路径是否都在 api.go 注册过
  4. 自己插进页面的 <img> 是否都走同源地址（外链会破图 + 泄露 Referer）
  5. 剪贴板是否统一走 copyText（非安全上下文没有 navigator.clipboard）
  6. 资料面板的「搜索用名字」是否真的进了请求体
  7. 预演（dry-run）的按钮 → dry=true → dry_run 是否全线接通
  8. 条目写入历史面板是否真的被拉取、回滚是否走二次确认
  9. 人物归并是否「先预演、再点两次」才可能写
 10. 磁力多源（javbus / javdb）的源开关与地址是否真的进了请求体
 11. 诊断包入口是否明确承诺脱敏

用法： python tools/check_frontend.py
退出码 0 = 全过。
"""
import os
import re
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def read(rel):
    with open(os.path.join(ROOT, rel), encoding="utf-8") as f:
        return f.read()


def collect_ids(html, js):
    """静态 id="x" + JS 模板里的 id="x" + JS 里的 el.id = 'x'。"""
    static = set(re.findall(r'\bid="([^"]+)"', html))
    tpl = set(re.findall(r"""id=\\?["']([A-Za-z0-9_-]+)\\?["']""", js))
    assign = set(re.findall(r"""\.id\s*=\s*['"]([A-Za-z0-9_-]+)['"]""", js))
    return static, tpl, assign


def route_matches(path, routes):
    """把前端路径和后端注册的模式对上，支持 {id} 占位符与拼接路径。"""
    for r in routes:
        if r == path:
            return True
        # /api/jobs/{id}   vs  前端拼接出的 "/api/jobs/"
        if r.startswith(path) and path.endswith("/"):
            rest = r[len(path):]
            if rest.startswith("{"):
                return True
        # 占位符整段匹配
        if "{" in r and re.fullmatch(re.sub(r"\{[^}]+\}", "[^/]+", r), path):
            return True
    return False


def find_node():
    """找一个能用的 node：环境变量 NODE_BIN > PATH > 常见安装位置。"""
    import shutil
    cand = [os.environ.get("NODE_BIN"), shutil.which("node")]
    cand += [
        r"C:\Program Files\nodejs\node.exe",
        os.path.expanduser(r"~\.workbuddy-ai\binaries\node"),
    ]
    for c in cand:
        if not c:
            continue
        if os.path.isfile(c):
            return c
        # 目录的话，往下找一层 node.exe
        if os.path.isdir(c):
            for root, _, files in os.walk(c):
                if "node.exe" in files:
                    return os.path.join(root, "node.exe")
    return None


def main():
    html = read("web/index.html")
    js = read("web/app.js")
    api = read("api.go")
    bad = 0

    # --- 1. JS 语法 ---
    node = find_node()
    if not node:
        print("SKIP  app.js 语法（找不到 node，设 NODE_BIN 环境变量）")
    else:
        r = subprocess.run([node, "--check", os.path.join(ROOT, "web/app.js")],
                           capture_output=True, text=True)
        if r.returncode == 0:
            print("PASS  app.js 语法")
        else:
            print("FAIL  app.js 语法：\n" + (r.stderr or r.stdout))
            bad += 1

    # --- 2. 元素 id ---
    static, tpl, assign = collect_ids(html, js)
    known = static | tpl | assign
    used = set(re.findall(r"""\$\(['"]#([A-Za-z0-9_-]+)['"]\)""", js))
    missing = sorted(used - known)
    print(f"      静态 id {len(static)} + 模板生成 {len(tpl)} + 赋值生成 {len(assign)}"
          f"，app.js 引用 {len(used)} 个")
    if missing:
        print("FAIL  app.js 引用了不存在的 id：" + ", ".join(missing))
        bad += 1
    else:
        print("PASS  元素 id 全部对得上")

    # --- 3. 接口路径 ---
    routes = set(re.findall(
        r'mux\.HandleFunc\("(?:GET|POST|PUT|DELETE) ([^"]+)"', api))
    # 三种写法都要认：api('/api/x')、直接拼字符串 '<img src="/api/x?u=...">'，
    # 以及 index.html 里的普通链接（比如诊断包那个 <a href="/api/diag/bundle">——
    # 它走的是浏览器下载，不经过 fetch，但同样是前端在调后端）。
    # 后面那种带查询串，所以先按引号取整段，再剥掉 ?query / #hash。
    raw_called = set(re.findall(r"""['"](/api/[^'"`\s]+)['"]""", js))
    raw_called |= set(re.findall(r"""['"](/api/[^'"`\s]+)['"]""", html))
    called = {p.split("?")[0].split("#")[0] for p in raw_called}
    bad_paths = sorted(c for c in called if not route_matches(c, routes))
    print(f"      后端注册 {len(routes)} 条路由，前端调用 {len(called)} 个路径")
    if bad_paths:
        print("FAIL  前端调用了后端没有的路径：" + ", ".join(bad_paths))
        bad += 1
    else:
        print("PASS  接口路径全部对得上")

    # --- 4. 后端注册了但前端没用的路由（仅提示，不算失败）---
    unused = sorted(r for r in routes if not any(
        route_matches(c, {r}) for c in called))
    if unused:
        print("提示  后端这些路由前端没直接调用（可能是给命令行/curl 用的）：")
        for u in unused:
            print("      " + u)

    # --- 5. img 模板必须走同源地址 ---
    # CSP 的 img-src 是 `'self' data: blob:`（见 auth.go），所以**我们自己插进页面的
    # 图**只能来自 imgSrc / embyImg / personImg。直连外链不止会被拦成破图，还会带上
    # Referer 泄露 Emby 地址；而且 javbus 这类图床有防盗链 —— 直连 403、经服务端代取才 200。
    # （v1.4.0 曾把 img-src 放开到 http/https 来「修」磁力预览，方向是错的：
    #  防盗链不是 CSP 能解决的。现在预览走 /api/javbus/samples + /api/img，CSP 已收回。）
    proxies = ("imgSrc(", "embyImg(", "personImg(")
    # 允许先算好再引用（missCard 里的 `const src = imgSrc(m.cover)` 就是这种写法）。
    proxied_vars = set(re.findall(
        r"(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*=\s*(?:imgSrc|embyImg|personImg)\(", js))
    bad_imgs = []
    for m in re.finditer(r"<img\b[^>]*>", js):
        tag = m.group(0)
        if "src=" not in tag:
            continue  # 没有 src 的占位 <img>，不需要过代理
        if any(p in tag for p in proxies):
            continue
        if any(("esc(" + v + ")") in tag for v in proxied_vars):
            continue
        bad_imgs.append(tag[:90].replace("\n", " "))
    print(f"      检查 {len(re.findall(r'<img', js))} 个 img 模板，代理变量 {len(proxied_vars)} 个")
    if bad_imgs:
        print("FAIL  这些 <img> 没走同源地址，会被 CSP 拦成破图：")
        for t in bad_imgs:
            print("      " + t)
        bad += 1
    else:
        print("PASS  img 模板全部走同源地址")

    # --- 6. 剪贴板只能走 copyText ---
    # navigator.clipboard 是**安全上下文**才提供的 API：本机 http://127.0.0.1:8097 有，
    # 但 Docker / NAS 上常见的 http://<局域网IP>:8097 没有。在那里直接调它会在 onclick
    # 里抛 TypeError，没人接 —— 连「复制失败」的提示都没有，用户看到的就是「点了没反应」。
    # 所以除了 copyText 内部那一次（它带存在性判断 + execCommand 回退），别处不许直接碰。
    code_only = "\n".join(
        ln for ln in js.splitlines()
        if not ln.strip().startswith(("//", "*", "/*")))
    m = re.search(r"function copyText\([\s\S]*?(?=\nfunction |\Z)", code_only)
    body = m.group(0) if m else ""
    leaks = [ln.strip()[:90] for ln in code_only.replace(body, "").splitlines()
             if "navigator.clipboard" in ln]
    print(f"      copyText 内 navigator.clipboard 出现 {body.count('navigator.clipboard')} 次，"
          f"其余地方 {len(leaks)} 次")
    if not body:
        print("FAIL  app.js 里找不到 copyText —— 复制必须走这个统一入口")
        bad += 1
    elif leaks:
        print("FAIL  这些地方绕过了 copyText，非安全上下文下会静默失效：")
        for t in leaks:
            print("      " + t)
        bad += 1
    elif "navigator.clipboard &&" not in body or "execCommand" not in js:
        print("FAIL  copyText 缺少「先判断 API 是否存在」或「execCommand 回退」，"
              "Docker 上依然复制不了")
        bad += 1
    else:
        print("PASS  剪贴板调用都走 copyText，且带回退")

    # --- 7. 搜索用名字必须进请求体 ---
    # 资料面板顶上的「搜索用名字」是个文本输入框，这类改动最容易出的错是
    # **界面能改、请求里没带**：用户改完名字点「抓取资料」，发出去的还是 Emby 里的
    # 原名，而界面上一点异常都看不出来（对照表照常渲染，只是命中的还是原来那个人）。
    # 所以静态钉死：组装入参的 profileBody 里必须出现 search_name。
    m = re.search(r"function profileBody\([\s\S]*?\n\}", code_only)
    pb = m.group(0) if m else ""
    print(f"      profileBody {'找到' if pb else '没找到'}，search_name 出现 {pb.count('search_name')} 次")
    if not pb:
        print("FAIL  app.js 里找不到 profileBody —— 抓取/写入入参的组装入口")
        bad += 1
    elif "search_name" not in pb:
        print("FAIL  profileBody 没带 search_name：「搜索用名字」改了也发不出去，静默失效")
        bad += 1
    else:
        print("PASS  搜索用名字进了请求体")

    # --- 8. 预演（dry-run）的接线必须完整 ---
    # 服务端的 dry-run 早就实现了，界面从来没人传 —— 这个功能白放了好几个版本。
    # 这类「后端支持、界面没接」的失效和 §7 是同一个病：按钮点下去看着正常，
    # 实际上正在**不可逆地写**（Emby 的 POST /Items/{id} 是整对象替换，没有历史版本）。
    # 所以把整条链静态钉死：按钮存在 → 绑到 dry=true → 请求体带 dry_run。
    dry_problems = []
    for bid, fn in (("lbDry", "runLbBatch"), ("cnDry", "cnScrapeSelected")):
        if f'id="{bid}"' not in html:
            dry_problems.append(f"index.html 缺少预演按钮 #{bid}")
    for fn in ("runLbBatch", "cnScrapeSelected"):
        m = re.search(r"async function " + fn + r"\([\s\S]*?\n\}", code_only)
        body = m.group(0) if m else ""
        if not body:
            dry_problems.append(f"app.js 找不到 {fn}")
        elif "dry_run" not in body:
            dry_problems.append(f"{fn} 的请求体没带 dry_run —— 预演按钮会真的写入")
    if not re.search(r"runLbBatch\(true\)", code_only):
        dry_problems.append("#lbDry 没有绑到 runLbBatch(true)")
    if not re.search(r"cnScrapeSelected\(true\)", code_only):
        dry_problems.append("#cnDry 没有绑到 cnScrapeSelected(true)")
    if "function showScrapePreview" not in code_only:
        dry_problems.append("app.js 缺少只读的 showScrapePreview（预演结果没法展示）")
    print(f"      预演接线问题 {len(dry_problems)} 处")
    if dry_problems:
        for p in dry_problems:
            print("FAIL  " + p)
        bad += 1
    else:
        print("PASS  预演按钮 → dry=true → dry_run 全线接通")

    # --- 9. 条目写入历史（可回滚）必须真的被拉取 ---
    # 服务端每次写条目之前都会存快照，但快照摆在那里没人看就等于没有。
    # 这条防的是「面板写好了、切到设置页却没触发加载」—— 界面上一片空白，
    # 用户会以为「这个功能没做」，而实际是入口没接上。
    hist_problems = []
    if 'id="stHistList"' not in html:
        hist_problems.append("index.html 缺少历史面板 #stHistList")
    if 'id="stHistReload"' not in html:
        hist_problems.append("index.html 缺少「刷新」按钮 #stHistReload")
    if "function loadItemHistory" not in code_only:
        hist_problems.append("app.js 缺少 loadItemHistory")
    if "loadItemHistory()" not in re.sub(r"function loadItemHistory", "", code_only):
        hist_problems.append("loadItemHistory 没有被调用（切到设置页时面板是空的）")
    if not re.search(r"\$\('#stHistReload'\)\.onclick", code_only):
        hist_problems.append("#stHistReload 没有绑定刷新")
    if "function rollbackItemWrite" not in code_only:
        hist_problems.append("app.js 缺少 rollbackItemWrite")
    # 回滚必须走二次确认：window.confirm() 在无头浏览器里会卡死，这个项目一律不用它
    if "window.confirm" in code_only:
        hist_problems.append("用了 window.confirm()（无头浏览器里会卡死，改成「点两次」）")
    print(f"      条目历史接线问题 {len(hist_problems)} 处")
    if hist_problems:
        for p in hist_problems:
            print("FAIL  " + p)
        bad += 1
    else:
        print("PASS  条目写入历史面板接通，回滚走二次确认")

    # --- 10. 人物归并：必须「先预演、再点两次」才可能写 ---
    # 归并是不可逆的：它会把一批作品的演职员表改挂到另一个人名下，再删掉
    # 多余的人物条目。Emby 没有版本历史，错了只能靠条目快照逐条还原 People，
    # 而被删的那个人物条目不会自己回来。所以这里把「不会误触」当硬约束钉死：
    # 归并按钮默认禁用 → 预演过才解锁 → 换「保留谁」立刻重新锁上。
    mg = []
    if 'data-view="merge"' not in html:
        mg.append("导航里没有 data-view=\"merge\"")
    for eid in ("mgScan", "mgMinScore", "mgBody", "mgSummary"):
        if f'id="{eid}"' not in html:
            mg.append(f"index.html 缺少 #{eid}")
    if "'merge'" not in code_only and '"merge"' not in code_only:
        mg.append("VIEW_TITLES / switchView 里没有 merge")
    for fn in ("loadDuplicates", "renderDuplicates", "bindMergeCard", "renderMergePlan"):
        if f"function {fn}" not in code_only:
            mg.append(f"app.js 缺少 {fn}")
    if not re.search(r"\$\('#mgScan'\)\.onclick", code_only):
        mg.append("#mgScan 没有绑定查重")
    if "api('/api/persons/duplicates" not in code_only:
        mg.append("查重没走 /api/persons/duplicates")
    if "api('/api/persons/merge" not in code_only:
        mg.append("归并没走 /api/persons/merge")
    # 预演那一次必须显式发 dry_run:true，否则「预演」按钮就是一个写入按钮
    if not re.search(r"dry_run:\s*true", code_only):
        mg.append("归并的预演没带 dry_run: true —— 点「预演」会直接改 Emby")
    # 归并按钮初始必须是禁用的
    if 'data-act="merge" disabled' not in code_only:
        mg.append("归并按钮不是默认禁用 —— 没看预演就能直接写")
    # 换「保留谁」之后必须重新禁用 + 清掉已武装状态
    if not re.search(r"btnM\.disabled = true", code_only):
        mg.append("换「保留谁」后没有重新禁用归并按钮（会拿着 A 的预演去点 B 的归并）")
    if "btnM.dataset.armed" not in code_only:
        mg.append("归并没有走「点两次」确认")
    if "window.confirm" in code_only:
        mg.append("用了 window.confirm()（无头浏览器里会卡死，改成「点两次」）")
    print(f"      人物归并接线问题 {len(mg)} 处")
    if mg:
        for p in mg:
            print("FAIL  " + p)
        bad += 1
    else:
        print("PASS  人物归并：查重只读、归并强制预演 + 点两次")

    # --- 11. 磁力多源：源开关与地址必须真的进请求体 ---
    # 「界面能改、请求里没带」是这个项目最典型的一类静默失效（§7 也是它）：
    # 设置页加了开关、读回来也回显正常，但保存时没塞进 body，于是永远用默认值。
    # 这里静态钉死 magnet_sources 必须在保存请求里出现。
    msrc = []
    for eid in ("stSrcJb", "stSrcJdb", "stJdb", "stJdbCookie"):
        if f'id="{eid}"' not in html:
            msrc.append(f"index.html 缺少 #{eid}")
    if "magnet_sources" not in code_only:
        msrc.append("app.js 里没有 magnet_sources —— 源开关改了也存不下来（静默失效）")
    if "javdb_url" not in code_only:
        msrc.append("app.js 里没有 javdb_url —— javdb 地址改了也存不下来")
    if "javdb_cookie" not in code_only:
        msrc.append("app.js 里没有 javdb_cookie")
    if "function renderMagnetSources" not in code_only:
        msrc.append("app.js 缺少 renderMagnetSources（源状态没处显示）")
    if "/api/magnets/sources" not in code_only:
        msrc.append("诊断没走 /api/magnets/sources")
    # 磁力列表要显示每条的来源，否则合并之后分不清哪条是哪个站给的
    if "m.source" not in code_only:
        msrc.append("磁力行没有显示来源（合并后分不清哪条来自哪个站）")
    print(f"      磁力多源接线问题 {len(msrc)} 处")
    if msrc:
        for p in msrc:
            print("FAIL  " + p)
        bad += 1
    else:
        print("PASS  磁力多源：源开关/地址进请求体，列表标出来源")

    # --- 12. 诊断包入口必须写着「已脱敏」---
    # 这个包是**要被贴到公开 issue 里**的。界面上如果只说「下载诊断包」，
    # 用户不知道里面有配置，就不敢点；反过来说，如果哪天脱敏被去掉而文案没改，
    # 用户会以为它是安全的。所以「入口存在」和「文案承诺脱敏」一起守。
    diag_problems = []
    if 'id="stDiag"' not in html:
        diag_problems.append("index.html 缺少诊断包入口 #stDiag")
    if "api/diag/bundle" not in html:
        diag_problems.append("诊断包入口没指向 /api/diag/bundle")
    a_tag = re.search(r"<a[^>]*id=\"stDiag\"[^>]*>", html or "")
    if not a_tag or "download" not in a_tag.group(0):
        diag_problems.append("#stDiag 没有 download 属性（会变成在页面里打开 zip）")
    diag_card = re.search(r"<h3>诊断</h3>[\s\S]{0,600}?</div>\s*</div>", html or "")
    card_txt = diag_card.group(0) if diag_card else ""
    if "脱敏" not in card_txt:
        diag_problems.append("诊断卡片上没有说明「密钥已脱敏」")
    if "不含" not in card_txt:
        diag_problems.append("诊断卡片上没有说明包里不含什么（用户才敢贴出去）")
    print(f"      诊断包接线问题 {len(diag_problems)} 处")
    if diag_problems:
        for p in diag_problems:
            print("FAIL  " + p)
        bad += 1
    else:
        print("PASS  诊断包入口接通，且明确承诺脱敏")

    # --- 13. 别名落盘：抓取（人工采用）之后必须把计数同步回界面 ---
    # 需求是「人工采用或成功同步后，把已确认的旧艺名别名持久化到本地」。后端两个
    # 入口都接了（单测盯着），界面这一层另有一个容易漏的点：抓完不刷新计数，用户会看到
    # 面板上写着「已记入 N 条」而侧栏总数还是旧值 —— 看起来就像根本没生效。
    # 这类「后端做了、界面没反映」和 §7/§11 的静默失效是同一族，一并静态钉死。
    alias_problems = []
    if "function refreshAliasCount" not in code_only:
        alias_problems.append("app.js 缺少 refreshAliasCount —— 抓取后没法把别名组数同步回界面")
    grab = re.search(r"async function fetchProfileInto\([\s\S]*?(?=\nasync function |\nfunction |\Z)",
                     code_only)
    grab_body = grab.group(0) if grab else ""
    if not grab_body:
        alias_problems.append("app.js 里找不到 fetchProfileInto —— 无法确认「抓取资料」这条路径")
    elif "refreshAliasCount(" not in grab_body:
        alias_problems.append("「抓取资料」成功后没调用 refreshAliasCount（人工采用这条路径的计数不会更新）")
    if "alias_memo" not in code_only:
        alias_problems.append("app.js 里没有 alias_memo —— 面板不会显示「已记入别名记忆」")
    if "写入成功后会记进别名记忆" in code_only:
        alias_problems.append("还留着旧文案「写入成功后会记进别名记忆」—— 抓取时就已经落盘了")
    if 'id="pfAliasN"' not in html:
        alias_problems.append("index.html 缺少 #pfAliasN —— 别名记忆组数没地方显示")
    print(f"      别名落盘接线问题 {len(alias_problems)} 处")
    if alias_problems:
        for p in alias_problems:
            print("FAIL  " + p)
        bad += 1
    else:
        print("PASS  别名落盘：抓取成功即落盘并把组数同步回界面")

    # --- 14. 离线资料库接线（设置页的开关与路径必须真的进请求体） ---
    # 需求是「离线资料库只读、启用后是演员资料第一优先级、不默认用它的头像」。
    # 后端与单测盯着「只读」和「排最前」那两条；界面这层要守的仍是 §7 那个老坑：
    # 设置页加了输入框、用户勾了也填了，但 saveSettings 没把它塞进 body ——
    # 界面回一句「设置已保存」，下次打开却是关的。光看界面复现不出来，只能静态钉死。
    off_problems = []
    saver = re.search(r"async function saveSettings\(\)[\s\S]*?(?=\nasync function |\nfunction |\Z)",
                      code_only)
    saver_body = saver.group(0) if saver else ""
    if not saver_body:
        off_problems.append("app.js 里找不到 saveSettings —— 无法确认离线库配置会不会被提交")
    else:
        if "offline_db_enabled" not in saver_body:
            off_problems.append("saveSettings 没提交 offline_db_enabled —— 开关保存不了")
        if "offline_db_path" not in saver_body:
            off_problems.append("saveSettings 没提交 offline_db_path —— 路径保存不了")
        if "$('#stOffOn')" not in saver_body:
            off_problems.append("saveSettings 读的不是 #stOffOn —— 提交的值和用户看到的开关无关")
        if "$('#stOffPath')" not in saver_body:
            off_problems.append("saveSettings 读的不是 #stOffPath —— 提交的路径和用户填的无关")
        # 保存后必须**重画设置页**：loadConfig() 只回填登录页那几个字段，设置页是切到
        # 「设置」时才画的。不重画的话，刚勾上「启用」+ 填了路径 + 点了保存，底下那行状态
        # 还是「未启用。」—— 用户的第一反应是「没保存成功」，然后再点一次。
        # （这条是 2026-09-28 由 tools/verify_offline_lib.py 抓出来的真缺陷。）
        if "fillSettings(" not in saver_body:
            off_problems.append("saveSettings 保存后没重画设置页 —— 开关/状态行会停在保存前的样子")
    filler = re.search(r"(?:async )?function fillSettings\([\s\S]*?(?=\nasync function |\nfunction |\Z)",
                       code_only)
    fill_body = filler.group(0) if filler else ""
    if not fill_body:
        off_problems.append("app.js 里找不到 fillSettings —— 打开设置页回填不了")
    else:
        if "$('#stOffOn').checked" not in fill_body:
            off_problems.append("fillSettings 没回填 #stOffOn —— 打开设置页开关永远是关的")
        if "stOffPath" not in fill_body:
            off_problems.append("fillSettings 没回填 #stOffPath —— 路径显示不出来")
        if "refreshOfflineStat(" not in fill_body:
            off_problems.append("fillSettings 没刷新状态行 —— 用户看不到读到几条、读不到是为什么")
    for need in ('id="stOffOn"', 'id="stOffPath"', 'id="stOffStat"'):
        if need not in html:
            off_problems.append("index.html 缺少 " + need)
    # 这个库是加密的、别人的、只读的，而且**不参与头像**（ActorFacts 里根本没有图片字段）。
    # 卡片上必须把这两点说清楚：否则用户不敢开（怕我们改了他的 .db），
    # 或者开了之后以为头像会从这里换掉。
    off_card = re.search(r"<h3>\s*离线演员资料库\s*</h3>[\s\S]{0,1600}?</div>\s*</div>", html or "")
    off_txt = off_card.group(0) if off_card else ""
    if not off_txt:
        off_problems.append("设置页找不到「离线演员资料库」卡片")
    else:
        if "只读" not in off_txt or "写入" not in off_txt:
            off_problems.append("卡片没说清「只读、不写回原库」")
        if "头像" not in off_txt:
            off_problems.append("卡片没说清「不提供头像，头像走独立顺序」")
    print(f"      离线资料库接线问题 {len(off_problems)} 处")
    if off_problems:
        for p in off_problems:
            print("FAIL  " + p)
        bad += 1
    else:
        print("PASS  离线资料库：开关/路径进请求体、能回填，文案讲清只读与头像")

    print(f"\n{'全部通过' if bad == 0 else str(bad) + ' 项未通过'}")
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
