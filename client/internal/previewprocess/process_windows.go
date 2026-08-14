//go:build windows

package previewprocess

import (
	"os/exec"
)

func configureProcessGroup(_ *exec.Cmd) {}

func signalProcess(command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return nil
	}
	return command.Process.Kill()
}

func killProcess(command *exec.Cmd) error { return signalProcess(command) }
