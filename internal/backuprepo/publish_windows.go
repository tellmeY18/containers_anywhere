package backuprepo

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func linkUnsupported(_ *os.Root, err error) bool {
	return errors.Is(err, windows.ERROR_NOT_SUPPORTED) || errors.Is(err, windows.ERROR_INVALID_FUNCTION)
}
