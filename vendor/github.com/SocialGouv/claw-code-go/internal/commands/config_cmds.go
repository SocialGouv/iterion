package commands

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ConfigSwitcher provides model and permission switching for config commands.
type ConfigSwitcher interface {
	CurrentModel() string
	SetModel(model string) error
	CurrentPermissionMode() string
	SetPermissionMode(mode string) error
}

// RegisterConfigCommands registers config-related slash commands.
func RegisterConfigCommands(r *Registry) {
	r.Register(Command{
		Name:            "config",
		Description:     "Inspect Claude config files or merged sections",
		ArgumentHint:    "[env|hooks|model|plugins]",
		ResumeSupported: true,
		Category:        CategoryConfig,
		Handler: func(args string, loop interface{}) error {
			sub := strings.TrimSpace(strings.ToLower(args))

			switch sub {
			case "env":
				fmt.Println("Environment variables:")
				for _, env := range relevantEnvVars() {
					val := os.Getenv(env)
					if val == "" {
						val = "(not set)"
					}
					fmt.Printf("  %s = %s\n", env, val)
				}

			case "model":
				cs, ok := loop.(ConfigSwitcher)
				if !ok {
					fmt.Println("Config not available in this context.")
					return nil
				}
				fmt.Printf("Current model: %s\n", cs.CurrentModel())

			case "hooks":
				fmt.Println("Hooks configuration:")
				fmt.Println("  Use /hooks list to inspect lifecycle hooks.")

			case "plugins":
				fmt.Println("Plugin configuration:")
				fmt.Println("  Use /plugin list to inspect installed plugins.")

			case "", "help":
				fmt.Println("Usage: /config [env|hooks|model|plugins]")
				fmt.Println("  env      — Show relevant environment variables")
				fmt.Println("  hooks    — Show hooks configuration")
				fmt.Println("  model    — Show current model")
				fmt.Println("  plugins  — Show plugin configuration")

			default:
				fmt.Printf("Unknown config subcommand: %s\n", sub)
				fmt.Println("Usage: /config [env|hooks|model|plugins]")
			}

			return nil
		},
	})

	r.Register(Command{
		Name:         "model",
		Description:  "Show or switch the active model",
		ArgumentHint: "[model]",
		Category:     CategoryConfig,
		Handler: func(args string, loop interface{}) error {
			cs, ok := loop.(ConfigSwitcher)
			if !ok {
				fmt.Println("Model switching not available in this context.")
				return nil
			}
			if args == "" {
				fmt.Printf("Current model: %s\n", cs.CurrentModel())
				return nil
			}
			if err := cs.SetModel(strings.TrimSpace(args)); err != nil {
				return fmt.Errorf("set model: %w", err)
			}
			fmt.Printf("Model set to %s\n", args)
			return nil
		},
	})

	r.Register(Command{
		Name:         "permissions",
		Description:  "Show or switch the active permission mode",
		ArgumentHint: "[read-only|workspace-write|danger-full-access]",
		Category:     CategoryConfig,
		Handler: func(args string, loop interface{}) error {
			cs, ok := loop.(ConfigSwitcher)
			if !ok {
				fmt.Println("Permission mode switching not available in this context.")
				return nil
			}
			if args == "" {
				fmt.Printf("Current permission mode: %s\n", cs.CurrentPermissionMode())
				return nil
			}
			if err := cs.SetPermissionMode(strings.TrimSpace(args)); err != nil {
				return fmt.Errorf("set permission mode: %w", err)
			}
			fmt.Printf("Permission mode set to %s\n", args)
			return nil
		},
	})

	r.Register(Command{
		Name:            "plan",
		Description:     "Toggle or inspect planning mode",
		ArgumentHint:    "[on|off]",
		ResumeSupported: true,
		Category:        CategoryConfig,
		Handler: func(args string, loop interface{}) error {
			type planToggler interface {
				PlanMode() bool
				SetPlanMode(on bool)
			}
			pt, ok := loop.(planToggler)
			if !ok {
				fmt.Println("Plan mode not available in this context.")
				return nil
			}
			switch strings.TrimSpace(strings.ToLower(args)) {
			case "on":
				pt.SetPlanMode(true)
				fmt.Println("Plan mode enabled.")
			case "off":
				pt.SetPlanMode(false)
				fmt.Println("Plan mode disabled.")
			case "":
				if pt.PlanMode() {
					fmt.Println("Plan mode: on")
				} else {
					fmt.Println("Plan mode: off")
				}
			default:
				fmt.Println("Usage: /plan [on|off]")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:            "compact",
		Description:     "Compact local session history",
		ResumeSupported: true,
		Category:        CategoryConfig,
		Handler: func(args string, loop interface{}) error {
			type compactor interface {
				CompactSession() error
			}
			c, ok := loop.(compactor)
			if !ok {
				fmt.Println("Compact not available in this context.")
				return nil
			}
			if err := c.CompactSession(); err != nil {
				return fmt.Errorf("compact: %w", err)
			}
			fmt.Println("Session compacted.")
			return nil
		},
	})

	r.Register(Command{
		Name:         "temperature",
		Aliases:      []string{"temp"},
		Description:  "Set or show sampling temperature",
		ArgumentHint: "[value]",
		Category:     CategoryConfig,
		Handler: func(args string, loop interface{}) error {
			type tempController interface {
				GetTemperature() float64
				SetTemperature(t float64) error
			}
			tc, ok := loop.(tempController)
			if !ok {
				fmt.Println("Temperature control not available in this context.")
				return nil
			}
			val := strings.TrimSpace(args)
			if val == "" {
				fmt.Printf("Temperature: %.2f\n", tc.GetTemperature())
				return nil
			}
			t, err := strconv.ParseFloat(val, 64)
			if err != nil {
				return fmt.Errorf("invalid temperature value: %s", val)
			}
			if t < 0 || t > 2 {
				return fmt.Errorf("temperature must be between 0 and 2")
			}
			if err := tc.SetTemperature(t); err != nil {
				return err
			}
			fmt.Printf("Temperature set to: %.2f\n", t)
			return nil
		},
	})

	r.Register(Command{
		Name:            "max-tokens",
		Description:     "Show or set the max output tokens",
		ArgumentHint:    "[count]",
		ResumeSupported: true,
		Category:        CategoryConfig,
		Handler: func(args string, loop interface{}) error {
			type tokenController interface {
				GetMaxTokens() int
				SetMaxTokens(n int) error
			}
			tc, ok := loop.(tokenController)
			if !ok {
				fmt.Println("Token limit control not available in this context.")
				return nil
			}
			val := strings.TrimSpace(args)
			if val == "" {
				fmt.Printf("Max tokens: %d\n", tc.GetMaxTokens())
				return nil
			}
			n, err := strconv.Atoi(val)
			if err != nil || n <= 0 {
				return fmt.Errorf("invalid token count: %s", val)
			}
			if err := tc.SetMaxTokens(n); err != nil {
				return err
			}
			fmt.Printf("Max tokens set to: %d\n", n)
			return nil
		},
	})

	r.Register(Command{
		Name:            "system-prompt",
		Aliases:         []string{"sysprompt"},
		Description:     "Show or edit the system prompt",
		ArgumentHint:    "[show|set <text>]",
		ResumeSupported: true,
		Category:        CategoryConfig,
		Handler: func(args string, loop interface{}) error {
			type sysPromptManager interface {
				GetSystemPrompt() string
				SetSystemPrompt(prompt string) error
			}
			sp, ok := loop.(sysPromptManager)
			if !ok {
				fmt.Println("System prompt management not available in this context.")
				return nil
			}
			parts := strings.SplitN(strings.TrimSpace(args), " ", 2)
			sub := "show"
			if len(parts) > 0 && parts[0] != "" {
				sub = strings.ToLower(parts[0])
			}
			switch sub {
			case "show", "":
				prompt := sp.GetSystemPrompt()
				if prompt == "" {
					fmt.Println("No custom system prompt set.")
				} else {
					fmt.Printf("System prompt:\n%s\n", prompt)
				}
			case "set":
				if len(parts) < 2 {
					fmt.Println("Usage: /system-prompt set <text>")
					return nil
				}
				if err := sp.SetSystemPrompt(parts[1]); err != nil {
					return err
				}
				fmt.Println("System prompt updated.")
			default:
				fmt.Println("Usage: /system-prompt [show|set <text>]")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:         "profile",
		Description:  "Switch configuration profile",
		ArgumentHint: "[profile-name]",
		Category:     CategoryConfig,
		Handler: func(args string, loop interface{}) error {
			type profileManager interface {
				CurrentProfile() string
				SwitchProfile(name string) error
				ListProfiles() []string
			}
			pm, ok := loop.(profileManager)
			if !ok {
				fmt.Println("Profile management not available in this context.")
				return nil
			}
			name := strings.TrimSpace(args)
			if name == "" {
				fmt.Printf("Current profile: %s\n", pm.CurrentProfile())
				profiles := pm.ListProfiles()
				if len(profiles) > 0 {
					fmt.Printf("Available: %s\n", strings.Join(profiles, ", "))
				}
				return nil
			}
			if err := pm.SwitchProfile(name); err != nil {
				return err
			}
			fmt.Printf("Switched to profile: %s\n", name)
			return nil
		},
	})

	r.Register(Command{
		Name:         "language",
		Aliases:      []string{"lang"},
		Description:  "Set or show output language",
		ArgumentHint: "[language]",
		Category:     CategoryConfig,
		Handler: func(args string, loop interface{}) error {
			type languageManager interface {
				GetLanguage() string
				SetLanguage(lang string) error
			}
			lm, ok := loop.(languageManager)
			if !ok {
				fmt.Println("Language setting not available in this context.")
				return nil
			}
			lang := strings.TrimSpace(args)
			if lang == "" {
				fmt.Printf("Language: %s\n", lm.GetLanguage())
				return nil
			}
			if err := lm.SetLanguage(lang); err != nil {
				return err
			}
			fmt.Printf("Language set to: %s\n", lang)
			return nil
		},
	})

	r.Register(Command{
		Name:        "ultraplan",
		Description: "Enter ultra plan mode (detailed planning with reasoning)",
		Category:    CategoryConfig,
		Handler: func(args string, loop interface{}) error {
			type planController interface {
				SetPlanMode(on bool)
				SetReasoningEffort(level string) error
			}
			pc, ok := loop.(planController)
			if !ok {
				fmt.Println("Ultra plan mode not available in this context.")
				return nil
			}
			pc.SetPlanMode(true)
			if err := pc.SetReasoningEffort("high"); err != nil {
				return err
			}
			fmt.Println("Ultra plan mode enabled. Maximum reasoning effort with detailed planning.")
			return nil
		},
	})
	r.Register(Command{
		Name:            "allowed-tools",
		Description:     "Show or modify the allowed tools list",
		ArgumentHint:    "[add|remove|list] [tool]",
		ResumeSupported: true,
		Category:        CategoryConfig,
		Handler: func(args string, loop interface{}) error {
			type toolsManager interface {
				ListAllowedTools() ([]string, error)
				AddAllowedTool(name string) error
				RemoveAllowedTool(name string) error
			}
			tm, ok := loop.(toolsManager)
			if !ok {
				fmt.Println("Allowed tools management not available in this context.")
				return nil
			}
			parts := strings.Fields(args)
			sub := "list"
			if len(parts) > 0 {
				sub = strings.ToLower(parts[0])
			}
			switch sub {
			case "list", "":
				tools, err := tm.ListAllowedTools()
				if err != nil {
					return fmt.Errorf("allowed-tools: %w", err)
				}
				if len(tools) == 0 {
					fmt.Println("No tools in allowed list.")
					return nil
				}
				fmt.Println("Allowed tools:")
				for _, t := range tools {
					fmt.Printf("  %s\n", t)
				}
			case "add":
				if len(parts) < 2 {
					fmt.Println("Usage: /allowed-tools add <tool>")
					return nil
				}
				if err := tm.AddAllowedTool(parts[1]); err != nil {
					return fmt.Errorf("allowed-tools add: %w", err)
				}
				fmt.Printf("Added tool: %s\n", parts[1])
			case "remove":
				if len(parts) < 2 {
					fmt.Println("Usage: /allowed-tools remove <tool>")
					return nil
				}
				if err := tm.RemoveAllowedTool(parts[1]); err != nil {
					return fmt.Errorf("allowed-tools remove: %w", err)
				}
				fmt.Printf("Removed tool: %s\n", parts[1])
			default:
				fmt.Println("Usage: /allowed-tools [add|remove|list] [tool]")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:         "api-key",
		Description:  "Show or set the Anthropic API key",
		ArgumentHint: "[key]",
		Category:     CategoryConfig,
		Handler: func(args string, loop interface{}) error {
			type apiKeyManager interface {
				GetAPIKey() string
				SetAPIKey(key string) error
			}
			ak, ok := loop.(apiKeyManager)
			if !ok {
				fmt.Println("API key management not available in this context.")
				return nil
			}
			key := strings.TrimSpace(args)
			if key == "" {
				current := ak.GetAPIKey()
				if current == "" {
					fmt.Println("No API key set.")
				} else {
					masked := current
					if len(masked) > 8 {
						masked = masked[:4] + "..." + masked[len(masked)-4:]
					} else {
						masked = "****"
					}
					fmt.Printf("API key: %s\n", masked)
				}
				return nil
			}
			if err := ak.SetAPIKey(key); err != nil {
				return fmt.Errorf("api-key: %w", err)
			}
			fmt.Println("API key updated.")
			return nil
		},
	})

	r.Register(Command{
		Name:            "telemetry",
		Description:     "Show or configure telemetry settings",
		ArgumentHint:    "[on|off|status]",
		ResumeSupported: true,
		Category:        CategoryConfig,
		Handler: func(args string, loop interface{}) error {
			tl, ok := loop.(toggleLoop)
			if !ok {
				fmt.Println("Telemetry settings not available in this context.")
				return nil
			}
			arg := strings.TrimSpace(strings.ToLower(args))
			switch arg {
			case "on":
				tl.SetToggle("telemetry", true)
				fmt.Println("Telemetry enabled.")
			case "off":
				tl.SetToggle("telemetry", false)
				fmt.Println("Telemetry disabled.")
			case "status", "":
				if tl.GetToggle("telemetry") {
					fmt.Println("Telemetry: on")
				} else {
					fmt.Println("Telemetry: off")
				}
			default:
				fmt.Println("Usage: /telemetry [on|off|status]")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:            "reasoning",
		Description:     "Toggle extended reasoning mode",
		ArgumentHint:    "[on|off|stream]",
		ResumeSupported: true,
		Category:        CategoryConfig,
		Handler: func(args string, loop interface{}) error {
			type reasoningController interface {
				SetReasoningMode(mode string) error
				GetReasoningMode() string
			}
			rc, ok := loop.(reasoningController)
			if !ok {
				fmt.Println("Reasoning mode not available in this context.")
				return nil
			}
			mode := strings.TrimSpace(strings.ToLower(args))
			if mode == "" {
				fmt.Printf("Reasoning mode: %s\n", rc.GetReasoningMode())
				return nil
			}
			switch mode {
			case "on", "off", "stream":
				// valid
			default:
				return fmt.Errorf("reasoning: unknown mode %q (use on, off, or stream)", mode)
			}
			if err := rc.SetReasoningMode(mode); err != nil {
				return fmt.Errorf("reasoning: %w", err)
			}
			fmt.Printf("Reasoning mode set to %s\n", mode)
			return nil
		},
	})

	r.Register(Command{
		Name:            "budget",
		Description:     "Show or set token budget limits",
		ArgumentHint:    "[show|set <limit>]",
		ResumeSupported: true,
		Category:        CategoryConfig,
		Handler: func(args string, loop interface{}) error {
			type budgetController interface {
				GetBudget() (string, error)
				SetBudget(limit string) error
			}
			bc, ok := loop.(budgetController)
			if !ok {
				fmt.Println("Budget control not available in this context.")
				return nil
			}
			parts := strings.Fields(args)
			sub := "show"
			if len(parts) > 0 {
				sub = strings.ToLower(parts[0])
			}
			switch sub {
			case "show", "":
				info, err := bc.GetBudget()
				if err != nil {
					return fmt.Errorf("budget: %w", err)
				}
				fmt.Println(info)
			case "set":
				if len(parts) < 2 {
					fmt.Println("Usage: /budget set <limit>")
					return nil
				}
				if err := bc.SetBudget(parts[1]); err != nil {
					return fmt.Errorf("budget set: %w", err)
				}
				fmt.Printf("Budget set to %s\n", parts[1])
			default:
				fmt.Println("Usage: /budget [show|set <limit>]")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:            "rate-limit",
		Description:     "Configure API rate limiting",
		ArgumentHint:    "[status|set <rpm>]",
		ResumeSupported: true,
		Category:        CategoryConfig,
		Handler: func(args string, loop interface{}) error {
			type rateLimitController interface {
				GetRateLimit() (string, error)
				SetRateLimit(rpm string) error
			}
			rl, ok := loop.(rateLimitController)
			if !ok {
				fmt.Println("Rate limit control not available in this context.")
				return nil
			}
			parts := strings.Fields(args)
			sub := "status"
			if len(parts) > 0 {
				sub = strings.ToLower(parts[0])
			}
			switch sub {
			case "status", "":
				info, err := rl.GetRateLimit()
				if err != nil {
					return fmt.Errorf("rate-limit: %w", err)
				}
				fmt.Println(info)
			case "set":
				if len(parts) < 2 {
					fmt.Println("Usage: /rate-limit set <rpm>")
					return nil
				}
				if err := rl.SetRateLimit(parts[1]); err != nil {
					return fmt.Errorf("rate-limit set: %w", err)
				}
				fmt.Printf("Rate limit set to %s RPM\n", parts[1])
			default:
				fmt.Println("Usage: /rate-limit [status|set <rpm>]")
			}
			return nil
		},
	})
}

func relevantEnvVars() []string {
	return []string{
		"ANTHROPIC_API_KEY",
		"CLAUDE_CODE_USE_BEDROCK",
		"CLAUDE_CODE_USE_VERTEX",
		"CLAUDE_CODE_USE_FOUNDRY",
		"ANTHROPIC_BASE_URL",
		"HTTP_PROXY",
		"HTTPS_PROXY",
		"NO_PROXY",
	}
}
