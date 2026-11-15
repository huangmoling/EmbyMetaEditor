#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""把上游的演员姓名映射表拉下来，覆盖 data/actor_mapping.xml（内嵌基线）。

为什么要这个脚本：`data/actor_mapping.xml` 是 **//go:embed** 打进 exe 与镜像的
（见 actormapping.go），所以「更新」有两个层次 ——

  1. **运行期更新**：界面「设置 → 姓名映射表 → 从上游更新」，落在
     `cache/actor_mapping.xml`，立即生效、不用重启。日常用这个。
  2. **出厂基线更新**（本脚本）：把最新数据换进仓库，跟着下一个版本发出去。
     换了之后**必须重新构建**，否则产物里还是旧数据。

两者都要有：只做 1 的话，新装的用户拿到的是发布当时的快照；只做 2 的话，
用户想更新就得等下一个版本。

上游：github.com/li-peifeng/Jav-Actors-Mapping 的 mapping_actor.xml（精简表，
只有 zh_cn / zh_tw / jp / keyword 四个属性）。注意上游 README 写明该数据
「仅适用于 AVdb」，本项目按「随产物分发」使用 —— 这是使用方明确接受的取舍，
边界写在 README 与 actormapping.go 的文件头。

校验（都通过才写盘，避免把错误页/半截文件当成数据）：
  - 能按 XML 解析，根节点是 actor 或 actor-mapping；
  - <a> 条目数 ≥ 10000（上游实测 2.6 万条，这条线拦的是数量级不对）。

用法：
    python tools/fetch_actor_mapping.py              # jsdelivr → raw.githubusercontent 依次尝试
    python tools/fetch_actor_mapping.py --url <URL>  # 用指定地址（自建镜像 / 本地文件）
    python tools/fetch_actor_mapping.py --check      # 只比对本地与上游是否一致，不写盘
"""
import argparse
import hashlib
import os
import sys
import urllib.request
import xml.etree.ElementTree as ET

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
OUT = os.path.join(ROOT, "data", "actor_mapping.xml")

# 与 actormapping.go 的 actorMappingUpstreamURLs 保持一致（jsdelivr 优先：国内可达性更好）。
UPSTREAMS = [
    "https://cdn.jsdelivr.net/gh/li-peifeng/Jav-Actors-Mapping@main/mapping_actor.xml",
    "https://raw.githubusercontent.com/li-peifeng/Jav-Actors-Mapping/main/mapping_actor.xml",
]

MIN_ENTRIES = 10000
UA = ("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
      "(KHTML, like Gecko) Chrome/124.0 Safari/537.36")


def fetch(url):
    req = urllib.request.Request(url, headers={
        "User-Agent": UA,
        "Accept": "application/xml,text/xml,*/*",
    })
    with urllib.request.urlopen(req, timeout=60) as r:
        return r.read()


def validate(raw, where):
    """解析并核对条目数；返回 (条目数, 键数)。不通过就抛异常。"""
    if raw.startswith(b"\xef\xbb\xbf"):
        raw = raw[3:]
    try:
        root = ET.fromstring(raw)
    except ET.ParseError as e:
        raise SystemExit(f"{where}：不是合法的 XML（{e}）—— 多半是错误页或半截文件")
    if root.tag not in ("actor", "actor-mapping"):
        raise SystemExit(f"{where}：根节点是 <{root.tag}>，期望 <actor> 或 <actor-mapping>")
    rows = root.findall("a")
    if len(rows) < MIN_ENTRIES:
        raise SystemExit(f"{where}：只有 {len(rows)} 条 <a>（阈值 {MIN_ENTRIES}）—— 拒绝覆盖")
    for r in rows:
        if not (r.get("jp") or "").strip():
            raise SystemExit(f"{where}：有一条记录没有 jp 属性，格式可能变了")
    keys = set()
    for r in rows:
        for n in [r.get("zh_cn"), r.get("zh_tw"), r.get("jp")] + (r.get("keyword") or "").split(","):
            if n and n.strip():
                keys.add(n.strip())
    return len(rows), len(keys)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--url", help="指定地址（默认依次试 jsdelivr / raw.githubusercontent）")
    ap.add_argument("--check", action="store_true", help="只比对，不写盘")
    args = ap.parse_args()

    local = None
    if os.path.isfile(OUT):
        local = open(OUT, "rb").read()
        local_entries, _ = validate(local, "本地 data/actor_mapping.xml")
        print(f"本地：{local_entries} 条，{len(local)} 字节，md5 {hashlib.md5(local).hexdigest()}")

    urls = [args.url] if args.url else UPSTREAMS
    raw = None
    for u in urls:
        print(f"尝试 {u} …")
        try:
            raw = fetch(u)
        except Exception as e:  # 网络问题换下一个源，不直接退出
            print(f"  失败：{e}")
            continue
        entries, keys = validate(raw, u)
        print(f"  成功：{entries} 条，{keys} 个写法，{len(raw)} 字节")
        break
    if raw is None:
        raise SystemExit("所有上游地址都失败了")

    if args.check:
        same = local is not None and local == raw
        print("本地已是最新" if same else "本地与上游**不一致**（可去掉 --check 更新）")
        return 0 if same else 1

    if local == raw:
        print("内容完全相同，不写盘。")
        return 0

    # 逐字节写入：数据要原样进 //go:embed，别让 Python 的换行/编码转换插一脚。
    tmp = OUT + ".tmp"
    with open(tmp, "wb") as f:
        f.write(raw)
    os.replace(tmp, OUT)
    print(f"已写入 {os.path.relpath(OUT, ROOT)}（{len(raw)} 字节，md5 {hashlib.md5(raw).hexdigest()}）")
    print("⚠️ 数据是编译期定死的：**必须重新构建** EmbyMetaEditor.exe 与镜像才生效。")
    print("   go build -trimpath -buildvcs=false -ldflags \"-s -w\" -o EmbyMetaEditor.exe .")
    return 0


if __name__ == "__main__":
    sys.exit(main())
