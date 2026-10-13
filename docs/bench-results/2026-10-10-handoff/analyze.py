import json, glob, os, sys, collections, statistics
runs = {}
for f in sorted(glob.glob(os.path.join(sys.argv[1], '[ABCX][0-9].json'))):
    runs[os.path.basename(f)[:-5]] = json.load(open(f))

def cost(run):
    out = run['out_dir']; per = collections.defaultdict(float)
    for f in glob.glob(os.path.join(out, '*', '.kopicode', 'sessions', '*', 'events.jsonl')):
        task = f.split(os.sep)[-5]
        for line in open(f):
            try: e = json.loads(line)
            except Exception: continue
            if e.get('type') == 'ProviderResponse':
                p = e.get('payload') or e.get('data') or e
                c = p.get('cost_usd')
                if c: per[task] += c
    return per

arms = collections.defaultdict(list)
for name, r in runs.items(): arms[name[0]].append((name, r, cost(r)))
label = {'A': 'A continue (cap 24)', 'B': 'B hand off at cap 8 (<=2 handoffs)', 'C': 'C cap 8, no handoff', 'X': 'X (invalid first attempt: blank narrative)'}
print('| arm | runs | passes per run | mean pass | mean turns | mean tokens | mean cost | handoffs/run |')
print('|---|---|---|---|---|---|---|---|')
per_task = {a: collections.defaultdict(list) for a in arms}
for a in sorted(arms):
    ps = []; turns = []; toks = []; costs = []; hos = []
    for name, r, c in arms[a]:
        t = r['tasks']; ps.append(sum(x['passed'] for x in t)); turns.append(sum(x['turns'] for x in t))
        toks.append(sum(x['tokens']['total'] for x in t)); costs.append(sum(c.values())); hos.append(sum(x.get('handoffs', 0) for x in t))
        for x in t: per_task[a][x['task_id']].append(x)
    n = len(ps)
    print(f"| {label[a]} | {n} | {'/'.join(map(str, ps))} of {len(arms[a][0][1]['tasks'])} | {sum(ps)/n:.2f} | {sum(turns)/n:.0f} | {sum(toks)/n/1e6:.2f}M | ${sum(costs)/n:.3f} | {sum(hos)/n:.1f} |")
print()
tasks = sorted({k for a in per_task for k in per_task[a]})
print('| task | ' + ' | '.join(f'{a} pass | {a} turns | {a} cost' if False else f'{a} pass/turns' for a in sorted(arms)) + ' | handoffs in B |')
print('|---|' + '---|' * (len(arms) + 1))
for tk in tasks:
    cells = []
    for a in sorted(arms):
        xs = per_task[a].get(tk, [])
        if not xs: cells.append('-'); continue
        cells.append(f"{sum(x['passed'] for x in xs)}/{len(xs)}, {'/'.join(str(x['turns']) for x in xs)}")
    hb = '/'.join(str(x.get('handoffs', 0)) for x in per_task.get('B', {}).get(tk, []))
    print(f"| {tk} | " + ' | '.join(cells) + f" | {hb} |")
