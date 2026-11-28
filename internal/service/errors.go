package service

// ReasonError 为传输层携带稳定的子原因，同时保留原错误的文字与错误链。
type ReasonError struct {
	Err    error
	Reason string
}

func (e *ReasonError) Error() string { return e.Err.Error() }
func (e *ReasonError) Unwrap() error { return e.Err }
