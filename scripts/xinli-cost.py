# 从 -logs 的评测报告拆分成本：python3 scripts/xinli-cost.py eval-results/<时间> [另一个报告目录…]
# 只读取合成评测数据，输出用量与调用数，不输出输入、模型回复或调用参数。
import argparse
from collections import Counter, defaultdict
import json
from pathlib import Path


def get(obj, name, default=None):
    # protojson 默认用 lowerCamelCase，也接受 use_proto_names 导出的日志。
    camel = name.split("_")[0] + "".join(p.title() for p in name.split("_")[1:])
    return obj.get(camel, obj.get(name, default))


def profile(log, split):
    totals = defaultdict(Counter)
    phases, first, tools = {}, {}, {}
    with log.open() as lines:
        for line in lines:
            e = json.loads(line)
            sid = get(e, "session_id", "")
            r = get(e, "run_requested")
            if r is not None:
                rid = get(r, "run_id")
                text = "".join(b.get("text", {}).get("text", "") for b in r.get("input", []))
                phase = "其他"
                if text.startswith("整理一个时段"):
                    phase = "按时段"
                elif text in ("整理今天", "只按账本整理今天"):
                    phase = "当天对账" if split else "整轮：整理今天"
                elif text == "整理交谈":
                    phase = "整轮：整理交谈"
                phases[sid, rid] = phase
                totals[phase]["runs"] += 1
            result = get(e, "tool_result")
            if result is not None:
                key = sid, get(result, "run_id")
                name = tools.get((sid, get(result, "call_id")), "")
                text = "".join(b.get("text", {}).get("text", "") for b in result.get("content", []))
                if name == "mcp_xinli__file_changes" and (get(result, "is_error", False) or "变更集没有被接受" in text):
                    totals[phases.get(key, "其他")]["rejected_changes"] += 1
            for kind in ("assistant_message", "context_compacted"):
                m = get(e, kind)
                if m is None:
                    continue
                key = sid, get(m, "run_id")
                row = totals[phases.get(key, "其他")]
                usage = m.get("usage", {})
                inp = int(get(usage, "input_tokens", 0))
                out = int(get(usage, "output_tokens", 0))
                cached = int(get(usage, "cached_input_tokens", 0))
                row["input"] += inp
                row["output"] += out
                row["cached"] += cached
                if kind == "context_compacted":
                    row["compactions"] += 1
                    row["compact_tokens"] += inp + out
                else:
                    row["model_calls"] += 1
                    if key not in first:
                        first[key] = inp
                        row["first_input"] += inp
                    row["input_growth"] += max(0, inp - first[key])
                    for c in get(m, "tool_calls", []):
                        row["calls"] += 1
                        row[get(c, "capability", "")] += 1
                        tools[sid, get(c, "call_id")] = get(c, "capability", "")
    return totals


def main():
    parser = argparse.ArgumentParser(description="按原轮次／时段／当天对账拆分 xinli 评测用量")
    parser.add_argument("reports", nargs="+", type=Path)
    args = parser.parse_args()
    for directory in args.reports:
        report = json.loads((directory / "report.json").read_text())
        print(f"\n报告：{directory}\n")
        print("| 用例／阶段 | 运行次数 | Run | 模型调用 | 工具调用 | 输入 | 输出 | 首调输入 | 相对首调增长 | 压缩用量 | 缓存命中 | 读取／召回／交出 | 拒收变更集 |")
        print("|---|---|---|---|---|---|---|---|---|---|---|---|---|")
        for case in report["cases"]:
            if case.get("skipped"):
                continue
            sums = defaultdict(Counter)
            logs = sorted((directory / "logs").glob(case["name"] + "-*.jsonl"))
            # 排除 capture-workday 的前缀匹配到了 capture-workday-oneshot。
            logs = [p for p in logs if p.stem[len(case["name"]) + 1:].isdigit()]
            if not logs:
                raise SystemExit(f"没有 {case['name']} 的日志；运行评测时需要 -logs")
            if len(logs) != case["trials"]:
                raise SystemExit(f"{case['name']} 日志 {len(logs)} 份，与 trials={case['trials']} 不一致")
            for log in logs:
                for phase, values in profile(log, case.get("xinli_span_runs", False)).items():
                    sums[phase].update(values)
                    sums["合计"].update(values)
            total = sums["合计"]
            # 与报告交叉核对，防止部分日志、字段名或 Session 识别错误给出看似合理的成本归因。
            if (total["input"] + total["output"]) // len(logs) != case["avg_tokens"]:
                raise SystemExit(f"{case['name']} 日志用量与 report.json 不一致")
            for phase, values in sums.items():
                n = len(logs)
                avg = lambda name: f"{values[name] / n:.1f}"
                hit = values["cached"] / values["input"] if values["input"] else 0
                tools = "/".join(avg(k) for k in ("mcp_xinli__captures", "mcp_xinli__find_related", "mcp_xinli__file_changes"))
                print(f"| {case['name']}／{phase} | {n} | {avg('runs')} | {avg('model_calls')} | {avg('calls')} | "
                      f"{avg('input')} | {avg('output')} | {avg('first_input')} | {avg('input_growth')} | "
                      f"{avg('compact_tokens')} | {hit:.0%} | {tools} | {avg('rejected_changes')} |")
        print("\n均为每次运行的平均值。相对首调增长是各 Run 后续调用输入超过首调的部分，含读取结果与历史，"
              "不是重复历史的精确计费；压缩用量已包含在输入／输出中。耗时见原报告（含评分）。")


if __name__ == "__main__":
    main()
