package cli

import (
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// The two CLI doors that never reach the bundle loader refuse an author
// document by name, before any effect: the remote upload before the file is
// read, the schedule when it is written — not at its first tick.
func TestAnAuthorDocumentNeverUploadsNorSchedules(t *testing.T) {
	if _, err := prepareUnit("does-not-exist.bot.yaml"); !errors.Is(err, bundle.ErrAuthorDocument) {
		t.Fatalf("prepareUnit: %v, want ErrAuthorDocument before the file is even read", err)
	}
	err := validateScheduleEntry(ScheduleEntry{Name: "nightly", Bot: "bots/x/main.bot.yaml", Cron: "0 3 * * *"})
	if !errors.Is(err, bundle.ErrAuthorDocument) {
		t.Fatalf("validateScheduleEntry: %v, want ErrAuthorDocument", err)
	}
}
