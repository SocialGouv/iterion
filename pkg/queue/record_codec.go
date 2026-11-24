package queue

import (
	"reflect"
	"sync"

	"github.com/SocialGouv/iterion/pkg/recordcodec"
)

var exactRunMessageCodec = sync.OnceValues(func() (*recordcodec.Plan, error) {
	return recordcodec.Compile(reflect.TypeFor[RunMessage](), "Vars", "Resume/Answers", "Resume/HostInputs")
})

// ExactRunMessageCodec adapts the three queue-owned payloads. It is inactive:
// publication/consumption must separately agree on an immutable format version.
func ExactRunMessageCodec() (recordcodec.Codec, error) { return exactRunMessageCodec() }
