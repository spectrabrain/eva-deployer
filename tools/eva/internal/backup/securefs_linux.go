package backup

import (
	"errors"
	"os"
	"strings"
	"syscall"
)

// Open each ancestor relative to a pinned descriptor. O_NOFOLLOW on every
// component (including files) avoids both symlink escapes and check/open races.
func openRoot(path string) (*os.File, error) {
	fd, err := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, errors.New("cannot open backup filesystem")
	}
	dir := os.NewFile(uintptr(fd), "/")
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		child, err := openChild(dir, part, true)
		dir.Close()
		if err != nil {
			return nil, err
		}
		dir = child
	}
	return dir, nil
}

func openChild(parent *os.File, name string, directory bool) (*os.File, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
		return nil, errors.New("invalid backup path segment")
	}
	flags := syscall.O_RDONLY | syscall.O_CLOEXEC | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
	if directory {
		flags |= syscall.O_DIRECTORY
	}
	fd, err := syscall.Openat(int(parent.Fd()), name, flags, 0)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return nil, os.ErrNotExist
		}
		return nil, errors.New("cannot open backup path: symlinks/non-regular files are refused; check target permissions (sudo may be required)")
	}
	file := os.NewFile(uintptr(fd), name)
	info, err := file.Stat()
	if err != nil || (directory && !info.IsDir()) || (!directory && !info.Mode().IsRegular()) {
		file.Close()
		return nil, errors.New("backup path has an unsafe file type")
	}
	return file, nil
}
