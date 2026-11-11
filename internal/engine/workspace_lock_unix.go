//go:build !windows

package engine

import (
	"os"
	"syscall"
)

func lockSurveyFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}
