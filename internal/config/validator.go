package config

import (
	"fmt"
	"net/http"
	"strings"
)

// Judge type constants.
const (
	judgeTypeRuleBased    = "rule_based"
	judgeTypeScript       = "script"
	judgeTypeAgentJudge   = "agent_judge"
	maxRetryPolicyRetries = 10
)

// Runtime type constants.
const (
	runtimeTypeNone        = "none"
	runtimeTypeOpenSandbox = "opensandbox"
)

// Custom engine transport constants.
const (
	customTransportLocal = "local"
	customTransportHTTP  = "http"
)

// builtinEngineNames mirrors the built-in agents recognized by the agent
// factory (internal/agent/factory.go). It is duplicated here — rather than
// imported — to avoid a config -> agent import cycle (agent imports config for
// CustomEngineConfig). Keep the two lists in sync when adding a built-in agent.
var builtinEngineNames = map[string]struct{}{
	"claude_code": {},
	"claude-code": {},
	"codex":       {},
	"qodercli":    {},
	"qoder":       {},
	"qoder-cli":   {},
}

func isBuiltinEngineName(name string) bool {
	_, ok := builtinEngineNames[name]
	return ok
}

// Validator checks eval and case documents against the v1alpha1 schema.
type Validator struct{}

// NewValidator creates a new Validator.
func NewValidator() *Validator {
	return &Validator{}
}

// ValidateEvalConfig validates an eval configuration.
func (v *Validator) ValidateEvalConfig(cfg *EvalConfig) error {
	var errs []string

	// schema_version is required and must be v1alpha1
	if cfg.SchemaVersion == "" {
		errs = append(errs, "schema_version is required")
	} else if cfg.SchemaVersion != "v1alpha1" {
		errs = append(errs, fmt.Sprintf("schema_version must be 'v1alpha1', got '%s'", cfg.SchemaVersion))
	}

	// environment.type is required
	if cfg.Environment.Type == "" {
		errs = append(errs, "environment.type is required (none, opensandbox)")
	} else if !isValidRuntimeType(cfg.Environment.Type) {
		errs = append(errs, "environment.type must be one of: none, opensandbox")
	}

	errs = append(errs, validateNetworkPolicy(cfg.Environment)...)

	// engine.name is required
	if cfg.Engine.Name == "" {
		errs = append(errs, "engine.name is required")
	}

	errs = append(errs, validateEngine(cfg.Engine)...)

	// engine.model.provider and engine.model.name are optional.
	// When omitted, the engine uses its local default model configuration.

	// cases.files is required and must have at least one file
	if len(cfg.Cases.Files) == 0 {
		errs = append(errs, "cases.files must contain at least one case file")
	}
	if cfg.Cases.RetryPolicy.MaxRetries > maxRetryPolicyRetries {
		errs = append(errs, fmt.Sprintf("cases.retry_policy.max_retries must be <= %d", maxRetryPolicyRetries))
	}

	if len(errs) > 0 {
		return fmt.Errorf("validation errors:\n  - %s", strings.Join(errs, "\n  - "))
	}

	return nil
}

// ValidateCaseConfig validates a case configuration.
func (v *Validator) ValidateCaseConfig(cfg *CaseConfig) error {
	var errs []string

	// id is optional - Loader auto-generates from filename if not specified
	// See loader.go:LoadCaseConfig for the fallback logic

	// input.prompt or input.turns is required
	if cfg.Input.Prompt == "" && len(cfg.Input.Turns) == 0 {
		errs = append(errs, "input.prompt or input.turns is required")
	}

	// if turns is specified, each turn must have role and content
	for i, turn := range cfg.Input.Turns {
		if turn.Role == "" {
			errs = append(errs, fmt.Sprintf("input.turns[%d].role is required", i))
		}
		if turn.Content == "" {
			errs = append(errs, fmt.Sprintf("input.turns[%d].content is required", i))
		}
	}

	// validate judge config if specified at case level
	if cfg.Judge.Type != "" {
		errs = append(errs, validateJudgeTypeAndFields(cfg.Judge)...)
	}

	if len(errs) > 0 {
		return fmt.Errorf("validation errors:\n  - %s", strings.Join(errs, "\n  - "))
	}

	return nil
}

// validateJudgeTypeAndFields validates judge type and its conditional fields.
func validateJudgeTypeAndFields(judge JudgeConfig) []string {
	var errs []string

	if judge.Type != "" && !isValidJudgeType(judge.Type) {
		errs = append(errs, "judge.type must be one of: rule_based, script, agent_judge")
	}

	// script type requires script_path
	if judge.Type == judgeTypeScript && judge.ScriptPath == "" {
		errs = append(errs, "judge.script_path is required when judge.type is script")
	}

	// agent_judge type requires model and criteria
	if judge.Type == judgeTypeAgentJudge && judge.Model == "" {
		errs = append(errs, "judge.model is required when judge.type is agent_judge")
	}

	if judge.Type == judgeTypeAgentJudge && len(judge.Criteria) == 0 {
		errs = append(errs, "judge.criteria is required when judge.type is agent_judge")
	}

	errs = append(errs, validatePassThreshold(judge.PassThreshold)...)

	if judge.TimeoutSeconds != nil && *judge.TimeoutSeconds < 0 {
		errs = append(errs, "judge.timeout_seconds must be non-negative")
	}

	return errs
}

func validatePassThreshold(threshold *float64) []string {
	if threshold == nil {
		return nil
	}
	if *threshold < 0.0 || *threshold > 1.0 {
		return []string{"judge.pass_threshold must be between 0.0 and 1.0"}
	}
	return nil
}

// ValidateAll validates an eval config and all its cases.
func (v *Validator) ValidateAll(result *EvalResult) error {
	if err := v.ValidateEvalConfig(result.Eval); err != nil {
		return err
	}

	for _, c := range result.Cases {
		if err := v.ValidateCaseConfig(c); err != nil {
			return fmt.Errorf("case %s: %w", c.ID, err)
		}
	}

	return nil
}

// validateEngine checks engine.custom against the Custom Engine contract.
// A non-built-in engine.name requires an engine.custom block; a built-in
// engine.name ignores engine.custom entirely.
func validateEngine(engine EngineConfig) []string {
	if engine.Name == "" {
		return nil
	}
	if isBuiltinEngineName(engine.Name) {
		return nil
	}
	if engine.Custom == nil {
		return []string{fmt.Sprintf("unsupported agent %q: missing engine.custom", engine.Name)}
	}
	return validateCustomEngine(engine.Custom)
}

func validateCustomEngine(custom *CustomEngineConfig) []string {
	var errs []string

	switch custom.Transport {
	case "":
		errs = append(errs, "engine.custom.transport is required (local, http)")
	case customTransportLocal, customTransportHTTP:
	default:
		errs = append(errs, fmt.Sprintf("engine.custom.transport must be one of: local, http (got %q)", custom.Transport))
	}

	if custom.ResponseFormat != "" &&
		custom.ResponseFormat != "session_result" && custom.ResponseFormat != "text" {
		errs = append(errs, fmt.Sprintf("engine.custom.response_format must be one of: session_result, text (got %q)", custom.ResponseFormat))
	}

	if custom.TimeoutSeconds < 0 {
		errs = append(errs, "engine.custom.timeout_seconds must be non-negative")
	}

	return append(errs, validateCustomTransportFields(custom)...)
}

// validateCustomTransportFields validates the transport-specific required fields.
func validateCustomTransportFields(custom *CustomEngineConfig) []string {
	switch custom.Transport {
	case customTransportLocal:
		if custom.Local == nil || custom.Local.Command == "" {
			return []string{"engine.custom.local.command is required when transport is local"}
		}
	case customTransportHTTP:
		if custom.HTTP == nil || custom.HTTP.URL == "" {
			return []string{"engine.custom.http.url is required when transport is http"}
		}
		if custom.HTTP.Method != "" && custom.HTTP.Method != http.MethodPost {
			return []string{fmt.Sprintf("engine.custom.http.method must be POST (got %q)", custom.HTTP.Method)}
		}
	}
	return nil
}

func isValidRuntimeType(t string) bool {
	return t == runtimeTypeNone || t == runtimeTypeOpenSandbox
}

func isValidJudgeType(t string) bool {
	return t == judgeTypeRuleBased || t == judgeTypeScript || t == judgeTypeAgentJudge
}

func validateNetworkPolicy(env Environment) []string {
	policy := env.NetworkPolicy
	if policy == "" {
		if len(env.AllowedEgress) > 0 {
			return []string{"allowed_egress requires network_policy: allow_declared"}
		}
		return nil
	}
	if policy != "deny_all" && policy != "allow_declared" {
		return []string{"network_policy must be one of: deny_all, allow_declared"}
	}
	if env.Type == runtimeTypeNone {
		return []string{"network_policy requires environment.type opensandbox (none cannot enforce network isolation)"}
	}

	var errs []string
	switch policy {
	case "allow_declared":
		if len(env.AllowedEgress) == 0 {
			errs = append(errs, "network_policy: allow_declared requires a non-empty allowed_egress list")
		}
		for i, target := range env.AllowedEgress {
			t := strings.TrimSpace(target)
			if t == "" {
				errs = append(errs, fmt.Sprintf("allowed_egress[%d] must not be empty", i))
				continue
			}
			if strings.ContainsAny(t, "/ ") {
				errs = append(errs, fmt.Sprintf("allowed_egress[%d] %q must be a bare FQDN or wildcard domain, not a URL", i, target))
			}
		}
	case "deny_all":
		if len(env.AllowedEgress) > 0 {
			errs = append(errs, "allowed_egress is only valid with network_policy: allow_declared")
		}
	}
	return errs
}
