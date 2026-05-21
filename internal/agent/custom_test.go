package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/skill-up/internal/config"
	"github.com/alibaba/skill-up/internal/credential"
	"github.com/alibaba/skill-up/internal/runtime"
	"github.com/alibaba/skill-up/pkg/transcript"
)

func newCustomTestRuntime(t *testing.T) *runtime.NoneRuntime {
	t.Helper()
	rt := &runtime.NoneRuntime{}
	if err := rt.Create(context.Background()); err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return rt
}

func customLocalAgent(custom *config.CustomEngineConfig) *CustomAgent {
	return NewCustomAgent(Config{Name: "my-agent", Custom: custom})
}

func userMessages() []transcript.Message {
	return []transcript.Message{{Role: transcript.RoleUser, Content: "review the diff"}}
}

func TestCustomAgent_RunLocal_StdoutSessionResult(t *testing.T) {
	t.Parallel()
	rt := newCustomTestRuntime(t)
	ag := customLocalAgent(&config.CustomEngineConfig{
		Transport: "local",
		Local: &config.CustomLocalConfig{
			Command: "sh",
			Args:    []string{"-c", `echo '{"exit_code":0,"final_message":"done","turns":2}'`},
		},
	})

	res, err := ag.Run(context.Background(), rt, ExecOptions{CaseID: "c1", Variant: "with_skill"}, userMessages())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 0 || res.FinalMessage != "done" || res.Turns != 2 {
		t.Fatalf("unexpected result: %#v", res)
	}
	if res.Engine != "my-agent" {
		t.Errorf("engine = %q, want my-agent", res.Engine)
	}
}

func TestCustomAgent_RunLocal_OutputFile(t *testing.T) {
	t.Parallel()
	rt := newCustomTestRuntime(t)
	ag := customLocalAgent(&config.CustomEngineConfig{
		Transport: "local",
		Local: &config.CustomLocalConfig{
			Command:    "sh",
			Args:       []string{"-c", `mkdir -p "$(dirname '${output_file}')" && echo '{"exit_code":0,"final_message":"from-file"}' > '${output_file}'`},
			OutputFile: "${output_file}",
		},
	})

	res, err := ag.Run(context.Background(), rt, ExecOptions{}, userMessages())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FinalMessage != "from-file" {
		t.Fatalf("final_message = %q, want from-file", res.FinalMessage)
	}
}

func TestCustomAgent_RunLocal_RelativeCwd(t *testing.T) {
	t.Parallel()
	rt := newCustomTestRuntime(t)
	// "inputs" is created under the workspace when the session input is
	// written; a relative cwd must resolve against the workspace.
	ag := customLocalAgent(&config.CustomEngineConfig{
		Transport: "local",
		Local: &config.CustomLocalConfig{
			Command: "sh",
			Cwd:     "inputs",
			Args:    []string{"-c", `printf '{"exit_code":0,"final_message":"%s"}' "$(pwd)"`},
		},
	})

	res, err := ag.Run(context.Background(), rt, ExecOptions{}, userMessages())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.HasSuffix(res.FinalMessage, "/inputs") {
		t.Fatalf("cwd = %q, want it resolved under the workspace inputs/ dir", res.FinalMessage)
	}
}

func TestCustomAgent_InstallMCP_NoopWithServers(t *testing.T) {
	t.Parallel()
	rt := newCustomTestRuntime(t)
	ag := customLocalAgent(&config.CustomEngineConfig{
		Transport: "local",
		Local:     &config.CustomLocalConfig{Command: "/opt/agent"},
	})

	// CLIAgent.InstallMCP would error here (empty InstallMCPCmd + servers);
	// CustomAgent overrides it to a no-op.
	err := ag.InstallMCP(context.Background(), rt, runtime.MCPConfig{
		Servers: []runtime.MCPServerConfig{{Name: "demo", Mode: "mocked"}},
	})
	if err != nil {
		t.Fatalf("InstallMCP: %v", err)
	}
}

//nolint:dupl // distinct scenario from InputFileEqualsOutputFile (relative cwd vs path collision)
func TestCustomAgent_RunLocal_RelativeOutputFileWithCwd(t *testing.T) {
	t.Parallel()
	rt := newCustomTestRuntime(t)
	// A relative output_file combined with a non-default cwd must still be
	// resolved against the workspace, so readRawResult finds the file.
	ag := customLocalAgent(&config.CustomEngineConfig{
		Transport: "local",
		Local: &config.CustomLocalConfig{
			Command:    "sh",
			Cwd:        "inputs",
			OutputFile: "result.json",
			Args:       []string{"-c", `echo '{"exit_code":0,"final_message":"rel-file"}' > '${output_file}'`},
		},
	})

	res, err := ag.Run(context.Background(), rt, ExecOptions{}, userMessages())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FinalMessage != "rel-file" {
		t.Fatalf("final_message = %q, want rel-file (result read from the relative output file)", res.FinalMessage)
	}
}

func TestCustomAgent_RunLocal_TextFormat(t *testing.T) {
	t.Parallel()
	rt := newCustomTestRuntime(t)
	ag := customLocalAgent(&config.CustomEngineConfig{
		Transport:      "local",
		ResponseFormat: "text",
		Local: &config.CustomLocalConfig{
			Command: "sh",
			Args:    []string{"-c", "echo plain output text"},
		},
	})

	res, err := ag.Run(context.Background(), rt, ExecOptions{}, userMessages())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FinalMessage != "plain output text" {
		t.Fatalf("final_message = %q, want plain output text", res.FinalMessage)
	}
}

func TestCustomAgent_RunLocal_IgnoresStaleOutputFile(t *testing.T) {
	t.Parallel()
	rt := newCustomTestRuntime(t)
	// Pre-seed a stale default output file, as a fixture or setup step might.
	stale := filepath.Join(rt.Workspace(), "outputs", "session-result.json")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte(`{"exit_code":0,"final_message":"STALE"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// The command returns its result on stdout and never writes the file.
	ag := customLocalAgent(&config.CustomEngineConfig{
		Transport: "local",
		Local: &config.CustomLocalConfig{
			Command: "sh",
			Args:    []string{"-c", `echo '{"exit_code":0,"final_message":"fresh-stdout"}'`},
		},
	})

	res, err := ag.Run(context.Background(), rt, ExecOptions{}, userMessages())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FinalMessage != "fresh-stdout" {
		t.Fatalf("final_message = %q, want fresh-stdout (stale output file must be ignored)", res.FinalMessage)
	}
	// output_file was not configured, so the default path must be left
	// untouched — it may be ordinary fixture input.
	if _, statErr := os.Stat(stale); statErr != nil {
		t.Fatalf("default output file was removed though output_file is unset: %v", statErr)
	}
}

func TestCustomAgent_RunLocal_KwargsInOutputFilePath(t *testing.T) {
	t.Parallel()
	rt := newCustomTestRuntime(t)
	// output_file references a kwarg; kwargs must be resolved before the I/O
	// path templates so ${kwargs.profile} expands rather than vanishing.
	ag := customLocalAgent(&config.CustomEngineConfig{
		Transport: "local",
		Kwargs:    map[string]string{"profile": "rep"},
		Local: &config.CustomLocalConfig{
			Command:    "sh",
			OutputFile: "outputs/${kwargs.profile}.json",
			Args:       []string{"-c", `mkdir -p "$(dirname '${output_file}')" && echo '{"exit_code":0,"final_message":"kwarg-path"}' > '${output_file}'`},
		},
	})

	res, err := ag.Run(context.Background(), rt, ExecOptions{}, userMessages())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FinalMessage != "kwarg-path" {
		t.Fatalf("final_message = %q, want kwarg-path (kwargs must resolve in output_file)", res.FinalMessage)
	}
}

func TestCustomAgent_RunLocal_ClearedStaleOutputRegistered(t *testing.T) {
	t.Parallel()
	rt := newCustomTestRuntime(t)
	// With an explicitly configured output_file, a stale file from a previous
	// run is cleared before the run; that path must be registered so the
	// deletion is excluded from workspace diffs even when the engine returns
	// its result on stdout and never recreates the file.
	stale := filepath.Join(rt.Workspace(), "result.json")
	if err := os.WriteFile(stale, []byte(`{"exit_code":0,"final_message":"STALE"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ag := customLocalAgent(&config.CustomEngineConfig{
		Transport: "local",
		Local: &config.CustomLocalConfig{
			Command:    "sh",
			OutputFile: "result.json",
			Args:       []string{"-c", `echo '{"exit_code":0,"final_message":"fresh"}'`},
		},
	})

	res, err := ag.Run(context.Background(), rt, ExecOptions{}, userMessages())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !containsBasename(res.Artifacts.GeneratedFiles, "result.json") {
		t.Fatalf("generated_files = %v, want the cleared output path registered", res.Artifacts.GeneratedFiles)
	}
}

func TestCustomAgent_RunLocal_TextIgnoresOutputFile(t *testing.T) {
	t.Parallel()
	rt := newCustomTestRuntime(t)
	// A text engine prints the answer on stdout but also writes a bookkeeping
	// file at the default ${output_file} path; stdout must be graded.
	ag := customLocalAgent(&config.CustomEngineConfig{
		Transport:      "local",
		ResponseFormat: "text",
		Local: &config.CustomLocalConfig{
			Command: "sh",
			Args:    []string{"-c", `mkdir -p outputs && echo bookkeeping > outputs/session-result.json && echo stdout-answer`},
		},
	})

	res, err := ag.Run(context.Background(), rt, ExecOptions{}, userMessages())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FinalMessage != "stdout-answer" {
		t.Fatalf("final_message = %q, want stdout-answer (output file must not be graded)", res.FinalMessage)
	}
}

func TestCustomAgent_RunLocal_NonZeroExitCodePreserved(t *testing.T) {
	t.Parallel()
	rt := newCustomTestRuntime(t)
	ag := customLocalAgent(&config.CustomEngineConfig{
		Transport: "local",
		Local: &config.CustomLocalConfig{
			Command: "sh",
			Args:    []string{"-c", `echo '{"exit_code":3,"final_message":"boom","stderr":"bad"}'`},
		},
	})

	res, err := ag.Run(context.Background(), rt, ExecOptions{}, userMessages())
	if err == nil {
		t.Fatal("expected error for non-zero exit_code")
	}
	if res == nil || res.ExitCode != 3 || res.FinalMessage != "boom" {
		t.Fatalf("session result not preserved: %#v", res)
	}
}

func TestCustomAgent_RunLocal_NonZeroExitWithoutJSON(t *testing.T) {
	t.Parallel()
	rt := newCustomTestRuntime(t)
	// The command crashes (non-zero exit) without ever emitting JSON.
	ag := customLocalAgent(&config.CustomEngineConfig{
		Transport: "local",
		Local: &config.CustomLocalConfig{
			Command: "sh",
			Args:    []string{"-c", `echo boom-stderr >&2; exit 42`},
		},
	})

	res, err := ag.Run(context.Background(), rt, ExecOptions{}, userMessages())
	if err == nil || !strings.Contains(err.Error(), "exited 42") {
		t.Fatalf("error = %v, want the real command exit surfaced", err)
	}
	if res.ExitCode != 42 {
		t.Fatalf("res.ExitCode = %d, want 42", res.ExitCode)
	}
	if !strings.Contains(res.Stderr, "boom-stderr") {
		t.Fatalf("res.Stderr = %q, want the command stderr preserved", res.Stderr)
	}
}

func TestCustomAgent_RunLocal_UnparseableResult(t *testing.T) {
	t.Parallel()
	rt := newCustomTestRuntime(t)
	ag := customLocalAgent(&config.CustomEngineConfig{
		Transport: "local",
		Local: &config.CustomLocalConfig{
			Command: "sh",
			Args:    []string{"-c", "echo not-json-at-all"},
		},
	})

	_, err := ag.Run(context.Background(), rt, ExecOptions{}, userMessages())
	if err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("error = %v, want JSON parse error", err)
	}
}

func TestCustomAgent_RunLocal_MissingExitCode(t *testing.T) {
	t.Parallel()
	rt := newCustomTestRuntime(t)
	ag := customLocalAgent(&config.CustomEngineConfig{
		Transport: "local",
		Local: &config.CustomLocalConfig{
			Command: "sh",
			Args:    []string{"-c", `echo '{"final_message":"hi"}'`},
		},
	})

	_, err := ag.Run(context.Background(), rt, ExecOptions{}, userMessages())
	if err == nil || !strings.Contains(err.Error(), "exit_code") {
		t.Fatalf("error = %v, want missing exit_code error", err)
	}
}

func TestCustomAgent_RunLocal_NonZeroProcessExitFails(t *testing.T) {
	t.Parallel()
	rt := newCustomTestRuntime(t)
	// The JSON reports success but the process crashes afterwards.
	ag := customLocalAgent(&config.CustomEngineConfig{
		Transport: "local",
		Local: &config.CustomLocalConfig{
			Command: "sh",
			Args:    []string{"-c", `echo '{"exit_code":0,"final_message":"ok"}'; exit 7`},
		},
	})

	res, err := ag.Run(context.Background(), rt, ExecOptions{}, userMessages())
	if err == nil || !strings.Contains(err.Error(), "exited 7") {
		t.Fatalf("error = %v, want non-zero process exit error", err)
	}
	if res == nil {
		t.Fatal("expected session result to be preserved")
	}
	// The real process exit code must surface in the result, not the JSON's 0.
	if res.ExitCode != 7 {
		t.Fatalf("res.ExitCode = %d, want 7 (the process exit code)", res.ExitCode)
	}
}

func TestCustomAgent_RunLocal_NilLocalConfigNoPanic(t *testing.T) {
	t.Parallel()
	rt := newCustomTestRuntime(t)
	// transport: local with no local block (can slip past validation via a
	// --engine override) must error, not panic.
	ag := customLocalAgent(&config.CustomEngineConfig{Transport: "local"})

	_, err := ag.Run(context.Background(), rt, ExecOptions{}, userMessages())
	if err == nil || !strings.Contains(err.Error(), "local.command is required") {
		t.Fatalf("error = %v, want local.command required error", err)
	}
}

func TestCustomAgent_RunLocal_FallbackTranscript(t *testing.T) {
	t.Parallel()
	rt := newCustomTestRuntime(t)
	// The engine returns final_message but no transcript.
	ag := customLocalAgent(&config.CustomEngineConfig{
		Transport: "local",
		Local: &config.CustomLocalConfig{
			Command: "sh",
			Args:    []string{"-c", `echo '{"exit_code":0,"final_message":"the answer"}'`},
		},
	})

	res, err := ag.Run(context.Background(), rt, ExecOptions{}, userMessages())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Transcript) != 2 {
		t.Fatalf("transcript = %#v, want input message + assistant reply", res.Transcript)
	}
	if res.Transcript[0].Role != transcript.RoleUser ||
		res.Transcript[1].Role != transcript.RoleAssistant ||
		res.Transcript[1].Content != "the answer" {
		t.Fatalf("unexpected fallback transcript: %#v", res.Transcript)
	}
}

func TestCustomAgent_RunLocal_RejectsAPIKeyInArgs(t *testing.T) {
	t.Parallel()
	rt := newCustomTestRuntime(t)
	ag := NewCustomAgent(Config{
		Name:   "my-agent",
		APIKey: "sk-super-secret",
		Custom: &config.CustomEngineConfig{
			Transport: "local",
			Local: &config.CustomLocalConfig{
				Command: "sh",
				Args:    []string{"-c", "true", "--api-key", "${api_key}"},
			},
		},
	})

	_, err := ag.Run(context.Background(), rt, ExecOptions{}, userMessages())
	if err == nil || !strings.Contains(err.Error(), "custom.env") {
		t.Fatalf("error = %v, want rejection of API key in command line", err)
	}
}

func TestCustomAgent_RunLocal_PathArtifactPreservesName(t *testing.T) {
	t.Parallel()
	rt := newCustomTestRuntime(t)
	artifactDir := t.TempDir()
	// The engine writes a file whose basename differs from the declared name.
	ag := customLocalAgent(&config.CustomEngineConfig{
		Transport: "local",
		Local: &config.CustomLocalConfig{
			Command: "sh",
			Args: []string{"-c", `mkdir -p outputs && echo body > outputs/gen-xyz.tmp && ` +
				`echo '{"exit_code":0,"final_message":"ok","artifacts":{"files":[{"name":"report.md","path":"outputs/gen-xyz.tmp"}]}}'`},
		},
	})

	res, err := ag.Run(context.Background(), rt, ExecOptions{ArtifactDir: artifactDir}, userMessages())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(artifactDir, "report.md")); statErr != nil {
		t.Fatalf("artifact not archived under declared name: %v", statErr)
	}
	if !containsBasename(res.Artifacts.GeneratedFiles, "report.md") {
		t.Fatalf("generated_files = %v, want an entry named report.md", res.Artifacts.GeneratedFiles)
	}
	// The original workspace path must also be registered so the workspace
	// diff collector excludes it.
	if !containsBasename(res.Artifacts.GeneratedFiles, "gen-xyz.tmp") {
		t.Fatalf("generated_files = %v, want the original path kept for diff exclusion", res.Artifacts.GeneratedFiles)
	}
}

// assertCustomGeneratedFile runs a local custom agent with the given shell
// args and asserts that a file with wantBasename is registered in
// GeneratedFiles (so it is excluded from workspace diffs).
func assertCustomGeneratedFile(t *testing.T, scriptArgs []string, wantBasename string) {
	t.Helper()
	rt := newCustomTestRuntime(t)
	ag := customLocalAgent(&config.CustomEngineConfig{
		Transport: "local",
		Local:     &config.CustomLocalConfig{Command: "sh", Args: scriptArgs},
	})

	res, err := ag.Run(context.Background(), rt, ExecOptions{}, userMessages())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !containsBasename(res.Artifacts.GeneratedFiles, wantBasename) {
		t.Fatalf("generated_files = %v, want %s registered", res.Artifacts.GeneratedFiles, wantBasename)
	}
}

func TestCustomAgent_RunLocal_RegistersFrameworkInputFile(t *testing.T) {
	t.Parallel()
	// The framework-written input file must be registered so it is excluded
	// from workspace diffs.
	assertCustomGeneratedFile(t,
		[]string{"-c", `echo '{"exit_code":0,"final_message":"ok"}'`},
		"messages.json")
}

func TestCustomAgent_RunLocal_PartialResultOnTimeout(t *testing.T) {
	t.Parallel()
	rt := newCustomTestRuntime(t)
	// The engine prints a valid result, then hangs well past the timeout.
	// The 2s budget keeps the echo reliably captured even on a loaded CI.
	ag := customLocalAgent(&config.CustomEngineConfig{
		Transport:      "local",
		TimeoutSeconds: 2,
		Local: &config.CustomLocalConfig{
			Command: "sh",
			Args:    []string{"-c", `echo '{"exit_code":0,"final_message":"partial answer"}'; sleep 30`},
		},
	})

	res, err := ag.Run(context.Background(), rt, ExecOptions{}, userMessages())
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if res == nil || res.FinalMessage != "partial answer" {
		t.Fatalf("res = %#v, want the partial result preserved", res)
	}
	// An interrupted run must not report exit_code 0 even though the partial
	// JSON did, or the evaluator could treat it as a clean success.
	if res.ExitCode == 0 {
		t.Fatalf("res.ExitCode = 0, want a non-zero code for an interrupted run")
	}
}

func TestCustomAgent_RunLocal_RendersTemplatedKwargs(t *testing.T) {
	t.Parallel()
	rt := newCustomTestRuntime(t)
	// kwargs reference a built-in template variable; it must be rendered
	// before reaching ${kwargs.*}.
	ag := customLocalAgent(&config.CustomEngineConfig{
		Transport:      "local",
		ResponseFormat: "text",
		Kwargs:         map[string]string{"cid": "${case_id}"},
		Local: &config.CustomLocalConfig{
			Command: "sh",
			Args:    []string{"-c", "printf %s ${kwargs.cid}"},
		},
	})

	res, err := ag.Run(context.Background(), rt, ExecOptions{CaseID: "case-42"}, userMessages())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FinalMessage != "case-42" {
		t.Fatalf("final_message = %q, want the rendered case_id (case-42)", res.FinalMessage)
	}
}

func TestCustomAgent_RunLocal_TimeoutEnforced(t *testing.T) {
	t.Parallel()
	rt := newCustomTestRuntime(t)
	ag := customLocalAgent(&config.CustomEngineConfig{
		Transport:      "local",
		TimeoutSeconds: 1,
		Local: &config.CustomLocalConfig{
			Command: "sh",
			Args:    []string{"-c", "sleep 5"},
		},
	})

	start := time.Now()
	_, err := ag.Run(context.Background(), rt, ExecOptions{}, userMessages())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	// 1s deadline + NoneRuntime's WaitDelay grace; well under the 5s sleep.
	if elapsed > 4500*time.Millisecond {
		t.Fatalf("run took %s, want the 1s timeout to be enforced", elapsed)
	}
}

func TestCustomAgent_RunLocal_InlineArtifactRegistered(t *testing.T) {
	t.Parallel()
	rt := newCustomTestRuntime(t)
	artifactDir := t.TempDir()
	ag := customLocalAgent(&config.CustomEngineConfig{
		Transport: "local",
		Local: &config.CustomLocalConfig{
			Command: "sh",
			Args:    []string{"-c", `echo '{"exit_code":0,"final_message":"ok","artifacts":{"files":[{"name":"report.md","content":"hello-artifact"}]}}'`},
		},
	})

	res, err := ag.Run(context.Background(), rt, ExecOptions{ArtifactDir: artifactDir}, userMessages())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !containsBasename(res.Artifacts.GeneratedFiles, "report.md") {
		t.Fatalf("generated_files = %v, want the inline artifact registered", res.Artifacts.GeneratedFiles)
	}
	data, readErr := os.ReadFile(filepath.Join(artifactDir, "report.md"))
	if readErr != nil || string(data) != "hello-artifact" {
		t.Fatalf("inline artifact = %q (err %v), want hello-artifact", data, readErr)
	}
}

func containsBasename(paths []string, base string) bool {
	for _, p := range paths {
		if filepath.Base(p) == base {
			return true
		}
	}
	return false
}

func TestCustomAgent_RunLocal_DefaultsTurnsFromTranscript(t *testing.T) {
	t.Parallel()
	rt := newCustomTestRuntime(t)
	// Engine returns the documented minimal SessionResult (no turns); the
	// agent must default Turns from the produced transcript.
	ag := customLocalAgent(&config.CustomEngineConfig{
		Transport: "local",
		Local: &config.CustomLocalConfig{
			Command: "sh",
			Args:    []string{"-c", `echo '{"exit_code":0,"final_message":"ok"}'`},
		},
	})

	res, err := ag.Run(context.Background(), rt, ExecOptions{}, userMessages())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Turns < 1 {
		t.Fatalf("res.Turns = %d, want it defaulted from the transcript (>=1)", res.Turns)
	}
}

func TestCustomAgent_RunLocal_DerivesTurnsFromTranscriptWithoutTurnField(t *testing.T) {
	t.Parallel()
	rt := newCustomTestRuntime(t)
	// Engine supplies a transcript whose messages omit the `turn` field
	// (matches the design's transcript example). Turns must still be > 0.
	ag := customLocalAgent(&config.CustomEngineConfig{
		Transport: "local",
		Local: &config.CustomLocalConfig{
			Command: "sh",
			Args:    []string{"-c", `echo '{"exit_code":0,"final_message":"answer","transcript":[{"role":"user","content":"q1"},{"role":"assistant","content":"a1"},{"role":"user","content":"q2"},{"role":"assistant","content":"answer"}]}'`},
		},
	})

	res, err := ag.Run(context.Background(), rt, ExecOptions{}, userMessages())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Two assistant replies → 2 turns inferred from conversation structure.
	if res.Turns != 2 {
		t.Fatalf("res.Turns = %d, want 2 (one per assistant reply)", res.Turns)
	}
}

func TestCustomAgent_RunLocal_DerivesFinalMessageFromTranscript(t *testing.T) {
	t.Parallel()
	rt := newCustomTestRuntime(t)
	// Engine omits final_message but provides a transcript; the agent must
	// derive final_message from the last assistant reply so judges and
	// reports do not grade or display a blank answer.
	ag := customLocalAgent(&config.CustomEngineConfig{
		Transport: "local",
		Local: &config.CustomLocalConfig{
			Command: "sh",
			Args:    []string{"-c", `echo '{"exit_code":0,"transcript":[{"role":"user","content":"q"},{"role":"assistant","content":"derived-answer"}]}'`},
		},
	})

	res, err := ag.Run(context.Background(), rt, ExecOptions{}, userMessages())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FinalMessage != "derived-answer" {
		t.Fatalf("final_message = %q, want it derived from the transcript", res.FinalMessage)
	}
}

func TestCustomAgent_RunLocal_PreservesResultOnWaitDelay(t *testing.T) {
	t.Parallel()
	rt := newCustomTestRuntime(t)
	// The parent prints a valid SessionResult and exits, but a backgrounded
	// child keeps stdout open past NoneRuntime's WaitDelay. rt.Exec returns
	// a non-nil exec.ErrWaitDelay; the agent must still read the JSON the
	// parent already emitted and surface the partial result.
	ag := customLocalAgent(&config.CustomEngineConfig{
		Transport: "local",
		Local: &config.CustomLocalConfig{
			Command: "sh",
			Args:    []string{"-c", `echo '{"exit_code":0,"final_message":"recovered"}'; (sleep 30 &); exit 0`},
		},
	})

	res, err := ag.Run(context.Background(), rt, ExecOptions{}, userMessages())
	if err == nil {
		t.Fatal("expected a WaitDelay-related error from rt.Exec")
	}
	if res == nil || res.FinalMessage != "recovered" {
		t.Fatalf("res = %#v, want the partial result preserved despite WaitDelay", res)
	}
}

//nolint:dupl // distinct scenario: input/output path collision, asserted via a different command shape than RelativeOutputFileWithCwd
func TestCustomAgent_RunLocal_InputFileEqualsOutputFile(t *testing.T) {
	t.Parallel()
	rt := newCustomTestRuntime(t)
	// Both paths resolve to the same file. The framework must not delete the
	// SessionInput it just wrote, so the command can still read it before
	// overwriting it with its own SessionResult.
	ag := customLocalAgent(&config.CustomEngineConfig{
		Transport: "local",
		Local: &config.CustomLocalConfig{
			Command:    "sh",
			InputFile:  "io.json",
			OutputFile: "io.json",
			Args: []string{"-c", `set -e
test -s '${input_file}' || { echo "input was deleted" >&2; exit 1; }
echo '{"exit_code":0,"final_message":"same-path-ok"}' > '${output_file}'`},
		},
	})

	res, err := ag.Run(context.Background(), rt, ExecOptions{}, userMessages())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FinalMessage != "same-path-ok" {
		t.Fatalf("final_message = %q, want same-path-ok (input must survive when paths collide)", res.FinalMessage)
	}
}

func TestCustomAgent_RunHTTP_NotImplemented(t *testing.T) {
	t.Parallel()
	rt := newCustomTestRuntime(t)
	ag := customLocalAgent(&config.CustomEngineConfig{
		Transport: "http",
		HTTP:      &config.CustomHTTPConfig{URL: "https://example.com"},
	})

	_, err := ag.Run(context.Background(), rt, ExecOptions{}, userMessages())
	if err == nil || !strings.Contains(err.Error(), "not yet implemented") {
		t.Fatalf("error = %v, want not-implemented error", err)
	}
}

func TestRenderTemplate(t *testing.T) {
	t.Setenv("RT_TEST_ENV", "env-value")

	vars := map[string]string{
		"workspace":      "/ws",
		"api_key":        "sk-secret",
		"kwargs.profile": "strict",
	}
	tests := []struct {
		in   string
		want string
	}{
		{"${workspace}/run", "/ws/run"},
		{"key=${api_key}", "key=sk-secret"},
		{"profile=${kwargs.profile}", "profile=strict"},
		{"missing kwarg=${kwargs.absent}", "missing kwarg="},
		{"${RT_TEST_ENV}", "env-value"},
		{"${UNSET_VAR:-fallback}", "fallback"},
		{"plain", "plain"},
	}
	for _, tc := range tests {
		got, err := renderTemplate(tc.in, vars)
		if err != nil {
			t.Errorf("renderTemplate(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("renderTemplate(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRenderTemplate_EmptyBuiltinHonorsDefaultAndError(t *testing.T) {
	t.Parallel()
	// Built-in vars present but empty (e.g. unconfigured api_key / model).
	vars := map[string]string{"api_key": "", "model": ""}

	got, err := renderTemplate("model=${model:-gpt-fallback}", vars)
	if err != nil || got != "model=gpt-fallback" {
		t.Fatalf("renderTemplate default = %q (err %v), want model=gpt-fallback", got, err)
	}

	if _, err := renderTemplate("${api_key?api key required}", vars); err == nil ||
		!strings.Contains(err.Error(), "api key required") {
		t.Fatalf("error = %v, want required-form error for empty api_key", err)
	}

	// A plain reference to an empty built-in still resolves to empty.
	if got, err := renderTemplate("[${model}]", vars); err != nil || got != "[]" {
		t.Fatalf("renderTemplate plain = %q (err %v), want []", got, err)
	}
}

func TestRenderTemplate_UnresolvedErrors(t *testing.T) {
	t.Parallel()
	if _, err := renderTemplate("${NOPE}", map[string]string{}); err == nil {
		t.Error("expected error for unresolved variable")
	}
	if _, err := renderTemplate("${NOPE?need it}", map[string]string{}); err == nil ||
		!strings.Contains(err.Error(), "need it") {
		t.Errorf("expected custom error message, got %v", err)
	}
}

func TestDetectAgent_CustomDispatch(t *testing.T) {
	t.Parallel()
	custom := &config.CustomEngineConfig{
		Transport: "local",
		Local:     &config.CustomLocalConfig{Command: "/opt/agent"},
	}
	ag, err := DetectAgent("my-agent", Config{Name: "my-agent", Custom: custom})
	if err != nil {
		t.Fatalf("DetectAgent: %v", err)
	}
	if _, ok := ag.(*CustomAgent); !ok {
		t.Fatalf("agent type = %T, want *CustomAgent", ag)
	}
}

func TestDetectAgent_NonBuiltinWithoutCustom(t *testing.T) {
	t.Parallel()
	_, err := DetectAgent("my-agent", Config{Name: "my-agent"})
	if err == nil || !strings.Contains(err.Error(), "missing engine.custom") {
		t.Fatalf("error = %v, want missing engine.custom", err)
	}
}

func TestDetectAgentWithInitParams_KeepsAutoModelForCustom(t *testing.T) {
	t.Parallel()
	custom := &config.CustomEngineConfig{
		Transport: "local",
		Local:     &config.CustomLocalConfig{Command: "/opt/agent"},
	}
	ag, err := DetectAgentWithInitParams("my-agent", credential.AgentInitParams{
		Model:  modelAuto,
		Custom: custom,
	})
	if err != nil {
		t.Fatalf("DetectAgentWithInitParams: %v", err)
	}
	ca, ok := ag.(*CustomAgent)
	if !ok {
		t.Fatalf("agent type = %T, want *CustomAgent", ag)
	}
	// "auto" must not be stripped for a custom engine.
	if ca.Cfg.ModelName != modelAuto {
		t.Fatalf("ModelName = %q, want auto preserved for custom engine", ca.Cfg.ModelName)
	}
}
