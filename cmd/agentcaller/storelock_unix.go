//go:build unix

package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// lockStore takes the account for this process. One linked device has one connection:
// a second process on the same store would replace the first one's, and a call under
// way would lose its signalling. The lock is a file next to the store; it is released
// when the process ends, however it ends.
func lockStore(path string) (release func(), err error) {
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open store lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s", errStoreBusy, path)
		}
		return nil, fmt.Errorf("lock store: %w", err)
	}
	return func() { f.Close() }, nil
}
