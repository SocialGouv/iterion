package commands

import (
	"fmt"
	"strings"
)

// RegisterInteractionCommands registers interaction and execution control slash commands.
func RegisterInteractionCommands(r *Registry) {
	r.Register(Command{
		Name:        "approve",
		Aliases:     []string{"yes", "y"},
		Description: "Approve a pending tool execution",
		Category:    CategoryInteraction,
		Handler: func(args string, loop interface{}) error {
			type approver interface {
				ApprovePending() error
			}
			if a, ok := loop.(approver); ok {
				if err := a.ApprovePending(); err != nil {
					return fmt.Errorf("approve: %w", err)
				}
				fmt.Println("Approved.")
			} else {
				fmt.Println("Approval not available in this context.")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:        "deny",
		Aliases:     []string{"no", "n"},
		Description: "Deny a pending tool execution",
		Category:    CategoryInteraction,
		Handler: func(args string, loop interface{}) error {
			type denier interface {
				DenyPending() error
			}
			if d, ok := loop.(denier); ok {
				if err := d.DenyPending(); err != nil {
					return fmt.Errorf("deny: %w", err)
				}
				fmt.Println("Denied.")
			} else {
				fmt.Println("Denial not available in this context.")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:        "undo",
		Description: "Undo the last file write or edit",
		Category:    CategoryInteraction,
		Handler: func(args string, loop interface{}) error {
			type undoer interface {
				UndoLast() (string, error)
			}
			if u, ok := loop.(undoer); ok {
				msg, err := u.UndoLast()
				if err != nil {
					return fmt.Errorf("undo: %w", err)
				}
				fmt.Println(msg)
			} else {
				fmt.Println("Undo not available in this context.")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:        "stop",
		Description: "Stop the current generation",
		Category:    CategoryInteraction,
		Handler: func(args string, loop interface{}) error {
			type stopper interface {
				StopGeneration() error
			}
			if s, ok := loop.(stopper); ok {
				if err := s.StopGeneration(); err != nil {
					return fmt.Errorf("stop: %w", err)
				}
				fmt.Println("Generation stopped.")
			} else {
				fmt.Println("Stop not available in this context.")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:        "retry",
		Description: "Retry the last failed message",
		Category:    CategoryInteraction,
		Handler: func(args string, loop interface{}) error {
			type retrier interface {
				RetryLast() error
			}
			if rt, ok := loop.(retrier); ok {
				if err := rt.RetryLast(); err != nil {
					return fmt.Errorf("retry: %w", err)
				}
				fmt.Println("Retrying last message...")
			} else {
				fmt.Println("Retry not available in this context.")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:        "paste",
		Description: "Paste clipboard content as input",
		Category:    CategoryInteraction,
		Handler: func(args string, loop interface{}) error {
			type paster interface {
				PasteFromClipboard() (string, error)
			}
			if p, ok := loop.(paster); ok {
				content, err := p.PasteFromClipboard()
				if err != nil {
					return fmt.Errorf("paste: %w", err)
				}
				if content == "" {
					fmt.Println("Clipboard is empty.")
				} else {
					fmt.Printf("Pasted %d characters from clipboard.\n", len(content))
				}
			} else {
				fmt.Println("Paste not available in this context.")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:        "screenshot",
		Description: "Take a screenshot and add to conversation",
		Category:    CategoryInteraction,
		Handler: func(args string, loop interface{}) error {
			type screenshotter interface {
				TakeScreenshot() (string, error)
			}
			if ss, ok := loop.(screenshotter); ok {
				path, err := ss.TakeScreenshot()
				if err != nil {
					return fmt.Errorf("screenshot: %w", err)
				}
				fmt.Printf("Screenshot saved: %s\n", path)
			} else {
				fmt.Println("Screenshot not available in this context.")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:         "image",
		Description:  "Add an image file to the conversation",
		ArgumentHint: "<path>",
		Category:     CategoryInteraction,
		Handler: func(args string, loop interface{}) error {
			path := strings.TrimSpace(args)
			if path == "" {
				fmt.Println("Usage: /image <path>")
				return nil
			}
			type imageAdder interface {
				AddImage(path string) error
			}
			if ia, ok := loop.(imageAdder); ok {
				if err := ia.AddImage(path); err != nil {
					return fmt.Errorf("image: %w", err)
				}
				fmt.Printf("Added image: %s\n", path)
			} else {
				fmt.Println("Image adding not available in this context.")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:         "multi",
		Description:  "Execute multiple slash commands in sequence",
		ArgumentHint: "<commands>",
		Category:     CategoryInteraction,
		Handler: func(args string, loop interface{}) error {
			commands := strings.TrimSpace(args)
			if commands == "" {
				fmt.Println("Usage: /multi <commands>")
				return nil
			}
			type multiExecutor interface {
				ExecuteMulti(commands string) error
			}
			if me, ok := loop.(multiExecutor); ok {
				if err := me.ExecuteMulti(commands); err != nil {
					return fmt.Errorf("multi: %w", err)
				}
			} else {
				fmt.Println("Multi-command execution not available in this context.")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:         "macro",
		Description:  "Record or replay command macros",
		ArgumentHint: "[record|stop|play <name>]",
		Category:     CategoryInteraction,
		Handler: func(args string, loop interface{}) error {
			type macroManager interface {
				MacroRecord(name string) error
				MacroStop() error
				MacroPlay(name string) error
			}
			mm, ok := loop.(macroManager)
			if !ok {
				fmt.Println("Macro management not available in this context.")
				return nil
			}
			parts := strings.Fields(args)
			sub := "help"
			if len(parts) > 0 {
				sub = strings.ToLower(parts[0])
			}
			switch sub {
			case "record":
				name := ""
				if len(parts) > 1 {
					name = parts[1]
				}
				if err := mm.MacroRecord(name); err != nil {
					return fmt.Errorf("macro record: %w", err)
				}
				fmt.Println("Recording macro...")
			case "stop":
				if err := mm.MacroStop(); err != nil {
					return fmt.Errorf("macro stop: %w", err)
				}
				fmt.Println("Macro recording stopped.")
			case "play":
				if len(parts) < 2 {
					fmt.Println("Usage: /macro play <name>")
					return nil
				}
				if err := mm.MacroPlay(parts[1]); err != nil {
					return fmt.Errorf("macro play: %w", err)
				}
			default:
				fmt.Println("Usage: /macro [record|stop|play <name>]")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:            "alias",
		Description:     "Create a command alias",
		ArgumentHint:    "<name> <command>",
		ResumeSupported: true,
		Category:        CategoryInteraction,
		Handler: func(args string, loop interface{}) error {
			parts := strings.SplitN(strings.TrimSpace(args), " ", 2)
			if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
				fmt.Println("Usage: /alias <name> <command>")
				return nil
			}
			type aliasManager interface {
				CreateAlias(name, command string) error
			}
			if am, ok := loop.(aliasManager); ok {
				if err := am.CreateAlias(parts[0], parts[1]); err != nil {
					return fmt.Errorf("alias: %w", err)
				}
				fmt.Printf("Alias created: /%s -> %s\n", parts[0], parts[1])
			} else {
				fmt.Println("Alias creation not available in this context.")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:         "parallel",
		Description:  "Run commands in parallel subagents",
		ArgumentHint: "<count> <prompt>",
		Category:     CategoryInteraction,
		Handler: func(args string, loop interface{}) error {
			parts := strings.SplitN(strings.TrimSpace(args), " ", 2)
			if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
				fmt.Println("Usage: /parallel <count> <prompt>")
				return nil
			}
			type parallelRunner interface {
				RunParallel(count string, prompt string) error
			}
			if pr, ok := loop.(parallelRunner); ok {
				if err := pr.RunParallel(parts[0], parts[1]); err != nil {
					return fmt.Errorf("parallel: %w", err)
				}
			} else {
				fmt.Println("Parallel execution not available in this context.")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:            "agent",
		Description:     "Manage sub-agents and spawned sessions",
		ArgumentHint:    "[list|spawn|kill]",
		ResumeSupported: true,
		Category:        CategoryInteraction,
		Handler: func(args string, loop interface{}) error {
			type agentController interface {
				AgentList() ([]string, error)
				AgentSpawn(prompt string) (string, error)
				AgentKill(id string) error
			}
			ac, ok := loop.(agentController)
			if !ok {
				fmt.Println("Agent management not available in this context.")
				return nil
			}
			parts := strings.Fields(args)
			sub := "list"
			if len(parts) > 0 {
				sub = strings.ToLower(parts[0])
			}
			switch sub {
			case "list", "":
				agents, err := ac.AgentList()
				if err != nil {
					return fmt.Errorf("agent list: %w", err)
				}
				if len(agents) == 0 {
					fmt.Println("No active agents.")
					return nil
				}
				fmt.Println("Active agents:")
				for _, a := range agents {
					fmt.Printf("  %s\n", a)
				}
			case "spawn":
				prompt := ""
				if len(parts) > 1 {
					prompt = strings.Join(parts[1:], " ")
				}
				id, err := ac.AgentSpawn(prompt)
				if err != nil {
					return fmt.Errorf("agent spawn: %w", err)
				}
				fmt.Printf("Spawned agent: %s\n", id)
			case "kill":
				if len(parts) < 2 {
					fmt.Println("Usage: /agent kill <id>")
					return nil
				}
				if err := ac.AgentKill(parts[1]); err != nil {
					return fmt.Errorf("agent kill: %w", err)
				}
				fmt.Printf("Killed agent %s\n", parts[1])
			default:
				fmt.Println("Usage: /agent [list|spawn|kill]")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:            "subagent",
		Description:     "Control active subagent execution",
		ArgumentHint:    "[list|steer <target> <msg>|kill <id>]",
		ResumeSupported: true,
		Category:        CategoryInteraction,
		Handler: func(args string, loop interface{}) error {
			type subagentController interface {
				SubagentList() ([]string, error)
				SubagentSteer(target, msg string) error
				SubagentKill(id string) error
			}
			sc, ok := loop.(subagentController)
			if !ok {
				fmt.Println("Subagent control not available in this context.")
				return nil
			}
			parts := strings.Fields(args)
			sub := "list"
			if len(parts) > 0 {
				sub = strings.ToLower(parts[0])
			}
			switch sub {
			case "list", "":
				agents, err := sc.SubagentList()
				if err != nil {
					return fmt.Errorf("subagent list: %w", err)
				}
				if len(agents) == 0 {
					fmt.Println("No active subagents.")
					return nil
				}
				fmt.Println("Active subagents:")
				for _, a := range agents {
					fmt.Printf("  %s\n", a)
				}
			case "steer":
				if len(parts) < 3 {
					fmt.Println("Usage: /subagent steer <target> <msg>")
					return nil
				}
				target := parts[1]
				msg := strings.Join(parts[2:], " ")
				if err := sc.SubagentSteer(target, msg); err != nil {
					return fmt.Errorf("subagent steer: %w", err)
				}
				fmt.Printf("Steered subagent %s\n", target)
			case "kill":
				if len(parts) < 2 {
					fmt.Println("Usage: /subagent kill <id>")
					return nil
				}
				if err := sc.SubagentKill(parts[1]); err != nil {
					return fmt.Errorf("subagent kill: %w", err)
				}
				fmt.Printf("Killed subagent %s\n", parts[1])
			default:
				fmt.Println("Usage: /subagent [list|steer <target> <msg>|kill <id>]")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:         "teleport",
		Description:  "Jump to a file or symbol",
		ArgumentHint: "<symbol-or-path>",
		Category:     CategoryInteraction,
		Handler: func(args string, loop interface{}) error {
			target := strings.TrimSpace(args)
			if target == "" {
				fmt.Println("Usage: /teleport <symbol-or-path>")
				return nil
			}
			type teleporter interface {
				Teleport(target string) (string, error)
			}
			if t, ok := loop.(teleporter); ok {
				location, err := t.Teleport(target)
				if err != nil {
					return fmt.Errorf("teleport: %w", err)
				}
				fmt.Println(location)
			} else {
				fmt.Println("Teleport not available in this context.")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:        "debug-tool-call",
		Description: "Replay the last tool call with debug details",
		Category:    CategoryInteraction,
		Handler: func(args string, loop interface{}) error {
			type toolDebugger interface {
				DebugLastToolCall() (string, error)
			}
			if td, ok := loop.(toolDebugger); ok {
				details, err := td.DebugLastToolCall()
				if err != nil {
					return fmt.Errorf("debug-tool-call: %w", err)
				}
				fmt.Println(details)
			} else {
				fmt.Println("Tool call debugging not available in this context.")
			}
			return nil
		},
	})
}
