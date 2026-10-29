package runtime

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// attachmentNamesIn lists the attachments a run's attachments directory
// holds — one subdirectory per attachment name, the file beneath it — or
// nothing when the directory is absent, empty or unreadable. It answers one
// question for the sandbox wiring: does this run carry attachments a driver
// has to deliver? A directory that cannot be read answers "no" here and
// fails where it is read for real.
func attachmentNamesIn(dir string) []string {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// attachmentsUndeliverable is the typed refusal of a run that holds
// attachments on a driver that can neither bind-mount their directory into
// the container nor copy it in. It carries the names so the record says
// what was given and not delivered.
func attachmentsUndeliverable(driver string, names []string) *RuntimeError {
	return &RuntimeError{
		Code:    ErrCodeAttachmentsUndeliverable,
		Message: fmt.Sprintf("the run's attachments (%s) cannot reach the %s sandbox: the driver has no host filesystem to bind their directory from and does not copy it in, so no node could open them", strings.Join(names, ", "), driver),
		Hint:    "use a sandbox driver that hands files to the container (one with a host filesystem, or one that copies directories in), or launch the run without attachments",
	}
}
