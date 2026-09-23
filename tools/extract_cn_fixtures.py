#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""从 cache/debug/ 里落盘的真实响应中，**逐字节**截出国产传媒搜索结果容器，
写进 testdata/cn/ 供 Go 单测当夹具。

为什么不用手写夹具：HTML5 的树构造规则会改写"不完整"的片段
（游离的 <tr>/<td> 会被丢弃），手写夹具和线上不一致时会出现
"单测全绿、线上全挂"。所以夹具一律从真实响应里截，一个字节都不改。

用法：python tools/extract_cn_fixtures.py
"""
import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DEBUG = os.path.join(ROOT, "cache", "debug")
OUT = os.path.join(ROOT, "testdata", "cn")

# 这些标签是空元素，不需要配对
VOID = {"area", "base", "br", "col", "embed", "hr", "img", "input", "link",
        "meta", "param", "source", "track", "wbr"}

TAG_RE = re.compile(r"<(/?)([a-zA-Z][a-zA-Z0-9]*)((?:[^>\"']|\"[^\"]*\"|'[^']*')*)>")


def slice_element(html, start_idx):
    """从 start_idx（指向某个开始标签的 '<'）起，截出该元素完整的一段。"""
    depth = 0
    for m in TAG_RE.finditer(html, start_idx):
        tag = m.group(2).lower()
        if tag in VOID:
            continue
        if m.group(1):  # 结束标签
            depth -= 1
            if depth == 0:
                return html[start_idx:m.end()]
        else:
            depth += 1
    raise ValueError("标签没有闭合")


def find_marker(html, marker):
    i = html.find(marker)
    if i < 0:
        raise ValueError("找不到标记：" + marker)
    return i


def extract(src, marker, dst, note):
    path = os.path.join(DEBUG, src)
    with open(path, encoding="utf-8", errors="replace") as f:
        html = f.read()
    i = find_marker(html, marker)
    frag = slice_element(html, i)
    os.makedirs(OUT, exist_ok=True)
    with open(os.path.join(OUT, dst), "w", encoding="utf-8", newline="") as f:
        f.write(frag)
    print("%-34s %6d 字节  (%s)" % (dst, len(frag.encode("utf-8")), note))
    return frag


def main():
    jobs = [
        ("xchina_s.html", '<div class="tab-container">',
         "xchina_search_91cm014.html", "xchina.co 搜 91CM-014，1 条精确命中"),
        ("madouqu_search.html", '<div class="row posts-wrapper">',
         "madouqu_search_91cm014.html", "madouqu.com 搜 91CM-014，模糊返回 91CM074/084/094"),
        ("madou_club_r.html", '<article class="excerpt excerpt-c5">',
         "madou_club_search.html", "madou.club 搜索结果首条（MDHG0010）"),
        ("7mmtv_ssni.html", "<div class=\"row content\">",
         "7mmtv_search_ssni989.html", "7mmtv.sx 搜 SSNI-989，3 条"),
    ]
    missing = [j for j in jobs if not os.path.exists(os.path.join(DEBUG, j[0]))]
    if missing:
        print("缺少原始转储：" + ", ".join(m[0] for m in missing))
        print("先从站点抓一份落盘到 cache/debug/ 再跑这个脚本。")
        return 2
    for j in jobs:
        extract(*j)
    print("\n夹具已写入 " + OUT)
    return 0


if __name__ == "__main__":
    sys.exit(main())
