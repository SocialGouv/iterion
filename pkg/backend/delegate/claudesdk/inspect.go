package claudesdk

// ResolveSpawn materialises a set of Options into the environment overrides
// and CLI arguments the spawned `claude` process would receive.
//
// It exists because assembling options is the part that goes wrong: an option
// built correctly but never appended, or appended to one of the two spawns
// (the main pass and the structured-output formatting pass) and not the other,
// is invisible to a test that only exercises the builder. This makes the
// composed result observable without spawning anything.
//
// The prompt is deliberately excluded — it is appended at call time, not
// derived from the options.
func ResolveSpawn(opts ...Option) (env map[string]string, args []string) {
	cfg := &config{}
	for _, o := range opts {
		if o != nil {
			o(cfg)
		}
	}
	env = cfg.env
	if env == nil {
		env = map[string]string{}
	}
	return env, buildArgs(configToProcess(cfg), false)
}
