package config

import (
	"os"
	"path/filepath"
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

func TestResolveCustomEngineEnv_BuiltinEngineSkipsResolution(t *testing.T) {
	// A built-in engine ignores engine.custom, so an unresolvable ${VAR}
	// inside an ignored custom block must not fail config loading.
	cfg := &EvalConfig{
		Engine: EngineConfig{
			Name: "codex",
			Custom: &CustomEngineConfig{
				Transport: "local",
				Local:     &CustomLocalConfig{Command: "${DEFINITELY_MISSING_VAR}"},
			},
		},
	}
	if err := resolveCustomEngineEnv(cfg); err != nil {
		t.Fatalf("resolveCustomEngineEnv for built-in engine: %v", err)
	}
}

func TestResolveCustomEngineConfig_ValidatesOverriddenEngine(t *testing.T) {
	// Simulates a --engine override turning a built-in engine (whose custom
	// block was skipped at load) into a custom one with an unrunnable transport.
	cfg := &EvalConfig{
		Engine: EngineConfig{
			Name:   "my-agent",
			Custom: &CustomEngineConfig{Transport: "http", HTTP: &CustomHTTPConfig{URL: "https://x"}},
		},
	}
	err := ResolveCustomEngineConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "http is not yet implemented") {
		t.Fatalf("error = %v, want the http transport to be rejected", err)
	}
}

func TestResolveCustomEngineConfig_ResolvesEnvForOverriddenEngine(t *testing.T) {
	t.Setenv("OVERRIDE_AGENT_BIN", "/opt/override-agent")
	cfg := &EvalConfig{
		Engine: EngineConfig{
			Name:   "my-agent",
			Custom: &CustomEngineConfig{Transport: "local", Local: &CustomLocalConfig{Command: "${OVERRIDE_AGENT_BIN}"}},
		},
	}
	if err := ResolveCustomEngineConfig(cfg); err != nil {
		t.Fatalf("ResolveCustomEngineConfig: %v", err)
	}
	if cfg.Engine.Custom.Local.Command != "/opt/override-agent" {
		t.Fatalf("command = %q, want the env reference resolved", cfg.Engine.Custom.Local.Command)
	}
}

func TestResolveCustomEngineConfig_RejectsSecretEnvInCommand(t *testing.T) {
	t.Setenv("CUSTOM_AGENT_TOKEN", "super-secret")
	cfg := customEngineEvalConfig("my-agent", &CustomEngineConfig{
		Transport: "local",
		Local: &CustomLocalConfig{
			Command: "/opt/agent",
			Args:    []string{"--token", "${CUSTOM_AGENT_TOKEN}"},
		},
	})

	err := ResolveCustomEngineConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "custom.env") {
		t.Fatalf("error = %v, want a secret env reference rejected", err)
	}
}

func TestResolveCustomEngineConfig_AllowsNonSecretEnvInCommand(t *testing.T) {
	t.Setenv("REVIEW_AGENT_BIN", "/opt/review-agent")
	cfg := customEngineEvalConfig("my-agent", &CustomEngineConfig{
		Transport: "local",
		Local:     &CustomLocalConfig{Command: "${REVIEW_AGENT_BIN}"},
	})

	if err := ResolveCustomEngineConfig(cfg); err != nil {
		t.Fatalf("ResolveCustomEngineConfig: %v", err)
	}
	if cfg.Engine.Custom.Local.Command != "/opt/review-agent" {
		t.Fatalf("command = %q, want the non-secret env ref resolved", cfg.Engine.Custom.Local.Command)
	}
}

func TestResolveCustomEngineConfig_RejectsSecretKwargInCommand(t *testing.T) {
	cfg := customEngineEvalConfig("my-agent", &CustomEngineConfig{
		Transport: "local",
		Kwargs:    map[string]string{"token": "super-secret"},
		Local: &CustomLocalConfig{
			Command: "/opt/agent",
			Args:    []string{"--token", "${kwargs.token}"},
		},
	})

	err := ResolveCustomEngineConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "custom.env") {
		t.Fatalf("error = %v, want a secret kwarg reference rejected", err)
	}
}

func TestResolveCustomEngineConfig_RejectsAPIKeyTemplateInCommand(t *testing.T) {
	cfg := customEngineEvalConfig("my-agent", &CustomEngineConfig{
		Transport: "local",
		Local: &CustomLocalConfig{
			Command: "/opt/agent",
			Args:    []string{"--key", "${api_key}"},
		},
	})

	err := ResolveCustomEngineConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "custom.env") {
		t.Fatalf("error = %v, want ${api_key} rejected in a command line", err)
	}
}

func TestResolveCustomEngineConfig_RejectsAggregateKwargsInCommand(t *testing.T) {
	for _, ref := range []string{"${kwargs}", "${kwargs_json}", "${session_input}", "${session_input_json}"} {
		cfg := customEngineEvalConfig("my-agent", &CustomEngineConfig{
			Transport: "local",
			Local: &CustomLocalConfig{
				Command: "/opt/agent",
				Args:    []string{"--config", ref},
			},
		})
		err := ResolveCustomEngineConfig(cfg)
		if err == nil || !strings.Contains(err.Error(), "custom.env") {
			t.Errorf("%s in args: error = %v, want it rejected", ref, err)
		}
	}
}

func TestResolveCustomEngineConfig_RejectsSecretInOutputFile(t *testing.T) {
	cfg := customEngineEvalConfig("my-agent", &CustomEngineConfig{
		Transport: "local",
		Local: &CustomLocalConfig{
			Command:    "/opt/agent",
			OutputFile: "out-${api_key}.json",
		},
	})

	err := ResolveCustomEngineConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "custom.env") {
		t.Fatalf("error = %v, want a secret reference in output_file rejected", err)
	}
}

func TestResolveCustomEngineConfig_RejectsSecretSourcedKwarg(t *testing.T) {
	t.Setenv("CUSTOM_AGENT_TOKEN", "tok")
	// kwargs values are resolved strictly: a secret-named env ref in a kwarg
	// value is rejected, since ${kwargs.<key>} would later leak that value
	// into a logged command line.
	cfg := customEngineEvalConfig("my-agent", &CustomEngineConfig{
		Transport: "local",
		Kwargs:    map[string]string{"profile": "${CUSTOM_AGENT_TOKEN}"},
		Local: &CustomLocalConfig{
			Command: "/opt/agent",
			Args:    []string{"--profile", "${kwargs.profile}"},
		},
	})

	err := ResolveCustomEngineConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "custom.env") {
		t.Fatalf("error = %v, want a secret-sourced kwarg value rejected", err)
	}
}

func TestResolveCustomEngineConfig_AllowsNonSecretKwargInCommand(t *testing.T) {
	cfg := customEngineEvalConfig("my-agent", &CustomEngineConfig{
		Transport: "local",
		Kwargs:    map[string]string{"profile": "strict"},
		Local: &CustomLocalConfig{
			Command: "/opt/agent",
			Args:    []string{"--profile", "${kwargs.profile}"},
		},
	})

	if err := ResolveCustomEngineConfig(cfg); err != nil {
		t.Fatalf("ResolveCustomEngineConfig: %v", err)
	}
}

func TestResolveCustomEngineConfig_RejectsWrappedSecretInCommand(t *testing.T) {
	t.Setenv("CUSTOM_AGENT_TOKEN", "tok")
	t.Setenv("WRAPPER", "${CUSTOM_AGENT_TOKEN}")
	// ${WRAPPER} resolves to a literal "${CUSTOM_AGENT_TOKEN}" string, which
	// would be re-expanded at run time. The strict resolver must reject this.
	cfg := customEngineEvalConfig("my-agent", &CustomEngineConfig{
		Transport: "local",
		Local: &CustomLocalConfig{
			Command: "/opt/agent",
			Args:    []string{"--token", "${WRAPPER}"},
		},
	})

	err := ResolveCustomEngineConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "custom.env") {
		t.Fatalf("error = %v, want a wrapped secret reference rejected", err)
	}
}

func TestResolveCustomEngineConfig_RejectsSecretLikeKwargKeyVariants(t *testing.T) {
	for _, key := range []string{"api-key", "apiKey", "bearerToken", "Authorization"} {
		cfg := customEngineEvalConfig("my-agent", &CustomEngineConfig{
			Transport: "local",
			Kwargs:    map[string]string{key: "literal"},
			Local: &CustomLocalConfig{
				Command: "/opt/agent",
				Args:    []string{"--cred", "${kwargs." + key + "}"},
			},
		})

		err := ResolveCustomEngineConfig(cfg)
		if err == nil || !strings.Contains(err.Error(), "custom.env") {
			t.Errorf("kwargs key %q: error = %v, want it rejected", key, err)
		}
	}
}

func TestIsSensitiveTemplateVar_KwargsKeyVariants(t *testing.T) {
	for _, name := range []string{
		"kwargs.api-key", "kwargs.apiKey", "kwargs.bearerToken",
		"kwargs.api_key", "kwargs.MY_PASSWORD",
		// Dotted and other non-alphanumeric separators also count.
		"kwargs.api.key", "kwargs.bearer token", "kwargs.access/key",
	} {
		if !isSensitiveTemplateVar(name) {
			t.Errorf("isSensitiveTemplateVar(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"kwargs.profile", "kwargs.maxFiles", "kwargs.report_format"} {
		if isSensitiveTemplateVar(name) {
			t.Errorf("isSensitiveTemplateVar(%q) = true, want false", name)
		}
	}
}

func TestResolveCustomEngineConfig_SkipsInactiveTransportBlock(t *testing.T) {
	// transport: local — a stale ${MISSING_VAR} in an unused custom.http
	// block must not block an otherwise runnable local config.
	cfg := customEngineEvalConfig("my-agent", &CustomEngineConfig{
		Transport: "local",
		Local:     &CustomLocalConfig{Command: "/opt/agent"},
		HTTP:      &CustomHTTPConfig{URL: "${DEFINITELY_MISSING_VAR}"},
	})

	if err := ResolveCustomEngineConfig(cfg); err != nil {
		t.Fatalf("inactive http block must not fail local-transport resolution: %v", err)
	}
}

func TestIsSensitiveEnvName(t *testing.T) {
	for _, name := range []string{"CUSTOM_AGENT_TOKEN", "OPENAI_API_KEY", "MY_SECRET", "DB_PASSWORD", "GH_ACCESS_KEY"} {
		if !isSensitiveEnvName(name) {
			t.Errorf("isSensitiveEnvName(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"REVIEW_AGENT_BIN", "MONKEY_PATH", "WORKSPACE_DIR", "AGENT_ENDPOINT"} {
		if isSensitiveEnvName(name) {
			t.Errorf("isSensitiveEnvName(%q) = true, want false", name)
		}
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

func customEngineEvalConfig(name string, custom *CustomEngineConfig) *EvalConfig {
	return &EvalConfig{
		SchemaVersion: "v1alpha1",
		Environment:   Environment{Type: "none"},
		Engine:        EngineConfig{Name: name, Custom: custom},
		Cases:         CasesConfig{Files: []string{"cases/a.yaml"}},
	}
}

func TestResolveCustomEngineConfig_NonBuiltinRequiresCustom(t *testing.T) {
	cfg := customEngineEvalConfig("my-agent", nil)

	err := ResolveCustomEngineConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), `unsupported agent "my-agent": missing engine.custom`) {
		t.Fatalf("error = %v, want missing engine.custom", err)
	}
}

func TestResolveCustomEngineConfig_CustomTransport(t *testing.T) {
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
			wantError: `engine.custom.transport must be "local"`,
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
			name:      "http not yet implemented",
			custom:    &CustomEngineConfig{Transport: "http", HTTP: &CustomHTTPConfig{URL: "https://x"}},
			wantError: "http is not yet implemented",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := customEngineEvalConfig("my-agent", tc.custom)

			err := ResolveCustomEngineConfig(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("error = %v, want %q", err, tc.wantError)
			}
		})
	}
}

func TestResolveCustomEngineConfig_ValidCustomLocal(t *testing.T) {
	cfg := customEngineEvalConfig("my-agent", &CustomEngineConfig{
		Transport: "local",
		Local:     &CustomLocalConfig{Command: "/opt/agent"},
	})

	if err := ResolveCustomEngineConfig(cfg); err != nil {
		t.Fatalf("ResolveCustomEngineConfig: %v", err)
	}
}

func TestResolveCustomEngineConfig_BuiltinIgnoresCustom(t *testing.T) {
	// A built-in engine ignores engine.custom entirely: neither a bogus
	// transport nor an unresolvable ${VAR} (which a --engine override to a
	// built-in engine may leave behind) must cause an error.
	cfg := customEngineEvalConfig("codex", &CustomEngineConfig{
		Transport: "bogus",
		Local:     &CustomLocalConfig{Command: "${DEFINITELY_MISSING_VAR}"},
	})

	if err := ResolveCustomEngineConfig(cfg); err != nil {
		t.Fatalf("ResolveCustomEngineConfig: %v", err)
	}
}

func TestResolveCustomEngineConfig_ResolvesModelEnv(t *testing.T) {
	t.Setenv("CUSTOM_MODEL_NAME", "gpt-4.2")
	cfg := customEngineEvalConfig("my-agent", &CustomEngineConfig{
		Transport: "local",
		Local:     &CustomLocalConfig{Command: "/opt/agent"},
	})
	cfg.Engine.Model = ModelConfig{
		Provider: "${CUSTOM_MODEL_PROVIDER:-openai}",
		Name:     "${CUSTOM_MODEL_NAME}",
	}

	if err := ResolveCustomEngineConfig(cfg); err != nil {
		t.Fatalf("ResolveCustomEngineConfig: %v", err)
	}
	if cfg.Engine.Model.Name != "gpt-4.2" {
		t.Errorf("model.name = %q, want gpt-4.2", cfg.Engine.Model.Name)
	}
	if cfg.Engine.Model.Provider != "openai" {
		t.Errorf("model.provider = %q, want openai", cfg.Engine.Model.Provider)
	}
}

func TestLoadEvalConfig_DefersCustomEngineEnv(t *testing.T) {
	// The loader must not abort on an unresolvable custom env reference;
	// resolution is deferred until the final engine name is known.
	dir := t.TempDir()
	path := filepath.Join(dir, "eval.yaml")
	const evalYAML = `schema_version: v1alpha1
environment:
  type: none
engine:
  name: my-agent
  custom:
    transport: local
    local:
      command: ${DEFINITELY_MISSING_VAR}
cases:
  files:
    - cases/a.yaml
`
	if err := os.WriteFile(path, []byte(evalYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLoader(path).LoadEvalConfig(); err != nil {
		t.Fatalf("LoadEvalConfig must not fail on a deferred custom env ref: %v", err)
	}
}
