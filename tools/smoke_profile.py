"""演员资料管理（抓取 / 只填空白写回 / 回滚）的冒烟验证。

为什么单独一支：
  「只填空白」和「写完能回滚」是这套功能仅有的两条安全承诺，而它们都是**在
  真实数据上**才看得出来的 —— 夹具里 Emby 值是空的，怎么写都「对」。所以这里
  要连真的 Emby：先预览，把「Emby 现有值 → 是否可写」逐字段对一遍；再在
  PROFILE_LIVE 下真的写一次、再回滚，比对写前写后是否逐字节一致。

默认**只读**（预览 / 历史 / 报错路径）。要跑写回+回滚那段：

    PROFILE_LIVE=1 python tools/smoke_profile.py

跑之前：
  1. `EMBYME_AUTH_PASSWORD=test-pass EmbyMetaEditor.exe -port 8097 -open=false`
     （用真实 config.json，要能连上 Emby 和 av-db.net）
  2. 本机若挂着 http_proxy，记得把 127.0.0.1 和 Emby 的内网地址放进 no_proxy。

退出码 0 = 全过。
"""
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

BASE = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:8097"
LIVE = os.environ.get("PROFILE_LIVE") == "1"

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import wbauth  # noqa: E402

COOKIE = {"Cookie": wbauth.cookie_header(BASE)}

# 资料写入只碰这 5 个字段，回滚的比对也只比这 5 个。
#
# **没有 tags**：实测这个 Emby 构建对 Person 不保存 Tags（POST 返回 204，但详情、
# 列表、TagItems、全局 /Tags 字典全都读不回来），所以服务端把各源的标签并进简介，
# 不再把它当可写字段。这里跟着排除，免得断言去比一个永远为空的字段。
PROFILE_KEYS = ["overview", "premiere_date", "production_year",
                "production_locations", "provider_ids"]

# 候选演员名。脚本按顺序试，挑第一个「能被源命中 + 确实有字段可写」的。
#
# 为什么需要「确实有字段可写」：这个库里绝大多数演员的资料早已被旧的
# 「Emby演员扩展器」填满，预览出来 write_count 是 0 —— 拿它做写回验证会
# 什么都没写就「成功」。前三个是实测在库里同时满足两个条件的。
CANDIDATES = ["玉木くるみ", "心花ゆら", "広瀬みやび",
              "江上しほ", "神ユキ", "月見若葉", "初川南", "葵つかさ", "三上悠亜"]

FAILED = []


def check(name, cond, detail=""):
    if cond:
        print("PASS  " + name)
    else:
        print("FAIL  " + name + (("  → " + str(detail)) if detail else ""))
        FAILED.append(name)


def req(method, path, body=None, timeout=180):
    data = None
    headers = dict(COOKIE)
    if body is not None:
        data = json.dumps(body).encode("utf-8")
        headers["Content-Type"] = "application/json"
    r = urllib.request.Request(BASE + path, data=data, method=method, headers=headers)
    try:
        with urllib.request.urlopen(r, timeout=timeout) as resp:
            return resp.status, json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as e:
        raw = e.read().decode("utf-8", "replace")
        try:
            return e.code, json.loads(raw)
        except Exception:
            return e.code, {"raw": raw[:300]}


def get(path, timeout=180):
    return req("GET", path, None, timeout)


def post(path, body, timeout=180):
    return req("POST", path, body, timeout)


def data(status, js):
    return js.get("data") if status == 200 else None


def find_person(name):
    st, js = get("/api/persons?" + urllib.parse.urlencode(
        {"q": name, "start": 0, "limit": 5, "missing_image": "false"}))
    if st != 200:
        return None
    for it in (js.get("data") or {}).get("items") or []:
        if it.get("Name") == name:
            return it
    return None


def preview(pid, name, sources=None):
    st, js = post("/api/profile/preview", {
        "person_id": pid or "", "name": name,
        "sources": sources or [], "use_alias_memo": True,
    })
    return st, data(st, js)


def fields_of(prof):
    return {f["key"]: f for f in (prof or {}).get("fields") or []}


def main():
    mode = "写回 + 回滚（PROFILE_LIVE=1）" if LIVE else "只读"
    print("== 演员资料冒烟 · %s · %s ==\n" % (BASE, mode))

    # ---------- 1. 资料源 ----------
    print("1) 资料源清单")
    st, js = get("/api/profile/sources")
    src = data(st, js)
    check("GET /api/profile/sources 200", st == 200, js)
    if not src:
        return 1
    keys = [s["key"] for s in src.get("sources") or []]
    check("至少注册了 3 个资料源", len(keys) >= 3, keys)
    check("批量写入策略固定为 only_blank", src.get("write_strategy") == "only_blank",
          src.get("write_strategy"))
    check("单卡勾选写入策略为 as_picked（勾了就写，含覆盖）",
          src.get("picked_write_strategy") == "as_picked", src.get("picked_write_strategy"))
    print("     源：%s" % "、".join("%s(%s)" % (s["label"], s["key"]) for s in src.get("sources") or []))

    # ---------- 2. 挑一个真实演员 ----------
    print("\n2) 从库里挑一个真实演员做预览")
    target = None
    for name in CANDIDATES:
        p = find_person(name)
        if not p:
            print("     %-10s 库里没有，跳过" % name)
            continue
        st, prof = preview(p["Id"], p["Name"])
        if st != 200:
            print("     %-10s 预览失败：%s" % (name, prof))
            continue
        if not prof.get("sources"):
            print("     %-10s 各源都没命中，跳过" % name)
            continue
        print("     %-10s 命中 %s，可写 %d 个字段" % (name, prof["sources"], prof.get("write_count") or 0))
        target = p
        target_prof = prof
        break
    check("找到一个至少能被一个源命中的演员", target is not None)
    if not target:
        print("\n（库里没有候选演员 —— 预览与写入相关断言无法进行）")
        print("\n" + ("全部通过" if not FAILED else "有 %d 项失败：%s" % (len(FAILED), FAILED)))
        return 1 if FAILED else 0

    # ---------- 3. 只填空白：整份预览的不变量 ----------
    print("\n3) 预览：逐字段核对「只填空白」")
    f = fields_of(target_prof)
    check("返回了全部 %d 个受管字段" % len(PROFILE_KEYS),
          set(f) == set(PROFILE_KEYS), sorted(f))
    check("不含写不进去的 tags 字段", "tags" not in f, sorted(f))

    bad = [k for k, v in f.items() if v["emby_value"] and v["will_write"]]
    check("Emby 已有值的字段一律 will_write=false", not bad, bad)

    bad = [k for k, v in f.items() if v["will_write"] and not v["value"]]
    check("will_write 的字段必定有抓取值", not bad, bad)

    bad = [k for k, v in f.items() if v["will_write"] and v["emby_value"]]
    check("will_write 的字段在 Emby 里必定为空", not bad, bad)

    n = sum(1 for v in f.values() if v["will_write"])
    check("write_count 与实际可写字段数一致", (target_prof.get("write_count") or 0) == n,
          "write_count=%s 实际=%d" % (target_prof.get("write_count"), n))

    # 「已有值 + 本次抓到」= 界面上可勾选覆盖的那一批。
    # 默认不写（只填空白），但必须**能被勾选** —— 这是新加的能力，
    # 至少要有样本，否则 live 阶段的覆盖验证会静默跳过。
    over = [k for k, v in f.items() if v["value"] and v["emby_value"]]
    check("可覆盖字段默认不写入（勾选才覆盖）",
          not [k for k in over if f[k]["will_write"]], over)
    print("     其中 %d 个字段可勾选覆盖：%s" % (len(over), "、".join(over) or "无"))

    for k in PROFILE_KEYS:
        v = f[k]
        print("     %-20s emby=%-26s got=%-26s %s" % (
            v["label"], repr(v["emby_value"])[:26], repr(v["value"])[:26],
            "→ 将写入" if v["will_write"] else ("— 跳过" if v["value"] else "· 未抓到")))

    # ---------- 4. 预览是只读的 ----------
    print("\n4) 预览不写历史、不改 Emby")
    st, js = get("/api/profile/history?limit=1")
    hist_after_preview = len((data(st, js) or {}).get("records") or [])
    st2, prof2 = preview(target["Id"], target["Name"])
    st, js = get("/api/profile/history?limit=1")
    hist_now = len((data(st, js) or {}).get("records") or [])
    check("连续预览不会产生同步历史", hist_after_preview == hist_now, "%d -> %d" % (hist_after_preview, hist_now))
    snap_a = {k: v["emby_value"] for k, v in fields_of(target_prof).items()}
    snap_b = {k: v["emby_value"] for k, v in fields_of(prof2).items()}
    check("两次预览读到的 Emby 现有值一致（预览没写库）", snap_a == snap_b,
          {k: (snap_a[k], snap_b[k]) for k in snap_a if snap_a[k] != snap_b[k]})

    # ---------- 5. 报错路径 ----------
    print("\n5) 报错路径（必须明确报错，不能假装成功）")
    st, js = post("/api/profile/preview", {"person_id": "", "name": ""})
    check("空名字预览 → 400", st == 400, (st, js))

    st, js = post("/api/profile/apply", {"person_id": "", "name": target["Name"]})
    check("空 ID 写入 → 400", st == 400, (st, js))

    st, js = post("/api/profile/batch", {"names": ["这个演员根本不存在xyz-9f3"]})
    check("批量只给不存在的名字 → 400（不给空 ID 的任务）", st == 400, (st, js))

    st, js = post("/api/profile/rollback", {"id": "no-such-record"})
    check("回滚不存在的记录 → 报错", st >= 400, (st, js))

    # ---------- 6. 真写 + 回滚 ----------
    if not LIVE:
        print("\n6) 写回 + 回滚  —— 跳过（设 PROFILE_LIVE=1 才跑，会真的改一次 Emby 再还原）")
        print("6b) 勾选覆盖 + 回滚  —— 跳过（同上）")
        print("7) 批量（items 带 ID）  —— 跳过（同上）")
    else:
        run_live(target, target_prof)
        run_live_overwrite(target, target_prof)
        run_live_batch(target)

    print("\n" + ("全部通过" if not FAILED else "有 %d 项失败：%s" % (len(FAILED), FAILED)))
    return 1 if FAILED else 0


def run_live(target, prof):
    """真的写一次再滚回去，比对写前写后是否一致。

    这是整套功能唯一会动用户库的地方，也是唯一能证明「回滚真的有用」的办法：
    回滚后再预览一次，6 个受管字段必须和写之前**逐个相同**。
    """
    pid, name = target["Id"], target["Name"]
    before = {k: v["emby_value"] for k, v in fields_of(prof).items()}
    keys = [k for k, v in fields_of(prof).items() if v["will_write"]]

    print("\n6) 写回 + 回滚（真实写入 %d 个字段：%s）" % (len(keys), keys))
    if not keys:
        print("     这个演员没有可写字段，改用「不勾选/无字段」路径验证")
        st, js = post("/api/profile/apply", {"person_id": pid, "name": name, "keys": []})
        check("无可写字段时明确回报，而不是静默成功",
              st == 200 and "没有需要写入" in (data(st, js) or {}).get("message", ""),
              (st, js))
        st, js = post("/api/profile/batch", {"items": [{"id": pid, "name": name}]})
        ok = st == 200
        check("批量接口接受 items（带 ID）形态", ok, (st, js))
        if ok:
            jid = data(st, js)["job_id"]
            fin = wait_job(jid)
            check("任务正常结束", fin.get("status") == "done", fin.get("status"))
            msgs = " ".join(l["message"] for l in fin.get("logs") or [])
            check("任务日志里没有「缺少演员 ID」", "缺少演员 ID" not in msgs, msgs[:400])
        return

    st, js = post("/api/profile/apply", {"person_id": pid, "name": name, "keys": keys})
    res = data(st, js)
    check("写入接口 200", st == 200, (st, js))
    if not res:
        return
    check("写入了期望的字段数", len(res.get("written") or []) == len(keys),
          (res.get("written"), keys))
    check("返回了 record_id（界面靠它回滚）", bool(res.get("record_id")), res)

    rid = res.get("record_id")
    st, js = get("/api/profile/history?limit=50")
    recs = (data(st, js) or {}).get("records") or []
    hit = [r for r in recs if r.get("id") == rid]
    check("刚才那次写入出现在同步历史里", bool(hit), rid)
    if hit:
        check("历史里记着改了哪些字段", bool(hit[0].get("changed")), hit[0])

    st, prof_mid = preview(pid, name)
    mid = {k: v["emby_value"] for k, v in fields_of(prof_mid).items()}
    check("写入确实生效（Emby 现有值变了）", mid != before,
          "写入后与写入前相同 —— 说不定什么都没写")

    st, js = post("/api/profile/rollback", {"id": rid})
    check("回滚接口 200", st == 200, (st, js))

    st, prof_after = preview(pid, name)
    after = {k: v["emby_value"] for k, v in fields_of(prof_after).items()}
    diff = {k: (before[k], after[k]) for k in before if before[k] != after[k]}
    check("回滚后每个受管字段都逐个还原", not diff, diff)

    st, js = get("/api/profile/history?limit=50")
    recs = (data(st, js) or {}).get("records") or []
    hit = [r for r in recs if r.get("id") == rid]
    check("历史记录被打上已回滚标记", bool(hit) and hit[0].get("rolled_back"), hit)

    st, js = post("/api/profile/rollback", {"id": rid})
    check("同一条记录不能回滚第二次", st >= 400, (st, js))


def norm_val(key, val):
    """把抓取值和 Emby 存回来的值拉到同一个尺度上再比。

    为什么不能直接比字符串：Emby 会把日期规范化（写进去 `1996-04-22`，读回来是
    `1996-04-22T00:00:00.0000000Z`），年份同理；`provider_ids` 更是**合并写**
    （新值写进去，原有的 MetaTube / gfriends 行都还留着）。这不是 bug，是 Emby 的
    存储语义 —— 断言得按语义比，否则满屏假 FAIL。
    """
    s = (val or "").strip()
    if key == "premiere_date":
        return s[:10]
    return s


def run_live_overwrite(target, prof):
    """把「已有值也被本次抓到的」那批字段显式勾选写入，验覆盖确实生效且能回滚。

    为什么单列一节：批量路径的承诺是「只填空白」，而单卡勾选是**唯一**能改动已有值的
    入口 —— 它的风险也最高（写坏了就是真覆盖用户数据），所以必须逼着它走一遍
    「真的改了 → 真的能逐字段还原」。没有可覆盖样本时明确跳过而不是假装通过。
    """
    pid, name = target["Id"], target["Name"]
    f0 = fields_of(prof)
    before = {k: v["emby_value"] for k, v in f0.items()}
    over = [k for k, v in f0.items() if v["value"] and v["emby_value"]]

    print("\n6b) 勾选覆盖 + 回滚（对已有值强制写入：%s）" % (over or "无可覆盖字段"))
    if not over:
        print("     这个演员没有「已有值 + 本次抓到」的字段，覆盖路径无法验证 —— 换个候选演员再跑")
        return

    labels = {k: f0[k]["label"] for k in over}
    want = {k: f0[k]["value"] for k in over}
    leave = {k: v["label"] for k, v in f0.items() if k not in over}

    st, js = post("/api/profile/apply", {"person_id": pid, "name": name, "keys": over})
    res = data(st, js)
    check("勾选覆盖写入接口 200", st == 200, (st, js))
    if not res:
        return
    check("written 报的就是勾选的那批字段",
          sorted(res.get("written") or []) == sorted(labels.values()),
          (res.get("written"), sorted(labels.values())))
    check("overwritten 恰好是「本来就有值」的那批（都是中文标签）",
          sorted(res.get("overwritten") or []) == sorted(labels.values()),
          (res.get("overwritten"), sorted(labels.values())))
    check("未勾选的字段没被顺手写进去",
          not set(res.get("written") or []) & set(leave.values()),
          (res.get("written"), sorted(leave.values())))
    check("未勾选的字段在 skipped 里注明「未勾选」",
          all(any(lb in s and "未勾选" in s for s in res.get("skipped") or [])
              for lb in leave.values()),
          (res.get("skipped"), sorted(leave.values())))
    check("提示语明确说了「覆盖」", "覆盖" in (res.get("message") or ""), res.get("message"))
    check("提示语里同时给了「可回滚」", "回滚" in (res.get("message") or ""), res.get("message"))
    check("提示语里的覆盖数等于勾选数",
          ("其中 %d 个覆盖了原值" % len(over)) in (res.get("message") or ""), res.get("message"))

    rid = res.get("record_id")
    check("覆盖写入也留了回滚快照", bool(rid), res)
    if not rid:
        return

    st, prof_mid = preview(pid, name)
    f_mid = fields_of(prof_mid)
    mid = {k: v["emby_value"] for k, v in f_mid.items()}

    bad = []
    for k in over:
        if k == "provider_ids":
            # Emby 把外部 ID 合并写 → 只要求「抓到的每一行都进了 Emby」。
            miss = [ln for ln in want[k].splitlines()
                    if ln.strip() and ln.strip() not in (mid.get(k) or "")]
            if miss:
                bad.append((k, "缺", miss))
        elif norm_val(k, mid.get(k)) != norm_val(k, want[k]):
            bad.append((k, norm_val(k, want[k]), norm_val(k, mid.get(k))))
    check("已有值已变成抓取值（按语义比，兼容 Emby 的规范化 / 合并写）", not bad, bad)

    check("覆盖不会把字段写空", not [k for k in over if not (mid.get(k) or "").strip()],
          {k: mid.get(k) for k in over})

    # 覆盖只作用于这次勾选的字段，「只填空白」的默认策略不受影响：
    # 覆盖前后「默认可写字段集合」必须一致（比如「出生地」这种空白+抓到值的字段
    # 本来就该是 will_write，别把它误判成覆盖引起的）。
    was = {k for k, v in f0.items() if v["will_write"]}
    now = {k for k, v in f_mid.items() if v["will_write"]}
    check("覆盖没有改变默认「只填空白」的可写字段集合", was == now, (sorted(was), sorted(now)))

    st, js = post("/api/profile/rollback", {"id": rid})
    check("覆盖后回滚接口 200", st == 200, (st, js))

    st, prof_after = preview(pid, name)
    after = {k: v["emby_value"] for k, v in fields_of(prof_after).items()}
    diff = {k: (before[k], after[k]) for k in before if before[k] != after[k]}
    check("覆盖回滚后逐字段还原到原值", not diff, diff)


def run_live_batch(target):
    """批量走一遍任务系统，然后把这次批量写进去的东西全部滚回去。

    批量是用户真正会用的形态，而它和单条走的是**同一个** applyActorProfile ——
    所以这里重点是「任务能跑完、日志里没有『缺少演员 ID』、失败数为 0」，
    以及「批量产生的历史记录也能一条条回滚」。
    """
    print("\n7) 批量（items 带 ID → 任务系统）")

    st, js = get("/api/profile/history?limit=200")
    before_ids = {r["id"] for r in (data(st, js) or {}).get("records") or []}

    items = [{"id": target["Id"], "name": target["Name"]}]
    st, js = post("/api/profile/batch", {
        "items": items, "sources": ["AvDataBank"], "use_alias_memo": True,
    })
    res = data(st, js)
    check("批量接口 200 并返回 job_id", st == 200 and bool(res and res.get("job_id")), (st, js))
    if not res or not res.get("job_id"):
        return
    fin = wait_job(res["job_id"])
    check("任务跑完", fin.get("status") == "done", fin.get("status"))
    check("没有失败项", not fin.get("failed"), fin.get("failed"))
    msgs = " ".join(l["message"] for l in fin.get("logs") or [])
    check("日志里没有「缺少演员 ID」", "缺少演员 ID" not in msgs, msgs[:300])
    check("日志里报了处理结果", ("已写入" in msgs) or ("没有需要写入" in msgs), msgs[:300])

    st, js = get("/api/profile/history?limit=200")
    new_ids = [r["id"] for r in (data(st, js) or {}).get("records") or []
               if r["id"] not in before_ids and not r.get("rolled_back")]
    print("     本次批量新增 %d 条历史，逐条回滚" % len(new_ids))
    for rid in new_ids:
        st, js = post("/api/profile/rollback", {"id": rid})
        check("回滚批量记录 %s" % rid, st == 200, (st, js))


def wait_job(jid, timeout=300):
    end = time.time() + timeout
    last = {}
    while time.time() < end:
        st, js = get("/api/jobs/" + jid, timeout=30)
        if st != 200:
            return last
        last = data(st, js) or {}
        if last.get("status") != "running":
            return last
        time.sleep(1.0)
    return last


if __name__ == "__main__":
    sys.exit(main())
