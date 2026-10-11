package session

import (
	"fmt"
	"testing"

	v1 "yanshi/gen/yanshi/v1"
)

func benchState(b *testing.B, runs int) *State {
	var evs []*v1.Event
	evs = append(evs, created())
	for i := range runs {
		r := fmt.Sprintf("r%d", i)
		calls := []string{r + "a", r + "b", r + "c", r + "d"}
		evs = append(evs, requested(r), attempt(r, 1), assistant(r, 1, calls...))
		for _, c := range calls {
			evs = append(evs, started(r, 1, c), result(r, 1, c))
		}
		evs = append(evs, assistant(r, 1), completed(r, 1))
	}
	evs = append(evs, requested("live"), attempt("live", 1))
	st, err := Reduce(build(evs...))
	if err != nil {
		b.Fatal(err)
	}
	return st
}

// BenchmarkCheck：每次提交的校验代价不应随 Session 中已结束的 Run 的数量明显增长（Clone 共享终态的 Run）。
func BenchmarkCheck(b *testing.B) {
	for _, n := range []int{10, 100, 1000} {
		st := benchState(b, n)
		payload := &v1.Event_AssistantMessage{AssistantMessage: &v1.AssistantMessage{RunId: "live", Attempt: 1}}
		b.Run(fmt.Sprint(n, "runs"), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				if err := st.Check(&v1.Event{SessionId: "s", Payload: payload}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
