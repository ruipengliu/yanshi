package fixture

import "errors"

var (
	errJSON    = errors.New("invalid JSON")
	errDate    = errors.New("from must be YYYY-MM-DD")
	errSession = errors.New("session is required")
)
