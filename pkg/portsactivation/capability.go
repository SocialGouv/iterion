package portsactivation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/internal/appinfo"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
)

// CapabilityDigest binds an activation to the binary's interpreter and
// persistence/wire capabilities. A changed build or scope requires a fresh
// proof; an old record never silently activates a newly deployed binary.
func CapabilityDigest(scope string) string {
	body, _ := json.Marshal(struct {
		Scope            string `json:"scope"`
		Version          string `json:"version"`
		Commit           string `json:"commit"`
		Modified         bool   `json:"modified"`
		Semantics        string `json:"semantics"`
		RunFormat        int    `json:"run_format"`
		ActivationFormat int    `json:"activation_format"`
		QueueVersion     int    `json:"queue_version"`
		NativePrefix     string `json:"native_prefix"`
		Namespace        string `json:"namespace"`
	}{scope, appinfo.Version, appinfo.Commit, appinfo.Modified,
		ir.RuntimeSemanticsPortsV1, store.NativeRunFormatVersion, store.PortActivationVersion,
		queue.SchemaVersion, store.NativeRunIDPrefix, store.NativeRunsDirectory})
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
