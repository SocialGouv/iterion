package main

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

	iterconfig "github.com/SocialGouv/iterion/pkg/config"
	"github.com/SocialGouv/iterion/pkg/portsactivation"
	queuecore "github.com/SocialGouv/iterion/pkg/queue"
	natsq "github.com/SocialGouv/iterion/pkg/queue/nats"
	"github.com/SocialGouv/iterion/pkg/store"
)

// runnerPortCapabilityFromEnv builds the public, credential-free census claim
// for one runner process. It is opt-in so existing deployments keep their
// current startup behavior; a distributed authority configured with
// RequireCensus will refuse activation until every queue holder is wired.
func runnerPortCapabilityFromEnv(cfg iterconfig.Config, conn *natsq.Conn, storeIdentity string,
	runnerEpoch uint64) (natsq.PortInstanceCapability, bool, error) {
	if conn == nil || storeIdentity == "" {
		return natsq.PortInstanceCapability{}, false, fmt.Errorf("runner capability census is missing its queue or store identity")
	}
	enabled, set, err := optionalBoolEnv("ITERION_PORT_CAPABILITY_ENABLED")
	if err != nil {
		return natsq.PortInstanceCapability{}, false, err
	}
	if !set || !enabled {
		return natsq.PortInstanceCapability{}, false, nil
	}
	principal := strings.TrimSpace(os.Getenv("ITERION_PORT_CAPABILITY_PRINCIPAL"))
	if principal == "" {
		parsed, parseErr := url.Parse(cfg.NATS.URL)
		if parseErr == nil && parsed.User != nil {
			principal = parsed.User.Username()
		}
	}
	instance := strings.TrimSpace(os.Getenv("ITERION_PORT_CAPABILITY_INSTANCE"))
	if instance == "" {
		instance, _ = os.Hostname()
	}
	account := strings.TrimSpace(os.Getenv("ITERION_PORT_CAPABILITY_ACCOUNT"))
	build := strings.TrimSpace(os.Getenv("ITERION_PORT_CAPABILITY_BUILD_DIGEST"))
	authorityEpochText := strings.TrimSpace(os.Getenv("ITERION_PORT_CAPABILITY_AUTHORITY_EPOCH"))
	authorityEpoch, parseErr := strconv.ParseUint(authorityEpochText, 10, 64)
	if parseErr != nil || authorityEpoch == 0 {
		return natsq.PortInstanceCapability{}, false, fmt.Errorf("runner capability census requires a positive authority epoch")
	}
	claim := natsq.PortInstanceCapability{Version: natsq.PortCapabilityVersion,
		Principal: principal, Instance: instance, BuildDigest: build,
		CapabilityDigest: portsactivation.CapabilityDigest(store.PortActivationDistributed), StoreIdentity: storeIdentity,
		Account: account, Stream: cfg.NATS.Stream, Consumer: natsq.ConsumerRunners,
		QueueVersion: queuecore.SchemaVersion, RunnerEpoch: runnerEpoch, AuthorityEpoch: authorityEpoch}
	if err := claim.Validate(); err != nil {
		return natsq.PortInstanceCapability{}, false, fmt.Errorf("runner capability census configuration is invalid")
	}
	return claim, true, nil
}

func optionalBoolEnv(key string) (value, set bool, err error) {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return false, false, nil
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true, true, nil
	case "0", "false", "no", "off":
		return false, true, nil
	default:
		return false, true, fmt.Errorf("%s must be a boolean", key)
	}
}
