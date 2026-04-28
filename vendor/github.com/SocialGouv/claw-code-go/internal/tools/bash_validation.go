// Ported from rust/crates/runtime/src/bash_validation.rs
package tools

import (
	"fmt"
	"github.com/SocialGouv/claw-code-go/internal/permissions"
	"strings"
	"unicode"
)

// ValidationKind classifies the outcome of a bash command validation.
type ValidationKind int

const (
	// ValidationAllow means the command is safe to execute.
	ValidationAllow ValidationKind = iota
	// ValidationBlock means the command should be blocked.
	ValidationBlock
	// ValidationWarn means the command requires user confirmation.
	ValidationWarn
)

// ValidationResult is the outcome of validating a bash command before execution.
type ValidationResult struct {
	Kind    ValidationKind
	Reason  string
	Message string
}

// CommandIntent is a semantic classification of a bash command's intent.
type CommandIntent int

const (
	// ReadOnly operations: ls, cat, grep, find, etc.
	ReadOnly CommandIntent = iota
	// Write operations: cp, mv, mkdir, touch, tee, etc.
	Write
	// Destructive operations: rm, shred, truncate, etc.
	Destructive
	// Network operations: curl, wget, ssh, etc.
	Network
	// ProcessManagement operations: kill, pkill, etc.
	ProcessManagement
	// PackageManagement operations: apt, brew, pip, npm, etc.
	PackageManagement
	// SystemAdmin operations: sudo, chmod, chown, mount, etc.
	SystemAdmin
	// Unknown or unclassifiable command.
	Unknown
)

// ---------------------------------------------------------------------------
// Command lists
// ---------------------------------------------------------------------------

// writeCommands are commands that perform write operations and should be blocked in read-only mode.
var writeCommands = []string{
	"cp", "mv", "rm", "mkdir", "rmdir", "touch", "chmod", "chown", "chgrp",
	"ln", "install", "tee", "truncate", "shred", "mkfifo", "mknod", "dd",
}

// stateModifyingCommands modify system state and should be blocked in read-only mode.
var stateModifyingCommands = []string{
	"apt", "apt-get", "yum", "dnf", "pacman", "brew", "pip", "pip3",
	"npm", "yarn", "pnpm", "bun", "cargo", "gem", "go", "rustup",
	"docker", "systemctl", "service", "mount", "umount",
	"kill", "pkill", "killall", "reboot", "shutdown", "halt", "poweroff",
	"useradd", "userdel", "usermod", "groupadd", "groupdel",
	"crontab", "at",
}

// writeRedirections are shell redirection operators that indicate writes.
var writeRedirections = []string{">", ">>", ">&"}

// gitReadOnlySubcommands are git subcommands that are read-only safe.
var gitReadOnlySubcommands = []string{
	"status", "log", "diff", "show", "branch", "tag", "stash", "remote",
	"fetch", "ls-files", "ls-tree", "cat-file", "rev-parse", "describe",
	"shortlog", "blame", "bisect", "reflog", "config",
}

// destructivePatterns are patterns that indicate potentially destructive commands.
var destructivePatterns = []struct {
	pattern string
	warning string
}{
	{"rm -rf /", "Recursive forced deletion at root — this will destroy the system"},
	{"rm -rf ~", "Recursive forced deletion of home directory"},
	{"rm -rf *", "Recursive forced deletion of all files in current directory"},
	{"rm -rf .", "Recursive forced deletion of current directory"},
	{"mkfs", "Filesystem creation will destroy existing data on the device"},
	{"dd if=", "Direct disk write — can overwrite partitions or devices"},
	{"> /dev/sd", "Writing to raw disk device"},
	{"chmod -R 777", "Recursively setting world-writable permissions"},
	{"chmod -R 000", "Recursively removing all permissions"},
	{":(){ :|:& };:", "Fork bomb — will crash the system"},
}

// alwaysDestructiveCommands are commands that are always destructive regardless of arguments.
var alwaysDestructiveCommands = []string{"shred", "wipefs"}

// semanticReadOnlyCommands are commands that are read-only (no filesystem or state modification).
var semanticReadOnlyCommands = []string{
	"ls", "cat", "head", "tail", "less", "more", "wc", "sort", "uniq",
	"grep", "egrep", "fgrep", "find", "which", "whereis", "whatis",
	"man", "info", "file", "stat", "du", "df", "free", "uptime", "uname",
	"hostname", "whoami", "id", "groups", "env", "printenv", "echo", "printf",
	"date", "cal", "bc", "expr", "test", "true", "false", "pwd", "tree",
	"diff", "cmp", "md5sum", "sha256sum", "sha1sum", "xxd", "od", "hexdump",
	"strings", "readlink", "realpath", "basename", "dirname", "seq", "yes",
	"tput", "column", "jq", "yq", "xargs", "tr", "cut", "paste", "awk", "sed",
}

// networkCommands are commands that perform network operations.
var networkCommands = []string{
	"curl", "wget", "ssh", "scp", "rsync", "ftp", "sftp", "nc", "ncat",
	"telnet", "ping", "traceroute", "dig", "nslookup", "host", "whois",
	"ifconfig", "ip", "netstat", "ss", "nmap",
}

// processCommands are commands that manage processes.
var processCommands = []string{
	"kill", "pkill", "killall", "ps", "top", "htop", "bg", "fg", "jobs",
	"nohup", "disown", "wait", "nice", "renice",
}

// packageCommands are commands that manage packages.
var packageCommands = []string{
	"apt", "apt-get", "yum", "dnf", "pacman", "brew", "pip", "pip3",
	"npm", "yarn", "pnpm", "bun", "cargo", "gem", "go", "rustup",
	"snap", "flatpak",
}

// systemAdminCommands are commands that require system administrator privileges.
var systemAdminCommands = []string{
	"sudo", "su", "chroot", "mount", "umount", "fdisk", "parted", "lsblk",
	"blkid", "systemctl", "service", "journalctl", "dmesg", "modprobe",
	"insmod", "rmmod", "iptables", "ufw", "firewall-cmd", "sysctl",
	"crontab", "at", "useradd", "userdel", "usermod", "groupadd", "groupdel",
	"passwd", "visudo",
}

// systemPaths are paths that indicate operations outside a typical workspace.
var systemPaths = []string{
	"/etc/", "/usr/", "/var/", "/boot/", "/sys/", "/proc/",
	"/dev/", "/sbin/", "/lib/", "/opt/",
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// extractFirstCommand extracts the first bare command from a pipeline/chain,
// stripping env vars and sudo.
func extractFirstCommand(command string) string {
	remaining := strings.TrimSpace(command)

	// Skip leading environment variable assignments (KEY=val cmd ...).
	for {
		next := strings.TrimLeft(remaining, " \t")
		eqPos := strings.Index(next, "=")
		if eqPos < 0 {
			break
		}
		beforeEq := next[:eqPos]
		if beforeEq == "" {
			break
		}
		validEnvVar := true
		for _, c := range beforeEq {
			if !unicode.IsLetter(c) && !unicode.IsDigit(c) && c != '_' {
				validEnvVar = false
				break
			}
		}
		if !validEnvVar {
			break
		}
		afterEq := next[eqPos+1:]
		space := findEndOfValue(afterEq)
		if space >= 0 {
			remaining = afterEq[space:]
			continue
		}
		// No space found means value goes to end of string — no actual command.
		return ""
	}

	fields := strings.Fields(remaining)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// extractSudoInner extracts the command following "sudo" (skip sudo flags).
func extractSudoInner(command string) string {
	parts := strings.Fields(command)
	sudoIdx := -1
	for i, p := range parts {
		if p == "sudo" {
			sudoIdx = i
			break
		}
	}
	if sudoIdx < 0 {
		return ""
	}
	rest := parts[sudoIdx+1:]
	for _, part := range rest {
		if !strings.HasPrefix(part, "-") {
			offset := strings.Index(command, part)
			if offset < 0 {
				return ""
			}
			return command[offset:]
		}
	}
	return ""
}

// findEndOfValue finds the end of a value in `KEY=value rest` (handles basic quoting).
// Returns -1 if not found (value extends to end of string or is empty).
func findEndOfValue(s string) int {
	trimmed := strings.TrimLeft(s, " \t")
	if trimmed == "" {
		return -1
	}
	// Offset from start of s to start of trimmed content.
	trimOffset := len(s) - len(trimmed)

	first := trimmed[0]
	if first == '"' || first == '\'' {
		quote := first
		i := 1
		for i < len(trimmed) {
			if trimmed[i] == quote && (i == 0 || trimmed[i-1] != '\\') {
				i++ // skip past quote
				// Find next whitespace.
				for i < len(trimmed) && trimmed[i] != ' ' && trimmed[i] != '\t' && trimmed[i] != '\n' && trimmed[i] != '\r' {
					i++
				}
				if i < len(trimmed) {
					return trimOffset + i
				}
				return -1
			}
			i++
		}
		return -1
	}

	idx := strings.IndexFunc(trimmed, unicode.IsSpace)
	if idx >= 0 {
		return trimOffset + idx
	}
	return -1
}

// ---------------------------------------------------------------------------
// readOnlyValidation
// ---------------------------------------------------------------------------

// ValidateReadOnly validates that a command is allowed under read-only mode.
func ValidateReadOnly(command string, mode permissions.PermissionMode) ValidationResult {
	if mode != permissions.ModeReadOnly {
		return ValidationResult{Kind: ValidationAllow}
	}

	firstCommand := extractFirstCommand(command)

	// Check for write commands.
	for _, writeCmd := range writeCommands {
		if firstCommand == writeCmd {
			return ValidationResult{
				Kind:   ValidationBlock,
				Reason: fmt.Sprintf("Command '%s' modifies the filesystem and is not allowed in read-only mode", writeCmd),
			}
		}
	}

	// Check for state-modifying commands.
	for _, stateCmd := range stateModifyingCommands {
		if firstCommand == stateCmd {
			return ValidationResult{
				Kind:   ValidationBlock,
				Reason: fmt.Sprintf("Command '%s' modifies system state and is not allowed in read-only mode", stateCmd),
			}
		}
	}

	// Check for sudo wrapping write commands.
	if firstCommand == "sudo" {
		inner := extractSudoInner(command)
		if inner != "" {
			innerResult := ValidateReadOnly(inner, mode)
			if innerResult.Kind != ValidationAllow {
				return innerResult
			}
		}
	}

	// Check for write redirections.
	for _, redir := range writeRedirections {
		if strings.Contains(command, redir) {
			return ValidationResult{
				Kind:   ValidationBlock,
				Reason: fmt.Sprintf("Command contains write redirection '%s' which is not allowed in read-only mode", redir),
			}
		}
	}

	// Check for git commands that modify state.
	if firstCommand == "git" {
		return validateGitReadOnly(command)
	}

	return ValidationResult{Kind: ValidationAllow}
}

// validateGitReadOnly checks git subcommands against the read-only list.
func validateGitReadOnly(command string) ValidationResult {
	parts := strings.Fields(command)
	// Skip past "git" and any flags (e.g., "git -C /path").
	var subcommand string
	for _, p := range parts[1:] {
		if !strings.HasPrefix(p, "-") {
			subcommand = p
			break
		}
	}

	if subcommand == "" {
		// bare "git" is fine
		return ValidationResult{Kind: ValidationAllow}
	}

	if containsStr(gitReadOnlySubcommands, subcommand) {
		return ValidationResult{Kind: ValidationAllow}
	}

	return ValidationResult{
		Kind:   ValidationBlock,
		Reason: fmt.Sprintf("Git subcommand '%s' modifies repository state and is not allowed in read-only mode", subcommand),
	}
}

// ---------------------------------------------------------------------------
// destructiveCommandWarning
// ---------------------------------------------------------------------------

// CheckDestructive warns if a command looks destructive.
func CheckDestructive(command string) ValidationResult {
	// Check known destructive patterns.
	for _, dp := range destructivePatterns {
		if strings.Contains(command, dp.pattern) {
			return ValidationResult{
				Kind:    ValidationWarn,
				Message: fmt.Sprintf("Destructive command detected: %s", dp.warning),
			}
		}
	}

	// Check always-destructive commands.
	first := extractFirstCommand(command)
	for _, cmd := range alwaysDestructiveCommands {
		if first == cmd {
			return ValidationResult{
				Kind:    ValidationWarn,
				Message: fmt.Sprintf("Command '%s' is inherently destructive and may cause data loss", cmd),
			}
		}
	}

	// Check for "rm -rf" with broad targets.
	if strings.Contains(command, "rm ") && strings.Contains(command, "-r") && strings.Contains(command, "-f") {
		return ValidationResult{
			Kind:    ValidationWarn,
			Message: "Recursive forced deletion detected — verify the target path is correct",
		}
	}

	return ValidationResult{Kind: ValidationAllow}
}

// ---------------------------------------------------------------------------
// modeValidation
// ---------------------------------------------------------------------------

// ValidateMode validates that a command is consistent with the given permission mode.
func ValidateMode(command string, mode permissions.PermissionMode) ValidationResult {
	switch mode {
	case permissions.ModeReadOnly:
		return ValidateReadOnly(command, mode)
	case permissions.ModeWorkspaceWrite:
		if commandTargetsOutsideWorkspace(command) {
			return ValidationResult{
				Kind:    ValidationWarn,
				Message: "Command appears to target files outside the workspace — requires elevated permission",
			}
		}
		return ValidationResult{Kind: ValidationAllow}
	default:
		// DangerFullAccess, Allow, Prompt
		return ValidationResult{Kind: ValidationAllow}
	}
}

// commandTargetsOutsideWorkspace is a heuristic: does the command reference
// absolute paths outside typical workspace dirs?
func commandTargetsOutsideWorkspace(command string) bool {
	first := extractFirstCommand(command)
	isWriteCmd := containsStr(writeCommands, first) || containsStr(stateModifyingCommands, first)
	if !isWriteCmd {
		return false
	}

	for _, sysPath := range systemPaths {
		if strings.Contains(command, sysPath) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// sedValidation
// ---------------------------------------------------------------------------

// ValidateSed validates sed expressions for safety.
func ValidateSed(command string, mode permissions.PermissionMode) ValidationResult {
	first := extractFirstCommand(command)
	if first != "sed" {
		return ValidationResult{Kind: ValidationAllow}
	}

	if mode == permissions.ModeReadOnly && strings.Contains(command, " -i") {
		return ValidationResult{
			Kind:   ValidationBlock,
			Reason: "sed -i (in-place editing) is not allowed in read-only mode",
		}
	}

	return ValidationResult{Kind: ValidationAllow}
}

// ---------------------------------------------------------------------------
// pathValidation
// ---------------------------------------------------------------------------

// ValidatePaths validates that command paths don't include suspicious traversal patterns.
func ValidatePaths(command string, workspace string) ValidationResult {
	// Check for directory traversal attempts.
	if strings.Contains(command, "../") {
		if !strings.Contains(command, workspace) {
			return ValidationResult{
				Kind:    ValidationWarn,
				Message: "Command contains directory traversal pattern '../' — verify the target path resolves within the workspace",
			}
		}
	}

	// Check for home directory references that could escape workspace.
	if strings.Contains(command, "~/") || strings.Contains(command, "$HOME") {
		return ValidationResult{
			Kind:    ValidationWarn,
			Message: "Command references home directory — verify it stays within the workspace scope",
		}
	}

	return ValidationResult{Kind: ValidationAllow}
}

// ---------------------------------------------------------------------------
// commandSemantics
// ---------------------------------------------------------------------------

// ClassifyCommand classifies the semantic intent of a bash command.
func ClassifyCommand(command string) CommandIntent {
	first := extractFirstCommand(command)
	return classifyByFirstCommand(first, command)
}

func classifyByFirstCommand(first, command string) CommandIntent {
	if containsStr(semanticReadOnlyCommands, first) {
		if first == "sed" && strings.Contains(command, " -i") {
			return Write
		}
		return ReadOnly
	}

	if containsStr(alwaysDestructiveCommands, first) || first == "rm" {
		return Destructive
	}

	if containsStr(writeCommands, first) {
		return Write
	}

	if containsStr(networkCommands, first) {
		return Network
	}

	if containsStr(processCommands, first) {
		return ProcessManagement
	}

	if containsStr(packageCommands, first) {
		return PackageManagement
	}

	if containsStr(systemAdminCommands, first) {
		return SystemAdmin
	}

	if first == "git" {
		return classifyGitCommand(command)
	}

	return Unknown
}

// classifyGitCommand classifies a git command by its subcommand.
func classifyGitCommand(command string) CommandIntent {
	parts := strings.Fields(command)
	var subcommand string
	for _, p := range parts[1:] {
		if !strings.HasPrefix(p, "-") {
			subcommand = p
			break
		}
	}
	if subcommand != "" && containsStr(gitReadOnlySubcommands, subcommand) {
		return ReadOnly
	}
	return Write
}

// ---------------------------------------------------------------------------
// Pipeline: run all validations
// ---------------------------------------------------------------------------

// ValidateCommand runs the full validation pipeline on a bash command.
// Returns the first non-Allow result, or Allow if all validations pass.
func ValidateCommand(command string, mode permissions.PermissionMode, workspace string) ValidationResult {
	// 0. Pipeline analysis: validate each segment independently.
	segments := SplitPipeline(command)
	if len(segments) > 1 {
		for _, seg := range segments {
			result := validateSingleCommand(seg.Command, mode, workspace)
			if result.Kind != ValidationAllow {
				return result
			}
		}
	}

	// 1. Mode-level validation (includes read-only checks).
	result := ValidateMode(command, mode)
	if result.Kind != ValidationAllow {
		return result
	}

	// 2. Sed-specific validation.
	result = ValidateSed(command, mode)
	if result.Kind != ValidationAllow {
		return result
	}

	// 3. Destructive command warnings.
	result = CheckDestructive(command)
	if result.Kind != ValidationAllow {
		return result
	}

	// 4. Path validation.
	result = ValidatePaths(command, workspace)
	if result.Kind != ValidationAllow {
		return result
	}

	// 5. Sudo elevated flags.
	if sudoResult := DetectSudoElevatedFlags(command); sudoResult != nil {
		return *sudoResult
	}

	// 6. Archive extraction safety.
	result = ValidateArchiveExtraction(command)
	if result.Kind != ValidationAllow {
		return result
	}

	// 7. Env var leak detection.
	result = DetectEnvVarLeak(command)
	if result.Kind != ValidationAllow {
		return result
	}

	// 8. Network timeout warnings.
	result = ValidateNetworkTimeout(command)
	if result.Kind != ValidationAllow {
		return result
	}

	return ValidationResult{Kind: ValidationAllow}
}

// validateSingleCommand validates a single pipeline segment.
func validateSingleCommand(command string, mode permissions.PermissionMode, _ string) ValidationResult {
	result := ValidateMode(command, mode)
	if result.Kind != ValidationAllow {
		return result
	}
	result = ValidateSed(command, mode)
	if result.Kind != ValidationAllow {
		return result
	}
	result = CheckDestructive(command)
	if result.Kind != ValidationAllow {
		return result
	}
	return ValidationResult{Kind: ValidationAllow}
}
