package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/alibaba/skill-up/internal/config"
	"github.com/alibaba/skill-up/internal/logging"
	"github.com/alibaba/skill-up/pkg/transcript"
)

const (
	customTransportLocal       = "local"
	customTransportHTTP        = "http"
	customResponseSessionJSON  = "session_result"
	customResponseText         = "text"
	customDefaultInputFile     = "inputs/messages.json"
	customDefaultOutputFile    = "outputs/session-result.json"
	customSessionResultMissing = "custom engine returned a result without exit_code"
)

// CustomAgent implements Agent for user-defined engines configured via
// engine.custom. Phase 1 supports the local transport; the http transport is
// designed but not yet implemented. See docs/design/custom-engine.md.
type CustomAgent struct {
	CLIAgent
}

// NewCustomAgent creates a CustomAgent from a resolved agent Config. The
// caller must populate cfg.Custom; factory dispatch guarantees this.
func NewCustomAgent(cfg Config) *CustomAgent {
	return &CustomAgent{CLIAgent: CLIAgent{BaseAgent: NewBaseAgent(cfg)}}
}

// Install is a no-op: a custom engine's command is provided and managed by the
// user, not installed by skill-eval.
func (a *CustomAgent) Install(_ context.Context, _ Runtime) error { return nil }

// Check is a no-op for custom engines.
func (a *CustomAgent) Check(_ context.Context, _ Runtime) error { return nil }

// CheckCredentials is a no-op: a custom engine references credentials
// explicitly via ${api_key}, so skill-eval does not pre-validate them.
func (a *CustomAgent) CheckCredentials(_ context.Context) error { return nil }

// Run executes the custom engine for a single case.
func (a *CustomAgent) Run(ctx context.Context, rt Runtime, opts ExecOptions, messages []transcript.Message) (*SessionResult, error) {
	custom := a.Cfg.Custom
	if custom == nil {
		return a.errorResult(0), errors.New("custom engine config is missing")
	}
	switch custom.Transport {
	case customTransportLocal:
		return a.runLocal(ctx, rt, opts, messages, custom)
	case customTransportHTTP:
		return a.errorResult(0), errors.New("custom engine http transport is not yet implemented")
	default:
		return a.errorResult(0), fmt.Errorf("custom engine transport %q is not supported", custom.Transport)
	}
}

func (a *CustomAgent) runLocal(ctx context.Context, rt Runtime, opts ExecOptions, messages []transcript.Message, custom *config.CustomEngineConfig) (*SessionResult, error) {
	start := time.Now()
	timeoutSec := custom.TimeoutSeconds
	if timeoutSec <= 0 {
		timeoutSec = opts.TimeoutSec
	}

	sess := a.buildSessionInput(rt, opts, messages, custom, timeoutSec)
	vars, err := a.buildTemplateVars(rt, opts, messages, custom, sess, timeoutSec)
	if err != nil {
		return a.errorResult(0), err
	}

	inputFile, outputFile, err := resolveCustomIOFiles(rt, custom, vars)
	if err != nil {
		return a.errorResult(0), err
	}
	vars["input_file"], vars["output_file"] = inputFile, outputFile

	sessJSON, err := json.Marshal(sess)
	if err != nil {
		return a.errorResult(0), fmt.Errorf("marshal session input: %w", err)
	}
	if err := persistRuntimeArtifact(ctx, rt, inputFile, string(sessJSON)); err != nil {
		return a.errorResult(0), fmt.Errorf("write session input: %w", err)
	}

	cmd, execOpts, err := a.buildLocalExec(ctx, rt, opts, custom, vars, timeoutSec)
	if err != nil {
		return a.errorResult(0), err
	}

	// Enforce the custom timeout via a context deadline: some runtimes
	// (e.g. NoneRuntime) do not honor ExecOptions.TimeoutSec directly.
	execCtx := ctx
	if timeoutSec > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
		defer cancel()
	}

	result, execErr := rt.Exec(execCtx, cmd, execOpts)
	return a.finishLocal(ctx, rt, opts, custom, result, execErr, outputFile, start)
}

// buildLocalExec renders the command, args and exec options for the local run.
func (a *CustomAgent) buildLocalExec(ctx context.Context, rt Runtime, opts ExecOptions, custom *config.CustomEngineConfig, vars map[string]string, timeoutSec int) (string, ExecOptions, error) {
	local := custom.Local
	command, err := renderTemplate(local.Command, vars)
	if err != nil {
		return "", ExecOptions{}, fmt.Errorf("render local.command: %w", err)
	}
	parts := []string{shellQuote(command)}
	for i, raw := range local.Args {
		arg, rErr := renderTemplate(raw, vars)
		if rErr != nil {
			return "", ExecOptions{}, fmt.Errorf("render local.args[%d]: %w", i, rErr)
		}
		parts = append(parts, shellQuote(arg))
	}

	cwd := rt.Workspace()
	if local.Cwd != "" {
		if cwd, err = renderTemplate(local.Cwd, vars); err != nil {
			return "", ExecOptions{}, fmt.Errorf("render local.cwd: %w", err)
		}
	}

	envVars, err := renderTemplateMap(custom.Env, vars)
	if err != nil {
		return "", ExecOptions{}, fmt.Errorf("render custom.env: %w", err)
	}

	execOpts := ExecOptions{Cwd: cwd, TimeoutSec: timeoutSec, ArtifactDir: opts.ArtifactDir}
	execOpts = a.mergeExecOptionsEnv(ctx, execOpts, envVars, a.buildAgentObservabilityAttrs(nil))
	return strings.Join(parts, " "), execOpts, nil
}

// finishLocal turns a runtime exec result into a SessionResult.
func (a *CustomAgent) finishLocal(ctx context.Context, rt Runtime, opts ExecOptions, custom *config.CustomEngineConfig, result ExecResult, execErr error, outputFile string, start time.Time) (*SessionResult, error) {
	durationMs := time.Since(start).Milliseconds()
	if execErr != nil {
		res := a.errorResult(result.ExitCode)
		res.DurationMs, res.Stderr = durationMs, result.Stderr
		return res, fmt.Errorf("custom engine run failed: %w", execErr)
	}

	raw := a.readRawResult(ctx, rt, custom, result, outputFile)

	if customResponseFormat(custom) == customResponseText {
		res := &SessionResult{
			Engine:       a.Name(),
			ExitCode:     result.ExitCode,
			DurationMs:   durationMs,
			FinalMessage: strings.TrimSpace(raw),
			Stderr:       result.Stderr,
			Artifacts:    &SessionArtifacts{},
		}
		if result.ExitCode != 0 {
			return res, fmt.Errorf("custom engine run failed (exit %d)", result.ExitCode)
		}
		return res, nil
	}

	res, err := a.parseSessionResult(ctx, opts, raw, durationMs)
	if err != nil {
		return res, err
	}
	// A non-zero process exit is a failed run even when the engine's JSON
	// reports exit_code 0 (e.g. a wrapper that crashed after printing output).
	if result.ExitCode != 0 {
		return res, fmt.Errorf("custom engine command exited %d: %s", result.ExitCode, result.Stderr)
	}
	if res.ExitCode != 0 {
		return res, fmt.Errorf("custom engine run failed (exit %d): %s", res.ExitCode, res.Stderr)
	}
	return res, nil
}

// readRawResult reads the result payload from the output file (preferred) or stdout.
func (a *CustomAgent) readRawResult(ctx context.Context, rt Runtime, custom *config.CustomEngineConfig, result ExecResult, outputFile string) string {
	if custom.Local.OutputFile == "" {
		return result.Stdout
	}
	// A per-call temp file avoids collisions when parallel cases share the
	// same output_file basename (e.g. the documented default).
	tmpFile, err := os.CreateTemp("", "skill-up-custom-result-*")
	if err != nil {
		logging.WarnContextf(ctx, "CustomAgent: cannot create temp file for output_file %s, falling back to stdout: %v", outputFile, err)
		return result.Stdout
	}
	tmp := tmpFile.Name()
	_ = tmpFile.Close()
	defer func() { _ = os.Remove(tmp) }()
	if err := rt.DownloadFile(ctx, outputFile, tmp); err != nil {
		logging.WarnContextf(ctx, "CustomAgent: cannot read output_file %s, falling back to stdout: %v", outputFile, err)
		return result.Stdout
	}
	data, err := os.ReadFile(tmp)
	if err != nil {
		logging.WarnContextf(ctx, "CustomAgent: cannot read output_file %s, falling back to stdout: %v", outputFile, err)
		return result.Stdout
	}
	return string(data)
}

// parsedSessionResult mirrors the SessionResult JSON contract but keeps
// exit_code as a pointer so a missing field can be distinguished from 0.
type parsedSessionResult struct {
	Engine       string                `json:"engine"`
	Model        string                `json:"model"`
	ExitCode     *int                  `json:"exit_code"`
	DurationMs   int64                 `json:"duration_ms"`
	Turns        int                   `json:"turns"`
	InputTokens  int                   `json:"input_tokens"`
	OutputTokens int                   `json:"output_tokens"`
	FinalMessage string                `json:"final_message"`
	Stderr       string                `json:"stderr"`
	Transcript   transcript.Transcript `json:"transcript"`
	Artifacts    *SessionArtifacts     `json:"artifacts"`
}

func (a *CustomAgent) parseSessionResult(ctx context.Context, opts ExecOptions, raw string, durationMs int64) (*SessionResult, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return a.errorResult(0), errors.New("custom engine returned an empty result")
	}
	var parsed parsedSessionResult
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		return a.errorResult(0), fmt.Errorf("custom engine result is not valid JSON: %w", err)
	}
	if parsed.ExitCode == nil {
		return a.errorResult(0), errors.New(customSessionResultMissing)
	}

	artifacts := parsed.Artifacts
	if artifacts == nil {
		artifacts = &SessionArtifacts{}
	}
	a.collectArtifacts(ctx, opts, artifacts)

	res := &SessionResult{
		Engine:       firstNonEmpty(parsed.Engine, a.Name()),
		Model:        firstNonEmpty(parsed.Model, formatAgentModel(a.Cfg.ModelProvider, a.Cfg.ModelName)),
		ExitCode:     *parsed.ExitCode,
		DurationMs:   parsed.DurationMs,
		Turns:        parsed.Turns,
		InputTokens:  parsed.InputTokens,
		OutputTokens: parsed.OutputTokens,
		FinalMessage: parsed.FinalMessage,
		Stderr:       parsed.Stderr,
		Transcript:   parsed.Transcript,
		Artifacts:    artifacts,
	}
	if res.DurationMs == 0 {
		res.DurationMs = durationMs
	}
	return res, nil
}

// collectArtifacts folds structured artifacts.files entries into the report
// pipeline: file paths join generated_files; inline content is written to the
// case artifact directory. url-based artifacts are deferred to the http phase.
func (a *CustomAgent) collectArtifacts(ctx context.Context, opts ExecOptions, artifacts *SessionArtifacts) {
	for _, f := range artifacts.Files {
		switch {
		case f.Path != "":
			artifacts.GeneratedFiles = append(artifacts.GeneratedFiles, f.Path)
		case f.Content != "" || f.ContentBase64 != "":
			if path := a.writeInlineArtifact(ctx, opts, f); path != "" {
				artifacts.GeneratedFiles = append(artifacts.GeneratedFiles, path)
			}
		case f.URL != "":
			logging.DebugContextf(ctx, "CustomAgent: artifact %q url is not downloaded in the local transport", f.Name)
		}
	}
}

// writeInlineArtifact materializes an inline artifact into the case artifact
// directory and returns its path so the caller can register it for archiving.
// It returns an empty string when nothing was written.
func (a *CustomAgent) writeInlineArtifact(ctx context.Context, opts ExecOptions, f ArtifactFile) string {
	if opts.ArtifactDir == "" || f.Name == "" {
		return ""
	}
	content := f.Content
	if f.ContentBase64 != "" {
		decoded, err := base64.StdEncoding.DecodeString(f.ContentBase64)
		if err != nil {
			logging.WarnContextf(ctx, "CustomAgent: artifact %q has invalid content_base64: %v", f.Name, err)
			return ""
		}
		content = string(decoded)
	}
	path, err := writeLocalArtifact(opts.ArtifactDir, f.Name, content)
	if err != nil {
		logging.WarnContextf(ctx, "CustomAgent: cannot write inline artifact %q: %v", f.Name, err)
		return ""
	}
	return path
}

func (a *CustomAgent) errorResult(exitCode int) *SessionResult {
	if exitCode == 0 {
		exitCode = 1
	}
	return &SessionResult{
		Engine:    a.Name(),
		ExitCode:  exitCode,
		Artifacts: &SessionArtifacts{},
	}
}

// sessionInput is the standard input handed to a custom engine.
type sessionInput struct {
	CaseID         string            `json:"case_id,omitempty"`
	Variant        string            `json:"variant,omitempty"`
	Workspace      string            `json:"workspace,omitempty"`
	Model          string            `json:"model,omitempty"`
	Kwargs         map[string]string `json:"kwargs,omitempty"`
	Messages       []sessionMessage  `json:"messages"`
	MaxTurns       int               `json:"max_turns,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
}

type sessionMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (a *CustomAgent) buildSessionInput(rt Runtime, opts ExecOptions, messages []transcript.Message, custom *config.CustomEngineConfig, timeoutSec int) sessionInput {
	msgs := make([]sessionMessage, 0, len(messages))
	for _, m := range messages {
		msgs = append(msgs, sessionMessage{Role: string(m.Role), Content: m.Content})
	}
	return sessionInput{
		CaseID:         opts.CaseID,
		Variant:        opts.Variant,
		Workspace:      rt.Workspace(),
		Model:          formatAgentModel(a.Cfg.ModelProvider, a.Cfg.ModelName),
		Kwargs:         custom.Kwargs,
		Messages:       msgs,
		MaxTurns:       opts.MaxTurns,
		TimeoutSeconds: timeoutSec,
	}
}

func (a *CustomAgent) buildTemplateVars(rt Runtime, opts ExecOptions, messages []transcript.Message, custom *config.CustomEngineConfig, sess sessionInput, timeoutSec int) (map[string]string, error) {
	sessJSON, err := json.Marshal(sess)
	if err != nil {
		return nil, fmt.Errorf("marshal session input: %w", err)
	}
	msgsJSON, err := json.Marshal(sess.Messages)
	if err != nil {
		return nil, fmt.Errorf("marshal messages: %w", err)
	}
	kwargsJSON, err := json.Marshal(orEmptyMap(custom.Kwargs))
	if err != nil {
		return nil, fmt.Errorf("marshal kwargs: %w", err)
	}

	workspace := rt.Workspace()
	vars := map[string]string{
		"workspace":          workspace,
		"prompt":             singleTurnPrompt(messages),
		"messages":           string(msgsJSON),
		"messages_json":      string(msgsJSON),
		"session_input":      string(sessJSON),
		"session_input_json": string(sessJSON),
		"input_file":         filepath.Join(workspace, customDefaultInputFile),
		"output_file":        filepath.Join(workspace, customDefaultOutputFile),
		"model":              formatAgentModel(a.Cfg.ModelProvider, a.Cfg.ModelName),
		"model_provider":     a.Cfg.ModelProvider,
		"model_name":         a.Cfg.ModelName,
		"api_key":            a.Cfg.APIKey,
		"case_id":            opts.CaseID,
		"variant":            opts.Variant,
		"max_turns":          strconv.Itoa(opts.MaxTurns),
		"timeout_seconds":    strconv.Itoa(timeoutSec),
		"kwargs":             string(kwargsJSON),
		"kwargs_json":        string(kwargsJSON),
	}
	for k, v := range custom.Kwargs {
		vars["kwargs."+k] = v
	}
	return vars, nil
}

// resolveCustomIOFiles renders the input/output file paths, applying defaults.
func resolveCustomIOFiles(rt Runtime, custom *config.CustomEngineConfig, vars map[string]string) (inputFile, outputFile string, err error) {
	inputFile = filepath.Join(rt.Workspace(), customDefaultInputFile)
	if custom.Local.InputFile != "" {
		if inputFile, err = renderTemplate(custom.Local.InputFile, vars); err != nil {
			return "", "", fmt.Errorf("render local.input_file: %w", err)
		}
	}
	outputFile = filepath.Join(rt.Workspace(), customDefaultOutputFile)
	if custom.Local.OutputFile != "" {
		if outputFile, err = renderTemplate(custom.Local.OutputFile, vars); err != nil {
			return "", "", fmt.Errorf("render local.output_file: %w", err)
		}
	}
	return inputFile, outputFile, nil
}

func customResponseFormat(custom *config.CustomEngineConfig) string {
	if custom.ResponseFormat == customResponseText {
		return customResponseText
	}
	return customResponseSessionJSON
}

func singleTurnPrompt(messages []transcript.Message) string {
	if len(messages) != 1 {
		return ""
	}
	if messages[0].Role != transcript.RoleUser {
		return ""
	}
	return messages[0].Content
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func orEmptyMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// renderTemplateMap renders every value of m, returning a new map.
func renderTemplateMap(m map[string]string, vars map[string]string) (map[string]string, error) {
	if len(m) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		rv, err := renderTemplate(v, vars)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		out[k] = rv
	}
	return out, nil
}

// renderTemplate resolves ${X}, ${X:-default} and ${X?message} references
// against vars, falling back to process environment variables.
func renderTemplate(s string, vars map[string]string) (string, error) {
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
			b.WriteString(s[start:])
			break
		}
		value, err := resolveTemplateToken(s[start+2:start+closeIdx], vars)
		if err != nil {
			return "", err
		}
		b.WriteString(value)
		i = start + closeIdx + 1
	}
	return b.String(), nil
}

func resolveTemplateToken(inner string, vars map[string]string) (string, error) {
	name := inner
	var defaultVal, errMsg string
	hasDefault, hasErrForm := false, false
	if before, after, found := strings.Cut(inner, ":-"); found {
		name, defaultVal, hasDefault = before, after, true
	} else if before, after, found := strings.Cut(inner, "?"); found {
		name, errMsg, hasErrForm = before, after, true
	}

	if v, ok := vars[name]; ok {
		// A present-but-empty built-in value (e.g. an unconfigured api_key)
		// is treated as unset so ${X:-default} and ${X?msg} still apply.
		if v != "" || (!hasDefault && !hasErrForm) {
			return v, nil
		}
	}
	if v := os.Getenv(name); v != "" {
		return v, nil
	}
	if hasDefault {
		return defaultVal, nil
	}
	if hasErrForm {
		if strings.TrimSpace(errMsg) == "" {
			errMsg = name + " is required"
		}
		return "", errors.New(errMsg)
	}
	// kwargs.<key> references that were not provided resolve to empty.
	if strings.HasPrefix(name, "kwargs.") {
		return "", nil
	}
	return "", fmt.Errorf("unresolved template variable %q", name)
}
