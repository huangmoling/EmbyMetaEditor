"""前端一致性自检：不打开浏览器，静态比对 HTML / JS / 后端路由。

检查这几件事：
  1. web/app.js 语法（交给 node --check，本脚本只做正则层）
  2. app.js 里 $() 引用的元素 id 是否真的存在（静态 + JS 动态生成的都算）
  3. app.js 调用的 /api/... 路径是否都在 api.go 注册过
  4. 自己插进页面的 <img> 是否都走同源地址（外链会破图 + 泄露 Referer）
  5. 剪贴板是否统一走 copyText（非安全上下文没有 navigator.clipboard）
  6. 资料面板的「搜索用名字」是否真的进了请求体

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
    # 两种写法都要认：api('/api/x') 和直接拼字符串 '<img src="/api/x?u=...">'。
    # 后者带查询串，所以先按引号取整段，再剥掉 ?query / #hash。
    raw_called = set(re.findall(r"""['"](/api/[^'"`\s]+)['"]""", js))
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
    # 服务端的 CSP 允许外部图片（img-src 里有 http/https，见 auth.go），但**我们自己
    # 插进页面的图**仍然只能来自 imgSrc / embyImg / personImg：直连外链会带上 Referer、
    # 泄露 Emby 地址，而且 javbus 这类图床有防盗链，直连 403、经服务端代取才 200。
    # （CSP 那条只放给第三方塞进来的图，比如磁力预览工具。）
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

    print(f"\n{'全部通过' if bad == 0 else str(bad) + ' 项未通过'}")
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
