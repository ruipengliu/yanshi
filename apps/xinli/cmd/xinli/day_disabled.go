//go:build !daytest

package main

import (
	"context"
	"flag"
	"io"
)

func testFlags(_ *flag.FlagSet, _ *config) func() error {
	return func() error { return nil }
}

func testCommand(context.Context, []string, io.Reader, io.Writer) (bool, error) {
	return false, nil
}
