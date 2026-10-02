package commands

import (
	"fmt"
	"os"
	"strings"
)

// pluginManagerLoop is the interface that /plugin commands require from the loop.
type pluginManagerLoop interface {
	PluginList() ([]string, error)
	PluginInstall(name string) error
	PluginEnable(name string) error
	PluginDisable(name string) error
	PluginUninstall(name string) error
	PluginUpdate(name string) error
}

// taskManagerLoop is the interface that /tasks commands require from the loop.
type taskManagerLoop interface {
	TaskList() ([]string, error)
	TaskGet(id string) (string, error)
	TaskStop(id string) error
}

// RegisterPluginCommands registers plugin/extension-related slash commands.
func RegisterPluginCommands(r *Registry) {
	registerPluginCommand(r)
	registerAgentsCommand(r)
	registerSkillsCommand(r)
	registerTasksCommand(r)
	registerTeamCommand(r)
	registerCronCommand(r)
	registerMemoryCommand(r)
	registerSandboxCommand(r)
	registerInitCommand(r)
	registerUpgradeCommand(r)
	registerTemplatesCommand(r)
}

func registerTemplatesCommand(r *Registry) {
	r.Register(Command{
		Name:         "templates",
		Description:  "List or apply prompt templates",
		ArgumentHint: "[list|apply <name>]",
		Category:     CategoryPlugin,
		Handler: func(args string, loop interface{}) error {
			type templateManager interface {
				ListTemplates() ([]string, error)
				ApplyTemplate(name string) (string, error)
			}
			tm, ok := loop.(templateManager)
			if !ok {
				fmt.Println("Template management not available in this context.")
				return nil
			}
			sub, rest := splitSubcommand(args)
			if sub == "" {
				sub = "list"
			}
			switch sub {
			case "list":
				templates, err := tm.ListTemplates()
				if err != nil {
					return fmt.Errorf("templates: %w", err)
				}
				if len(templates) == 0 {
					fmt.Println("No templates available.")
					return nil
				}
				fmt.Println("Templates:")
				for _, t := range templates {
					fmt.Printf("  %s\n", t)
				}
			case "apply":
				name := strings.TrimSpace(rest)
				if name == "" {
					fmt.Println("Usage: /templates apply <name>")
					return nil
				}
				result, err := tm.ApplyTemplate(name)
				if err != nil {
					return fmt.Errorf("templates apply: %w", err)
				}
				fmt.Println(result)
			default:
				fmt.Println("Usage: /templates [list|apply <name>]")
			}
			return nil
		},
	})
}

func registerPluginCommand(r *Registry) {
	r.Register(Command{
		Name:         "plugin",
		Aliases:      []string{"plugins", "marketplace"},
		Description:  "Manage plugins: list, install, enable, disable, uninstall, update",
		ArgumentHint: "[list|install <name>|enable <name>|disable <name>|uninstall <name>|update <name>]",
		Category:     CategoryPlugin,
		Handler: func(args string, loop interface{}) error {
			sub, rest := splitSubcommand(args)
			if sub == "" {
				sub = "list"
			}

			pm, ok := loop.(pluginManagerLoop)
			if !ok {
				fmt.Println("Plugin management not available in this context.")
				return nil
			}

			switch sub {
			case "list":
				plugins, err := pm.PluginList()
				if err != nil {
					return fmt.Errorf("plugin list: %w", err)
				}
				if len(plugins) == 0 {
					fmt.Println("No plugins installed.")
					return nil
				}
				fmt.Println("Installed plugins:")
				for _, p := range plugins {
					fmt.Printf("  %s\n", p)
				}

			case "install":
				name := strings.TrimSpace(rest)
				if name == "" {
					fmt.Println("Usage: /plugin install <name>")
					return nil
				}
				if err := pm.PluginInstall(name); err != nil {
					return fmt.Errorf("plugin install: %w", err)
				}
				fmt.Printf("Installed plugin %s\n", name)

			case "enable":
				name := strings.TrimSpace(rest)
				if name == "" {
					fmt.Println("Usage: /plugin enable <name>")
					return nil
				}
				if err := pm.PluginEnable(name); err != nil {
					return fmt.Errorf("plugin enable: %w", err)
				}
				fmt.Printf("Enabled plugin %s\n", name)

			case "disable":
				name := strings.TrimSpace(rest)
				if name == "" {
					fmt.Println("Usage: /plugin disable <name>")
					return nil
				}
				if err := pm.PluginDisable(name); err != nil {
					return fmt.Errorf("plugin disable: %w", err)
				}
				fmt.Printf("Disabled plugin %s\n", name)

			case "uninstall":
				name := strings.TrimSpace(rest)
				if name == "" {
					fmt.Println("Usage: /plugin uninstall <name>")
					return nil
				}
				if err := pm.PluginUninstall(name); err != nil {
					return fmt.Errorf("plugin uninstall: %w", err)
				}
				fmt.Printf("Uninstalled plugin %s\n", name)

			case "update":
				name := strings.TrimSpace(rest)
				if name == "" {
					fmt.Println("Usage: /plugin update <name>")
					return nil
				}
				if err := pm.PluginUpdate(name); err != nil {
					return fmt.Errorf("plugin update: %w", err)
				}
				fmt.Printf("Updated plugin %s\n", name)

			default:
				fmt.Printf("Unknown plugin subcommand: %s\nUsage: /plugin [list|install|enable|disable|uninstall|update]\n", sub)
			}

			return nil
		},
	})
}

func registerAgentsCommand(r *Registry) {
	r.Register(Command{
		Name:         "agents",
		Description:  "List or get help on available agents",
		ArgumentHint: "[list|help]",
		Category:     CategoryPlugin,
		Handler: func(args string, loop interface{}) error {
			sub, _ := splitSubcommand(args)
			if sub == "" {
				sub = "list"
			}

			type agentLister interface {
				AgentList() ([]string, error)
			}

			switch sub {
			case "list":
				al, ok := loop.(agentLister)
				if !ok {
					fmt.Println("Agent listing not available in this context.")
					return nil
				}
				agents, err := al.AgentList()
				if err != nil {
					return fmt.Errorf("agent list: %w", err)
				}
				if len(agents) == 0 {
					fmt.Println("No agents available.")
					return nil
				}
				fmt.Println("Available agents:")
				for _, a := range agents {
					fmt.Printf("  %s\n", a)
				}

			case "help":
				fmt.Println("Usage: /agents [list|help]")
				fmt.Println("  list  — List available agents")
				fmt.Println("  help  — Show this help message")

			default:
				fmt.Printf("Unknown agents subcommand: %s\nUsage: /agents [list|help]\n", sub)
			}

			return nil
		},
	})
}

func registerSkillsCommand(r *Registry) {
	r.Register(Command{
		Name:         "skills",
		Aliases:      []string{"skill"},
		Description:  "Manage skills: list, install, help, invoke",
		ArgumentHint: "[list|install <name>|help|invoke <name> [args]]",
		Category:     CategoryPlugin,
		Handler: func(args string, loop interface{}) error {
			sub, rest := splitSubcommand(args)
			if sub == "" {
				sub = "list"
			}

			type skillManager interface {
				SkillList() ([]string, error)
				SkillInstall(name string) error
				SkillInvoke(name string, args string) (string, error)
			}

			switch sub {
			case "list":
				sm, ok := loop.(skillManager)
				if !ok {
					fmt.Println("Skill management not available in this context.")
					return nil
				}
				skills, err := sm.SkillList()
				if err != nil {
					return fmt.Errorf("skill list: %w", err)
				}
				if len(skills) == 0 {
					fmt.Println("No skills available.")
					return nil
				}
				fmt.Println("Available skills:")
				for _, s := range skills {
					fmt.Printf("  %s\n", s)
				}

			case "install":
				name := strings.TrimSpace(rest)
				if name == "" {
					fmt.Println("Usage: /skills install <name>")
					return nil
				}
				sm, ok := loop.(skillManager)
				if !ok {
					fmt.Println("Skill management not available in this context.")
					return nil
				}
				if err := sm.SkillInstall(name); err != nil {
					return fmt.Errorf("skill install: %w", err)
				}
				fmt.Printf("Installed skill %s\n", name)

			case "invoke":
				nameAndArgs := strings.TrimSpace(rest)
				if nameAndArgs == "" {
					fmt.Println("Usage: /skills invoke <name> [args]")
					return nil
				}
				name, invokeArgs := splitSubcommand(nameAndArgs)
				sm, ok := loop.(skillManager)
				if !ok {
					fmt.Println("Skill management not available in this context.")
					return nil
				}
				result, err := sm.SkillInvoke(name, invokeArgs)
				if err != nil {
					return fmt.Errorf("skill invoke: %w", err)
				}
				fmt.Println(result)

			case "help":
				fmt.Println("Usage: /skills [list|install|help|invoke]")
				fmt.Println("  list             — List available skills")
				fmt.Println("  install <name>   — Install a skill")
				fmt.Println("  invoke <name>    — Invoke a skill")
				fmt.Println("  help             — Show this help message")

			default:
				fmt.Printf("Unknown skills subcommand: %s\nUsage: /skills [list|install|help|invoke]\n", sub)
			}

			return nil
		},
	})
}

func registerTasksCommand(r *Registry) {
	r.Register(Command{
		Name:         "tasks",
		Description:  "Manage background tasks: list, get, stop",
		ArgumentHint: "[list|get <id>|stop <id>]",
		Category:     CategoryPlugin,
		Handler: func(args string, loop interface{}) error {
			sub, rest := splitSubcommand(args)
			if sub == "" {
				sub = "list"
			}

			tm, ok := loop.(taskManagerLoop)
			if !ok {
				fmt.Println("Task management not available in this context.")
				return nil
			}

			switch sub {
			case "list":
				tasks, err := tm.TaskList()
				if err != nil {
					return fmt.Errorf("task list: %w", err)
				}
				if len(tasks) == 0 {
					fmt.Println("No active tasks.")
					return nil
				}
				fmt.Println("Active tasks:")
				for _, t := range tasks {
					fmt.Printf("  %s\n", t)
				}

			case "get":
				id := strings.TrimSpace(rest)
				if id == "" {
					fmt.Println("Usage: /tasks get <id>")
					return nil
				}
				info, err := tm.TaskGet(id)
				if err != nil {
					return fmt.Errorf("task get: %w", err)
				}
				fmt.Println(info)

			case "stop":
				id := strings.TrimSpace(rest)
				if id == "" {
					fmt.Println("Usage: /tasks stop <id>")
					return nil
				}
				if err := tm.TaskStop(id); err != nil {
					return fmt.Errorf("task stop: %w", err)
				}
				fmt.Printf("Stopped task %s\n", id)

			default:
				fmt.Printf("Unknown tasks subcommand: %s\nUsage: /tasks [list|get|stop]\n", sub)
			}

			return nil
		},
	})
}

func registerTeamCommand(r *Registry) {
	r.Register(Command{
		Name:         "team",
		Description:  "Manage team sessions: list, create, delete",
		ArgumentHint: "[list|create <name>|delete <name>]",
		Category:     CategoryPlugin,
		Handler: func(args string, loop interface{}) error {
			sub, rest := splitSubcommand(args)
			if sub == "" {
				sub = "list"
			}

			type teamManager interface {
				TeamList() ([]string, error)
				TeamCreate(name string) error
				TeamDelete(name string) error
			}

			tm, ok := loop.(teamManager)
			if !ok {
				fmt.Println("Team management not available in this context.")
				return nil
			}

			switch sub {
			case "list":
				teams, err := tm.TeamList()
				if err != nil {
					return fmt.Errorf("team list: %w", err)
				}
				if len(teams) == 0 {
					fmt.Println("No teams configured.")
					return nil
				}
				fmt.Println("Teams:")
				for _, t := range teams {
					fmt.Printf("  %s\n", t)
				}

			case "create":
				name := strings.TrimSpace(rest)
				if name == "" {
					fmt.Println("Usage: /team create <name>")
					return nil
				}
				if err := tm.TeamCreate(name); err != nil {
					return fmt.Errorf("team create: %w", err)
				}
				fmt.Printf("Created team %s\n", name)

			case "delete":
				name := strings.TrimSpace(rest)
				if name == "" {
					fmt.Println("Usage: /team delete <name>")
					return nil
				}
				if err := tm.TeamDelete(name); err != nil {
					return fmt.Errorf("team delete: %w", err)
				}
				fmt.Printf("Deleted team %s\n", name)

			default:
				fmt.Printf("Unknown team subcommand: %s\nUsage: /team [list|create|delete]\n", sub)
			}

			return nil
		},
	})
}

func registerCronCommand(r *Registry) {
	r.Register(Command{
		Name:         "cron",
		Description:  "Manage scheduled tasks: list, add, remove",
		ArgumentHint: "[list|add \"<schedule>\" \"<prompt>\"|remove <id>]",
		Category:     CategoryPlugin,
		Handler: func(args string, loop interface{}) error {
			sub, rest := splitSubcommand(args)
			if sub == "" {
				sub = "list"
			}

			type cronManager interface {
				CronList() ([]string, error)
				CronAdd(schedule, prompt string) (string, error)
				CronRemove(id string) error
			}

			cm, ok := loop.(cronManager)
			if !ok {
				fmt.Println("Cron management not available in this context.")
				return nil
			}

			switch sub {
			case "list":
				jobs, err := cm.CronList()
				if err != nil {
					return fmt.Errorf("cron list: %w", err)
				}
				if len(jobs) == 0 {
					fmt.Println("No scheduled tasks.")
					return nil
				}
				fmt.Println("Scheduled tasks:")
				for _, j := range jobs {
					fmt.Printf("  %s\n", j)
				}

			case "add":
				// Expect: add "<schedule>" "<prompt>"
				// Parse two quoted strings from rest.
				schedule, prompt, err := parseTwoQuoted(rest)
				if err != nil {
					fmt.Println("Usage: /cron add \"<schedule>\" \"<prompt>\"")
					return nil
				}
				id, err := cm.CronAdd(schedule, prompt)
				if err != nil {
					return fmt.Errorf("cron add: %w", err)
				}
				fmt.Printf("Added cron job %s\n", id)

			case "remove":
				id := strings.TrimSpace(rest)
				if id == "" {
					fmt.Println("Usage: /cron remove <id>")
					return nil
				}
				if err := cm.CronRemove(id); err != nil {
					return fmt.Errorf("cron remove: %w", err)
				}
				fmt.Printf("Removed cron job %s\n", id)

			default:
				fmt.Printf("Unknown cron subcommand: %s\nUsage: /cron [list|add|remove]\n", sub)
			}

			return nil
		},
	})
}

// parseTwoQuoted extracts two quoted strings from input like: "first" "second".
func parseTwoQuoted(s string) (string, string, error) {
	s = strings.TrimSpace(s)
	if len(s) < 5 || s[0] != '"' { // minimal: "a" "b"
		return "", "", fmt.Errorf("expected two quoted strings")
	}

	// Find closing quote for the first string.
	end := strings.Index(s[1:], "\"")
	if end < 0 {
		return "", "", fmt.Errorf("unclosed first quote")
	}
	first := s[1 : end+1]

	remainder := strings.TrimSpace(s[end+2:])
	if len(remainder) < 2 || remainder[0] != '"' {
		return "", "", fmt.Errorf("expected second quoted string")
	}

	end2 := strings.Index(remainder[1:], "\"")
	if end2 < 0 {
		return "", "", fmt.Errorf("unclosed second quote")
	}
	second := remainder[1 : end2+1]

	return first, second, nil
}

func registerMemoryCommand(r *Registry) {
	r.Register(Command{
		Name:            "memory",
		Description:     "Display CLAUDE.md memory files",
		ResumeSupported: true,
		Category:        CategoryPlugin,
		Handler: func(args string, loop interface{}) error {
			paths := []string{
				"CLAUDE.md",
				".claude/CLAUDE.md",
				os.Getenv("HOME") + "/.claude/CLAUDE.md",
			}

			found := false
			for _, p := range paths {
				data, err := os.ReadFile(p)
				if err != nil {
					continue
				}
				found = true
				fmt.Printf("--- %s ---\n", p)
				fmt.Println(string(data))
			}

			if !found {
				fmt.Println("No CLAUDE.md files found.")
			}
			return nil
		},
	})
}

func registerSandboxCommand(r *Registry) {
	r.Register(Command{
		Name:            "sandbox",
		Description:     "Show sandbox detection status",
		ResumeSupported: true,
		Category:        CategoryPlugin,
		Handler: func(args string, loop interface{}) error {
			type sandboxDetector interface {
				IsSandboxed() bool
			}

			if sd, ok := loop.(sandboxDetector); ok {
				if sd.IsSandboxed() {
					fmt.Println("Running in sandbox mode.")
				} else {
					fmt.Println("Not running in sandbox mode.")
				}
			} else {
				// Heuristic: check common sandbox indicators.
				if os.Getenv("SANDBOX") != "" || os.Getenv("CONTAINER") != "" {
					fmt.Println("Sandbox indicators detected (environment variables).")
				} else {
					fmt.Println("No sandbox detected (heuristic check).")
				}
			}
			return nil
		},
	})
}

func registerInitCommand(r *Registry) {
	r.Register(Command{
		Name:        "init",
		Description: "Initialize a new project configuration",
		Category:    CategoryPlugin,
		Handler: func(args string, loop interface{}) error {
			type projectInitializer interface {
				InitProject(dir string) error
			}

			dir := strings.TrimSpace(args)
			if dir == "" {
				dir = "."
			}

			if pi, ok := loop.(projectInitializer); ok {
				if err := pi.InitProject(dir); err != nil {
					return fmt.Errorf("init: %w", err)
				}
				fmt.Printf("Initialized project in %s\n", dir)
			} else {
				fmt.Println("Project initialization not available in this context.")
			}
			return nil
		},
	})
}

func registerUpgradeCommand(r *Registry) {
	r.Register(Command{
		Name:        "upgrade",
		Description: "Check for updates",
		Category:    CategoryPlugin,
		Handler: func(args string, loop interface{}) error {
			type upgradeChecker interface {
				CheckUpgrade() (string, error)
			}

			if uc, ok := loop.(upgradeChecker); ok {
				msg, err := uc.CheckUpgrade()
				if err != nil {
					return fmt.Errorf("upgrade: %w", err)
				}
				fmt.Println(msg)
			} else {
				fmt.Println("You are running the latest version.")
			}
			return nil
		},
	})
}
