#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# 打流测试 · 后台源健康维护器（探测候选源，健康即自动写入 AI 源池并纳入使用）
# 策略：AI 探测新增源免人工审核直接入库（ai_sources.json），客户端 /api/builtin 会
#       合并 baseline∪AI 源使用；后台可逐条删除。仅自动管理 AI 源池，绝不改 baseline
#       或 admin_profiles.json。新源须多轮探测健康且速率达标才纳入；已失效的 AI 源自动剔除。
import json, os, socket, statistics, sys, time, urllib.request

CONF = os.environ.get("PML_MAINT_CONF", "/opt/pml/maintainer.json")
UA = "okhttp/4.12.0"
ORIGIN = os.environ.get("PML_MAINT_ORIGIN", "http://203.0.113.10")
AI_MIN_MBPS = 5.0            # 新源纳入的最低中位速率(Mbps)
AI_CAP = 40                  # AI 源池每向上限
DL_CAP = 12 * 1024 * 1024      # 下载探测最多读取字节
DL_SEC = 6.0                   # 下载探测最长秒数
UL_BODY = 2 * 1024 * 1024      # 上传探测负载字节（需大于小文件服务的截断阈值，才能识破"提前收兵"）
UL_SEC = 6.0

def _req(url, data=None, method=None, origin=None, rng=None, timeout=8):
    r = urllib.request.Request(url, data=data, method=method)
    r.add_header("User-Agent", UA)
    r.add_header("Accept-Encoding", "identity")
    if origin:
        r.add_header("Origin", origin)
    if rng:
        r.add_header("Range", rng)
    return urllib.request.urlopen(r, timeout=timeout)

def probe_dl(url):
    t0 = time.time()
    got = 0
    try:
        resp = _req(url, rng="bytes=0-%d" % (DL_CAP - 1), timeout=10)
        while time.time() - t0 < DL_SEC and got < DL_CAP:
            chunk = resp.read(256 * 1024)
            if not chunk:
                break
            got += len(chunk)
        resp.close()
    except Exception as e:
        return {"url": url, "ok": False, "mbps": 0.0, "err": type(e).__name__}
    dt = max(time.time() - t0, 0.1)
    mbps = got * 8 / dt / 1e6
    return {"url": url, "ok": got > 0, "mbps": round(mbps, 1)}

def probe_ul(url):
    body = os.urandom(UL_BODY)
    t0 = time.time()
    cors = False
    try:
        resp = _req(url, data=body, method="POST", origin=ORIGIN, timeout=15)
        code = getattr(resp, "status", 200)
        # 浏览器能不能读到这个响应的决定因素：跨域许可头。读得到才谈得上"服务端确认字节"，
        # 读不到的源只能按请求往返完成来结算，精度更低，排序时靠后。
        acao = (resp.headers.get("access-control-allow-origin") or "").strip()
        cors = acao == "*" or acao == ORIGIN
        resp.read(4096)
        resp.close()
    except Exception as e:
        return {"url": url, "ok": False, "mbps": 0.0, "err": type(e).__name__}
    dt = max(time.time() - t0, 0.1)
    mbps = len(body) * 8 / dt / 1e6
    return {"url": url, "ok": 200 <= code < 300, "code": code, "cors": cors, "mbps": round(mbps, 1)}

def probe(url, kind, rounds):
    rs = []
    for _ in range(max(1, rounds)):
        rs.append(probe_dl(url) if kind == "download" else probe_ul(url))
        time.sleep(0.3)
    ok = [r for r in rs if r.get("ok")]
    med = statistics.median([r["mbps"] for r in ok]) if ok else 0.0
    return {"url": url, "healthy": len(ok) >= max(1, rounds - 1), "ok_rounds": len(ok),
            "rounds": rounds, "median_mbps": round(med, 1),
            "cors": any(bool(r.get("cors")) for r in ok),
            "last_err": (rs[-1].get("err") if rs else None)}

def load_ai(path):
    try:
        o = json.load(open(path, encoding="utf-8"))
        return list(o.get("download", [])), list(o.get("upload", []))
    except Exception:
        return [], []


def save_ai(path, dl, ul):
    d = os.path.dirname(path)
    if d:
        os.makedirs(d, exist_ok=True)
    tmp = path + ".tmp"
    json.dump({"download": dl, "upload": ul}, open(tmp, "w", encoding="utf-8"), ensure_ascii=False, indent=2)
    os.replace(tmp, path)


def main():
    cfg = json.load(open(CONF, encoding="utf-8"))
    rounds = int(cfg.get("rounds", 3))
    baseline = cfg.get("baseline", {})
    candidates = cfg.get("candidates", {})
    report_out = cfg.get("report_out", "/var/lib/pml-geo/source_health.json")
    ai_out = cfg.get("ai_out", "/var/lib/pml-geo/ai_sources.json")
    base_dl = baseline.get("download", []); base_ul = baseline.get("upload", [])
    fk = cfg.get("foreign_keep", {})
    fk_all = set(fk.get("download", [])) | set(fk.get("upload", []))
    ai_dl, ai_ul = load_ai(ai_out)
    # 1) baseline 健康度：仅报告，编译基线永不自动删（安全兜底）
    live = []; dead = []
    for kind, urls in (("download", base_dl), ("upload", base_ul)):
        for u in urls:
            if u in fk_all:
                continue
            r = probe(u, kind, rounds); r["kind"] = kind
            (live if r["healthy"] else dead).append(r)
    # 2) 现有 AI 源：探测并剔除失效项
    ai_alive_dl = []; ai_alive_ul = []; ai_pruned = []
    for kind, urls, keep in (("download", ai_dl, ai_alive_dl), ("upload", ai_ul, ai_alive_ul)):
        for u in urls:
            if u in fk_all:
                keep.append(u); continue
            r = probe(u, kind, rounds); r["kind"] = kind
            (keep.append(u) if r["healthy"] else ai_pruned.append(u))
    # 3) 候选：健康且达标即自动纳入（免人工审核），去重、上限保护
    added_dl = []; added_ul = []; cand_ranked = []
    for kind, urls in (("download", candidates.get("download", [])), ("upload", candidates.get("upload", []))):
        for u in urls:
            if u in fk_all:
                continue
            r = probe(u, kind, rounds); r["kind"] = kind; cand_ranked.append(r)
    cand_ranked.sort(key=lambda x: x["median_mbps"], reverse=True)
    for r in cand_ranked:
        if not r["healthy"] or r["median_mbps"] < AI_MIN_MBPS:
            continue
        u = r["url"]; kind = r["kind"]
        if kind == "download":
            if u in base_dl or u in ai_alive_dl or len(ai_alive_dl) >= AI_CAP:
                continue
            ai_alive_dl.append(u); added_dl.append(u)
        else:
            if u in base_ul or u in ai_alive_ul or len(ai_alive_ul) >= AI_CAP:
                continue
            ai_alive_ul.append(u); added_ul.append(u)
    for u in fk.get("download", []):
        if u not in ai_alive_dl and len(ai_alive_dl) < AI_CAP:
            ai_alive_dl.append(u); added_dl.append(u)
    for u in fk.get("upload", []):
        if u not in ai_alive_ul and len(ai_alive_ul) < AI_CAP:
            ai_alive_ul.append(u); added_ul.append(u)
    save_ai(ai_out, ai_alive_dl, ai_alive_ul)
    rep = {"generatedAt": int(time.time()), "rounds": rounds,
           "live_healthy": live, "live_dead": dead,
           "ai_added": {"download": added_dl, "upload": added_ul},
           "ai_pruned": ai_pruned,
           "ai_pool": {"download": ai_alive_dl, "upload": ai_alive_ul},
           "candidates": cand_ranked,
           "note": "AI 探测源已自动纳入使用（baseline∪AI）；失效 AI 源自动剔除；回退请在后台逐条删除。"}
    d = os.path.dirname(report_out)
    if d:
        os.makedirs(d, exist_ok=True)
    tmp = report_out + ".tmp"
    json.dump(rep, open(tmp, "w", encoding="utf-8"), ensure_ascii=False, indent=2)
    os.replace(tmp, report_out)
    print("MAINT_OK live_h=%d live_dead=%d ai_add_dl=%d ai_add_ul=%d ai_prune=%d ai_pool=%d/%d" % (
        len(live), len(dead), len(added_dl), len(added_ul), len(ai_pruned), len(ai_alive_dl), len(ai_alive_ul)))


if __name__ == "__main__":
    try:
        main()
    except Exception as e:
        print("MAINT_ERR", type(e).__name__, e)
        sys.exit(1)
