package runner

import (
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runtime"
)

// contributionsFromWire converts the queue's contribution payload into the
// engine's domain type (the same wire-mirror split as queue.BudgetOverrides →
// ir.BudgetOverrides).
//
// The payload is what the launching instance read from ITS iterion home:
// enabled-plugin markdown plus the skill-library skills the workflow
// references. This pod has neither on disk, so this is the only way an
// operator-installed plugin's skill reaches the workspace here.
func contributionsFromWire(c *queue.Contributions) *runtime.Contributions {
	if c == nil {
		return nil
	}
	out := &runtime.Contributions{}
	for _, f := range c.Plugin {
		out.Plugin = append(out.Plugin, runtime.ContributionFile{
			Kind:    f.Kind,
			Name:    f.Name,
			Content: f.Content,
		})
	}
	for _, s := range c.Library {
		out.Library = append(out.Library, runtime.LibrarySkillFile{
			Name:        s.Name,
			Description: s.Description,
			Content:     s.Content,
		})
	}
	return out
}
