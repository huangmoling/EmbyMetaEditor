"""匿名校验 Docker Hub 上的镜像：确认推上去的确实是本地这份代码。

为什么要单独验：
  「Actions 绿了」只说明流水线跑完了，不说明推成功、更不说明推的是**我们的**提交 ——
  同名仓库完全可能是别人的，或者 tag 被旧的一次构建覆盖了。这里按 registry 协议
  自己拉一遍：先看是不是多架构、再看配置 blob 里的 `org.opencontainers.image.revision`
  是否等于本地 HEAD、`source` 是否指向本仓库。全部用匿名 pull token，不需要登录。

最后还会**解开 amd64 的层**，在二进制里实查内嵌的 `web/`：这正是唯一能抓出
「源码修了、镜像没重建」的断言 —— 静态检查（revision / 入口 / 非 root）在那种情况下全是绿的。

用法： python tools/verify_docker_image.py [镜像名] [tag]
默认： aag111/emby-meta-editor:latest
退出码 0 = 校验通过。

注意：`workflow_dispatch`（不带 tag）构建出来的镜像 `revision` = 触发时的 main HEAD。
所以**在这个脚本之后又提交了东西的话，本脚本会 FAIL** —— 那是真话（镜像确实落后于 HEAD），
重新触发一次构建即可。
"""
import gzip
import json
import re
import subprocess
import sys
import time
import urllib.request

REPO = sys.argv[1] if len(sys.argv) > 1 else "aag111/emby-meta-editor"
TAG = sys.argv[2] if len(sys.argv) > 2 else "latest"
REGISTRY = "https://registry-1.docker.io"
FAILED = []

ACCEPT = ", ".join([
    "application/vnd.oci.image.index.v1+json",
    "application/vnd.docker.distribution.manifest.list.v2+json",
    "application/vnd.oci.image.manifest.v1+json",
    "application/vnd.docker.distribution.manifest.v2+json",
])


def check(name, cond, detail=""):
    print(("PASS  " if cond else "FAIL  ") + name + (("  → " + str(detail)) if detail else ""))
    if not cond:
        FAILED.append(name)


def token():
    url = ("https://auth.docker.io/token?service=registry.docker.io"
           "&scope=repository:%s:pull" % REPO)
    with urllib.request.urlopen(url, timeout=30) as r:
        return json.loads(r.read())["token"]


def get(url, tok, accept=ACCEPT, tries=4):
    """带重试：blob 会 302 到 CDN（Cloudflare / CloudFront），国内直连时握手偶发超时。"""
    last = None
    for _ in range(tries):
        req = urllib.request.Request(url)
        req.add_header("Authorization", "Bearer " + tok)
        req.add_header("Accept", accept)
        try:
            with urllib.request.urlopen(req, timeout=60) as r:
                return r.read(), r.headers
        except Exception as e:  # noqa: BLE001
            last = e
            time.sleep(1.5)
    raise RuntimeError("拉取 %s 失败：%s" % (url, last))


def candidates(version):
    """镜像应当对应的提交。先看 release tag（v1.0.8），再看 HEAD。

    正常情况下两者是同一个提交；但 tag 之后还可能有 merge / 文档提交，
    那时 HEAD 已经往前走了 —— 镜像跟着 tag 走才是对的，所以两个都接受。
    """
    out = []
    refs = [("HEAD", ["git", "rev-parse", "HEAD"])]
    if version:
        refs.insert(0, ("tag v" + version, ["git", "rev-parse", "v" + version]))
    for name, args in refs:
        try:
            sha = subprocess.check_output(args, stderr=subprocess.DEVNULL,
                                          text=True).strip()
            if sha:
                out.append((name, sha))
        except Exception:
            pass
    return out


def expected_version_strings(version, revision):
    """镜像里的二进制应当带有哪个版本串（返回值 + 说明）。

    tag 构建时 metadata-action 给出的 version 就是语义版本（`1.1.0`），二进制里是 `v1.1.0`。
    **workflow_dispatch 构建时只有一个 raw 标签 `latest`** —— 拿它去拼 `vlatest` 永远找不到，
    那不是镜像的毛病。这时改从**镜像对应提交**的 `version.go` 里取常量：既避开了假失败，
    又顺带验了「镜像里的二进制确实来自那个提交的源码」。
    """
    if version and version[:1].isdigit():
        return ["v" + version], "标签 v" + version
    ref = revision or "HEAD"
    try:
        src = subprocess.check_output(["git", "show", "%s:version.go" % ref],
                                      stderr=subprocess.DEVNULL, text=True)
    except Exception:  # noqa: BLE001
        return [], "拿不到 %s 的 version.go" % ref[:12]
    m = re.search(r'appVersion\s*=\s*"([^"]+)"', src)
    if not m:
        return [], "%s 的 version.go 里没有 appVersion" % ref[:12]
    return [m.group(1)], "%s（来自镜像提交 %s 的 version.go）" % (m.group(1), ref[:12])


def layer_blob(tok, amd_manifest):
    """把 amd64 的各层解开、拼成一个大字节串（前端资源与二进制都在里面）。"""
    blob = b""
    for layer in amd_manifest.get("layers", []):
        raw, _ = get("%s/v2/%s/blobs/%s" % (REGISTRY, REPO, layer["digest"]), tok)
        try:
            blob += gzip.decompress(raw)
        except Exception:  # noqa: BLE001
            blob += raw
    return blob


def check_embedded_frontend(blob, version, revision=""):
    """确认二进制里内嵌的前端确实是修好的那一版。

    为什么必须查这个：web/ 是 `go:embed` 编进二进制的。源码修对了、镜像忘了重建，
    Actions 照样绿、镜像照样能拉、容器照样能起 —— 只有界面还是坏的。
    2026-09 的 gfriends 头像弹窗就是这么漏出去的：v1.0.8 镜像里仍是 CDN 裸外链。
    """
    i = blob.find(b"function pickAvatar")
    if i < 0:
        check("镜像里能找到内嵌的 app.js", False, "没找到 pickAvatar")
        return
    seg = blob[i:i + 2000].decode("utf-8", "replace")
    # 头像弹窗的缩略图必须走同源 /api/img（imgSrc()）。写成 CDN 裸外链的话会被
    # CSP 的 `img-src 'self'` 拦掉，界面就是一片破图 —— 而命令行验 CDN 地址全是 200，
    # 光看地址永远发现不了。
    check("候选头像走同源代理（imgSrc）", "imgSrc(en.f)" in seg)
    check("候选头像已无 CDN 裸外链", "'src=\"' + esc(en.f)" not in seg)

    want, detail = expected_version_strings(version, revision)
    if not want:
        print("  提示：%s，跳过版本串检查" % detail)
        return
    check("二进制版本串与镜像对应提交一致",
          any(w.encode() in blob for w in want), detail)


def check_embedded_backend(blob):
    """确认镜像里的**后端**也确实是这一份源码。

    revision 相等已经能推出这一点，但那是「标签说的」；这里再在二进制里实查两条
    只有这份代码才有的字面量，避免将来出现「标签对、内容不对」（比如复用旧产物）时
    静态检查全绿。改动前端资源类的功能时这条不用动，改后端常量时记得同步。

    注意查的是**源码里真实存在的字面量**：jsdelivr 的节点名（`cdn`/`gcore`/`fastly`）
    是 `fmt.Sprintf` 拼进 URL 的，二进制里根本没有 `gcore.jsdelivr.net` 这种完整串 ——
    写成那样会误报 FAIL。
    """
    for marker, what in [
        ("xinxin8816/gfriends", "gfriends 镜像仓库（CDN 容错）"),
        ("https://%s.jsdelivr.net/gh/%s@%s/", "jsdelivr 分片节点模板"),
    ]:
        check("后端含 %s" % what, marker.encode() in blob, marker)


def main():
    try:
        tok = token()
    except Exception as e:
        print("拿不到匿名 pull token：%s" % e)
        return 1

    raw, _ = get("%s/v2/%s/manifests/%s" % (REGISTRY, REPO, TAG), tok)
    manifest = json.loads(raw)
    print("  manifest mediaType = %s" % manifest.get("mediaType"))

    is_index = "manifests" in manifest
    check("是多架构镜像（image index）", is_index, manifest.get("mediaType"))
    if not is_index:
        print("\n提示：不是 index，可能是单架构镜像。")
        return 1

    plats = {}
    for m in manifest["manifests"]:
        p = m.get("platform") or {}
        key = "%s/%s" % (p.get("os"), p.get("architecture"))
        plats[key] = m["digest"]
    print("  平台 = %s" % ", ".join(sorted(plats)))
    check("包含 linux/amd64", "linux/amd64" in plats)
    check("包含 linux/arm64", "linux/arm64" in plats)

    digest = plats.get("linux/amd64")
    raw, _ = get("%s/v2/%s/manifests/%s" % (REGISTRY, REPO, digest),
                 tok, accept="application/vnd.oci.image.manifest.v1+json,"
                             "application/vnd.docker.distribution.manifest.v2+json")
    amd = json.loads(raw)

    total = sum(l.get("size", 0) for l in amd.get("layers", []))
    print("  amd64 压缩后 %.1f MB（%d 层）" % (total / 1048576.0, len(amd.get("layers", []))))

    cfg_digest = amd["config"]["digest"]
    raw, _ = get("%s/v2/%s/blobs/%s" % (REGISTRY, REPO, cfg_digest), tok)
    try:
        cfg = json.loads(raw)
    except Exception:
        cfg = json.loads(gzip.decompress(raw))

    labels = (cfg.get("config") or {}).get("Labels") or {}
    history = cfg.get("history") or []
    print("  revision = %s" % labels.get("org.opencontainers.image.revision"))
    print("  source   = %s" % labels.get("org.opencontainers.image.source"))
    print("  version  = %s" % labels.get("org.opencontainers.image.version"))

    rev = (labels.get("org.opencontainers.image.revision") or "")
    refs = candidates(labels.get("org.opencontainers.image.version") or "")
    if rev and refs:
        # revision 是完整 sha，本地 rev-parse 也是完整 sha；保险起见按前 12 位比
        ok = any(rev.startswith(sha[:12]) or sha.startswith(rev[:12]) for _, sha in refs)
        detail = " / ".join("%s=%s" % (n, s[:12]) for n, s in refs)
        check("revision 与本地提交一致", ok, "镜像 %s 本地 %s" % (rev[:12], detail))
    else:
        print("  提示：拿不到本地提交（不在 git 仓库里？），跳过一致性比对")
    src = labels.get("org.opencontainers.image.source") or ""
    check("source 指向本仓库", "EmbyMetaEditor" in src, src)

    # 入口参数：容器里必须监听 0.0.0.0 且不开浏览器
    cmd = (cfg.get("config") or {}).get("Cmd") or []
    entry = (cfg.get("config") or {}).get("Entrypoint") or []
    joined = " ".join(entry + cmd)
    print("  入口 = %s" % joined)
    check("默认监听 0.0.0.0", "0.0.0.0" in joined, joined)
    check("容器内不开浏览器", "-open=false" in joined, joined)
    check("数据目录固定为 /data", "/data" in ((cfg.get("config") or {}).get("Env") or [""])[0]
          or any(e.startswith("EMBYME_HOME=/data") for e in (cfg.get("config") or {}).get("Env") or []),
          (cfg.get("config") or {}).get("Env"))

    # 非 root 运行
    user = (cfg.get("config") or {}).get("User") or ""
    print("  User = %r" % user)
    check("以非 root 运行", user not in ("", "root", "0"))

    # 最后一道：镜像里的前后端到底是不是这一份 —— 静态检查看不出来，只有解开层才知道
    blob = b""
    try:
        blob = layer_blob(tok, amd)
    except Exception as e:  # noqa: BLE001
        print("  提示：层拉不下来（%s），跳过内嵌内容检查" % e)
    if blob:
        check_embedded_frontend(blob, labels.get("org.opencontainers.image.version") or "", rev)
        check_embedded_backend(blob)

    print("\n%s" % ("全部通过" if not FAILED else "%d 项未通过：%s" % (len(FAILED), "；".join(FAILED))))
    return 1 if FAILED else 0


if __name__ == "__main__":
    sys.exit(main())
