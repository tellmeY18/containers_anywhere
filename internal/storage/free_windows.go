//go:build windows

package storage

import "errors"

func freeFraction(string) (float64, error) { return 0, errors.New("not supported on Windows") }
