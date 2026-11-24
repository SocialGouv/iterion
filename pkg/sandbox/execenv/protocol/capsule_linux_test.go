//go:build linux && (amd64 || arm64)

package protocol

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestEnvironmentCapsuleRequiresImmutableCapture(t *testing.T) {
	for seals := 0; seals < 16; seals++ {
		t.Run(fmt.Sprintf("seals-%d", seals), func(t *testing.T) {
			fd, err := unix.MemfdCreate("capsule-test", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
			if err != nil {
				t.Fatal(err)
			}
			f := os.NewFile(uintptr(fd), "capsule")
			defer f.Close()
			if err := f.Chmod(0600); err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteString("KEY=value\x00"); err != nil {
				t.Fatal(err)
			}
			if _, err := f.Seek(0, 0); err != nil {
				t.Fatal(err)
			}
			if _, err := unix.FcntlInt(f.Fd(), unix.F_ADD_SEALS, seals); err != nil {
				t.Fatal(err)
			}
			err = validateEnvironmentCapsule(f)
			if seals != 15 {
				if err == nil {
					t.Fatal("accepted incomplete seals")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteAt([]byte("changed"), 0); err == nil {
				t.Fatal("sealed capture writable")
			}
			for _, size := range []int64{0, 100} {
				if err := f.Truncate(size); err == nil {
					t.Fatal("sealed capture resizable")
				}
			}
			if _, err := f.Seek(1, 0); err != nil {
				t.Fatal(err)
			}
			if validateEnvironmentCapsule(f) == nil {
				t.Fatal("accepted nonzero offset")
			}
			if _, err := f.Seek(0, 0); err != nil {
				t.Fatal(err)
			}
			if err := f.Chmod(0644); err != nil {
				t.Fatal(err)
			}
			if validateEnvironmentCapsule(f) == nil {
				t.Fatal("accepted public capsule")
			}
		})
	}
}

func TestEnvironmentCapsuleRejectsOrdinaryDescriptors(t *testing.T) {
	for _, root := range []string{t.TempDir(), "/dev/shm"} {
		t.Run(filepath.Base(root), func(t *testing.T) {
			f, err := os.CreateTemp(root, "iterion-capsule-refusal-")
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			defer os.Remove(f.Name())
			if validateEnvironmentCapsule(f) == nil {
				t.Fatal("accepted linked ordinary file")
			}
			if err := os.Remove(f.Name()); err != nil {
				t.Fatal(err)
			}
			if validateEnvironmentCapsule(f) == nil {
				t.Fatal("accepted deleted ordinary file")
			}
		})
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if validateEnvironmentCapsule(r) == nil {
		t.Fatal("accepted pipe")
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if validateEnvironmentCapsule(r) == nil {
		t.Fatal("accepted closed descriptor")
	}
}

func TestEnvironmentCapsuleNonExecutable(t *testing.T) {
	fd, e := unix.MemfdCreate("native-capture", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING|unix.MFD_NOEXEC_SEAL)
	if errors.Is(e, unix.EINVAL) {
		t.Skip("kernel lacks MFD_NOEXEC_SEAL")
	}
	if e != nil {
		t.Fatal(e)
	}
	f := os.NewFile(uintptr(fd), "capture")
	defer f.Close()
	if e = f.Chmod(0600); e != nil {
		t.Fatal(e)
	}
	if _, e = f.WriteString("SECRET=synthetic\x00"); e != nil {
		t.Fatal(e)
	}
	if _, e = unix.FcntlInt(f.Fd(), unix.F_ADD_SEALS, 15); e != nil {
		t.Fatal(e)
	}
	if _, e = f.Seek(0, 0); e != nil {
		t.Fatal(e)
	}
	seals, e := unix.FcntlInt(f.Fd(), unix.F_GET_SEALS, 0)
	t.Logf("seals=%d errno=%v", seals, e)
	if _, e = f.WriteAt([]byte("changed"), 0); e == nil {
		t.Fatal("capture mutable")
	}
	if e = validateEnvironmentCapsule(f); e != nil {
		t.Fatalf("immutable non-executable capture refused: %v", e)
	}
}
func TestEnvironmentCapsuleRequiredSealMatrix(t *testing.T) {
	for mask := 0; mask < 64; mask++ {
		t.Run(fmt.Sprintf("mask-%d", mask), func(t *testing.T) {
			fd, e := unix.MemfdCreate("matrix", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
			if e != nil {
				t.Fatal(e)
			}
			f := os.NewFile(uintptr(fd), "matrix")
			defer f.Close()
			if e = f.Chmod(0600); e != nil {
				t.Fatal(e)
			}
			if _, e = f.WriteString("K=v\x00"); e != nil {
				t.Fatal(e)
			}
			if _, e = f.Seek(0, 0); e != nil {
				t.Fatal(e)
			}
			if _, e = unix.FcntlInt(f.Fd(), unix.F_ADD_SEALS, mask); e != nil {
				if mask&48 != 0 && errors.Is(e, unix.EINVAL) {
					t.Skip("kernel lacks optional additional seal")
				}
				t.Fatalf("mask%d: %v", mask, e)
			}
			got := validateEnvironmentCapsule(f)
			want := (mask & 15) == 15
			if (got == nil) != want {
				t.Fatalf("mask%d acceptance=%v want%v", mask, got == nil, want)
			}
			if want {
				alias, e := unix.Dup(fd)
				if e != nil {
					t.Fatal(e)
				}
				defer unix.Close(alias)
				if _, e = unix.Pwrite(alias, []byte("x"), 0); e == nil {
					t.Fatal("alias writable")
				}
				if e = unix.Ftruncate(alias, 0); e == nil {
					t.Fatal("alias truncatable")
				}
			}
		})
	}
}
