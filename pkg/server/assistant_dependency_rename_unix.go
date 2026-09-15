//go:build linux || darwin

package server

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func openDependencyDirectory(path string, expected os.FileInfo) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil || expected == nil || !os.SameFile(expected, info) || info.Mode() != expected.Mode() {
		_ = file.Close()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("dependency rename parent changed")
	}
	return file, nil
}

func renameDependencyNoReplace(from, to string, fromInfo, toInfo os.FileInfo) error {
	source, err := openDependencyDirectory(filepath.Dir(from), fromInfo)
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	target, err := openDependencyDirectory(filepath.Dir(to), toInfo)
	if err != nil {
		return err
	}
	defer func() { _ = target.Close() }()
	return renameDependencyAt(int(source.Fd()), filepath.Base(from), int(target.Fd()), filepath.Base(to))
}
