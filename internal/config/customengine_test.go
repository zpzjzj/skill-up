package config

import (
	"strings"
	"testing"
)

func TestResolveCustomEngineEnv_AllForms(t *testing.T) {
	t.Setenv("CUSTOM_BIN", "/opt/agent")
	t.Setenv("EMPTY_VAR", "")

	cfg := &EvalConfig{
		Engine: EngineConfig{
			Name: "my-agent",
			Custom: &CustomEngineConfig{
				Transport: "local",
				Env: map[string]string{
					"WITH_DEFAULT": "${MISSING_VAR:-fallback}",
					"FROM_ENV":     "${CUSTOM_BIN}",
				},
				Local: &CustomLocalConfig{
					Command: "${CUSTOM_BIN}",
					Args:    []string{"--input", "${input_file}", "--bin=${CUSTOM_BIN}"},
				},
			},
		},
	}

	if err := resolveCustomEngineEnv(cfg); err != nil {
		t.Fatalf("resolveCustomEngineEnv: %v", err)
	}

	custom := cfg.Engine.Custom
	if custom.Local.Command != "/opt/agent" {
		t.Errorf("command = %q, want /opt/agent", custom.Local.Command)
	}
	if custom.Env["WITH_DEFAULT"] != "fallback" {
		t.Errorf("WITH_DEFAULT = %q, want fallback", custom.Env["WITH_DEFAULT"])
	}
	if custom.Env["FROM_ENV"] != "/opt/agent" {
		t.Errorf("FROM_ENV = %q, want /opt/agent", custom.Env["FROM_ENV"])
	}
	// Built-in template variable must be left intact for run-time resolution.
	if custom.Local.Args[1] != "${input_file}" {
		t.Errorf("args[1] = %q, want ${input_file} preserved", custom.Local.Args[1])
	}
	if custom.Local.Args[2] != "--bin=/opt/agent" {
		t.Errorf("args[2] = %q, want --bin=/opt/agent", custom.Local.Args[2])
	}
}

func TestResolveCustomEngineEnv_MissingRequiredVar(t *testing.T) {
	cfg := &EvalConfig{
		Engine: EngineConfig{
			Name: "my-agent",
			Custom: &CustomEngineConfig{
				Transport: "local",
				Local:     &CustomLocalConfig{Command: "${DEFINITELY_MISSING_VAR}"},
			},
		},
	}

	err := resolveCustomEngineEnv(cfg)
	if err == nil {
		t.Fatal("expected error for missing required env var")
	}
	if !strings.Contains(err.Error(), "DEFINITELY_MISSING_VAR") {
		t.Errorf("error = %q, want it to name the missing var", err)
	}
}

func TestResolveCustomEngineEnv_ErrorForm(t *testing.T) {
	cfg := &EvalConfig{
		Engine: EngineConfig{
			Name: "my-agent",
			Custom: &CustomEngineConfig{
				Transport: "local",
				Local:     &CustomLocalConfig{Command: "${MISSING?token is required}"},
			},
		},
	}

	err := resolveCustomEngineEnv(cfg)
	if err == nil || !strings.Contains(err.Error(), "token is required") {
		t.Fatalf("error = %v, want custom error message", err)
	}
}

func TestResolveCustomEngineEnv_NoCustomIsNoop(t *testing.T) {
	cfg := &EvalConfig{Engine: EngineConfig{Name: "claude_code"}}
	if err := resolveCustomEngineEnv(cfg); err != nil {
		t.Fatalf("resolveCustomEngineEnv: %v", err)
	}
}

func TestIsBuiltinTemplateVar(t *testing.T) {
	for _, name := range []string{"workspace", "prompt", "api_key", "input_file", "kwargs", "kwargs.profile"} {
		if !IsBuiltinTemplateVar(name) {
			t.Errorf("IsBuiltinTemplateVar(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"CUSTOM_BIN", "OPENAI_API_KEY", "workspace_dir"} {
		if IsBuiltinTemplateVar(name) {
			t.Errorf("IsBuiltinTemplateVar(%q) = true, want false", name)
		}
	}
}

func validBaseEvalConfig() *EvalConfig {
	return &EvalConfig{
		SchemaVersion: "v1alpha1",
		Environment:   Environment{Type: "none"},
		Engine:        EngineConfig{Name: "claude_code"},
		Cases:         CasesConfig{Files: []string{"cases/a.yaml"}},
	}
}

func TestValidateEvalConfig_NonBuiltinRequiresCustom(t *testing.T) {
	cfg := validBaseEvalConfig()
	cfg.Engine.Name = "my-agent"

	err := NewValidator().ValidateEvalConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), `unsupported agent "my-agent": missing engine.custom`) {
		t.Fatalf("error = %v, want missing engine.custom", err)
	}
}

func TestValidateEvalConfig_CustomTransport(t *testing.T) {
	tests := []struct {
		name      string
		custom    *CustomEngineConfig
		wantError string
	}{
		{
			name:      "missing transport",
			custom:    &CustomEngineConfig{Local: &CustomLocalConfig{Command: "x"}},
			wantError: "engine.custom.transport is required",
		},
		{
			name:      "invalid transport",
			custom:    &CustomEngineConfig{Transport: "grpc"},
			wantError: "engine.custom.transport must be one of",
		},
		{
			name:      "local missing command",
			custom:    &CustomEngineConfig{Transport: "local"},
			wantError: "engine.custom.local.command is required",
		},
		{
			name:      "invalid response_format",
			custom:    &CustomEngineConfig{Transport: "local", Local: &CustomLocalConfig{Command: "x"}, ResponseFormat: "xml"},
			wantError: "engine.custom.response_format must be one of",
		},
		{
			name:      "http missing url",
			custom:    &CustomEngineConfig{Transport: "http"},
			wantError: "engine.custom.http.url is required",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validBaseEvalConfig()
			cfg.Engine.Name = "my-agent"
			cfg.Engine.Custom = tc.custom

			err := NewValidator().ValidateEvalConfig(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("error = %v, want %q", err, tc.wantError)
			}
		})
	}
}

func TestValidateEvalConfig_ValidCustomLocal(t *testing.T) {
	cfg := validBaseEvalConfig()
	cfg.Engine.Name = "my-agent"
	cfg.Engine.Custom = &CustomEngineConfig{
		Transport: "local",
		Local:     &CustomLocalConfig{Command: "/opt/agent"},
	}

	if err := NewValidator().ValidateEvalConfig(cfg); err != nil {
		t.Fatalf("ValidateEvalConfig: %v", err)
	}
}

func TestValidateEvalConfig_BuiltinIgnoresCustom(t *testing.T) {
	cfg := validBaseEvalConfig()
	cfg.Engine.Name = "codex"
	// A built-in engine with a bogus custom block must not be validated.
	cfg.Engine.Custom = &CustomEngineConfig{Transport: "bogus"}

	if err := NewValidator().ValidateEvalConfig(cfg); err != nil {
		t.Fatalf("ValidateEvalConfig: %v", err)
	}
}
