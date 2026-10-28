package fixture

import "errors"

var (
	errPageOptions = errors.New("invalid page options")
	errJSON        = errors.New("invalid JSON")
	errDate        = errors.New("from must be YYYY-MM-DD")
	errSession     = errors.New("session is required")
)
