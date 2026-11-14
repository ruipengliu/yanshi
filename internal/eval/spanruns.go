package eval

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"yanshi/internal/capability"
	"yanshi/internal/capture"
	"yanshi/internal/eval/xinlifixture"
)

const (
	xinliSpanInput    = "整理一个时段"
	xinliSpanAllInput = "整理一个时段（包括未结束的时段）"
	xinliDayInput     = "只按账本整理今天"
)

// 只给评测派生的 AgentDef 追加实验规则；交谈、承诺与 Memory 的要求沿用原指令，便于消融 Run 边界的影响。
const xinliSpanInstructions = `

本次采用「每个完整时段一个 Run」的实验流程。以下规则替代前面关于输入、继续读取下一时段与当天收尾的规则，其余交谈、承诺、记忆与健康信息的要求都照旧：
- 输入「整理一个时段」：只做第 1–3 步，第一次 mcp_xinli__captures 用 {"after":"new"}。
- 输入「整理一个时段（包括未结束的时段）」：也只做第 1–3 步，第一次 mcp_xinli__captures 用 {"after":"new","all":true}，续页同样带 all:true。
- 只处理这一次读到的第一个完整时段。页尾写「未完」且 after 带 ~ 时，照抄 after 继续读取同一个时段，读完所有续页再用一次 file_changes 交出完整时段。页尾写「到此结束」后不要再读下一时段。
- file_changes 的 source.from/to 必须覆盖本时段从第一段到最后一段的完整范围；即使全是闲聊、没有交谈或承诺，也要以这个 source 交出空变更集（exchanges:{}, commitments:{}），作为已经整理的回执。被拒绝时在本次 Run 内改正后重交整个变更集；业务线接受后用一两句话回复并结束，不做当天整理、不调用 file_day。没有新旁听、或时段还在进行时直接回复。
- 输入「只按账本整理今天」：所有时段已经在之前的 Run 中交出，本次只做第 4–6 步。直接用 day_ledger 取回账本，按需交 source:{day:true} 的当天整理，再调用 file_day。不要调用 mcp_xinli__captures，不重新按时段交出。
`

// spanTurns 扮演业务线的串行调度：一个原轮次内拆出多个 Run，仍只提交一次旁听、检查一次断言。
// 触发的「同一位置只触发一次」不支持连续排空，这里只用普通输入实验，不改平台触发（ADR-0040）。
func (in *instance) spanTurns(ctx context.Context, sid, bl, endUser string, turn Turn, timeout time.Duration) (*observation, error) {
	// 每个 Run 都复制原轮次的脚本：卡片动作、等待后台结果与杀 Worker 只该发生一次，拆开后语义就变了，不作消融。
	if turn.CardAction != nil || turn.AwaitBackground || turn.CrashAfterCalls > 0 {
		return nil, fmt.Errorf("span runs do not support card_action, await_background or crash_after_calls")
	}
	if timeout <= 0 {
		timeout = in.cfg.TurnTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	obs := &observation{input: turn.Input, status: "completed"}
	all := turn.Input == "整理今天"
	var ran bool
	run := func(input string) error {
		t := turn
		t.Input = input
		businessInput := input
		if input == xinliSpanInput || input == xinliSpanAllInput {
			businessInput = "整理交谈"
		} else if input == xinliDayInput {
			businessInput = "整理今天"
		}
		if err := in.xinli.Submitted(ctx, bl, endUser, sid, businessInput); err != nil {
			return err
		}
		o, err := in.turn(ctx, sid, t, timeout)
		if o != nil {
			obs.appendRun(o)
		}
		if err != nil {
			return err
		}
		if o.status != "completed" {
			// 停在提问处时 turn 不报错、也不中断：这里要停止实验，先中断，不留下等待回答的 Run。
			err := fmt.Errorf("span run ended with status %s", o.status)
			if st, loadErr := in.svc.Load(ctx, sid); loadErr != nil {
				err = errors.Join(err, loadErr)
			} else if r := st.Active(); r != nil {
				if stopErr := in.svc.Interrupt(context.WithoutCancel(ctx), sid, r.ID); stopErr != nil {
					err = errors.Join(err, fmt.Errorf("interrupt span run: %w", stopErr))
				}
			}
			return err
		}
		if businessInput == "整理交谈" && (slices.Contains(o.calls, "mcp_xinli__day_ledger") || slices.Contains(o.calls, "mcp_xinli__file_day")) {
			return fmt.Errorf("span run called day reconciliation tools")
		}
		return nil
	}
	for {
		position, err := in.xinli.CapturePosition(ctx, bl, endUser, sid)
		if err != nil {
			return obs, err
		}
		from, to, err := in.nextSpan(ctx, sid, all)
		if err != nil {
			return obs, err
		}
		input := xinliSpanInput
		if all {
			input = xinliSpanAllInput
		}
		if from == "" {
			// 空轮次仍让模型读一次，检验「已读完／还在进行」的退出；读过时段后不再多开探测 Run。
			if !ran {
				old, err := in.xinli.State(ctx, bl, endUser)
				if err != nil {
					return obs, err
				}
				if err := run(input); err != nil {
					return obs, err
				}
				after, err := in.xinli.CapturePosition(ctx, bl, endUser, sid)
				if err != nil {
					return obs, err
				}
				if after != position {
					return obs, fmt.Errorf("empty span run advanced the capture position")
				}
				filed, err := in.xinli.State(ctx, bl, endUser)
				if err != nil {
					return obs, err
				}
				if len(newSessionChangesets(old.Changesets, filed.Changesets, sid)) != 0 {
					return obs, fmt.Errorf("empty span run filed changes without reading a span")
				}
			}
			break
		}
		before, err := in.xinli.State(ctx, bl, endUser)
		if err != nil {
			return obs, err
		}
		if err := run(input); err != nil {
			return obs, err
		}
		ran = true
		after, err := in.xinli.CapturePosition(ctx, bl, endUser, sid)
		if err != nil {
			return obs, err
		}
		accepted, err := in.xinli.State(ctx, bl, endUser)
		if err != nil {
			return obs, err
		}
		if !spanAccepted(before.Changesets, accepted.Changesets, sid, from, to) {
			return obs, fmt.Errorf("span #%s–#%s: no accepted changeset; stopping before after=new can skip an unfiled span", from, to)
		}
		if got := after; got != to {
			return obs, fmt.Errorf("span #%s–#%s: capture position %q, want %q (no progress, partial span or overread)", from, to, got, to)
		}
	}
	if all {
		before, err := in.xinli.CapturePosition(ctx, bl, endUser, sid)
		if err != nil {
			return obs, err
		}
		old, err := in.xinli.State(ctx, bl, endUser)
		if err != nil {
			return obs, err
		}
		callStart := len(obs.calls)
		if err := run(xinliDayInput); err != nil {
			return obs, err
		}
		after, err := in.xinli.CapturePosition(ctx, bl, endUser, sid)
		if err != nil {
			return obs, err
		}
		if before != after {
			return obs, fmt.Errorf("day reconciliation advanced the capture position")
		}
		if slices.Contains(obs.calls[callStart:], "mcp_xinli__captures") {
			return obs, fmt.Errorf("day reconciliation reread captures instead of using the ledger")
		}
		filed, err := in.xinli.State(ctx, bl, endUser)
		if err != nil {
			return obs, err
		}
		for _, cs := range newSessionChangesets(old.Changesets, filed.Changesets, sid) {
			if cs.Author != xinlifixture.AuthorDay || !cs.Source.Day {
				return obs, fmt.Errorf("day reconciliation filed a span changeset")
			}
		}
		if !slices.ContainsFunc(filed.Days, func(d xinlifixture.Day) bool {
			return d.Session == sid && !slices.ContainsFunc(old.Days, func(old xinlifixture.Day) bool {
				return old.Session == sid && old.FiledAt.Equal(d.FiledAt)
			})
		}) {
			return obs, fmt.Errorf("day reconciliation has no accepted file_day")
		}
	}
	return obs, nil
}

// nextSpan 经夹具的隔离快照复用分页规则预览边界，不推进模型已读位置、不写入内核日志。
// 只复用页的结构与游标，不解析给模型看的中文，也不把业务日批次当成 Event 序号。
func (in *instance) nextSpan(ctx context.Context, sid string, all bool) (from, to string, err error) {
	st, err := in.svc.Load(ctx, sid)
	if err != nil {
		return "", "", err
	}
	d, err := in.agents.Get(st.Agent)
	if err != nil {
		return "", "", err
	}
	bl, eu := st.Created.GetBusinessLine(), st.Created.GetEndUser()
	opts := capture.OptionsFor(capability.Invocation{Agent: d, State: st})
	after := "new"
	for {
		raw, err := in.xinli.CapturePreview(ctx, bl, eu, sid, after, all, opts)
		if err != nil {
			return "", "", err
		}
		page, err := capture.ParsePage(raw, opts)
		if err != nil {
			return "", "", err
		}
		if page.Kind != "page" {
			return "", "", nil
		}
		if from == "" {
			from = page.Segments[0].GetRef()
		}
		_, next, err := capture.Render(page, opts.MaxToolResult)
		if err != nil {
			return "", "", err
		}
		if next.Span.Seq == 0 {
			return from, next.String(), nil
		}
		if next.String() == after {
			return "", "", fmt.Errorf("xinli capture preview made no progress")
		}
		after = next.String()
	}
}

func spanAccepted(before, after []xinlifixture.Changeset, sid, from, to string) bool {
	news := newSessionChangesets(before, after, sid)
	return len(news) == 1 && news[0].Author == xinlifixture.AuthorDay && !news[0].Source.Day &&
		news[0].Source.From == "#"+from && news[0].Source.To == "#"+to
}

func newSessionChangesets(before, after []xinlifixture.Changeset, sid string) []xinlifixture.Changeset {
	var news []xinlifixture.Changeset
	for _, c := range after {
		if c.Session == sid && !slices.ContainsFunc(before, func(old xinlifixture.Changeset) bool { return old.ID == c.ID }) {
			news = append(news, c)
		}
	}
	return news
}

func (o *observation) appendRun(r *observation) {
	o.runID = r.runID
	o.status = r.status
	o.calls = append(o.calls, r.calls...)
	o.callArgs = append(o.callArgs, r.callArgs...)
	o.callResults = append(o.callResults, r.callResults...)
	o.steps = append(o.steps, r.steps...)
	o.approvals += r.approvals
	o.questions += r.questions
	if r.reply != "" {
		o.reply = strings.TrimSpace(o.reply + "\n\n" + r.reply)
	}
	o.tokens += r.tokens
	o.inTokens += r.inTokens
	o.cachedTokens += r.cachedTokens
	o.compacted = o.compacted || r.compacted
	o.compactions += r.compactions
	o.takeovers += r.takeovers
}
