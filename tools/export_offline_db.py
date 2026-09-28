#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""读「离线演员资料库」并导出成 EmbyMetaEditor 能吃的 JSON。

背景
----
那个 `20260924资料库.db` 是 **SQLCipher 加密**的：
  - 文件头不是 `SQLite format 3`，整文件字节熵 ≈ 8.0（真加密，不是异或混淆）；
  - 体积 12906496 B = 4096 x 3151，正好是整数页 —— SQLite 家族的典型特征；
  - 口令硬编码在打包/混淆过的 `Emby演员扩展器.exe` 里（连它自己的配置键名
    `OfflineDatabaseEnabled` 都在二进制里搜不到，说明字符串被保护了），
    静态提取不出来，所以**只能拿口令来试**。
  - 同目录的 `e_sqlcipher.dll` 就是它用的那个原生 SQLCipher，本脚本直接加载它，
    因此「口令对不对」是**权威结论**，不是自己猜的。

本脚本对原始 .db **严格只读**：一律用 SQLITE_OPEN_READONLY 打开，永远不写回。

用法
----
  # 1) 先看看这个文件到底能不能读
  python tools/export_offline_db.py --db "F:/.../20260924资料库.db"

  # 2) 有口令了：验证 + 打印库结构
  python tools/export_offline_db.py --db "..." --key "口令"

  # 3) 验证通过后导出（原库不动，只写新文件）
  python tools/export_offline_db.py --db "..." --key "口令" --out offline_library.json

  # 4) 没有口令：拿一份候选词表 + 指定文件里的可打印串去爆破（很慢，量力而行）
  python tools/export_offline_db.py --db "..." --crack --crack-from "F:/.../Emby演员扩展器.exe"

退出码：0 成功；2 口令不对/读不出来；3 用法或环境问题。
"""

import argparse
import ctypes
import json
import os
import re
import sys
import time
from ctypes import CFUNCTYPE, POINTER, c_char_p, c_int, c_void_p

SQLITE_OK = 0
SQLITE_ROW = 100

SQLITE_OPEN_READONLY = 0x00000001
SQLITE_OPEN_READWRITE = 0x00000002
SQLITE_OPEN_CREATE = 0x00000004
SQLITE_OPEN_URI = 0x00000040

# 真正的动态库名（Windows 上 SQLitePCLRaw 把它叫 e_sqlcipher）。
DLL_NAMES = ["e_sqlcipher.dll", "sqlcipher.dll", "libsqlcipher.so", "libsqlcipher.dylib"]


def log(*a):
    print(*a, flush=True)


# ---------------------------------------------------------------- 动态库加载

def find_dll(explicit, db_path):
    """按优先级找 SQLCipher 动态库；找不到就返回 None。

    默认会去**数据库所在目录**找 —— 那份资料库就是从那个目录里读的，
    `e_sqlcipher.dll` 本来就躺在旁边。
    """
    cands = []
    if explicit:
        cands.append(explicit)
    env = os.environ.get("SQLCIPHER_DLL")
    if env:
        cands.append(env)
    if db_path:
        d = os.path.dirname(os.path.abspath(db_path))
        for n in DLL_NAMES:
            cands.append(os.path.join(d, n))
    for n in DLL_NAMES:
        cands.append(os.path.join(os.getcwd(), n))
    cands.extend(DLL_NAMES)
    for c in cands:
        if c and os.path.exists(c):
            return os.path.abspath(c)
    return None


class SQLCipher(object):
    """把 ctypes 那点样板收在这儿。"""

    def __init__(self, dll_path):
        self.path = dll_path
        try:
            os.add_dll_directory(os.path.dirname(dll_path))
        except Exception:
            pass
        self.lib = ctypes.CDLL(dll_path)
        L = self.lib
        L.sqlite3_libversion.restype = c_char_p
        L.sqlite3_open_v2.argtypes = [c_char_p, POINTER(c_void_p), c_int, c_char_p]
        L.sqlite3_open_v2.restype = c_int
        L.sqlite3_open.argtypes = [c_char_p, POINTER(c_void_p)]
        L.sqlite3_open.restype = c_int
        L.sqlite3_close.argtypes = [c_void_p]
        L.sqlite3_close.restype = c_int
        L.sqlite3_errmsg.argtypes = [c_void_p]
        L.sqlite3_errmsg.restype = c_char_p
        L.sqlite3_exec.argtypes = [c_void_p, c_char_p, c_void_p, c_void_p, POINTER(c_char_p)]
        L.sqlite3_exec.restype = c_int
        self.has_key = hasattr(L, "sqlite3_key")
        if self.has_key:
            L.sqlite3_key.argtypes = [c_void_p, c_char_p, c_int]
            L.sqlite3_key.restype = c_int
        self._cb = CFUNCTYPE(c_int, c_void_p, c_int, POINTER(c_char_p), POINTER(c_char_p))
        self._cb_ref = self._cb(lambda ud, n, vals, cols: 0)  # 占位，防 GC

    def version(self):
        try:
            return self.lib.sqlite3_libversion().decode("utf-8", "replace")
        except Exception:
            return "?"

    def open_ro(self, path):
        db = c_void_p()
        flags = SQLITE_OPEN_READONLY | SQLITE_OPEN_URI
        rc = self.lib.sqlite3_open_v2(path.encode("utf-8"), ctypes.byref(db), flags, None)
        if rc != SQLITE_OK:
            # 个别构建没有 open_v2：退回 open（注意它会**创建**不存在的文件，
            # 所以调用前一定要确认路径存在 —— 本脚本在 main 里保证了这点）。
            db2 = c_void_p()
            rc = self.lib.sqlite3_open(path.encode("utf-8"), ctypes.byref(db2))
            db = db2
        return db, rc

    def close(self, db):
        try:
            self.lib.sqlite3_close(db)
        except Exception:
            pass

    def errmsg(self, db):
        try:
            return self.lib.sqlite3_errmsg(db).decode("utf-8", "replace")
        except Exception:
            return ""

    def set_key(self, db, key):
        if not self.has_key:
            return -1
        if isinstance(key, str):
            key = key.encode("utf-8")
        return self.lib.sqlite3_key(db, key, len(key))

    def exec_sql(self, db, sql, want_rows=False):
        """执行一条 SQL。返回 (rc, errmsg, rows, cols)。

        ⚠️ 列名只能从**有返回行**的查询里拿：sqlite3_exec 的回调是按行调的，
        一行都没有就永远不会被调用（所以 `SELECT * ... LIMIT 0` 拿列名是不行的）。
        """
        rows, cols = [], []
        err = c_char_p()
        if want_rows:
            def _cb(ud, ncol, vals, cnames):
                if not cols and ncol:
                    for i in range(ncol):
                        cols.append((cnames[i] or b"").decode("utf-8", "replace"))
                rows.append([(vals[i] or b"").decode("utf-8", "replace") for i in range(ncol)])
                return 0
            cb = self._cb(_cb)
        else:
            cb = self._cb(lambda ud, n, v, c: 0)
        rc = self.lib.sqlite3_exec(db, sql.encode("utf-8"), cb, None, ctypes.byref(err))
        msg = err.value.decode("utf-8", "replace") if err.value else ""
        return rc, msg, rows, cols

    def try_key(self, path, key):
        """用某个口令开一次库并读一下 sqlite_master。返回 (ok, msg)。

        ⚠️ 口令**必须**在第一次读之前设进去：SQLCipher 是在读第一页时才校验的，
        先 SELECT 再 PRAGMA key 是无效的。
        """
        db, rc = self.open_ro(path)
        if rc != SQLITE_OK:
            self.close(db)
            return False, "打不开文件"
        if self.has_key and key is not None:
            k = key if isinstance(key, (str, bytes)) else key
            rck = self.set_key(db, k)
            if rck != SQLITE_OK:
                self.close(db)
                return False, "sqlite3_key 返回 %d" % rck
        rc, msg, _, _ = self.exec_sql(db, "SELECT count(*) FROM sqlite_master", want_rows=True)
        self.close(db)
        if rc == SQLITE_OK:
            return True, msg
        return False, (msg or ("rc=%d" % rc))


# ---------------------------------------------------------------- 各功能

def cmd_probe(sc, db_path, key):
    log("== %s ==" % db_path)
    size = os.path.getsize(db_path)
    with open(db_path, "rb") as f:
        head = f.read(16)
    log("  体积    : %d 字节%s" % (size, "（%d 页）" % (size // 4096) if size % 4096 == 0 else ""))
    log("  文件头  : %s" % head.hex())
    log("  SQLite? : %s" % ("是" if head.startswith(b"SQLite format 3") else "否 —— 被加密或不是 SQLite"))
    log("  SQLCipher 版本: %s" % sc.version())

    if key is None:
        ok, msg = sc.try_key(db_path, None)
        log("  不带口令打开: %s%s" % ("成功（未加密）" if ok else "失败", "" if ok else " —— " + msg))
        if not ok:
            log("")
            log("  → 这个库需要口令。带上 --key 再试；没有口令就先看 --crack。")
        return 0 if ok else 2

    ok, msg = sc.try_key(db_path, key)
    if not ok:
        log("  口令「%s」: 不对（%s）" % (key, msg))
        return 2
    log("  口令「%s」: **正确**" % key)
    return 0


def cmd_schema(sc, db_path, key):
    db, rc = sc.open_ro(db_path)
    if rc != SQLITE_OK:
        log("打不开：%s" % sc.errmsg(db))
        return 2
    if key is not None:
        sc.set_key(db, key)
    rc, msg, rows, _ = sc.exec_sql(
        db,
        "SELECT type, name, sql FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name",
        want_rows=True)
    if rc != SQLITE_OK:
        log("读 sqlite_master 失败：%s" % msg)
        sc.close(db)
        return 2
    log("== 库结构 ==")
    for typ, name, sql in rows:
        n = 0
        rc2, _, r2, _ = sc.exec_sql(db, 'SELECT count(*) FROM "%s"' % name.replace('"', '""'), want_rows=True)
        if rc2 == SQLITE_OK and r2:
            n = r2[0][0]
        log("  [%s] %-28s %s 行" % (typ, name, n))
        if sql:
            for line in str(sql).splitlines():
                log("        " + line.strip())
    sc.close(db)
    return 0


def cmd_dump(sc, db_path, key, out_path, limit):
    db, rc = sc.open_ro(db_path)
    if rc != SQLITE_OK:
        log("打不开：%s" % sc.errmsg(db))
        return 2
    if key is not None:
        sc.set_key(db, key)
    rc, msg, tables, _ = sc.exec_sql(
        db,
        "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name",
        want_rows=True)
    if rc != SQLITE_OK:
        log("列不出表：%s" % msg)
        sc.close(db)
        return 2

    dump = {
        "Version": 1,
        "Source": "离线演员资料库",
        "ExportedUtc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "Origin": {"file": os.path.basename(db_path), "bytes": os.path.getsize(db_path)},
        "Tables": {},
    }
    for (tname,) in tables:
        q = tname.replace('"', '""')
        sql = 'SELECT * FROM "%s"' % q
        if limit:
            sql += " LIMIT %d" % limit
        rc2, msg2, rows, cols = sc.exec_sql(db, sql, want_rows=True)
        if rc2 != SQLITE_OK:
            log("  跳过表 %s：%s" % (tname, msg2))
            continue
        log("  表 %-28s %d 行 × %d 列" % (tname, len(rows), len(cols)))
        dump["Tables"][tname] = {"Columns": cols, "Rows": rows}
    sc.close(db)

    # Entries 走启发式映射：列名不认识就原样留 Raw，等看过真实结构再改映射。
    entries = []
    for tname, t in dump["Tables"].items():
        mapped = map_rows(t)
        if mapped:
            entries.extend(mapped)
    dump["Entries"] = entries
    dump["Note"] = ("Entries 是启发式映射的结果（列名->字段）。第一次导出后请人工核对一次，"
                    "如需调整映射改 map_rows()。Tables 里保留了原样行，映射改了可以重新生成，"
                    "不必再读一次加密库。")

    with open(out_path, "w", encoding="utf-8", newline="\n") as f:
        json.dump(dump, f, ensure_ascii=False, indent=1)
    log("")
    log("已写出 %s（%d 字节，%d 张表，%d 条映射条目）"
        % (out_path, os.path.getsize(out_path), len(dump["Tables"]), len(entries)))
    return 0


# 列名 -> 我们的字段。**故意写得宽松**：不同版本的资料库列名可能不一样，
# 认不出来就只留 Raw，宁缺勿错（错映射会把 A 的简介写到 B 头上）。
COL_PATTERNS = [
    ("Name",      r"^(name|name_jp|name_jpn|japanese|actress|actor|名前|演员|艺名|艺名|名称)$"),
    ("Aliases",   r"^(alias|aliases|alias_names|別名|别名|旧艺名|旧名|other_?names?)$"),
    ("Overview",  r"^(overview|bio|biography|desc|description|introduction|简介|介绍|人物简介)$"),
    ("BirthDate", r"^(birth_?date|birthday|dob|生年月日|出生日期|生日)$"),
    ("BirthPlace", r"^(birth_?place|birthplace|hometown|出身|出生地|籍贯)$"),
    ("Height",    r"^(height|身長|身高)$"),
    ("Bust",      r"^(bust|bust_?size|胸围|胸囲)$"),
    ("Waist",     r"^(waist|腰围|腰囲)$"),
    ("Hip",       r"^(hip|hips|臀围|ヒップ)$"),
    ("Cup",       r"^(cup|cup_?size|罩杯)$"),
    ("BloodType", r"^(blood|blood_?type|血型)$"),
    ("DebutDate", r"^(debut|debut_?date|出道|出道日)$"),
    ("Agency",    r"^(agency|company|事务所|经纪公司)$"),
    ("Tags",      r"^(tags?|标签|標籤)$"),
]


def _norm_col(c):
    return re.sub(r"[^0-9a-zA-Z_\u4e00-\u9fff\u3040-\u30ff]", "_", str(c)).strip("_").lower()


def map_rows(t):
    """把一张表按列名映射成条目。第一列能当名字用才产出条目。"""
    cols = t.get("Columns") or []
    rows = t.get("Rows") or []
    if not rows:
        return []
    # Columns 没抓到时按第一行长度兜底 —— 但那样就没有列名，映射无从谈起。
    if not cols:
        return []
    name_i = -1
    field_of = {}
    for i, c in enumerate(cols):
        n = _norm_col(c)
        for field, pat in COL_PATTERNS:
            if re.match(pat, n, re.I):
                if field == "Name" and name_i < 0:
                    name_i = i
                else:
                    field_of.setdefault(field, i)
                break
    if name_i < 0:
        return []
    out = []
    for r in rows:
        if name_i >= len(r):
            continue
        nm = (r[name_i] or "").strip()
        if not nm:
            continue
        e = {"Name": nm, "Raw": dict(zip(cols, r))}
        for field, i in field_of.items():
            if i < len(r) and r[i] not in (None, ""):
                v = r[i]
                if field == "Aliases":
                    e[field] = [x.strip() for x in re.split(r"[,\|\uff0c;、]", v) if x.strip()]
                elif field == "Tags":
                    e[field] = [x.strip() for x in re.split(r"[,\|\uff0c;、]", v) if x.strip()]
                else:
                    e[field] = v
        out.append(e)
    return out


# ---------------------------------------------------------------- 爆破

def collect_candidates(files, extra):
    """从文件里捡可打印串当候选口令。ASCII 与 UTF-16LE 两种都捡。"""
    cands = []
    seen = set()

    def add(s):
        s = s.strip()
        if not s or s in seen:
            return
        if not (4 <= len(s) <= 96):
            return
        seen.add(s)
        cands.append(s)

    for f in files:
        try:
            b = open(f, "rb").read()
        except Exception as e:
            log("  跳过 %s：%s" % (f, e))
            continue
        for m in re.finditer(rb"[\x20-\x7e]{4,96}", b):
            add(m.group().decode("ascii", "ignore"))
        for m in re.finditer(rb"(?:[\x20-\x7e]\x00){4,96}", b):
            add(m.group().decode("utf-16-le", "ignore"))
        for m in re.finditer(rb"(?:[\x20-\x7e]\x00|\x00[\x20-\x7e]){4,96}", b):
            try:
                add(m.group().decode("utf-16-le", "ignore"))
            except Exception:
                pass
    for s in extra:
        add(s)
    return cands


def cmd_crack(sc, db_path, files, extra, limit):
    cands = collect_candidates(files, extra)
    if limit:
        cands = cands[:limit]
    log("候选口令 %d 条，开始试（每条一次 PBKDF2，很慢）…" % len(cands))
    t0 = time.time()
    for i, c in enumerate(cands, 1):
        ok, _ = sc.try_key(db_path, c)
        if ok:
            log("")
            log("*** 命中口令：%r ***" % c)
            log("用时 %.1fs，第 %d/%d 条" % (time.time() - t0, i, len(cands)))
            return 0
        if i % 50 == 0:
            log("  …%d/%d，已用 %.0fs" % (i, len(cands), time.time() - t0))
    log("试完 %d 条，没命中。" % len(cands))
    return 2


# ---------------------------------------------------------------- main

BUILTIN = [
    "", "emby", "Emby", "embyactor", "EmbyActor", "EmbyActorManager",
    "演员扩展器", "Emby演员扩展器", "资料库", "离线资料库", "offline", "offline_db",
    "20260924", "123456", "12345678", "password", "admin", "sqlite", "sqlcipher",
]


def main(argv=None):
    ap = argparse.ArgumentParser(description="读 SQLCipher 加密的离线演员资料库（对原文件只读）")
    ap.add_argument("--db", required=True, help="资料库 .db 路径")
    ap.add_argument("--key", default=None, help="口令；给了就验证并读结构")
    ap.add_argument("--out", default=None, help="导出 JSON 的路径")
    ap.add_argument("--schema", action="store_true", help="只打印库结构")
    ap.add_argument("--limit", type=int, default=0, help="导出时每表最多导多少行（0=全部）")
    ap.add_argument("--crack", action="store_true", help="用候选词表爆破口令")
    ap.add_argument("--crack-from", action="append", default=[], help="从这些文件里捡候选串（可多次）")
    ap.add_argument("--crack-limit", type=int, default=0, help="最多试多少条候选")
    ap.add_argument("--dll", default=None, help="SQLCipher 动态库路径")
    args = ap.parse_args(argv)

    db_path = os.path.abspath(args.db)
    if not os.path.exists(db_path):
        log("找不到文件：%s" % db_path)
        return 3

    dll = find_dll(args.dll, db_path)
    if not dll:
        log("找不到 SQLCipher 动态库。用 --dll 指定，或设 SQLCIPHER_DLL，")
        log("或把 e_sqlcipher.dll 放到资料库旁边。")
        return 3
    log("SQLCipher 库：%s" % dll)
    sc = SQLCipher(dll)

    if args.crack:
        return cmd_crack(sc, db_path, args.crack_from, BUILTIN, args.crack_limit)

    if args.key is None:
        return cmd_probe(sc, db_path, None)

    if cmd_probe(sc, db_path, args.key) != 0:
        return 2
    cmd_schema(sc, db_path, args.key)
    if args.out:
        return cmd_dump(sc, db_path, args.key, args.out, args.limit)
    return 0


if __name__ == "__main__":
    sys.exit(main())
