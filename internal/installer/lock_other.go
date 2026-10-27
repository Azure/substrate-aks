//go:build !linux && !darwin

package installer

import (
	"errors"
	"os"
	"os/exec"
)

func configureProcess(*exec.Cmd) {}

func Lock(Config) (*os.File, error) {
	return nil, errors.New("the experimental CLI currently supports Linux and macOS hosts")
}
