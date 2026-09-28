# -*- coding: utf-8 -*-
"""从加密的离线演员资料库导出 CSV / JSON（**严格只读**）。

背景
----
`20260924资料库.db` 是 SQLCipher 4 加密的 SQLite 库，归另一个工具所有。
本项目**既不解析也不写入它一个字节** —— 这个脚本是唯一的桥：
它以 `SQLITE_OPEN_READONLY` 打开那个 .db，导出成 CSV；那份 CSV 放在
`data/actresses_export.csv`，由 `offlinelib.go` 用 `//go:embed` **编译进 exe 与镜像**，
在程序里当只读资料源读。导出是**一次性、手动的一步**（不是程序运行时行为），
重新导出之后必须重新构建才生效。

为什么自己用 ctypes 调 DLL
--------------------------
SQLCipher 版 SQLite 就在那个工具自己的目录里（`e_sqlcipher.dll`），
直接用系统 python 的 sqlite3 打不开加密库，装第三方 SQLCipher 又没必要 ——
ctypes 调它自带的 DLL 最省事，而且顺带保证了「读到什么就是那个工具看到的什么」。

用法
----
    # 口令从参数来（推荐），或从环境变量 EMBYME_OFFLINE_KEY 来
    python tools/sqlcipher_dump.py --db "<资料库>.db" --key "<口令>" --csv data/actresses_export.csv

    python tools/sqlcipher_dump.py --db "<资料库>.db" --key "<口令>"   # 只看加密参数/表结构/记录数
    python tools/sqlcipher_dump.py --db "<资料库>.db" --key "<口令>" --sql "SELECT id,name_ja FROM actresses LIMIT 5"

    EMBYME_OFFLINE_KEY="<口令>" python tools/sqlcipher_dump.py --db "<资料库>.db" --csv data/actresses_export.csv

⚠️ 内嵌 = 公开分发：导出到 `data/` 的那份 CSV 会进仓库，并随 Release 的 exe 与
Docker Hub 的镜像一起对外分发（见 README 的「内嵌的离线资料库」一节）。

关于口令
--------
**本脚本不内置任何口令。** 加密保护这类程序时，不管上层怎么加壳/混淆，
密钥最终都必须以明文交给原生 `sqlite3_key_v2()`（SQLCipher 内部再用它做 PBKDF2），
所以在那个文件上挂一个钩子就能拿到它 —— 但这属于用户自己对自己机器上的数据做的事，
不该被固化进一个要发到公开仓库的脚本里。
（拿到口令的具体做法见随资料库附带的逆向分析报告。）

注意
----
* 全程只读：只调 `sqlite3_open_v2(..., SQLITE_OPEN_READONLY, ...)`，
  没有任何 INSERT/UPDATE/CREATE/PRAGMA 写操作。
* 口令不对会**明确报错**，不会静默导出 0 行 —— SQLCipher 是在第一次读页时才校验的，
  不主动读一次的话，「口令错」和「这个表是空的」看起来一模一样。
"""
import argparse
import csv
import ctypes
import json
import os
import sys
from ctypes import POINTER, byref, c_char_p, c_int, c_void_p

SQLITE_OK = 0
SQLITE_ROW = 100
SQLITE_DONE = 101
SQLITE_OPEN_READONLY = 0x00000001

DEFAULT_TABLE = "actresses"


class SQLCipher:
    """一个只读的 SQLCipher 连接。"""

    def __init__(self, dll_path, db_path, key):
        self.lib = load_sqlcipher(dll_path)
        self.db = c_void_p()
        rc = self.lib.sqlite3_open_v2(db_path.encode("utf-8"), byref(self.db),
                                      SQLITE_OPEN_READONLY, None)
        if rc != SQLITE_OK:
            raise RuntimeError("打开数据库失败 rc=%d（路径对吗？）: %s" % (rc, db_path))
        # SQLCipher：open 之后必须**立刻**下 key，否则后面任何查询都是 not a database。
        rc = self.lib.sqlite3_key_v2(self.db, b"main", key, len(key))
        if rc != SQLITE_OK:
            raise RuntimeError("下 key 失败 rc=%d %s" % (rc, self.errmsg()))

    def errmsg(self):
        m = self.lib.sqlite3_errmsg(self.db)
        return m.decode("utf-8", "replace") if m else "?"

    def query(self, sql):
        """返回 (列名, 行)。任何一步出错都抛异常，不静默返回空。"""
        st = c_void_p()
        tail = c_char_p()
        rc = self.lib.sqlite3_prepare_v2(self.db, sql.encode("utf-8"), -1,
                                         byref(st), byref(tail))
        if rc != SQLITE_OK:
            raise RuntimeError("prepare 失败 rc=%d %s (SQL: %s)" % (rc, self.errmsg(), sql))
        try:
            n = self.lib.sqlite3_column_count(st)
            cols = [self.lib.sqlite3_column_name(st, i).decode("utf-8", "replace")
                    for i in range(n)]
            rows = []
            while True:
                rc = self.lib.sqlite3_step(st)
                if rc == SQLITE_ROW:
                    row = []
                    for i in range(n):
                        v = self.lib.sqlite3_column_text(st, i)
                        row.append(v.decode("utf-8", "replace") if v else None)
                    rows.append(row)
                    continue
                if rc == SQLITE_DONE:
                    break
                # 关键：**别把错误当成「读完」**。口令不对时这里会返回
                # SQLITE_NOTADB(26)，而旧写法（只判断 == SQLITE_ROW）会安静地
                # 结束循环，导出一个 0 行的文件 —— 看起来像「这个库是空的」。
                raise RuntimeError("step 失败 rc=%d %s（口令不对？这不是 SQLCipher 库？）"
                                   % (rc, self.errmsg()))
            return cols, rows
        finally:
            self.lib.sqlite3_finalize(st)

    def verify_key(self):
        """主动读一次，把「口令错」和「表是空的」区分开。"""
        self.query("SELECT count(*) FROM sqlite_master")

    def close(self):
        if self.db:
            self.lib.sqlite3_close(self.db)
            self.db = None


def load_sqlcipher(dll_path):
    lib = ctypes.WinDLL(dll_path)
    sig = {
        "sqlite3_libversion": ([], c_char_p),
        "sqlite3_open_v2": ([c_char_p, POINTER(c_void_p), c_int, c_char_p], c_int),
        "sqlite3_key_v2": ([c_void_p, c_char_p, c_void_p, c_int], c_int),
        "sqlite3_errmsg": ([c_void_p], c_char_p),
        "sqlite3_prepare_v2": ([c_void_p, c_char_p, c_int,
                                POINTER(c_void_p), POINTER(c_char_p)], c_int),
        "sqlite3_step": ([c_void_p], c_int),
        "sqlite3_column_count": ([c_void_p], c_int),
        "sqlite3_column_text": ([c_void_p, c_int], c_char_p),
        "sqlite3_column_name": ([c_void_p, c_int], c_char_p),
        "sqlite3_finalize": ([c_void_p], c_int),
        "sqlite3_close": ([c_void_p], c_int),
    }
    for name, (argtypes, restype) in sig.items():
        fn = getattr(lib, name)
        fn.argtypes = argtypes
        fn.restype = restype
    return lib


def find_dll(explicit, db_path):
    """默认去**数据库所在目录**找 e_sqlcipher.dll —— 它和 .db 是同一个工具装在一起的。"""
    if explicit:
        return explicit
    cand = os.path.join(os.path.dirname(os.path.abspath(db_path)), "e_sqlcipher.dll")
    if not os.path.exists(cand):
        raise SystemExit("找不到 e_sqlcipher.dll（默认去 .db 同目录找）。用 --dll 指定路径。")
    return cand


def read_key(args):
    key = args.key or os.environ.get("EMBYME_OFFLINE_KEY") or ""
    key = key.strip().strip('"').strip("'")   # 从文件里复制出来常带引号/换行
    if not key:
        raise SystemExit(
            "缺少口令。用 --key \"<口令>\" 或设环境变量 EMBYME_OFFLINE_KEY。\n"
            "（本脚本不内置口令：那是你机器上那份数据的密钥，不该出现在这个仓库里。）")
    return key.encode("utf-8")


def print_overview(db, table):
    print("SQLCipher:", db.lib.sqlite3_libversion().decode())
    print("\n-- 加密参数 --")
    for p in ("cipher_version", "cipher_page_size", "kdf_iter",
              "cipher_hmac_algorithm", "cipher_kdf_algorithm"):
        try:
            print("  %-22s %s" % (p, db.query("PRAGMA %s" % p)[1]))
        except RuntimeError as e:
            print("  %-22s (%s)" % (p, e))
    print("\n-- 表结构 --")
    for t, n in db.query("SELECT type,name FROM sqlite_master "
                         "WHERE sql IS NOT NULL ORDER BY type,name")[1]:
        print("  %-8s %s" % (t, n))
    print("\n-- 记录数 --")
    for t, in db.query("SELECT name FROM sqlite_master WHERE type='table'")[1]:
        print("  %-18s %s" % (t, db.query('SELECT count(*) FROM "%s"' % t)[1][0][0]))
    print("\n-- %s 的列 --" % table)
    cols, _ = db.query('SELECT * FROM "%s" LIMIT 1' % table)
    for i, c in enumerate(cols):
        print("  %2d %s" % (i, c))


def main():
    ap = argparse.ArgumentParser(description="从加密的离线演员资料库导出 CSV/JSON（只读）")
    ap.add_argument("--db", required=True, help="SQLCipher 资料库路径（.db）")
    ap.add_argument("--dll", default=None, help="e_sqlcipher.dll（默认取 .db 同目录）")
    ap.add_argument("--key", default=None, help="口令；也可用环境变量 EMBYME_OFFLINE_KEY")
    ap.add_argument("--table", default=DEFAULT_TABLE, help="要导出的表（默认 %(default)s）")
    ap.add_argument("--csv", help="导出到这个 CSV")
    ap.add_argument("--json", help="导出到这个 JSON")
    ap.add_argument("--sql", help="执行这条 SQL 并打印结果")
    ap.add_argument("--limit", type=int, default=0, help="只导出前 N 行（0 = 全部）")
    args = ap.parse_args()

    if not os.path.exists(args.db):
        raise SystemExit("找不到数据库：%s" % args.db)
    dll = find_dll(args.dll, args.db)
    db = SQLCipher(dll, args.db, read_key(args))
    try:
        db.verify_key()   # 先确认口令对，再谈导出

        if args.sql:
            cols, rows = db.query(args.sql)
            print("\t".join(cols))
            for r in rows:
                print("\t".join("" if v is None else v for v in r))
            return 0

        if args.csv or args.json:
            sql = 'SELECT * FROM "%s"' % args.table
            if args.limit > 0:
                sql += " LIMIT %d" % args.limit
            cols, rows = db.query(sql)
            if args.csv:
                # utf-8-sig：写 BOM。Go 那边会剥掉它 —— 两种都要能读，
                # 所以 `offlinelib.go` 里有专门的用例守着 BOM 的剥离。
                with open(args.csv, "w", newline="", encoding="utf-8-sig") as f:
                    w = csv.writer(f)
                    w.writerow(cols)
                    w.writerows(rows)
                print("CSV 已写出: %s (%d 行 × %d 列)" % (args.csv, len(rows), len(cols)))
            if args.json:
                with open(args.json, "w", encoding="utf-8") as f:
                    json.dump([dict(zip(cols, r)) for r in rows], f,
                              ensure_ascii=False, indent=1)
                print("JSON 已写出: %s (%d 行)" % (args.json, len(rows)))
            return 0

        print_overview(db, args.table)
        return 0
    finally:
        db.close()


if __name__ == "__main__":
    sys.exit(main())
