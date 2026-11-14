package eval

import (
	"fmt"
	"path"
	"strings"
)

type observedCallResult struct {
	callID     string
	capability string
	text       string
}

func callResultBounds(want CallResultMatch) (int, *int) {
	min := 1
	var max *int
	if want.Count != nil {
		if want.Count.Min != nil {
			min = *want.Count.Min
		}
		max = want.Count.Max
	}
	return min, max
}

func validateCallResultMatch(want CallResultMatch) error {
	if want.Capability == "" {
		return fmt.Errorf("capability is required")
	}
	if _, err := path.Match(want.Capability, ""); err != nil {
		return fmt.Errorf("invalid capability glob: %w", err)
	}
	min, max := callResultBounds(want)
	if min < 0 || max != nil && (*max < 0 || *max < min) {
		return fmt.Errorf("count must satisfy 0 <= min <= max (default min is 1)")
	}
	return nil
}

func checkCallResults(add func(string, bool, string), wants []CallResultMatch, results []observedCallResult) {
	for i, want := range wants {
		n := 0
		for _, result := range results {
			if result.capability != "" && matches([]string{want.Capability}, result.capability) && strings.Contains(result.text, want.Contains) {
				n++
			}
		}
		min, max := callResultBounds(want)
		bounds := fmt.Sprintf("至少 %d 条", min)
		if max != nil {
			bounds += fmt.Sprintf("、至多 %d 条", *max)
		}
		add(fmt.Sprintf("call_results %d %s", i+1, want.Capability), n >= min && (max == nil || n <= *max),
			fmt.Sprintf("调用 %s 的完整结果中包含 %q 的有 %d 条，要求%s", want.Capability, want.Contains, n, bounds))
	}
}
