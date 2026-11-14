package eval

import "yanshi/internal/eval/xinlifixture"

// 只取最终投影里的文本；历史变更集和拒收参数不能代表仍然落库的摘要。
func xinliTexts(st *xinlifixture.State) []string {
	var out []string
	for _, x := range st.Exchanges {
		if !x.Live() {
			continue
		}
		out = append(out, x.Title, x.Summary)
		out = append(out, x.Risks...)
		out = append(out, x.Awareness...)
		for _, d := range x.Decisions {
			out = append(out, d.Text)
		}
		for _, p := range x.Projects {
			out = append(out, p.Proposed)
		}
	}
	for _, d := range st.Days {
		out = append(out, d.Headline)
		out = append(out, d.Highlights...)
		out = append(out, d.Awareness.Habits...)
		out = append(out, d.Awareness.Suggestions...)
	}
	for _, p := range st.Projects {
		if p.MergedInto != "" {
			continue
		}
		out = append(out, p.Name, p.Status)
		out = append(out, p.NextSteps...)
		out = append(out, p.OpenQuestions...)
	}
	return out
}
