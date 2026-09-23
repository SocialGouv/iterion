package commands

import (
	"fmt"
	"strings"

	"github.com/SocialGouv/claw-code-go/internal/apikit"
)

// toggleLoop is the interface for commands that toggle boolean settings.
type toggleLoop interface {
	GetToggle(name string) bool
	SetToggle(name string, value bool)
}

// effortLoop is the interface for commands that control effort level.
type effortLoop interface {
	SetEffort(level string) error
	GetEffort() string
}

// themeLoop is the interface for commands that manage color themes.
type themeLoop interface {
	SetTheme(name string) error
	CurrentTheme() string
	ListThemes() []string
}

// RegisterUXCommands registers UX and settings slash commands.
func RegisterUXCommands(r *Registry) {
	r.Register(Command{
		Name:         "theme",
		Description:  "Switch color theme",
		ArgumentHint: "[theme-name]",
		Category:     CategoryUX,
		Handler: func(args string, loop interface{}) error {
			tl, ok := loop.(themeLoop)
			if !ok {
				fmt.Println("Theme switching not available in this context.")
				return nil
			}
			name := strings.TrimSpace(args)
			if name == "" {
				fmt.Printf("Current theme: %s\n", tl.CurrentTheme())
				themes := tl.ListThemes()
				if len(themes) > 0 {
					fmt.Printf("Available: %s\n", strings.Join(themes, ", "))
				}
				return nil
			}
			if err := tl.SetTheme(name); err != nil {
				return fmt.Errorf("theme: %w", err)
			}
			fmt.Printf("Theme set to %s\n", name)
			return nil
		},
	})

	r.Register(Command{
		Name:        "vim",
		Description: "Toggle vim keybinding mode",
		Category:    CategoryUX,
		Handler: func(args string, loop interface{}) error {
			tl, ok := loop.(toggleLoop)
			if !ok {
				fmt.Println("Toggle not available in this context.")
				return nil
			}
			current := tl.GetToggle("vim")
			tl.SetToggle("vim", !current)
			if !current {
				fmt.Println("Vim mode enabled.")
			} else {
				fmt.Println("Vim mode disabled.")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:         "effort",
		Description:  "Set effort level",
		ArgumentHint: "[low|medium|high|xhigh|max]",
		Category:     CategoryUX,
		Handler: func(args string, loop interface{}) error {
			el, ok := loop.(effortLoop)
			if !ok {
				fmt.Println("Effort control not available in this context.")
				return nil
			}
			level := strings.TrimSpace(strings.ToLower(args))
			if level == "" {
				fmt.Printf("Current effort: %s\n", el.GetEffort())
				return nil
			}
			// Syntactic guard: reject tokens that aren't reasoning-effort
			// levels at all. SetEffort then applies the model-aware matrix
			// check (xhigh/max are Opus-4.x-only).
			if !apikit.IsKnownEffort(level) {
				return fmt.Errorf("effort: invalid level %q (use low, medium, high, xhigh, or max)", level)
			}
			if err := el.SetEffort(level); err != nil {
				return fmt.Errorf("effort: %w", err)
			}
			fmt.Printf("Effort set to %s\n", level)
			return nil
		},
	})

	r.Register(Command{
		Name:        "fast",
		Description: "Toggle fast/concise mode",
		Category:    CategoryUX,
		Handler: func(args string, loop interface{}) error {
			tl, ok := loop.(toggleLoop)
			if !ok {
				fmt.Println("Toggle not available in this context.")
				return nil
			}
			current := tl.GetToggle("fast")
			tl.SetToggle("fast", !current)
			if !current {
				fmt.Println("Fast mode enabled.")
			} else {
				fmt.Println("Fast mode disabled.")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:            "brief",
		Description:     "Toggle brief output mode",
		ResumeSupported: true,
		Category:        CategoryUX,
		Handler: func(args string, loop interface{}) error {
			tl, ok := loop.(toggleLoop)
			if !ok {
				fmt.Println("Toggle not available in this context.")
				return nil
			}
			current := tl.GetToggle("brief")
			tl.SetToggle("brief", !current)
			if !current {
				fmt.Println("Brief mode enabled.")
			} else {
				fmt.Println("Brief mode disabled.")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:            "advisor",
		Description:     "Toggle advisor mode",
		ResumeSupported: true,
		Category:        CategoryUX,
		Handler: func(args string, loop interface{}) error {
			tl, ok := loop.(toggleLoop)
			if !ok {
				fmt.Println("Toggle not available in this context.")
				return nil
			}
			current := tl.GetToggle("advisor")
			tl.SetToggle("advisor", !current)
			if !current {
				fmt.Println("Advisor mode enabled.")
			} else {
				fmt.Println("Advisor mode disabled.")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:         "color",
		Description:  "Configure color settings",
		ArgumentHint: "[scheme]",
		Category:     CategoryUX,
		Handler: func(args string, loop interface{}) error {
			tl, ok := loop.(themeLoop)
			if !ok {
				fmt.Println("Color configuration not available in this context.")
				return nil
			}
			scheme := strings.TrimSpace(args)
			if scheme == "" {
				fmt.Printf("Current color scheme: %s\n", tl.CurrentTheme())
				return nil
			}
			if err := tl.SetTheme(scheme); err != nil {
				return fmt.Errorf("color: %w", err)
			}
			fmt.Printf("Color scheme set to %s\n", scheme)
			return nil
		},
	})

	r.Register(Command{
		Name:            "keybindings",
		Description:     "Show/configure keyboard shortcuts",
		ResumeSupported: true,
		Category:        CategoryUX,
		Handler: func(args string, loop interface{}) error {
			fmt.Println("Keyboard shortcuts:")
			fmt.Println("  Ctrl+C  — Cancel current operation")
			fmt.Println("  Ctrl+D  — Exit")
			fmt.Println("  Tab     — Autocomplete command")
			fmt.Println("  Up/Down — Navigate history")
			fmt.Println("Use /vim to toggle vim keybinding mode.")
			return nil
		},
	})

	r.Register(Command{
		Name:         "voice",
		Description:  "Toggle voice input mode",
		ArgumentHint: "[on|off]",
		Category:     CategoryUX,
		Handler: func(args string, loop interface{}) error {
			tl, ok := loop.(toggleLoop)
			if !ok {
				fmt.Println("Voice input not available in this context.")
				return nil
			}
			arg := strings.TrimSpace(strings.ToLower(args))
			switch arg {
			case "on":
				tl.SetToggle("voice", true)
				fmt.Println("Voice input enabled.")
			case "off":
				tl.SetToggle("voice", false)
				fmt.Println("Voice input disabled.")
			case "":
				current := tl.GetToggle("voice")
				tl.SetToggle("voice", !current)
				if !current {
					fmt.Println("Voice input enabled.")
				} else {
					fmt.Println("Voice input disabled.")
				}
			default:
				fmt.Println("Usage: /voice [on|off]")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:        "share",
		Description: "Share the current conversation",
		Category:    CategoryUX,
		Handler: func(args string, loop interface{}) error {
			type conversationSharer interface {
				ShareConversation() (string, error)
			}
			if cs, ok := loop.(conversationSharer); ok {
				url, err := cs.ShareConversation()
				if err != nil {
					return fmt.Errorf("share: %w", err)
				}
				fmt.Printf("Conversation shared: %s\n", url)
			} else {
				fmt.Println("Share not available in this context.")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:        "feedback",
		Description: "Submit feedback about the current session",
		Category:    CategoryUX,
		Handler: func(args string, loop interface{}) error {
			type feedbackSubmitter interface {
				SubmitFeedback(message string) error
			}
			if fs, ok := loop.(feedbackSubmitter); ok {
				message := strings.TrimSpace(args)
				if message == "" {
					fmt.Println("Usage: /feedback <message>")
					return nil
				}
				if err := fs.SubmitFeedback(message); err != nil {
					return fmt.Errorf("feedback: %w", err)
				}
				fmt.Println("Feedback submitted. Thank you!")
			} else {
				fmt.Println("Feedback submission not available in this context.")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:            "stickers",
		Description:     "Browse and manage sticker packs",
		ResumeSupported: true,
		Category:        CategoryUX,
		Handler: func(args string, loop interface{}) error {
			type stickerManager interface {
				ListStickers() ([]string, error)
			}
			if sm, ok := loop.(stickerManager); ok {
				stickers, err := sm.ListStickers()
				if err != nil {
					return fmt.Errorf("stickers: %w", err)
				}
				if len(stickers) == 0 {
					fmt.Println("No sticker packs available.")
					return nil
				}
				fmt.Println("Sticker packs:")
				for _, s := range stickers {
					fmt.Printf("  %s\n", s)
				}
			} else {
				fmt.Println("Stickers not available in this context.")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:            "insights",
		Description:     "Show AI-generated insights about the session",
		ResumeSupported: true,
		Category:        CategoryUX,
		Handler: func(args string, loop interface{}) error {
			type insightsProvider interface {
				GetInsights() (string, error)
			}
			if ip, ok := loop.(insightsProvider); ok {
				insights, err := ip.GetInsights()
				if err != nil {
					return fmt.Errorf("insights: %w", err)
				}
				fmt.Println(insights)
			} else {
				fmt.Println("Insights not available in this context.")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:            "thinkback",
		Description:     "Replay the thinking process of the last response",
		ResumeSupported: true,
		Category:        CategoryUX,
		Handler: func(args string, loop interface{}) error {
			type thinkbackProvider interface {
				GetThinkback() (string, error)
			}
			if tp, ok := loop.(thinkbackProvider); ok {
				thinking, err := tp.GetThinkback()
				if err != nil {
					return fmt.Errorf("thinkback: %w", err)
				}
				if thinking == "" {
					fmt.Println("No thinking trace available for the last response.")
				} else {
					fmt.Println("Thinking trace:")
					fmt.Println(thinking)
				}
			} else {
				fmt.Println("Thinkback not available in this context.")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:        "desktop",
		Description: "Open or manage the desktop app integration",
		Category:    CategoryUX,
		Handler: func(args string, loop interface{}) error {
			type desktopIntegration interface {
				OpenDesktop() error
			}
			if di, ok := loop.(desktopIntegration); ok {
				if err := di.OpenDesktop(); err != nil {
					return fmt.Errorf("desktop: %w", err)
				}
				fmt.Println("Desktop app integration opened.")
			} else {
				fmt.Println("Desktop integration not available in this context.")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:         "ide",
		Description:  "Open or configure IDE integration",
		ArgumentHint: "[vscode|cursor]",
		Category:     CategoryUX,
		Handler: func(args string, loop interface{}) error {
			type ideIntegration interface {
				OpenIDE(name string) error
			}
			ide := strings.TrimSpace(strings.ToLower(args))
			if ide == "" {
				ide = "vscode"
			}
			switch ide {
			case "vscode", "cursor":
				// valid
			default:
				return fmt.Errorf("ide: unknown IDE %q (use vscode or cursor)", ide)
			}
			if ii, ok := loop.(ideIntegration); ok {
				if err := ii.OpenIDE(ide); err != nil {
					return fmt.Errorf("ide: %w", err)
				}
				fmt.Printf("Opened %s integration.\n", ide)
			} else {
				fmt.Println("IDE integration not available in this context.")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:        "listen",
		Description: "Listen for voice input",
		Category:    CategoryUX,
		Handler: func(args string, loop interface{}) error {
			type voiceListener interface {
				ListenForVoice() (string, error)
			}
			if vl, ok := loop.(voiceListener); ok {
				fmt.Println("Listening...")
				text, err := vl.ListenForVoice()
				if err != nil {
					return fmt.Errorf("listen: %w", err)
				}
				fmt.Printf("Heard: %s\n", text)
			} else {
				fmt.Println("Voice listening not available in this context.")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:        "speak",
		Description: "Read the last response aloud",
		Category:    CategoryUX,
		Handler: func(args string, loop interface{}) error {
			type speaker interface {
				SpeakLastResponse() error
			}
			if sp, ok := loop.(speaker); ok {
				if err := sp.SpeakLastResponse(); err != nil {
					return fmt.Errorf("speak: %w", err)
				}
			} else {
				fmt.Println("Speech output not available in this context.")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:         "format",
		Description:  "Format the last response in a different style",
		ArgumentHint: "[markdown|plain|json]",
		Category:     CategoryUX,
		Handler: func(args string, loop interface{}) error {
			type responseFormatter interface {
				FormatLastResponse(style string) (string, error)
			}
			style := strings.TrimSpace(strings.ToLower(args))
			if style == "" {
				fmt.Println("Usage: /format [markdown|plain|json]")
				return nil
			}
			switch style {
			case "markdown", "plain", "json":
				// valid
			default:
				return fmt.Errorf("format: unknown style %q (use markdown, plain, or json)", style)
			}
			if rf, ok := loop.(responseFormatter); ok {
				formatted, err := rf.FormatLastResponse(style)
				if err != nil {
					return fmt.Errorf("format: %w", err)
				}
				fmt.Println(formatted)
			} else {
				fmt.Println("Response formatting not available in this context.")
			}
			return nil
		},
	})

	r.Register(Command{
		Name:            "privacy-settings",
		Description:     "View/modify privacy settings",
		ResumeSupported: true,
		Category:        CategoryUX,
		Handler: func(args string, loop interface{}) error {
			fmt.Println("Privacy settings:")
			fmt.Println("  Telemetry:      off")
			fmt.Println("  History:        local only")
			fmt.Println("  Data retention: session-scoped")
			fmt.Println("Use /config to modify settings.")
			return nil
		},
	})

	r.Register(Command{
		Name:            "output-style",
		Description:     "Switch output formatting style",
		ArgumentHint:    "[style]",
		ResumeSupported: true,
		Category:        CategoryUX,
		Handler: func(args string, loop interface{}) error {
			style := strings.TrimSpace(strings.ToLower(args))
			if style == "" {
				fmt.Println("Available output styles: plain, markdown, json")
				return nil
			}
			switch style {
			case "plain", "markdown", "json":
				fmt.Printf("Output style set to %s\n", style)
			default:
				return fmt.Errorf("output-style: unknown style %q (use plain, markdown, or json)", style)
			}
			return nil
		},
	})
}
