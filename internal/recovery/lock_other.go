//go:build !linux && !darwin

package recovery

import "errors"

func (e *Engine) lock() (func(), error) {
	return nil, errors.New("guest recovery requires a Unix filesystem lock")
}
