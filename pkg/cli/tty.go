package cli

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/store"
)

// IsTTY reports whether stdin is connected to a terminal.
func IsTTY() bool {
	return isTerminal(int(os.Stdin.Fd()))
}

// PromptHumanAnswers displays the interaction questions and prompts the user
// for answers interactively via stdin. Returns the answers as a map.
func PromptHumanAnswers(interaction *store.Interaction) (map[string]interface{}, error) {
	reader := bufio.NewReader(os.Stdin)
	answers := make(map[string]interface{})

	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  Human input required:")
	fmt.Fprintln(os.Stderr, "  ─────────────────────")

	// Sort keys for deterministic display order.
	keys := make([]string, 0, len(interaction.Questions))
	for k := range interaction.Questions {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	if len(keys) > 0 {
		fmt.Fprintln(os.Stderr, "  Context:")
		for _, k := range keys {
			fmt.Fprintf(os.Stderr, "    %s: %v\n", k, interaction.Questions[k])
		}
		fmt.Fprintln(os.Stderr)
	}

	// Prompt for each question key.
	for _, k := range keys {
		fmt.Fprintf(os.Stderr, "  %s: ", k)
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("failed to read answer for %q: %w", k, err)
		}
		answers[k] = strings.TrimRight(line, "\n\r")
	}

	// If there are no question keys, ask for free-form input.
	if len(interaction.Questions) == 0 {
		fmt.Fprintf(os.Stderr, "  answer: ")
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("failed to read answer: %w", err)
		}
		answers["answer"] = strings.TrimRight(line, "\n\r")
	}

	fmt.Fprintln(os.Stderr)
	return answers, nil
}
