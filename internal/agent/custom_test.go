package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/alibaba/skill-up/internal/config"
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
