#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""把 README 里那几处「会过期」的数字同步成真实值。

背景：README 里散着版本号、exe 体积、md5、单测条数。这些都是**复制粘贴**进去的，
一旦忘了改就会互相矛盾 —— 最糟的是 md5：它看起来最精确，所以最容易被当成可信来源，
而它恰恰是最容易过期的那个（重建 exe 就会变）。

数据来源（都在本仓库里，不联网）：
  - 版本号：version.go 的 appVersion
  - 体积 / md5：仓库里有意跟踪的 EmbyMetaEditor.exe
  - 单测条数：跑一遍 go test -v 数 `--- PASS` / `--- SKIP`（只在带 --with-tests 时才跑）

用法：
    python tools/sync_readme.py                 # 改写 README（版本 + exe）
    python tools/sync_readme.py --with-tests    # 顺带更新单测条数（要跑一遍测试，约一分钟）
    python tools/sync_readme.py --check         # 只检查是否一致，不一致就退出码 1（CI 用）

设计要点：每个锚点都要求**恰好命中一处**。命中 0 处说明 README 的文字改过了，
脚本会直接报错而不是悄悄跳过 —— 静默不同步正是这个脚本要消灭的东西。
"""
import argparse
import hashlib
import os
import re
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
README = os.path.join(ROOT, "README.md")
EXE = os.path.join(ROOT, "EmbyMetaEditor.exe")
VERSION_GO = os.path.join(ROOT, "version.go")


def read_version():
    with open(VERSION_GO, encoding="utf-8") as f:
        m = re.search(r'appVersion\s*=\s*"([^"]+)"', f.read())
    if not m:
        raise SystemExit("读不到 version.go 里的 appVersion")
    return m.group(1)


def exe_info():
    if not os.path.isfile(EXE):
        raise SystemExit("找不到 EmbyMetaEditor.exe —— 先构建一次再同步 README")
    data = open(EXE, "rb").read()
    return len(data), hashlib.md5(data).hexdigest()


def test_counts():
    """跑一遍单测并数条数。

    数的是**顶层测试函数**（`^--- PASS` 顶格），子测试（缩进 4 空格）不计 ——
    否则加一个子测试就会让 README 的数字莫名其妙地跳。
    """
    env = dict(os.environ)
    env.setdefault("NO_PROXY", "127.0.0.1,localhost")
    r = subprocess.run(["go", "test", "-count=1", "-v", "."], cwd=ROOT,
                       capture_output=True, text=True, env=env)
    out = r.stdout or ""
    passed = len(re.findall(r"^--- PASS", out, re.M))
    skipped = len(re.findall(r"^--- SKIP", out, re.M))
    failed = len(re.findall(r"^--- FAIL", out, re.M))
    if failed or (r.returncode != 0 and not passed):
        raise SystemExit("单测没跑通，先修测试再来同步 README：\n" + (out or r.stderr)[-2000:])
    return passed, skipped, passed + skipped


def thousands(n):
    return "{:,}".format(n)


def build_rules(version, size, md5, tests=None):
    """returns [(name, pattern, replacement)] —— pattern 必须恰好命中一处。"""
    major, minor = (version.lstrip("v").split(".") + ["0", "0"])[:2]
    mb = round(size / 1048576.0, 1)
    rules = [
        ("下载行的体积",
         r"（Windows x64，约 [\d.]+ MB，无运行库依赖）",
         "（Windows x64，约 %s MB，无运行库依赖）" % mb),

        ("对齐版本与 md5",
         r"> 当前对齐 \*\*v[\d.]+\*\*：Release 与 main 的 md5 同为 `[0-9a-f]+`（[\d,]+ 字节）。",
         "> 当前对齐 **%s**：Release 与 main 的 md5 同为 `%s`（%s 字节）。"
         % (version, md5, thousands(size))),

        ("发版示例里的 tag",
         r"git tag v[\d.]+ && git push origin v[\d.]+",
         "git tag %s && git push origin %s" % (version, version)),

        ("镜像标签推导示例",
         r"镜像标签由 tag 推导：`v[\d.]+` → `[\d.]+` / `[\d.]+` / `\d+` / `latest`",
         "镜像标签由 tag 推导：`%s` → `%s` / `%s` / `%s` / `latest`"
         % (version, version.lstrip("v"), major + "." + minor, version.lstrip("v").split(".")[0])),
    ]
    if tests is not None:
        passed, skipped, total = tests
        rules.append((
            "单测条数",
            r"`go test \./\.\.\.` 共 \*\*\d+ 个用例\*\*(（通过 \d+，跳过 \d+）)?",
            "`go test ./...` 共 **%d 个用例**（通过 %d，跳过 %d）" % (total, passed, skipped)))
    return rules


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--check", action="store_true", help="只检查，不写入；不一致退出码 1")
    ap.add_argument("--with-tests", action="store_true", help="顺带重算单测条数（要跑一遍测试）")
    args = ap.parse_args()

    version = read_version()
    size, md5 = exe_info()
    tests = test_counts() if args.with_tests else None
    rules = build_rules(version, size, md5, tests)

    with open(README, encoding="utf-8") as f:
        text = f.read()
    original = text

    problems = []
    for name, pat, repl in rules:
        hits = len(re.findall(pat, text))
        if hits != 1:
            problems.append("%s：锚点命中 %d 处（应为 1，README 的文字可能改过了）" % (name, hits))
            continue
        text = re.sub(pat, lambda _m, r=repl: r, text, count=1)

    if problems:
        for p in problems:
            print("FAIL  " + p)
        return 1

    if text == original:
        print("README 已经是最新的（版本 %s，%s 字节，md5 %s）" % (version, thousands(size), md5))
        if tests is not None:
            print("  单测：通过 %d，跳过 %d，合计 %d" % tests)
        return 0

    if args.check:
        print("FAIL  README 与实际不一致 —— 跑一下 `python tools/sync_readme.py` 同步：")
        for i, (a, b) in enumerate(zip(original.splitlines(), text.splitlines())):
            if a != b:
                print("  第 %d 行" % (i + 1))
                print("    - " + a.strip())
                print("    + " + b.strip())
        return 1

    with open(README, "w", encoding="utf-8", newline="\n") as f:
        f.write(text)
    print("README 已同步：版本 %s，%s 字节（约 %s MB），md5 %s"
          % (version, thousands(size), round(size / 1048576.0, 1), md5))
    if tests is not None:
        print("  单测：通过 %d，跳过 %d，合计 %d" % tests)
    return 0


if __name__ == "__main__":
    sys.exit(main())
