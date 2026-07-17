//go:build windows

package runshell

import (
	"os"
	"os/exec"
)

type SpawnOptions struct {
	WorkDir    string
	Env        []string
	Cols, Rows uint16
}

type Session struct {
	PTY *os.File
	Cmd *exec.Cmd
}

func Spawn(SpawnOptions) (*Session, error) { return nil, ErrUnsupported }

func (s *Session) Resize(cols, rows uint16) error { return ErrUnsupported }

func (s *Session) Terminate() {}
