package natsconfig

import (
	"fmt"
	"strings"

	queue "github.com/SocialGouv/iterion/pkg/queue/nats"
)

// QueueTopology is the observed queue identity to protect. The authority
// must compare this structure with the live broker/Store before using its
// ACL result. The initial static profile excludes cross-account imports,
// exports and JetStream domains. The distinct system account is classified
// as a privileged authority rather than mistaken for an ordinary queue user.
type QueueTopology struct {
	Account       string `json:"account"`
	SystemAccount string `json:"system_account"`
	Stream        string `json:"stream"`
	Consumer      string `json:"consumer"`
	DLQStream     string `json:"dlq_stream"`
	RunSubject    string `json:"run_subject"`
	DLQSubject    string `json:"dlq_subject"`
	LockBucket    string `json:"lock_bucket"`
	RolloutBucket string `json:"rollout_bucket"`
}

func (q QueueTopology) Validate() error {
	for _, name := range []string{q.Account, q.SystemAccount, q.Stream, q.Consumer, q.DLQStream, q.LockBucket, q.RolloutBucket} {
		if !profileAccountName(name) {
			return fmt.Errorf("NATS queue protection needs literal account, stream, consumer and bucket names")
		}
	}
	if q.Account == q.SystemAccount || q.Stream == q.DLQStream || q.LockBucket == q.RolloutBucket {
		return fmt.Errorf("NATS queue protection needs separate queue/system accounts, streams and KV identities")
	}
	for _, subject := range []string{q.RunSubject, q.DLQSubject} {
		if strings.ContainsAny(subject, "*>") {
			return fmt.Errorf("NATS queue protection requires literal queue subjects")
		}
		if _, err := parseSubjectPattern(subject); err != nil {
			return err
		}
	}
	if q.RunSubject == q.DLQSubject {
		return fmt.Errorf("NATS queue protection needs distinct run and DLQ subjects")
	}
	return nil
}

// AccessExposure is a concrete broker subject witness within a protected
// surface and one effective principal permission language. An exposure is
// not proof of a connected holder; the later trusted custody inventory has
// to account for every holder, including disconnected and dormant ones.
type AccessExposure struct {
	Surface   string `json:"surface"`
	Direction string `json:"direction"`
	Witness   string `json:"witness"`
}

// AnalyzeProtectedAccess conservatively classifies every static principal in
// the queue account. It covers all $JS.API variants, including ones not in
// the pinned client's known request templates. Unrecognized variants get a
// separate exposure, so an authority must refuse them rather than treating
// them as harmless. NATS account isolation is valid only because the profile
// loader has already excluded imports and exports.
func AnalyzeProtectedAccess(principal Principal, topology QueueTopology) ([]AccessExposure, error) {
	if err := topology.Validate(); err != nil {
		return nil, err
	}
	if principal.Account == topology.SystemAccount {
		if principal.Identity == "" {
			return nil, fmt.Errorf("NATS system authority requires a named principal")
		}
		witness, found, err := principal.Publish.Intersects([]string{"$SYS.REQ.>"})
		if err != nil {
			return nil, err
		}
		if found {
			return []AccessExposure{{"system_authority", "publish", witness}}, nil
		}
		return nil, nil
	}
	if principal.Account != topology.Account {
		return nil, nil
	}
	if principal.Identity == "" {
		return nil, fmt.Errorf("NATS queue protection requires a named principal")
	}
	type surface struct {
		name      string
		direction string
		patterns  []string
	}
	control := []string{
		fmt.Sprintf(queue.SubjectCancelFmt, "*"),
		fmt.Sprintf(queue.SubjectHeartFmt, "*"),
		fmt.Sprintf(queue.SubjectSteerFmt, ">"),
	}
	kv := []string{"$KV." + topology.LockBucket + ".>", "$KV." + topology.RolloutBucket + ".>"}
	surfaces := []surface{
		{"queue_messages", "publish", []string{topology.RunSubject, topology.DLQSubject}},
		{"queue_messages", "subscribe", []string{topology.RunSubject, topology.DLQSubject}},
		{"native_control", "publish", control},
		{"native_control", "subscribe", control},
		{"jetstream_api", "publish", []string{"$JS.API.>"}},
		{"jetstream_request_visibility", "subscribe", []string{"$JS.API.>"}},
		{"jetstream_reply_injection", "publish", []string{"_INBOX.>"}},
		{"jetstream_reply", "subscribe", []string{"_INBOX.>"}},
		{"acknowledgment", "publish", []string{"$JS.ACK.>", "$ACK.>"}},
		{"queue_kv", "publish", kv},
		{"queue_kv", "subscribe", kv},
	}
	var exposures []AccessExposure
	apiGranted := false
	for _, candidate := range surfaces {
		permissions := principal.Publish
		if candidate.direction == "subscribe" {
			permissions = principal.Subscribe
		}
		witness, found, err := permissions.Intersects(candidate.patterns)
		if err != nil {
			return nil, err
		}
		if found {
			exposures = append(exposures, AccessExposure{candidate.name, candidate.direction, witness})
			apiGranted = apiGranted || candidate.name == "jetstream_api" && candidate.direction == "publish"
		}
	}
	if !apiGranted {
		return exposures, nil
	}
	known := knownJetStreamAPIs(topology)
	if len(known) > 128 {
		return nil, fmt.Errorf("NATS protected API catalog exceeds supported size")
	}
	witness, found, err := principal.Publish.intersects([]string{"$JS.API.>"}, known)
	if err != nil {
		return nil, err
	}
	if found {
		exposures = append(exposures, AccessExposure{"unreviewed_jetstream_api", "publish", witness})
	}
	return exposures, nil
}

// These are the request families the pinned 2.14.5 client uses for the
// observed run, DLQ and KV backing streams. The all-API check above protects
// any additional broker API; this list only identifies the supported subset.
func knownJetStreamAPIs(q QueueTopology) []string {
	known := []string{"$JS.API.INFO", "$JS.API.STREAM.LIST", "$JS.API.STREAM.NAMES"}
	streams := []string{q.Stream, q.DLQStream, "KV_" + q.LockBucket, "KV_" + q.RolloutBucket}
	for _, stream := range streams {
		for _, suffix := range []string{
			"STREAM.CREATE.", "STREAM.INFO.", "STREAM.UPDATE.", "STREAM.DELETE.", "STREAM.PURGE.",
			"STREAM.MSG.GET.", "STREAM.MSG.DELETE.", "DIRECT.GET.",
		} {
			known = append(known, "$JS.API."+suffix+stream)
		}
		known = append(known, "$JS.API.DIRECT.GET."+stream+".>")
		for _, suffix := range []string{
			"CONSUMER.CREATE.", "CONSUMER.DURABLE.CREATE.", "CONSUMER.INFO.",
			"CONSUMER.MSG.NEXT.", "CONSUMER.DELETE.", "CONSUMER.PAUSE.",
			"CONSUMER.RESET.", "CONSUMER.UNPIN.",
		} {
			known = append(known, "$JS.API."+suffix+stream+".*")
		}
		known = append(known, "$JS.API.CONSUMER.CREATE."+stream+".*.>",
			"$JS.API.CONSUMER.LIST."+stream, "$JS.API.CONSUMER.NAMES."+stream)
	}
	return known
}
