//go:build !linux

package fswatch

func resourceError(err error) error { return err }
