package scripthost

// raceSafeLimits raises a test's memory bound to whatever the child needs just
// to exist. Zero outside a race build, so the ordinary run exercises the real
// defaults.
func raceSafeLimits(l Limits) Limits {
	if childMemoryFloor > l.Memory {
		l.Memory = childMemoryFloor
	}
	return l
}
