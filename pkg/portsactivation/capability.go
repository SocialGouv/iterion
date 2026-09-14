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

// CapabilityDigest binds a NEW launch to the exact binary that was probed.
// A changed build or scope requires a fresh proof; an old record never
// silently activates a newly deployed binary.
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

// ResumeCompatibilityVersion must advance whenever a native interpreter
// change can no longer safely read and continue a previously admitted run.
// The persisted execution identity is separately checked on every resume.
const ResumeCompatibilityVersion = 1

// ResumeCompatibilityDigest intentionally excludes build identity, the
// activation-record format and queue schema. Those govern new admission and
// deployment, while an already admitted run must survive a compatible
// upgrade, proof expiry or rollback. It still binds the runtime format and
// isolated namespace and must be bumped on incompatible semantic changes.
func ResumeCompatibilityDigest(scope string) string {
	body, _ := json.Marshal(struct {
		Scope        string `json:"scope"`
		Version      int    `json:"resume_compatibility_version"`
		Semantics    string `json:"semantics"`
		RunFormat    int    `json:"run_format"`
		NativePrefix string `json:"native_prefix"`
		Namespace    string `json:"namespace"`
	}{scope, ResumeCompatibilityVersion, ir.RuntimeSemanticsPortsV1,
		store.NativeRunFormatVersion, store.NativeRunIDPrefix, store.NativeRunsDirectory})
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
