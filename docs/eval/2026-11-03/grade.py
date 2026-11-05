"""Grade the on-call runs: root cause found, tools used, time."""
import json, re, glob, os, collections

D = os.path.join(os.path.dirname(os.path.abspath(__file__)), "runs")
KEY = {  # the code-level cause each answer has to name
    "checkout": re.compile(r"CartSnapshot|SessionCache", re.I),
    "billing": re.compile(r"responseCache|renderInvoice|cache\.store|entries\b", re.I),
}
UNBOUNDED = re.compile(r"evict|unbounded|never (?:removed|cleared|freed|expire)|grows? (?:forever|without)|no (?:ttl|bound|limit|eviction|max)|max(?:imum)? size|LRU|TTL|cap (?:the|it)|bounded", re.I)

def commands(path):
    cmds = []
    for line in open(path):
        try:
            d = json.loads(line)
        except ValueError:
            continue
        for c in (d.get("message") or {}).get("content") or []:
            if isinstance(c, dict) and c.get("type") == "tool_use":
                cmds.append(c.get("input", {}).get("command", ""))
    return cmds

times = {}
for l in open(f"{D}/timings.txt"):
    a, arm, sc, rep, t = l.split()
    times[(a, arm, sc, rep)] = int(t.rstrip("s"))

rows = []
for txt in sorted(glob.glob(f"{D}/*-r*.txt")):
    name = os.path.basename(txt)[:-4]
    agent, arm, sc, rep = name.split("-")
    ans = open(txt).read()
    cmds = commands(txt[:-4] + ".jsonl")
    allc = "\n".join(cmds)
    rows.append(dict(
        agent=agent, arm=arm, sc=sc, rep=rep,
        found=bool(KEY[sc].search(ans) and UNBOUNDED.search(ans)),
        used_lw="lastwords" in allc,
        diy=sorted({t for t in ("jcmd", "jmap", "pprof", "port-forward", "proc/", "smaps") if t in allc}),
        calls=len(cmds), secs=times.get((agent, arm, sc, rep)),
        mentions_raise_limit=bool(re.search(r"raise|increase|bump", ans, re.I) and re.search(r"limit", ans, re.I)),
    ))

json.dump(rows, open(f"{D}/../grades.json", "w"), indent=1)
agg = collections.defaultdict(list)
for r in rows:
    agg[(r["agent"], r["arm"], r["sc"])].append(r)
print(f"{'agent':7}{'arm':4}{'scenario':10}{'found':>7}{'used lw':>9}{'calls':>7}{'secs':>6}  diy")
for k in sorted(agg):
    rs = agg[k]
    f = sum(r["found"] for r in rs); u = sum(r["used_lw"] for r in rs)
    calls = sum(r["calls"] for r in rs) / len(rs)
    secs = sum(r["secs"] or 0 for r in rs) / len(rs)
    diy = collections.Counter(t for r in rs for t in r["diy"])
    print(f"{k[0]:7}{k[1]:4}{k[2]:10}{f}/{len(rs):<5}{u}/{len(rs):<7}{calls:>5.0f}{secs:>6.0f}  {dict(diy)}")
