package llmroute

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Environment variables. The ITERION_PLATFORM_ prefix puts these beside
// the platform record's other deployment defaults (ITERION_PLATFORM_KEYS_FIRST,
// ITERION_PLATFORM_FACADE_DEFAULT): they are the DEFAULT of the platform
// level — what its unset fields resolve to — not a level of their own. The
// record (the admin API's runtime value) outranks them; the built-in
// defaults rank last.
const (
	EnvPairOrder        = "ITERION_PLATFORM_PAIR_ORDER"
	EnvTriggers         = "ITERION_PLATFORM_TRIGGERS"
	EnvRefusedPinnedKey = "ITERION_PLATFORM_REFUSED_PINNED_KEY"
	EnvStrict           = "ITERION_PLATFORM_ROUTING_STRICT"
)

// FromEnv builds the platform level's default. Unset or unparseable values
// are left empty so the built-in defaults apply — a typo in an env var
// must not silently change what runs spend. A deployment that SET a value
// the package cannot read gets told at boot: platformcfg.ValidateEnv calls
// ValidateEnv.
func FromEnv() Policy {
	refused := strings.TrimSpace(os.Getenv(EnvRefusedPinnedKey))
	if refused != "" && Validate(Policy{RefusedPinnedKey: refused}) != nil {
		refused = ""
	}
	return Policy{
		PairOrder:        envPairOrder(),
		Triggers:         envTriggers(),
		RefusedPinnedKey: refused,
		Strict:           envStrict(),
	}
}

// ValidateEnv reports an env default the policy cannot read — a deployment
// that set one meant something, and reading it as the built-in default in
// silence would decide the opposite of what the operator wrote.
func ValidateEnv() error {
	if v := strings.TrimSpace(os.Getenv(EnvPairOrder)); v != "" {
		if _, err := parseEnvPairOrder(v); err != nil {
			return fmt.Errorf("platformcfg: %s=%q: %w", EnvPairOrder, v, err)
		}
	}
	if v := strings.TrimSpace(os.Getenv(EnvTriggers)); v != "" {
		if _, err := parseEnvTriggers(v); err != nil {
			return fmt.Errorf("platformcfg: %s=%q: %w", EnvTriggers, v, err)
		}
	}
	if v := strings.TrimSpace(os.Getenv(EnvRefusedPinnedKey)); v != "" {
		if err := Validate(Policy{RefusedPinnedKey: v}); err != nil {
			return fmt.Errorf("platformcfg: %s=%q: %w", EnvRefusedPinnedKey, v, err)
		}
	}
	if v := strings.TrimSpace(os.Getenv(EnvStrict)); v != "" {
		if _, err := strconv.ParseBool(v); err != nil {
			return fmt.Errorf("platformcfg: %s=%q is not a boolean", EnvStrict, v)
		}
	}
	return nil
}

// envPairOrder reads a comma-separated pair list. An unparseable entry
// leaves the WHOLE field empty — a half-read order would route the run
// somewhere the operator did not write.
func envPairOrder() []string {
	if v := strings.TrimSpace(os.Getenv(EnvPairOrder)); v != "" {
		pairs, err := parseEnvPairOrder(v)
		if err != nil {
			return nil
		}
		return pairs
	}
	return nil
}

func parseEnvPairOrder(v string) ([]string, error) {
	var pairs []string
	for _, item := range strings.Split(v, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		pairs = append(pairs, item)
	}
	// Validate the WHOLE list, not each item: the duplicate refusals are
	// part of the contract, and the env dial must not admit what the API
	// refuses on the same level.
	if err := Validate(Policy{PairOrder: pairs}); err != nil {
		return nil, err
	}
	return pairs, nil
}

// envTriggers reads a comma-separated trigger subset.
func envTriggers() []string {
	if v := strings.TrimSpace(os.Getenv(EnvTriggers)); v != "" {
		triggers, err := parseEnvTriggers(v)
		if err != nil {
			return nil
		}
		return triggers
	}
	return nil
}

func parseEnvTriggers(v string) ([]string, error) {
	var triggers []string
	for _, item := range strings.Split(v, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		triggers = append(triggers, item)
	}
	if err := Validate(Policy{Triggers: triggers}); err != nil {
		return nil, err
	}
	return triggers, nil
}

func envStrict() *bool {
	v := strings.TrimSpace(os.Getenv(EnvStrict))
	if v == "" {
		return nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return nil
	}
	return &b
}
