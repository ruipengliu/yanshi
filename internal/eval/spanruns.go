package eval

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"yanshi/internal/agentdef"
	"yanshi/internal/capture"
	"yanshi/internal/session"
	"yanshi/internal/xinlimock"
)

const (
	xinliSpanInput    = "整理一个时段"
	xinliSpanAllInput = "整理一个时段（包括未结束的时段）"
	xinliDayInput     = "只按账本整理今天"
)

// 只给评测派生的 AgentDef 追加实验规则；交谈、承诺与 Memory 的要求沿用原指令，便于消融 Run 边界的影响。
const xinliSpanInstructions = `

本次采用「每个完整时段一个 Run」的实验流程。以下规则替代前面关于输入、继续读取下一时段与当天收尾的规则，其余交谈、承诺、记忆与健康信息的要求都照旧：
- 输入「整理一个时段」：只做第 1–3 步，第一次 captures 用 {"after":"new"}。
- 输入「整理一个时段（包括未结束的时段）」：也只做第 1–3 步，第一次 captures 用 {"after":"new","all":true}，续页同样带 all:true。
- 只处理这一次读到的第一个完整时段。页尾写「未完」且 after 带 ~ 时，照抄 after 继续读取同一个时段，读完所有续页再用一次 file_changes 交出完整时段。页尾写「到此结束」后不要再读下一时段。
- file_changes 的 source.from/to 必须覆盖本时段从第一段到最后一段的完整范围；即使全是闲聊、没有交谈或承诺，也要以这个 source 交出空变更集（exchanges:{}, commitments:{}），作为已经整理的回执。被拒绝时在本次 Run 内改正后重交整个变更集；业务线接受后用一两句话回复并结束，不做当天整理、不调用 file_day。没有新旁听、或时段还在进行时直接回复。
- 输入「只按账本整理今天」：所有时段已经在之前的 Run 中交出，本次只做第 4–6 步。直接用 day_ledger 取回账本，按需交 source:{day:true} 的当天整理，再调用 file_day。不要调用 captures，不重新按时段交出。
`

// spanTurns 扮演业务线的串行调度：一个原轮次内拆出多个 Run，仍只提交一次旁听、检查一次断言。
// 触发的「同一位置只触发一次」不支持连续排空，这里只用普通输入实验，不改平台触发（ADR-0040）。
func (in *instance) spanTurns(ctx context.Context, sid, bl, endUser string, turn Turn, timeout time.Duration) (*observation, error) {
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
		in.xinli.Submitted(bl, endUser, sid, businessInput)
		o, err := in.turn(ctx, sid, t, timeout)
		if o != nil {
			obs.appendRun(o)
		}
		if err != nil {
			return err
		}
		if o.status != "completed" {
			return fmt.Errorf("span run ended with status %s", o.status)
		}
		if businessInput == "整理交谈" && (slices.Contains(o.calls, "mcp_xinli__day_ledger") || slices.Contains(o.calls, "mcp_xinli__file_day")) {
			return fmt.Errorf("span run called day reconciliation tools")
		}
		return nil
	}
	for {
		st, err := in.svc.Load(ctx, sid)
		if err != nil {
			return obs, err
		}
		from, to, err := in.nextSpan(ctx, sid, st, all)
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
				old := in.xinli.State(bl, endUser).Changesets
				if err := run(input); err != nil {
					return obs, err
				}
				after, err := in.svc.Load(ctx, sid)
				if err != nil {
					return obs, err
				}
				if after.Captures.Read[capture.Stream].GetAfter() != st.Captures.Read[capture.Stream].GetAfter() {
					return obs, fmt.Errorf("empty span run advanced the capture position")
				}
				if len(newSessionChangesets(old, in.xinli.State(bl, endUser).Changesets, sid)) != 0 {
					return obs, fmt.Errorf("empty span run filed changes without reading a span")
				}
			}
			break
		}
		before := in.xinli.State(bl, endUser)
		if err := run(input); err != nil {
			return obs, err
		}
		ran = true
		after, err := in.svc.Load(ctx, sid)
		if err != nil {
			return obs, err
		}
		if got := after.Captures.Read[capture.Stream].GetAfter(); got != to {
			return obs, fmt.Errorf("span #%s–#%s: capture position %q, want %q (no progress, partial span or overread)", from, to, got, to)
		}
		accepted := in.xinli.State(bl, endUser)
		if !spanAccepted(before.Changesets, accepted.Changesets, sid, from, to) {
			return obs, fmt.Errorf("span #%s–#%s: no accepted changeset; stopping before after=new can skip an unfiled span", from, to)
		}
	}
	if all {
		before, err := in.svc.Load(ctx, sid)
		if err != nil {
			return obs, err
		}
		old := in.xinli.State(bl, endUser)
		callStart := len(obs.calls)
		if err := run(xinliDayInput); err != nil {
			return obs, err
		}
		after, err := in.svc.Load(ctx, sid)
		if err != nil {
			return obs, err
		}
		if before.Captures.Read[capture.Stream].GetAfter() != after.Captures.Read[capture.Stream].GetAfter() {
			return obs, fmt.Errorf("day reconciliation advanced the capture position")
		}
		if slices.Contains(obs.calls[callStart:], capture.Capability) {
			return obs, fmt.Errorf("day reconciliation reread captures instead of using the ledger")
		}
		filed := in.xinli.State(bl, endUser)
		for _, cs := range newSessionChangesets(old.Changesets, filed.Changesets, sid) {
			if cs.Author != xinlimock.AuthorDay || !cs.Source.Day {
				return obs, fmt.Errorf("day reconciliation filed a span changeset")
			}
		}
		if !slices.ContainsFunc(filed.Days, func(d xinlimock.Day) bool {
			return d.Session == sid && !slices.ContainsFunc(old.Days, func(old xinlimock.Day) bool {
				return old.Session == sid && old.FiledAt.Equal(d.FiledAt)
			})
		}) {
			return obs, fmt.Errorf("day reconciliation has no accepted file_day")
		}
	}
	return obs, nil
}

// nextSpan 用工具自身的分页规则预览下一完整时段的边界。续页不拆 Run，也不解析给模型看的中文提示。
func (in *instance) nextSpan(ctx context.Context, sid string, st *session.State, all bool) (from, to string, err error) {
	read := st.Captures.Read[capture.Stream]
	at := capture.Cursor{}
	gap := agentdef.DefaultExchangeGap
	if read != nil {
		gap = time.Duration(read.GetExchangeGapMs()) * time.Millisecond
		if s := read.GetAfter(); s != "" && s != "0" {
			var ok bool
			if at.Last, ok = capture.ParseRef(s); !ok {
				return "", "", fmt.Errorf("span run must start at a complete span boundary, got %q", s)
			}
		}
	} else {
		d, err := in.agents.Get(st.Agent)
		if err != nil {
			return "", "", err
		}
		if s, ok := d.Stream(capture.Stream); ok {
			gap = s.ExchangeGap
		}
	}
	start := at.Last
	opts := capture.Options{Limit: capture.DefaultLimit, MaxChars: capture.MaxChars, Gap: gap, Now: time.Now(), All: all}
	for {
		_, next, err := capture.Page(ctx, in.svc.Store.Log, sid, at, opts)
		if err != nil || next.Last.Seq == 0 {
			return "", "", err
		}
		if next.Span.Seq == 0 {
			to = next.String()
			break
		}
		at = next
	}
	// Capture 的位置没有包含在内容块中，从日志取边界后的第一段即可；不读取或复制原话到业务线。
	pos := start.Seq
	if pos > 0 {
		pos--
	}
	for {
		events, err := in.svc.Store.Log.Read(ctx, sid, pos, 200)
		if err != nil {
			return "", "", err
		}
		for _, e := range events {
			pos = e.GetSeq()
			c := e.GetCaptureAppended()
			if c.GetStream() != capture.Stream {
				continue
			}
			for i := range c.GetSegments() {
				r := capture.Ref{Seq: e.GetSeq(), Index: i}
				if r.Seq > start.Seq || r.Seq == start.Seq && r.Index > start.Index {
					return r.String(), to, nil
				}
			}
		}
		if len(events) < 200 {
			return "", "", fmt.Errorf("span start not found after %s", start)
		}
	}
}

func spanAccepted(before, after []xinlimock.Changeset, sid, from, to string) bool {
	news := newSessionChangesets(before, after, sid)
	return len(news) == 1 && news[0].Author == xinlimock.AuthorDay && !news[0].Source.Day &&
		news[0].Source.From == "#"+from && news[0].Source.To == "#"+to
}

func newSessionChangesets(before, after []xinlimock.Changeset, sid string) []xinlimock.Changeset {
	var news []xinlimock.Changeset
	for _, c := range after {
		if c.Session == sid && !slices.ContainsFunc(before, func(old xinlimock.Changeset) bool { return old.ID == c.ID }) {
			news = append(news, c)
		}
	}
	return news
}

func (o *observation) appendRun(r *observation) {
	o.status = r.status
	o.calls = append(o.calls, r.calls...)
	o.callArgs = append(o.callArgs, r.callArgs...)
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
