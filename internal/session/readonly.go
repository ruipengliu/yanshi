package session

// ReadOnlyInAttempt 只采信该 Attempt 执行前记下的证据；缺失按未知处理。
func (c *Call) ReadOnlyInAttempt(attempt uint32) bool {
	for i, n := range c.StartedAttempts {
		if n == attempt {
			return i < len(c.StartedReadOnly) && c.StartedReadOnly[i]
		}
	}
	return false
}

// CallsReadOnly 判断 Run 的每个调用是否都有只读证据，跨 Attempt 取与。
// 没有 Started（如参数错误或能力不可用）不算只读；没有调用时为 true。
// 它只判断证据，不包含重答的其他资格条件，也不豁免 show_card。
func (r *Run) CallsReadOnly() bool {
	if r == nil {
		return false
	}
	for _, c := range r.Calls {
		if !c.Started() || len(c.StartedReadOnly) != len(c.StartedAttempts) {
			return false
		}
		for _, readOnly := range c.StartedReadOnly {
			if !readOnly {
				return false
			}
		}
	}
	return true
}
