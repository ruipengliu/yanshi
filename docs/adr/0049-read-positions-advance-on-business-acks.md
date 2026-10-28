---
status: superseded by ADR-0050
---

# 声明了交付确认的流，读取位置按业务线的回执推进

> 已被 [ADR-0050](./0050-capture-text-in-the-business-line.md) 取代：旁听原文存放在业务线。交付确认曾经实现（internal/session/ack.go），已按 ADR-0050 的退役条件随内核的 Capture 路径一并删除；下文的代码名是当时的实现。

读取位置在 `RunCompleted` 时提交本 Run 读到的最后一页（ADR-0039）。工具调用失败只是一条错误结果，模型仍可正常结束 Run；业务线拒收变更集时，返回的也可能是成功的 MCP 响应。于是"读页成功 → 交出失败 → 模型回复稍后再试 → Run 完成"会让读取位置越过没有保存的时段，之后的 `after: "new"` 再也读不到它。读取位置的契约本身没有错，错在业务流程把"读过"当成"已保存"。

## 决定

（设计见 `docs/design/m7-kernel-primitives.md` §11）

- 业务线保存成功时，在 MCP 结果中附结构化回执 `_meta["yanshi/ack"] = {stream, from, through}`；拒收与部分保存不附。回执是平台读取的字段，不是给模型看的文字。
- AgentDef 在流上声明 `ack: true` 后，读取位置只提交到"从已提交位置起、由回执连续拼出的已确认前缀"的末端，且不超过本 Run 实际读到的位置。没有内容的时段也要交出空的变更集，前缀才连续。
- 回执随 `ToolResult.CaptureAck` 写入日志（`proto/yanshi/v1/event.proto` 的 CaptureAck），只采信同一业务线、登记中声明了 `acks: true`（mcpcap.Server.Acks）的 MCP Server 的调用结果，session.CheckCaptureAck 与 `State.Apply` 校验；投影只从日志计算前缀，确定性不变。
- 连续性以本 Run 读过的页拼链：`CaptureRead.Start` 记实际开始游标，`first` 记本页第一段，`ack` 记 agentdef.Stream.Ack 声明。session.Run.CapturePages 与 session.Run.CaptureAcks 随快照保存；完成时从已提交游标沿被完整覆盖的页推进，同起点取最远被覆盖页，提交真实的页尾游标（含续页的时段起点）与间隔。Ref 是日志位置，两批 Capture 之间可能夹着别的 Event，不能靠序号相邻判断范围相接。
- 未声明 `ack` 的流行为不变。

## Considered Options

- **"最后一次成功调用某能力之前读到的页"**：成功调用不是保存的证明（业务拒收也可能返回成功），读了 A、B 两页而只交出 A 时会确认到 B。
- **由业务线告诉模型从哪里接着读**：失去平台记录的、结构化的读取位置（ADR-0039 要去掉的正是靠文字找位置）。
- **调用失败即让 Run 失败**：工具错误多数可以由模型改正后重交，一律失败会让正常的拒收—改正循环无法进行。

## Consequences

- xinli-day 声明 `ack: true`，Persona 的 `file_changes` 在保存成功时附回执；没有新内容的时段也交出空变更集。
- 读取位置没有前进时，触发的"同一位置只触发一次"仍按最后一个 Capture 的序号判断：未确认的时段由下一次触发或当天整理补上。
- `internal/sim` 覆盖业务线拒收与 MCP 故障下读取位置不越过未确认的时段。
