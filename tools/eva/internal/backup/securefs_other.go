//go:build !linux

package backup

import (
	"errors"
	"os"
)

func openRoot(string) (*os.File, error) {
	return nil, errors.New("secure backup inspection requires Linux; run on the actual Ansible target")
}

func openChild(*os.File, string, bool) (*os.File, error) {
	return nil, errors.New("secure backup inspection requires Linux; run on the actual Ansible target")
}
