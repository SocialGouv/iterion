//go:build desktop && darwin

package main

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// applyArtifact extracts the .zip (which contains Iterion.app/) into a sibling
// directory of the running .app and atomically swaps. Failure leaves the
// running .app intact — we never destructively unzip on top of it.
func applyArtifact(body []byte, _ *Release) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	appDir := exe
	for filepath.Ext(appDir) != ".app" && filepath.Dir(appDir) != appDir {
		appDir = filepath.Dir(appDir)
	}
	if filepath.Ext(appDir) != ".app" {
		return errors.New("updater: cannot locate enclosing .app bundle")
	}
	parent := filepath.Dir(appDir)
	stage := filepath.Join(parent, "Iterion.app.update")
	_ = os.RemoveAll(stage)
	if err := unzipBytes(body, stage); err != nil {
		return err
	}
	old := appDir + ".old"
	_ = os.RemoveAll(old)
	if err := os.Rename(appDir, old); err != nil {
		return err
	}
	if err := os.Rename(stage, appDir); err != nil {
		_ = os.Rename(old, appDir)
		return err
	}
	go os.RemoveAll(old)
	return nil
}

func unzipBytes(body []byte, dst string) error {
	r, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return err
	}
	dstClean := filepath.Clean(dst)
	for _, f := range r.File {
		out := filepath.Join(dst, f.Name)
		if !strings.HasPrefix(out, dstClean+string(os.PathSeparator)) && out != dstClean {
			return errors.New("updater: archive contains escaping path")
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(out, f.Mode()); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		w, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, f.Mode())
		if err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			w.Close()
			return err
		}
		_, copyErr := io.Copy(w, rc)
		rc.Close()
		w.Close()
		if copyErr != nil {
			return copyErr
		}
	}
	return nil
}
