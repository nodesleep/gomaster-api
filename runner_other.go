//go:build !linux

package main

import (
	"context"
	"errors"
)

type localRunner struct{}

func newLocalRunner(string) (*localRunner, error) {
	return nil, errors.New("local runner currently requires Linux for process-group cleanup")
}
func (*localRunner) Run(context.Context, request) result { return result{Status: "internal_error"} }
