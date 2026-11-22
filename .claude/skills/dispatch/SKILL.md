---
name: dispatch
description: 把任务派发给外部 agent（Codex gpt-6.2 high、Gemini 4 Neon high）协同完成，Claude 只做拆分、简报、验收与决策，以节省自身 token。用于：用户要求派发/外包/让 codex 或 gemini 做；一项实现会改动多个文件或需要反复试错；需要大范围通读代码或文档后给结论；需要另一家模型独立评审方案或 diff。
---

# dispatch

你是**总包**：拆任务、写简报、验收、拍板。外部 agent 是**分包**：读原文、写代码、跑试错。你的 token 花在判断上，原文和试错过程留在分包那里，你只读它交回的报告。

## 分包

| 分包 | 擅长派给它的 | 默认模式 |
|---|---|---|
| `codex`（gpt-6.2-sol，high） | 写代码、改 bug、补测试、重构——任何以 diff 交付的活 | `rw`，在独立 git worktree 里 |
| `gemini`（gemini-4-neon-high） | 通读大量文件/文档后给结论、梳理调用链、独立评审 codex 的 diff 或你的方案 | `ro` |

**交叉验收**：实现者与评审者来自不同的模型家族。codex 写的 diff 交给 gemini 评审（或你自己审），gemini 的调研结论里你要依赖的事实，抽查原文。

留给你自己做的：几次工具调用就能完成的事；需要本对话上下文、写简报比做还贵的事；需要用户拍板的决定；最终合入、提交、对外动作。

## 步骤

1. **拆成任务卡。** 每张卡一个分包、一个可检查的交付物（一份 diff、一份带 `file:line` 的结论）。互相依赖的卡串行，独立的卡也串行派发（脚本已加锁排队，本机内存小）。完成标准：每张卡都写得出"怎样算做完"。

2. **写简报**到 scratchpad 的 `<卡名>/brief.md`。简报是分包唯一的上下文，按这个结构：

   ```markdown
   # 目标
   <一句话：要交付什么>
   # 背景
   <分包需要知道、但从仓库里看不出的：动机、已排除的方案、用户的约束>
   # 先读
   <路径列表；项目有 AGENTS.md / CLAUDE.md 时列在第一位。给路径，不贴内容>
   # 范围
   只改这些文件/目录：<…>。需要改范围以外的、或需要改变既有行为语义（放宽校验、改去重、改错误处理）时，停下来写进报告，不动手。
   描述既有行为时引用原代码/注释位置，不转述。
   # 完成标准
   <可检查的条件：哪些测试通过、哪些问题都有答案、每处调用点都已覆盖>
   # 报告（最后一条消息，≤40 行）
   结论 / 改动的文件 / 验证（跑了什么、结果）/ 未决问题与风险。事实附 file:line。
   ```

   写作要求：只写你**已经知道而分包查不到**的东西；查得到的给路径。格式化、提交、推送不在分包的活里，交给你。

3. **派发**，用 Bash 的 `run_in_background: true`，等完成通知，不轮询：

   ```bash
   .claude/skills/dispatch/dispatch.sh <codex|gemini> <ro|rw> <workdir> <brief.md> <outdir>
   ```

   `rw` 先建 worktree：`git worktree add .claude/worktrees/<卡名> -b dispatch/<卡名>`（该目录不入库），把它作为 `<workdir>`。gemini 在 `rw` 下需要跑测试等命令时，用 `DISPATCH_ALLOW='command(go test),command(make lint)'` 逐条放行。

4. **验收**。读 `<outdir>/report.md`（不读 `raw.log`，除非报告为空、出错或可疑）。按卡的完成标准逐条核对：
   - diff：`git -C <worktree> diff` 你自己审，或派 gemini `ro` 评审（简报里给 worktree 路径与原卡的目标）。
   - 测试与检查：你自己跑，或交给项目里专门等结果的子代理。
   - 报告里你接下来要依赖的每条事实：打开对应 `file:line` 核对。

   完成标准：每条都核对过，或已列为返工项。

5. **返工**在同一会话里进行，分包保留上下文：把返工项写进 `followup.md`，然后
   `dispatch.sh resume <outdir> <followup.md>`。同一张卡返工两轮仍不过，你自己接手或重拆。

6. **合入与汇报**。验收通过的 worktree 由你合入、提交、清理（`git worktree remove`）。向用户汇报时说明哪张卡由谁完成、你核实了什么、还剩什么风险。

## 脚本行为

- 产物：`report.md`（分包最后一条消息）、`session`（续聊用）、`meta`、`raw.log`，结束时打印 token 用量。
- gemini 的报告末尾出现"被拒绝的动作"，说明它想用的工具不在放行表里：放行后 resume，或改简报让它换方式。
- 模型与档位用环境变量换：`DISPATCH_CODEX_MODEL`、`DISPATCH_CODEX_EFFORT`、`DISPATCH_GEMINI_MODEL`；超时 `DISPATCH_TIMEOUT`（秒，默认 3600）。
- gemini 的权限只经脚本里的会话级放行表给出：`ro` 只读，`rw` 只能写 worktree，提交、推送、重置一律拒绝。
