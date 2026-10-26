# 由剧本生成 capture-multiday 的旁听与金标（docs/design/m4-eval.md §13）。改了剧本之后重新生成，生成的 YAML 随剧本一并提交：
#   python3 scripts/gen-multiday.py evals/data/multiday/script.txt evals/data/multiday
#
# 剧本格式：
#   !gold <id> <mine|theirs> <YYYY-MM-DD|-> <说明>      金标承诺（!opt 为可选的）
#   # day YYYY-MM-DD [part]                             新的一天（或同一天的后半，文件名加 -part）
#   @ HH:MM-HH:MM <topic|->                             一段：行在起止时刻之间均匀分布
#   <W|key>: <文字> [| g=a b; rev=due YYYY-MM-DD|done|cancelled; asr=…; heard=wearer|other; hk=key]
#   rev= 作用于 g= 中的第一个 ID，生效时间是这一行的时间。
import sys, os, re, json

src, out = sys.argv[1], sys.argv[2]
gold, order, files = {}, [], []
cur = None
block = None


def flush():
    global block
    if not block:
        return
    (h1, m1), (h2, m2) = block['span']
    t1, t2 = h1 * 60 + m1, h2 * 60 + m2
    n = len(block['lines'])
    for i, ln in enumerate(block['lines']):
        t = t1 + (t2 - t1) * i // max(n - 1, 1)
        ln['at'] = f"{cur['date']} {t // 60:02d}:{t % 60:02d}"
        cur['segs'].append(ln)
        if ln.get('rev'):
            gid = ln['gold'].split()[0]
            kind, *rest = ln['rev'].split()
            r = {'at': ln['at']}
            if kind == 'due':
                r['due'] = rest[0]
            else:
                r['status'] = kind
            gold[gid]['revisions'].append(r)
    block = None


for raw in open(src, encoding='utf-8'):
    line = raw.rstrip('\n')
    if not line.strip() or line.startswith('//'):
        continue
    if line.startswith('!gold ') or line.startswith('!opt '):
        tag, gid, kind, due, what = line.split(None, 4)
        assert gid not in gold, gid
        gold[gid] = {'id': gid, 'kind': kind, 'what': what, 'due': None if due == '-' else due,
                     'optional': tag == '!opt', 'revisions': []}
        order.append(gid)
        continue
    if line.startswith('# day '):
        flush()
        parts = line[6:].split()
        cur = {'date': parts[0], 'name': parts[0] + ('-' + parts[1] if len(parts) > 1 else ''), 'segs': []}
        files.append(cur)
        continue
    if line.startswith('@ '):
        flush()
        span, topic = line[2:].split()
        a, b = span.split('-')
        block = {'span': [tuple(map(int, a.split(':'))), tuple(map(int, b.split(':')))],
                 'topic': None if topic == '-' else topic, 'lines': []}
        continue
    who, rest = line.split(':', 1)
    text, _, attrs = rest.partition('|')
    seg = {'speaker': 'wearer' if who.strip() == 'W' else 'other', 'text': text.strip()}
    if seg['speaker'] == 'other':
        seg['key'] = who.strip()
    if block['topic']:
        seg['topic'] = block['topic']
    for kv in filter(None, (x.strip() for x in attrs.split(';'))):
        k, v = kv.split('=', 1)
        seg[{'g': 'gold', 'hk': 'heard_key'}.get(k, k)] = v.strip()
    if 'gold' in seg:
        for gid in seg['gold'].split():
            assert gid in gold, (gid, line)
    block['lines'].append(seg)
flush()


def q(s):
    return json.dumps(s, ensure_ascii=False)


os.makedirs(out, exist_ok=True)
fields = ['speaker', 'key', 'text', 'at', 'topic', 'gold', 'asr', 'heard', 'heard_key']
used = set()
for f in files:
    with open(os.path.join(out, f['name'] + '.yaml'), 'w', encoding='utf-8') as w:
        w.write(f"# capture-multiday 的旁听，{f['date']}：佩戴者方晴是连锁餐饮禾苗食堂的区域运营经理。每段带主题（family 家事、"
                "binjiang 滨江店开业、menu 冬季新菜单、safety 城西店食安整改、delivery 滨江店外卖入驻、hiring 服务员招聘；闲聊不标），"
                "gold 是金标承诺（gold.yaml），asr、heard、heard_key 是识别有误的版本（带噪声的用例提交它们）。由 script.txt 经 "
                "scripts/gen-multiday.py 生成，"
                "段内的行在起止时刻之间均匀分布。\n")
        for s in f['segs']:
            used.update(s.get('gold', '').split())
            w.write('- {' + ', '.join(f"{k}: {q(s[k])}" for k in fields if k in s) + '}\n')
missing = [g for g in order if g not in used]
assert not missing, missing
with open(os.path.join(out, 'gold.yaml'), 'w', encoding='utf-8') as w:
    w.write("# capture-multiday 的金标（m4-eval.md §13）：2026-10-12（星期一）至 10-15（星期四）旁听中的承诺。旁听里标了 gold 的段是"
            "答应、改期、完成或改由他人去做的原话，revisions 的时间是说那句话的时间；用户在 App 中的改动写在用例的 xinli_edit 中。"
            "optional 的是琐事、转述或是否算答应拿不准的：记不记都可以。他人之间的、佩戴者否认或拒绝的、有条件的说法不在这里，"
            "记下了算误报。由 script.txt 经 scripts/gen-multiday.py 生成。\ncommitments:\n")
    for gid in order:
        g = gold[gid]
        parts = [f"id: {g['id']}", f"kind: {g['kind']}", f"what: {q(g['what'])}"]
        if g['due']:
            parts.append(f"due: {q(g['due'])}")
        if g['optional']:
            parts.append('optional: true')
        if g['revisions']:
            parts.append('revisions: [' + ', '.join('{' + ', '.join(f"{k}: {q(v)}" for k, v in r.items()) + '}' for r in g['revisions']) + ']')
        w.write('  - {' + ', '.join(parts) + '}\n')
print({f['name']: len(f['segs']) for f in files}, 'gold', len(order), 'counted', sum(not gold[g]['optional'] for g in order))
