package credroute

// SandboxHomeState carries what the run's sandbox will actually bind of the
// operator's home, as the runtime resolved it: Mounted is false when
// host_state resolves to none, when the selected driver cannot bind host
// paths (kubernetes), or when the home itself is withheld (a project .env
// planted HOME). HomeDir is the host home the binds serve, empty when nothing
// of it crosses. The arms take it as an input rather than re-deriving it, so
// the admission and the sandbox start read the SAME resolution (#1768).
type SandboxHomeState struct {
	Mounted bool
	HomeDir string
}
