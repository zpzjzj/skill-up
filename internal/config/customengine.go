package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"unicode"
)

// sensitiveEnvNamePattern matches environment variable names that look like
// credentials. Such values must not be rendered into a command line (where
// runtimes record them on exec spans and in failure logs); they belong in
// engine.custom.env. Word boundaries avoid false positives like MONKEY_PATH.
var sensitiveEnvNamePattern = regexp.MustCompile(
	`(?i)(^|_)(API_?KEY|ACCESS_?KEY|KEY|TOKEN|SECRET|PASSWORD|PASSWD|CREDENTIALS?|AUTHORIZATION)(_|$)`,
)

func isSensitiveEnvName(name string) bool {
	return sensitiveEnvNamePattern.MatchString(name)
}

// isSensitiveTemplateVar reports whether a built-in template variable may carry
// a credential and so must not be rendered into a command line: ${api_key}; a
// ${kwargs.<key>} whose key name looks secret-like; or an aggregate variable
// (${kwargs}, ${kwargs_json}, ${session_input}, ${session_input_json}) that
// embeds the whole kwargs map and could therefore contain secret-like keys.
func isSensitiveTemplateVar(name string) bool {
	switch name {
	case "api_key", "kwargs", "kwargs_json", "session_input", "session_input_json":
		return true
	}
	if key, ok := strings.CutPrefix(name, "kwargs."); ok {
		// kwarg keys may use hyphens or camelCase (e.g. "api-key", "apiKey",
		// "bearerToken"); normalize before the underscore-bounded check.
		return isSensitiveEnvName(normalizeKeyForSensitiveCheck(key))
	}
	return false
}

// normalizeKeyForSensitiveCheck converts a kwarg key into UPPER_SNAKE_CASE so
// the sensitive-name pattern recognizes alternative naming conventions —
// hyphenated ("api-key"), dotted ("api.key"), camelCase ("apiKey",
// "bearerToken"), and other non-alphanumeric separators.
func normalizeKeyForSensitiveCheck(key string) string {
	var sep strings.Builder
	sep.Grow(len(key))
	for _, r := range key {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			sep.WriteRune(r)
		} else {
			sep.WriteByte('_')
		}
	}
	normalized := sep.String()
	var b strings.Builder
	b.Grow(len(normalized) + 4)
	prevLower := false
	for _, r := range normalized {
		if prevLower && unicode.IsUpper(r) {
			b.WriteByte('_')
		}
		b.WriteRune(r)
		prevLower = unicode.IsLower(r)
	}
	return strings.ToUpper(b.String())
}

// builtinTemplateVars is the set of run-time template variable names provided
// by skill-up. References to these are left intact during config-time env
// resolution and resolved later when a custom engine runs a case.
var builtinTemplateVars = map[string]struct{}{
	"workspace":          {},
	"prompt":             {},
	"messages":           {},
	"messages_json":      {},
	"session_input":      {},
	"session_input_json": {},
	"input_file":         {},
	"output_file":        {},
	"model":              {},
	"model_provider":     {},
	"model_name":         {},
	"api_key":            {},
	"case_id":            {},
	"variant":            {},
	"max_turns":          {},
	"timeout_seconds":    {},
	"kwargs":             {},
	"kwargs_json":        {},
}

// IsBuiltinTemplateVar reports whether name is a skill-up-provided template
// variable (including any kwargs.<key> reference). Such names are resolved at
// run time, not at config-load time.
func IsBuiltinTemplateVar(name string) bool {
	if strings.HasPrefix(name, "kwargs.") {
		return true
	}
	_, ok := builtinTemplateVars[name]
	return ok
}

// ResolveCustomEngineConfig runs environment-variable resolution and engine
// validation for the current engine.Name. The loader intentionally defers
// this: the final engine name is only known after CLI overrides (--engine),
// so callers invoke this once that name is settled. It is a no-op for built-in
// engines, which ignore any engine.custom block.
func ResolveCustomEngineConfig(cfg *EvalConfig) error {
	if err := resolveCustomEngineEnv(cfg); err != nil {
		return err
	}
	if errs := validateEngine(cfg.Engine); len(errs) > 0 {
		return fmt.Errorf("validation errors:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// resolveCustomEngineEnv resolves ${VAR} environment-variable references inside
// the custom engine config tree. It is a no-op when engine.custom is absent or
// when engine.name is a built-in agent (which ignores engine.custom entirely,
// matching validateEngine). Built-in template variables are left intact for
// run-time resolution.
func resolveCustomEngineEnv(cfg *EvalConfig) error {
	custom := cfg.Engine.Custom
	if custom == nil || IsBuiltinEngineName(cfg.Engine.Name) {
		return nil
	}

	var errs []string
	errs = append(errs, resolveScalarEnv("transport", &custom.Transport, false)...)
	errs = append(errs, resolveScalarEnv("response_format", &custom.ResponseFormat, false)...)
	errs = append(errs, resolveStringMapEnv("env", custom.Env, false)...)
	// kwargs values can be expanded into a command line via ${kwargs.<key>},
	// and per the design kwargs are not for sensitive values; resolve strictly.
	errs = append(errs, resolveStringMapEnv("kwargs", custom.Kwargs, true)...)
	// Only the active transport block is resolved, so stale ${VAR} refs in an
	// inactive block (e.g. a leftover custom.http while transport: local) do
	// not fail an otherwise runnable config.
	switch custom.Transport {
	case customTransportLocal:
		if custom.Local != nil {
			errs = append(errs, resolveLocalEnv(custom.Local)...)
		}
	case customTransportHTTP:
		if custom.HTTP != nil {
			errs = append(errs, resolveHTTPEnv(custom.HTTP)...)
		}
	}
	errs = append(errs, resolveModelEnv(&cfg.Engine.Model)...)

	return aggregateConfigErrors(errs)
}

// resolveScalarEnv resolves a single string field. When strict, it additionally
// rejects secret-like references (for fields that become a command line).
func resolveScalarEnv(field string, target *string, strict bool) []string {
	resolveFn := resolveEnvRefs
	if strict {
		resolveFn = resolveEnvRefsStrict
	}
	v, err := resolveFn(*target)
	if err != nil {
		return []string{fmt.Sprintf("engine.custom.%s: %s", field, err)}
	}
	*target = v
	return nil
}

// resolveStringMapEnv resolves every value of a string map in place. When
// strict, it additionally rejects secret-like references in the values (used
// for kwargs, whose entries can be expanded into command lines via
// ${kwargs.<key>}; per the design, kwargs are not for sensitive values).
func resolveStringMapEnv(field string, m map[string]string, strict bool) []string {
	resolveFn := resolveEnvRefs
	if strict {
		resolveFn = resolveEnvRefsStrict
	}
	var errs []string
	for k, v := range m {
		rv, err := resolveFn(v)
		if err != nil {
			errs = append(errs, fmt.Sprintf("engine.custom.%s.%s: %s", field, k, err))
			continue
		}
		m[k] = rv
	}
	return errs
}

// resolveLocalEnv resolves the engine.custom.local fields. command, args, cwd
// and the I/O paths all become (or are logged as part of) a command line, so
// they reject secret-like references.
func resolveLocalEnv(l *CustomLocalConfig) []string {
	var errs []string
	errs = append(errs, resolveScalarEnv("local.command", &l.Command, true)...)
	errs = append(errs, resolveScalarEnv("local.cwd", &l.Cwd, true)...)
	errs = append(errs, resolveScalarEnv("local.input_file", &l.InputFile, true)...)
	errs = append(errs, resolveScalarEnv("local.output_file", &l.OutputFile, true)...)
	for i := range l.Args {
		rv, err := resolveEnvRefsStrict(l.Args[i])
		if err != nil {
			errs = append(errs, fmt.Sprintf("engine.custom.local.args[%d]: %s", i, err))
			continue
		}
		l.Args[i] = rv
	}
	return errs
}

// resolveHTTPEnv resolves the engine.custom.http fields.
func resolveHTTPEnv(h *CustomHTTPConfig) []string {
	var errs []string
	errs = append(errs, resolveScalarEnv("http.url", &h.URL, false)...)
	errs = append(errs, resolveScalarEnv("http.method", &h.Method, false)...)
	errs = append(errs, resolveStringMapEnv("http.headers", h.Headers, false)...)
	for i := range h.Files {
		rv, err := resolveEnvRefs(h.Files[i].Path)
		if err != nil {
			errs = append(errs, fmt.Sprintf("engine.custom.http.files[%d].path: %s", i, err))
			continue
		}
		h.Files[i].Path = rv
	}
	if rb, err := resolveEnvRefsInAny(h.RequestBody); err != nil {
		errs = append(errs, fmt.Sprintf("engine.custom.http.request_body: %s", err))
	} else if m, ok := rb.(map[string]any); ok {
		h.RequestBody = m
	}
	return errs
}

// resolveModelEnv resolves env references in engine.model string values.
func resolveModelEnv(model *ModelConfig) []string {
	var errs []string
	if v, err := resolveEnvRefs(model.Provider); err != nil {
		errs = append(errs, fmt.Sprintf("engine.model.provider: %s", err))
	} else {
		model.Provider = v
	}
	if v, err := resolveEnvRefs(model.Name); err != nil {
		errs = append(errs, fmt.Sprintf("engine.model.name: %s", err))
	} else {
		model.Name = v
	}
	if v, err := resolveEnvRefs(model.BaseURL); err != nil {
		errs = append(errs, fmt.Sprintf("engine.model.base_url: %s", err))
	} else {
		model.BaseURL = v
	}
	for k, v := range model.Params {
		rv, err := resolveEnvRefs(v)
		if err != nil {
			errs = append(errs, fmt.Sprintf("engine.model.params.%s: %s", k, err))
			continue
		}
		model.Params[k] = rv
	}
	return errs
}

func aggregateConfigErrors(errs []string) error {
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("custom engine config errors:\n  - %s", strings.Join(errs, "\n  - "))
}

// resolveEnvRefsInAny resolves env references inside an arbitrary YAML value,
// recursing through maps and slices and resolving string leaves.
func resolveEnvRefsInAny(v any) (any, error) {
	switch t := v.(type) {
	case string:
		return resolveEnvRefs(t)
	case map[string]any:
		for k, val := range t {
			rv, err := resolveEnvRefsInAny(val)
			if err != nil {
				return nil, err
			}
			t[k] = rv
		}
		return t, nil
	case []any:
		for i, val := range t {
			rv, err := resolveEnvRefsInAny(val)
			if err != nil {
				return nil, err
			}
			t[i] = rv
		}
		return t, nil
	default:
		return v, nil
	}
}

// resolveEnvRefs replaces ${VAR}, ${VAR:-default} and ${VAR?message} env
// references in s. References to built-in template variables are left intact.
func resolveEnvRefs(s string) (string, error) {
	return resolveEnvRefsWith(s, false)
}

// resolveEnvRefsStrict behaves like resolveEnvRefs but rejects references to
// secret-like environment variables. It is used for fields that become a
// command line (local.command / local.args), keeping credentials out of
// process listings and exec traces.
//
// The resolver iterates: if the produced value itself embeds further ${...}
// references (a wrapper env var whose value is "${CUSTOM_AGENT_TOKEN}"), the
// next pass re-checks them, so a non-sensitive wrapper cannot smuggle a
// sensitive name through to run-time rendering. Iteration stops at a fixed
// point or at maxStrictExpansionDepth to bound pathological cycles.
func resolveEnvRefsStrict(s string) (string, error) {
	const maxStrictExpansionDepth = 10
	for range maxStrictExpansionDepth {
		resolved, err := resolveEnvRefsWith(s, true)
		if err != nil {
			return "", err
		}
		if resolved == s || !strings.Contains(resolved, "${") {
			return resolved, nil
		}
		s = resolved
	}
	return "", errors.New("strict env resolution exceeded maximum depth (possible reference cycle)")
}

func resolveEnvRefsWith(s string, rejectSecrets bool) (string, error) {
	if s == "" || !strings.Contains(s, "${") {
		return s, nil
	}

	var b strings.Builder
	for i := 0; i < len(s); {
		open := strings.Index(s[i:], "${")
		if open < 0 {
			b.WriteString(s[i:])
			break
		}
		b.WriteString(s[i : i+open])
		start := i + open
		closeIdx := strings.IndexByte(s[start:], '}')
		if closeIdx < 0 {
			// Unterminated reference: keep the remainder verbatim.
			b.WriteString(s[start:])
			break
		}
		inner := s[start+2 : start+closeIdx]
		value, leaveIntact, err := resolveEnvToken(inner, rejectSecrets)
		if err != nil {
			return "", err
		}
		if leaveIntact {
			b.WriteString(s[start : start+closeIdx+1])
		} else {
			b.WriteString(value)
		}
		i = start + closeIdx + 1
	}
	return b.String(), nil
}

func resolveEnvToken(inner string, rejectSecrets bool) (value string, leaveIntact bool, err error) {
	name := inner
	var defaultVal, errMsg string
	hasDefault, hasErrForm := false, false
	if before, after, found := strings.Cut(inner, ":-"); found {
		name, defaultVal, hasDefault = before, after, true
	} else if before, after, found := strings.Cut(inner, "?"); found {
		name, errMsg, hasErrForm = before, after, true
	}

	if IsBuiltinTemplateVar(name) {
		if rejectSecrets && isSensitiveTemplateVar(name) {
			return "", false, fmt.Errorf(
				"secret-like template variable ${%s} must not be referenced in a command line; pass credentials via engine.custom.env instead",
				name,
			)
		}
		return "", true, nil
	}

	if rejectSecrets && isSensitiveEnvName(name) {
		return "", false, fmt.Errorf(
			"secret-like environment variable %q must not be referenced in a command line; pass credentials via engine.custom.env instead",
			name,
		)
	}

	if v := os.Getenv(name); v != "" {
		return v, false, nil
	}
	if hasDefault {
		return defaultVal, false, nil
	}
	if hasErrForm {
		if strings.TrimSpace(errMsg) == "" {
			errMsg = name + " is required"
		}
		return "", false, errors.New(errMsg)
	}
	return "", false, fmt.Errorf("environment variable %s is required but not set", name)
}
