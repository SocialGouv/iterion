package envtrust

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The round-8 (H-10 on doctrine A, #2844) admissions to the execution
// class, each with the proof the class's own bar asks — a process iterion
// starts READS or EXECUTES the name:
//
//   - GIT_EDITOR / GIT_SEQUENCE_EDITOR / GIT_PAGER: the programs git
//     itself runs — the H-10 probe measured the editor EXECUTED by `git
//     commit --allow-empty` with no -m and no TTY (the editor's marker
//     file created), the first commit an agent makes in a workspace it
//     plants into. The pager answers the same question (a program the
//     binary pipes output through); the refusal is free of TTY conditions
//     by being a name question.
//   - AWS_CA_BUNDLE: the vendored AWS SDK's TLS trust root. The read is IN
//     this tree — the completeness-style walk below checks the vendored
//     source itself, and the S3 blob store boots the SDK config on this
//     process's environment.
//   - the interpreter family: PYTHONPATH's sitecustomize EXECUTES at
//     interpreter startup — the H-10 probe measured the marker file
//     created by `python3 -c "print(1)"` with no user code at all, the
//     BASH_ENV form for the python family. The rest are the same question
//     for the sibling interpreters: PYTHONHOME/PYTHONSTARTUP, RUBYOPT,
//     PERL5OPT/PERL5LIB, JAVA_TOOL_OPTIONS/JDK_JAVA_OPTIONS/CLASSPATH —
//     NODE_PATH's and GOFLAGS's own answer, generalized.
//   - CLOUDSDK_PYTHON: the interpreter gcloud runs its own commands with —
//     wherever a tool node drives gcloud.
//   - OPENCODE_CONFIG / _DIR / _CONTENT / OPENCODE_PERMISSION: opencode is
//     one of the engine's own backends (pkg/backend/delegate runs its CLI
//     live); the config names move what the CODEX_HOME admission answers
//     for — the repository's own settings, permission bits included. The
//     XDG_ prefix covers the DEFAULT locations, not the explicit
//     variables.
//   - ANTHROPIC_UNIX_SOCKET: the route the claude CLI's own client dials —
//     a reader in an external binary, which the Go-form grep cannot see;
//     the devcontainer table has named that reader since #2303, the same
//     blindness the routing class confessed when GIT_PROXY_COMMAND joined.
func TestTheRound8AdmissionsAreRefusedWithTheirProofs(t *testing.T) {
	admitted := []string{
		"GIT_EDITOR", "GIT_SEQUENCE_EDITOR", "GIT_PAGER",
		"AWS_CA_BUNDLE",
		"PYTHONPATH", "PYTHONHOME", "PYTHONSTARTUP",
		"RUBYOPT", "PERL5OPT", "PERL5LIB",
		"JAVA_TOOL_OPTIONS", "JDK_JAVA_OPTIONS", "CLASSPATH",
		"CLOUDSDK_PYTHON",
		"OPENCODE_CONFIG", "OPENCODE_CONFIG_DIR", "OPENCODE_CONFIG_CONTENT",
		"OPENCODE_PERMISSION",
		"ANTHROPIC_UNIX_SOCKET",
	}
	isolate := func(t *testing.T) {
		t.Helper()
		ResetForTest()
		t.Cleanup(ResetForTest)
		t.Setenv(DotenvEndpointsOptIn, "")
	}
	for _, name := range admitted {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			if !DotenvPlantRefused(name) {
				t.Errorf("%s is not refused at the plant: the file a project holds chooses what iterion's children execute, load or trust", name)
			}
			if !DotenvPlantRefused(strings.ToLower(name)) {
				t.Errorf("%s refused, its lowercase spelling is not: the question must not depend on the case", name)
			}
			if !ExecutionGradeName(name) {
				t.Errorf("%s is refused but not by the execution class: the admission's proof is an execution-grade read", name)
			}
			// The operator's opt-in answers, like every execution name: the
			// legitimate life these names have in an operator's own file.
			t.Setenv(DotenvEndpointsOptIn, "allow")
			if DotenvPlantRefused(name) {
				t.Errorf("%s is not answered by the opt-in: the operator's grant decides, not the repository's file", name)
			}
		})
	}
}

// The AWS admission's proof is a read IN the tree: the vendored SDK's
// env_config.go carries the AWS_CA_BUNDLE constant and reads it into the
// config's CustomCABundle, and the S3 blob store boots that config on this
// process's environment (pkg/store/blob/s3.go, LoadDefaultConfig). A vendored
// bump that drops the read, or a store that stops booting the SDK config,
// reddens here — the witness follows the reader, not the memory of it.
func TestTheAWSBundleReadIsInTheTreeTheClassCites(t *testing.T) {
	root := moduleRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "vendor", "github.com", "aws", "aws-sdk-go-v2", "config", "env_config.go"))
	if err != nil {
		t.Fatalf("read the vendored AWS config source: %v", err)
	}
	src := string(raw)
	if !strings.Contains(src, "awsCABundleEnv = \"AWS_CA_BUNDLE\"") {
		t.Error("the vendored AWS config no longer spells AWS_CA_BUNDLE: the execution admission's in-tree read is gone, re-ask the name")
	}
	if !strings.Contains(src, "os.Getenv(awsCABundleEnv)") {
		t.Error("the vendored AWS config no longer reads the CA bundle constant: the read the admission stands on is gone")
	}
	store, err := os.ReadFile(filepath.Join(root, "pkg", "store", "blob", "s3.go"))
	if err != nil {
		t.Fatalf("read the S3 blob store: %v", err)
	}
	if !strings.Contains(string(store), "awsconfig.LoadDefaultConfig") {
		t.Error("the S3 blob store no longer boots the AWS SDK config: the env read the admission stands on lost its reader")
	}
}

// The ANTHROPIC_UNIX_SOCKET admission rests on the reader the devcontainer
// table names (the claude CLI's own client, an external binary): the two
// tables must keep agreeing that the reader exists — the routing class's
// grep-exclusion does not get to call a name the tree documents as read
// "found only in lists and comments" again.
func TestTheUnixSocketReaderTheTreeDocumentsStaysNamed(t *testing.T) {
	root := moduleRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "pkg", "sandbox", "devcontainer", "spec.go"))
	if err != nil {
		t.Fatalf("read the devcontainer spec: %v", err)
	}
	if !strings.Contains(string(raw), "ANTHROPIC_UNIX_SOCKET") {
		t.Error("the devcontainer table no longer names ANTHROPIC_UNIX_SOCKET: the documented reader the admission cites is gone, re-ask the name")
	}
}
