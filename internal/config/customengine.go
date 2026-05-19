package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// builtinTemplateVars is the set of run-time template variable names provided
// by skill-eval. References to these are left intact during config-time env
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

// IsBuiltinTemplateVar reports whether name is a skill-eval-provided template
// variable (including any kwargs.<key> reference). Such names are resolved at
// run time, not at config-load time.
func IsBuiltinTemplateVar(name string) bool {
	if strings.HasPrefix(name, "kwargs.") {
		return true
	}
	_, ok := builtinTemplateVars[name]
	return ok
}

// resolveCustomEngineEnv resolves ${VAR} environment-variable references inside
// the custom engine config tree. It is a no-op when engine.custom is absent.
// Built-in template variables are left intact for run-time resolution.
func resolveCustomEngineEnv(cfg *EvalConfig) error {
	custom := cfg.Engine.Custom
	if custom == nil {
		return nil
	}

	var errs []string
	resolve := func(field string, target *string) {
		v, err := resolveEnvRefs(*target)
		if err != nil {
			errs = append(errs, fmt.Sprintf("engine.custom.%s: %s", field, err))
			return
		}
		*target = v
	}
	resolveMap := func(field string, m map[string]string) {
		for k, v := range m {
			rv, err := resolveEnvRefs(v)
			if err != nil {
				errs = append(errs, fmt.Sprintf("engine.custom.%s.%s: %s", field, k, err))
				continue
			}
			m[k] = rv
		}
	}

	resolve("transport", &custom.Transport)
	resolve("response_format", &custom.ResponseFormat)
	resolveMap("env", custom.Env)
	resolveMap("kwargs", custom.Kwargs)

	if custom.Local != nil {
		l := custom.Local
		resolve("local.command", &l.Command)
		resolve("local.cwd", &l.Cwd)
		resolve("local.input_file", &l.InputFile)
		resolve("local.output_file", &l.OutputFile)
		for i := range l.Args {
			rv, err := resolveEnvRefs(l.Args[i])
			if err != nil {
				errs = append(errs, fmt.Sprintf("engine.custom.local.args[%d]: %s", i, err))
				continue
			}
			l.Args[i] = rv
		}
	}

	if custom.HTTP != nil {
		h := custom.HTTP
		resolve("http.url", &h.URL)
		resolve("http.method", &h.Method)
		resolveMap("http.headers", h.Headers)
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
	}

	errs = append(errs, resolveModelEnv(&cfg.Engine.Model)...)

	return aggregateConfigErrors(errs)
}

// resolveModelEnv resolves env references in engine.model string values.
func resolveModelEnv(model *ModelConfig) []string {
	var errs []string
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
		value, leaveIntact, err := resolveEnvToken(inner)
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

func resolveEnvToken(inner string) (value string, leaveIntact bool, err error) {
	name := inner
	var defaultVal, errMsg string
	hasDefault, hasErrForm := false, false
	if before, after, found := strings.Cut(inner, ":-"); found {
		name, defaultVal, hasDefault = before, after, true
	} else if before, after, found := strings.Cut(inner, "?"); found {
		name, errMsg, hasErrForm = before, after, true
	}

	if IsBuiltinTemplateVar(name) {
		return "", true, nil
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
