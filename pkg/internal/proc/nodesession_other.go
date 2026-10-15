//go:build !linux

package proc

// BootTicksNow has no /proc to read on this platform.
func BootTicksNow() (BootTicks, error) { return 0, ErrNodeSessionSweepUnsupported }

// FindNodeSession cannot look on a platform with no /proc.
func FindNodeSession(token string) ([]SessionProcess, error) {
	if err := checkNodeSessionToken(token); err != nil {
		return nil, err
	}
	return nil, ErrNodeSessionSweepUnsupported
}

// SweepNodeSession cannot look on a platform with no /proc.
func SweepNodeSession(token string, _ BootTicks) (SweepReport, error) {
	if err := checkNodeSessionToken(token); err != nil {
		return SweepReport{}, err
	}
	return SweepReport{}, ErrNodeSessionSweepUnsupported
}
