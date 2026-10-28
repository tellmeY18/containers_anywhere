//go:build !darwin && !linux && !windows

package main

import "errors"

func deviceID(string) (string, error) { return "", errors.New("not supported on this platform") }
