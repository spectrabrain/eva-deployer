package operation

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

const runLockName = "run.lock"

// Liveness of an operation whose recorded status is running.
const (
	// LivenessActive: a live process still holds the run lock.
	LivenessActive = "active"
	// LivenessInterrupted: the run lock exists but nobody holds it, so the
	// process died (Ctrl+C, SSH disconnect, kill) before recording a result.
	LivenessInterrupted = "interrupted"
	// LivenessUnknown: no run lock, written by a tool version without one.
	LivenessUnknown = "unknown"
)

// RunLock is an exclusive lock held for as long as the operation runs.
type RunLock struct{ file *os.File }

// File is passed to child processes (exec.Cmd.ExtraFiles) so the lock stays
// held while an orphaned Ansible run is still changing the host.
func (lock *RunLock) File() *os.File { return lock.file }

func (lock *RunLock) Release() {
	if lock != nil && lock.file != nil {
		lock.file.Close()
	}
}

// AcquireRunLock takes the operation's run lock. The kernel drops it when
// every holder exits for any reason, so a running status without a held lock
// reliably means the run was interrupted.
func AcquireRunLock(root, id string) (*RunLock, error) {
	if root == "" {
		root = DefaultRoot
	}
	if !operationIDPattern.MatchString(id) {
		return nil, fmt.Errorf("invalid operation ID %q", id)
	}
	file, err := os.OpenFile(filepath.Join(root, id, runLockName), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open operation run lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("operation %s is already running in another process", id)
		}
		return nil, fmt.Errorf("lock operation %s: %w", id, err)
	}
	return &RunLock{file: file}, nil
}

// RunLiveness reports whether a running operation is still being executed.
func RunLiveness(root string, record Record) (string, error) {
	if root == "" {
		root = DefaultRoot
	}
	if !operationIDPattern.MatchString(record.ID) {
		return "", fmt.Errorf("invalid operation ID %q", record.ID)
	}
	file, err := os.OpenFile(filepath.Join(root, record.ID, runLockName), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return LivenessUnknown, nil
	}
	if err != nil {
		return "", fmt.Errorf("open operation run lock: %w", err)
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return LivenessActive, nil
		}
		return "", fmt.Errorf("probe operation %s run lock: %w", record.ID, err)
	}
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	return LivenessInterrupted, nil
}
