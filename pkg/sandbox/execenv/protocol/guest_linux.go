//go:build linux && (amd64 || arm64)

package protocol

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

const envFDKey = "IENV_NATIVE_ENV_FD"
const imageFDKey = "IENV_NATIVE_IMAGE_FD"
const oPath = 0x200000
const atEmptyPath = 0x1000
const atEffectiveAccess = 0x200
const sysFaccessat2 = 439 // Linux amd64 and arm64.

// GuestPaths is immutable build metadata supplied by the tiny main. Neither
// path comes from the invocation frame, pod environment, argv or config.
type GuestPaths struct{ Capsule, Payload string }

// RunGuest owns only the two descriptors from the native control environment.
// It validates the executed image chain before reading FD0, closes its owned
// descriptors before target exec (also on refusal), and never installs pod
// variables into the Go runtime environment. On successful exec it cannot return.
func RunGuest(paths GuestPaths) (int, error) {
	envFile, imageFile, err := nativeDescriptors(os.Environ())
	if envFile != nil {
		defer envFile.Close()
	}
	if imageFile != nil {
		defer imageFile.Close()
	}
	if err != nil {
		return 125, err
	}
	if err = validateGuestChain(paths, envFile, imageFile); err != nil {
		return 125, err
	}
	inherited, err := ReadNativeEnvironment(envFile)
	if err != nil {
		return 125, err
	}
	// Close both before reading the invocation. No workload can retain these
	// even if parsing, cwd resolution or target exec subsequently fails.
	if err = envFile.Close(); err != nil {
		return 125, refused("capsule", "close", err)
	}
	if err = imageFile.Close(); err != nil {
		return 125, refused("image", "close", err)
	}
	in, err := ReadInvocation(os.Stdin)
	if err != nil {
		return 125, err
	}
	environment, err := ApplyEnvironmentOverrides(inherited, in.Env)
	if err != nil {
		return 125, err
	}
	if in.WorkDir != nil {
		if err = syscall.Chdir(*in.WorkDir); err != nil {
			return 125, refused("directory", "chdir", err)
		}
		if _, explicit := in.Env["PWD"]; !explicit {
			cwd, e := syscall.Getwd()
			if e != nil {
				return 125, refused("directory", "getcwd", e)
			}
			environment, err = ApplyEnvironmentOverrides(environment, map[string]string{"PWD": cwd})
			if err != nil {
				return 125, err
			}
		}
	}
	if err = writeAll(os.Stdout, []byte(InvocationReadiness(in))); err != nil {
		return 125, refused("ready", "write", err)
	}
	return execNativeTarget(in.Argv, environment, in.Profile)
}

func nativeDescriptors(environment []string) (*os.File, *os.File, error) {
	fds := map[string]int{}
	bad := false
	for _, entry := range environment {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || (key != envFDKey && key != imageFDKey) {
			bad = true
			continue
		}
		if _, seen := fds[key]; seen {
			bad = true
			continue
		}
		valid := value != ""
		for i := range value {
			if value[i] < '0' || value[i] > '9' {
				valid = false
			}
		}
		n, e := strconv.Atoi(value)
		if e != nil || n < 3 || !valid {
			bad = true
			continue
		}
		fds[key] = n
	}
	var envFile, imageFile *os.File
	if fd, ok := fds[envFDKey]; ok {
		envFile = os.NewFile(uintptr(fd), "native environment")
	}
	if fd, ok := fds[imageFDKey]; ok {
		if envFile != nil && fd == fds[envFDKey] {
			bad = true
		} else {
			imageFile = os.NewFile(uintptr(fd), "native image")
		}
	}
	if bad || envFile == nil || imageFile == nil {
		return envFile, imageFile, refused("control", "descriptors", nil)
	}
	return envFile, imageFile, nil
}

func validateGuestChain(paths GuestPaths, envFile, imageFile *os.File) error {
	if err := validateEnvironmentCapsule(envFile); err != nil {
		return err
	}
	if err := verifyImage(imageFile, paths.Capsule); err != nil {
		return err
	}
	// Identity and effective-access checks do not require read permission on
	// an otherwise executable image (for example an operator's mode0111 file).
	selfFD, err := syscall.Open("/proc/self/exe", oPath|syscall.O_CLOEXEC, 0)
	if err != nil {
		return refused("image", "proc-required", err)
	}
	self := os.NewFile(uintptr(selfFD), "executed payload")
	defer self.Close()
	return verifyImage(self, paths.Payload)
}

// validateEnvironmentCapsule checks immutable capture rather than st_nlink:
// gVisor reports one link for a memfd, while Linux reports zero. An ordinary
// tmpfs file starts with F_SEAL_SEAL and cannot acquire these content seals.
func validateEnvironmentCapsule(file *os.File) error {
	st, err := file.Stat()
	if err != nil {
		return refused("capsule", "stat", err)
	}
	if !st.Mode().IsRegular() || st.Mode().Perm() != 0600 {
		return refused("capsule", "shape", nil)
	}
	const fGetSeals = 1034
	const captureSeals = 15 // F_SEAL_SEAL | SHRINK | GROW | WRITE.
	seals, _, errno := syscall.Syscall(syscall.SYS_FCNTL, file.Fd(), fGetSeals, 0)
	if errno != 0 {
		return refused("capsule", "seals", errno)
	}
	if seals&captureSeals != captureSeals {
		return refused("capsule", "seals", nil)
	}
	offset, err := file.Seek(0, io.SeekCurrent)
	if err != nil || offset != 0 {
		return refused("capsule", "offset", err)
	}
	return nil
}

// verifyImage checks the actual descriptor against the build path, then checks
// both the declared and resolved ancestor chains. It does not attest a hostile
// image or claim safety against arbitrary concurrent filesystem mutation.
func verifyImage(actual *os.File, expected string) error {
	if !filepath.IsAbs(expected) {
		return refused("image", "build-path", nil)
	}
	fd, err := syscall.Open(expected, oPath|syscall.O_CLOEXEC, 0)
	if err != nil {
		return refused("image", "open", err)
	}
	named := os.NewFile(uintptr(fd), "expected image")
	defer named.Close()
	a, err := actual.Stat()
	if err != nil {
		return refused("image", "actual-stat", err)
	}
	b, err := named.Stat()
	if err != nil {
		return refused("image", "expected-stat", err)
	}
	if !a.Mode().IsRegular() || !os.SameFile(a, b) {
		return refused("image", "identity", nil)
	}
	if err = rejectWritable(actual); err != nil {
		return err
	}
	canonical, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
	if err != nil {
		return refused("image", "proc-required", err)
	}
	if !filepath.IsAbs(canonical) {
		return refused("image", "path", nil)
	}
	actualPath, err := os.Readlink("/proc/self/fd/" + strconv.FormatUint(uint64(actual.Fd()), 10))
	if err != nil {
		return refused("image", "proc-required", err)
	}
	if !filepath.IsAbs(actualPath) {
		return refused("image", "actual-path", nil)
	}
	for _, path := range []string{expected, canonical, actualPath} {
		// Retain raw components, including symlink/.., while checking every
		// traversed prefix; filepath.Dir would clean away a traversed directory.
		components := strings.Split(path, "/")
		for i := 1; i < len(components); i++ {
			parent := strings.Join(components[:i], "/")
			if parent == "" {
				parent = "/"
			}
			pfd, e := syscall.Open(parent, oPath|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
			if e != nil {
				return refused("image", "parent-open", e)
			}
			pf := os.NewFile(uintptr(pfd), "image parent")
			e = rejectWritable(pf)
			closeErr := pf.Close()
			if e != nil {
				return e
			}
			if closeErr != nil {
				return refused("image", "parent-close", closeErr)
			}
		}
	}
	return nil
}

func rejectWritable(file *os.File) error {
	empty := [1]byte{0}
	_, _, errno := syscall.RawSyscall6(sysFaccessat2, file.Fd(), uintptr(unsafe.Pointer(&empty[0])), 2, atEmptyPath|atEffectiveAccess, 0, 0)
	if errno == syscall.EACCES || errno == syscall.EROFS {
		return nil
	}
	if errno != 0 {
		return refused("image", "access-check", errno)
	}
	return refused("image", "writable", nil)
}

// Exec uses final target environment for PATH, never the helper's closed env.
// Unset PATH uses the operator profile; present (even empty) PATH wins.
// ENOEXEC invokes the operator shell with the script path; E2BIG never falls back.
func execNativeTarget(argv, environment []string, requested *ExecutionProfile) (int, error) {
	profile := effectiveExecutionProfile(requested)
	if len(argv) == 0 || argv[0] == "" {
		return 127, refused("exec", "not-found", nil)
	}
	try := func(path string) (error, bool) {
		err := syscall.Exec(path, argv, environment)
		if errors.Is(err, syscall.ENOEXEC) {
			shellArgs := append([]string{profile.Shell, path}, argv[1:]...)
			return syscall.Exec(profile.Shell, shellArgs, environment), true
		}
		return err, false
	}
	if strings.Contains(argv[0], "/") {
		err, _ := try(argv[0])
		return execError(err)
	}
	path := profile.DefaultPath
	for _, entry := range environment {
		if value, ok := strings.CutPrefix(entry, "PATH="); ok {
			path = value
			break
		}
	}
	var denied error
	for _, dir := range strings.Split(path, ":") {
		candidate := argv[0]
		if dir != "" {
			candidate = dir + "/" + argv[0]
		}
		err, terminal := try(candidate)
		if terminal {
			return execError(err)
		}
		switch {
		case errors.Is(err, syscall.EACCES):
			denied = err
		case errors.Is(err, syscall.ENOENT), errors.Is(err, syscall.ENOTDIR):
		default:
			return execError(err)
		}
	}
	if denied != nil {
		return execError(denied)
	}
	return execError(syscall.ENOENT)
}
func execError(err error) (int, error) {
	code := 126
	if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ENOTDIR) {
		code = 127
	}
	return code, refused("exec", "target", err)
}
