package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/alibaba/skill-up/internal/config"
	"github.com/alibaba/skill-up/internal/logging"
	"github.com/alibaba/skill-up/internal/runtime"
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
	// customStaleClearedMarker is printed by the pre-run cleanup when it
	// actually deletes a stale output file.
	customStaleClearedMarker = "__skill_up_stale_output_cleared__"
	// customArtifactReadTimeout bounds the post-timeout artifact read, which
	// runs on a fresh context detached from the (possibly canceled) run context.
	customArtifactReadTimeout = 30 * time.Second
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
// user, not installed by skill-up.
func (a *CustomAgent) Install(_ context.Context, _ Runtime) error { return nil }

// InstallMCP is a no-op for custom engines: a custom engine discovers MCP
// servers from the runtime environment itself. Declared servers are logged so
// an eval is not silently missing expected MCP wiring, but no installation is
// attempted (unlike the inherited CLIAgent.InstallMCP, which would error).
func (a *CustomAgent) InstallMCP(ctx context.Context, _ Runtime, mcpCfg runtime.MCPConfig) error {
	if len(mcpCfg.Servers) > 0 {
		logging.InfoContextf(ctx, "CustomAgent %q: %d MCP server(s) declared; a custom engine manages MCP itself, skipping installation", a.Name(), len(mcpCfg.Servers))
	}
	return nil
}

// Check is a no-op for custom engines.
func (a *CustomAgent) Check(_ context.Context, _ Runtime) error { return nil }

// CheckCredentials is a no-op: a custom engine references credentials
// explicitly via ${api_key}, so skill-up does not pre-validate them.
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
	// Guard against a nil local block: validation skips engine.custom for
	// built-in engines, so a `--engine` override can reach here unvalidated.
	if custom.Local == nil {
		return a.errorResult(0), errors.New("engine.custom.local.command is required when transport is local")
	}

	start := time.Now()
	timeoutSec := custom.TimeoutSeconds
	if timeoutSec <= 0 {
		timeoutSec = opts.TimeoutSec
	}

	// Build the full template variable set first: kwargs may reference
	// built-in variables (e.g. ${case_id}), and the I/O path templates may in
	// turn reference ${kwargs.<key>}, so kwargs must be resolved before paths.
	baseVars := a.buildBaseVars(rt, opts, messages, timeoutSec)

	renderedKwargs, err := renderTemplateMap(custom.Kwargs, baseVars)
	if err != nil {
		return a.errorResult(0), fmt.Errorf("render custom.kwargs: %w", err)
	}

	sess := a.buildSessionInput(rt, opts, messages, renderedKwargs, timeoutSec)
	vars, err := a.completeTemplateVars(baseVars, renderedKwargs, sess)
	if err != nil {
		return a.errorResult(0), err
	}

	// Resolve the I/O paths with the full variable set (so they may use
	// ${kwargs.<key>}), then expose the resolved paths to command rendering.
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

	// Remove any stale output file so a result left by a fixture or a previous
	// run is never mistaken for this invocation's output. Only an explicitly
	// configured output_file is cleared — the default ${output_file} path is
	// left untouched, as it may be ordinary fixture input the agent reads.
	clearedStaleOutput := false
	if custom.Local.OutputFile != "" {
		clearedStaleOutput = a.clearStaleOutputFile(ctx, rt, outputFile)
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
	return a.finishLocal(ctx, rt, opts, custom, result, execErr, inputFile, outputFile, clearedStaleOutput, start, messages)
}

// clearStaleOutputFile removes a pre-existing output file before the run and
// reports whether one was actually deleted, so a fixture/previous-run file is
// not parsed as this invocation's result and the deletion can be excluded from
// workspace diffs.
func (a *CustomAgent) clearStaleOutputFile(ctx context.Context, rt Runtime, outputFile string) bool {
	q := shellQuote(outputFile)
	cmd := "if [ -e " + q + " ]; then rm -f -- " + q + " && printf %s " + shellQuote(customStaleClearedMarker) + "; fi"
	result, err := rt.Exec(ctx, cmd, ExecOptions{})
	if err != nil {
		logging.DebugContextf(ctx, "CustomAgent: could not clear stale output file %s: %v", outputFile, err)
		return false
	}
	return strings.Contains(result.Stdout, customStaleClearedMarker)
}

// buildLocalExec renders the command, args and exec options for the local run.
func (a *CustomAgent) buildLocalExec(ctx context.Context, rt Runtime, opts ExecOptions, custom *config.CustomEngineConfig, vars map[string]string, timeoutSec int) (string, ExecOptions, error) {
	local := custom.Local
	command, err := renderTemplate(local.Command, vars)
	if err != nil {
		return "", ExecOptions{}, fmt.Errorf("render local.command: %w", err)
	}
	if a.containsAPIKey(command) {
		return "", ExecOptions{}, errSecretInCommand("local.command")
	}
	parts := []string{shellQuote(command)}
	for i, raw := range local.Args {
		arg, rErr := renderTemplate(raw, vars)
		if rErr != nil {
			return "", ExecOptions{}, fmt.Errorf("render local.args[%d]: %w", i, rErr)
		}
		if a.containsAPIKey(arg) {
			return "", ExecOptions{}, errSecretInCommand(fmt.Sprintf("local.args[%d]", i))
		}
		parts = append(parts, shellQuote(arg))
	}

	cwd := rt.Workspace()
	if local.Cwd != "" {
		rendered, rErr := renderTemplate(local.Cwd, vars)
		if rErr != nil {
			return "", ExecOptions{}, fmt.Errorf("render local.cwd: %w", rErr)
		}
		// Resolve a relative cwd against the runtime workspace so local
		// transport behaves consistently across runtimes (NoneRuntime would
		// otherwise pass it through relative to the skill-up process).
		cwd = workspacePath(rt, rendered)
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
func (a *CustomAgent) finishLocal(ctx context.Context, rt Runtime, opts ExecOptions, custom *config.CustomEngineConfig, result ExecResult, execErr error, inputFile, outputFile string, clearedStaleOutput bool, start time.Time, messages []transcript.Message) (*SessionResult, error) {
	durationMs := time.Since(start).Milliseconds()

	// On a timeout/cancel the command may already have emitted a valid result;
	// still read it so judges and expect checks can inspect the partial answer.
	var (
		raw                string
		outputFileProduced bool
	)
	if execErr == nil || isTimeoutError(execErr) {
		// After a timeout/cancel, ctx is already done; read artifacts on a
		// fresh context so a partial output file is still recoverable
		// (notably for remote runtimes whose DownloadFile honors ctx).
		readCtx := ctx
		if execErr != nil {
			var cancel context.CancelFunc
			readCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), customArtifactReadTimeout)
			defer cancel()
		}
		raw, outputFileProduced = a.readRawResult(readCtx, rt, custom, result, outputFile)
	}

	res, parseErr := a.buildResult(ctx, rt, opts, custom, raw, result, durationMs, messages)

	if execErr != nil {
		switch {
		case parseErr != nil:
			// Nothing usable was produced before the failure.
			res = a.errorResult(result.ExitCode)
			res.DurationMs = durationMs
			res.Transcript = minimalCustomTranscript(messages, "")
		case res.ExitCode == 0:
			// The run was interrupted; a parsed exit_code 0 must not let the
			// evaluator treat an interrupted run as a success.
			res.ExitCode = result.ExitCode
			if res.ExitCode == 0 {
				res.ExitCode = 1
			}
		}
		if res.Stderr == "" {
			res.Stderr = result.Stderr
		}
		a.registerFrameworkIO(res, inputFile, outputFile, outputFileProduced || clearedStaleOutput)
		return res, fmt.Errorf("custom engine run failed: %w", execErr)
	}

	a.registerFrameworkIO(res, inputFile, outputFile, outputFileProduced || clearedStaleOutput)
	// A non-zero process exit is a failed run even when the engine's JSON
	// reports exit_code 0 (e.g. a wrapper that crashed after printing output)
	// or never emitted parseable JSON at all. Reflect the real process
	// exit/stderr so reports surface the actual command failure.
	if result.ExitCode != 0 {
		res.ExitCode = result.ExitCode
		if res.Stderr == "" {
			res.Stderr = result.Stderr
		}
		return res, fmt.Errorf("custom engine command exited %d: %s", result.ExitCode, result.Stderr)
	}
	if parseErr != nil {
		return res, parseErr
	}
	if res.ExitCode != 0 {
		return res, fmt.Errorf("custom engine run failed (exit %d): %s", res.ExitCode, res.Stderr)
	}
	return res, nil
}

// buildResult parses raw engine output into a SessionResult according to the
// configured response_format. The text format never errors and always grades
// stdout (never the output file); session_result returns a parse error when
// the payload is missing or malformed.
func (a *CustomAgent) buildResult(ctx context.Context, rt Runtime, opts ExecOptions, custom *config.CustomEngineConfig, raw string, result ExecResult, durationMs int64, messages []transcript.Message) (*SessionResult, error) {
	if customResponseFormat(custom) == customResponseText {
		// Per the contract, text responses come from stdout; an output file
		// produced for bookkeeping must not be graded as the final answer.
		finalMsg := strings.TrimSpace(result.Stdout)
		return &SessionResult{
			Engine:       a.Name(),
			ExitCode:     result.ExitCode,
			DurationMs:   durationMs,
			FinalMessage: finalMsg,
			Stderr:       result.Stderr,
			Transcript:   minimalCustomTranscript(messages, finalMsg),
			Artifacts:    &SessionArtifacts{},
		}, nil
	}
	return a.parseSessionResult(ctx, rt, opts, raw, durationMs, messages)
}

// isTimeoutError reports whether err is a context deadline/cancellation, which
// means the command was interrupted rather than failing to start.
func isTimeoutError(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// registerFrameworkIO records the framework-written input/output files in
// GeneratedFiles so the workspace-diff collector excludes them (they would
// otherwise show up as user changes) and they are archived for debugging.
func (a *CustomAgent) registerFrameworkIO(res *SessionResult, inputFile, outputFile string, usedOutputFile bool) {
	if res.Artifacts == nil {
		res.Artifacts = &SessionArtifacts{}
	}
	if inputFile != "" {
		res.Artifacts.GeneratedFiles = append(res.Artifacts.GeneratedFiles, inputFile)
	}
	if usedOutputFile && outputFile != "" {
		res.Artifacts.GeneratedFiles = append(res.Artifacts.GeneratedFiles, outputFile)
	}
}

// readRawResult reads the result payload from the output file, or from stdout.
// Per the contract, the output file is read only when local.output_file is
// explicitly configured; when it is unset the result comes from stdout and the
// default ${output_file} path is left untouched (it may be ordinary fixture
// input). The boolean reports whether the configured output file was produced
// (exists in the runtime) — independent of whether it supplied the payload —
// so an empty produced file is still excluded from workspace diffs.
func (a *CustomAgent) readRawResult(ctx context.Context, rt Runtime, custom *config.CustomEngineConfig, result ExecResult, outputFile string) (raw string, produced bool) {
	if custom.Local.OutputFile == "" || outputFile == "" {
		return result.Stdout, false
	}

	// A per-call temp file avoids collisions when parallel cases share the
	// same output_file basename.
	tmpFile, err := os.CreateTemp("", "skill-up-custom-result-*")
	if err != nil {
		logging.WarnContextf(ctx, "CustomAgent: cannot create temp file for output_file %s, falling back to stdout: %v", outputFile, err)
		return result.Stdout, false
	}
	tmp := tmpFile.Name()
	_ = tmpFile.Close()
	defer func() { _ = os.Remove(tmp) }()

	if err := rt.DownloadFile(ctx, outputFile, tmp); err != nil {
		logging.WarnContextf(ctx, "CustomAgent: cannot read output_file %s, falling back to stdout: %v", outputFile, err)
		return result.Stdout, false
	}
	// The output file exists in the runtime; it is "produced" even if empty.
	data, err := os.ReadFile(tmp)
	if err != nil {
		logging.WarnContextf(ctx, "CustomAgent: cannot read output_file %s, falling back to stdout: %v", outputFile, err)
		return result.Stdout, true
	}
	if strings.TrimSpace(string(data)) == "" {
		return result.Stdout, true
	}
	return string(data), true
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

func (a *CustomAgent) parseSessionResult(ctx context.Context, rt Runtime, opts ExecOptions, raw string, durationMs int64, messages []transcript.Message) (*SessionResult, error) {
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
	a.collectArtifacts(ctx, rt, opts, artifacts)

	finalMsg := parsed.FinalMessage
	trans := parsed.Transcript
	if finalMsg == "" && len(trans) > 0 {
		// The engine provided a transcript but omitted final_message; derive
		// it from the last assistant reply so judges and reports do not grade
		// or display a blank answer.
		finalMsg = trans.FinalAssistantMessage()
	}
	if len(trans) == 0 {
		// Build the documented minimal transcript so judges still receive the
		// conversation when the engine omits an explicit transcript.
		trans = minimalCustomTranscript(messages, finalMsg)
	}

	turns := parsed.Turns
	if turns == 0 {
		// Default missing turns from the transcript so an otherwise
		// successful custom run does not report 0 turns to judges/reports.
		turns = deriveTurns(trans)
	}

	res := &SessionResult{
		Engine:       firstNonEmpty(parsed.Engine, a.Name()),
		Model:        firstNonEmpty(parsed.Model, formatAgentModel(a.Cfg.ModelProvider, a.Cfg.ModelName)),
		ExitCode:     *parsed.ExitCode,
		DurationMs:   parsed.DurationMs,
		Turns:        turns,
		InputTokens:  parsed.InputTokens,
		OutputTokens: parsed.OutputTokens,
		FinalMessage: finalMsg,
		Stderr:       parsed.Stderr,
		Transcript:   trans,
		Artifacts:    artifacts,
	}
	if res.DurationMs == 0 {
		res.DurationMs = durationMs
	}
	return res, nil
}

// deriveTurns infers a turn count from a transcript. It prefers the highest
// explicit Message.Turn value; when no message carries Turn (the
// design-example transcript only sets role/content) it falls back to the
// assistant-reply count so a successful run does not report turns=0.
func deriveTurns(trans transcript.Transcript) int {
	maxTurn, assistantCount := 0, 0
	for _, m := range trans {
		if m.Turn > maxTurn {
			maxTurn = m.Turn
		}
		if m.Role == transcript.RoleAssistant {
			assistantCount++
		}
	}
	if maxTurn > 0 {
		return maxTurn
	}
	return assistantCount
}

// minimalCustomTranscript builds a fallback transcript from the input messages
// plus the engine's final assistant reply. It returns nil when there is nothing
// to record.
func minimalCustomTranscript(messages []transcript.Message, finalMsg string) transcript.Transcript {
	trans := make(transcript.Transcript, 0, len(messages)+1)
	maxTurn := 0
	for _, m := range messages {
		trans = append(trans, m)
		if m.Turn > maxTurn {
			maxTurn = m.Turn
		}
	}
	if finalMsg != "" {
		if maxTurn == 0 {
			maxTurn = 1
		}
		trans = append(trans, transcript.Message{
			Role:    transcript.RoleAssistant,
			Content: finalMsg,
			Turn:    maxTurn,
		})
	}
	if len(trans) == 0 {
		return nil
	}
	return trans
}

// collectArtifacts folds structured artifacts.files entries into the report
// pipeline: path entries register the original workspace path (so the
// workspace-diff collector excludes it) and, when the declared name differs
// from the basename, additionally archive a copy under that name; inline
// content is written to the case artifact directory. url-based artifacts are
// deferred to the http phase.
func (a *CustomAgent) collectArtifacts(ctx context.Context, rt Runtime, opts ExecOptions, artifacts *SessionArtifacts) {
	for _, f := range artifacts.Files {
		switch {
		case f.Path != "":
			// Keep the original path: the archiver downloads it (under its
			// basename) and the workspace-diff collector excludes it.
			artifacts.GeneratedFiles = append(artifacts.GeneratedFiles, f.Path)
			if renamed := a.archiveRenamedPathArtifact(ctx, rt, opts, f); renamed != "" {
				artifacts.GeneratedFiles = append(artifacts.GeneratedFiles, renamed)
			}
		case f.Content != "" || f.ContentBase64 != "":
			if path := a.writeInlineArtifact(ctx, opts, f); path != "" {
				artifacts.GeneratedFiles = append(artifacts.GeneratedFiles, path)
			}
		case f.URL != "":
			logging.DebugContextf(ctx, "CustomAgent: artifact %q url is not downloaded in the local transport", f.Name)
		}
	}
}

// archiveRenamedPathArtifact materializes a path-based artifact into the case
// artifact directory under its declared name when that name differs from the
// file's basename, so the archiver (which keys on basename) preserves it. It
// returns the host copy path, or an empty string when no rename is needed.
func (a *CustomAgent) archiveRenamedPathArtifact(ctx context.Context, rt Runtime, opts ExecOptions, f ArtifactFile) string {
	name := filepath.Base(f.Name)
	if f.Name == "" || opts.ArtifactDir == "" || name == filepath.Base(f.Path) {
		return ""
	}
	dest := filepath.Join(opts.ArtifactDir, name)
	if err := rt.DownloadFile(ctx, f.Path, dest); err != nil {
		logging.WarnContextf(ctx, "CustomAgent: cannot archive artifact %q from %s: %v", name, f.Path, err)
		return ""
	}
	return dest
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

// containsAPIKey reports whether s embeds the configured API key value, used
// to keep credentials out of the rendered command line.
func (a *CustomAgent) containsAPIKey(s string) bool {
	return a.Cfg.APIKey != "" && strings.Contains(s, a.Cfg.APIKey)
}

// errSecretInCommand reports a rendered command/arg that would leak a credential.
func errSecretInCommand(field string) error {
	return fmt.Errorf(
		"engine.custom.%s renders the API key into the command line, which would expose it in process listings and traces; reference ${api_key} from engine.custom.env instead",
		field,
	)
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

func (a *CustomAgent) buildSessionInput(rt Runtime, opts ExecOptions, messages []transcript.Message, kwargs map[string]string, timeoutSec int) sessionInput {
	msgs := make([]sessionMessage, 0, len(messages))
	for _, m := range messages {
		msgs = append(msgs, sessionMessage{Role: string(m.Role), Content: m.Content})
	}
	return sessionInput{
		CaseID:         opts.CaseID,
		Variant:        opts.Variant,
		Workspace:      rt.Workspace(),
		Model:          formatAgentModel(a.Cfg.ModelProvider, a.Cfg.ModelName),
		Kwargs:         kwargs,
		Messages:       msgs,
		MaxTurns:       opts.MaxTurns,
		TimeoutSeconds: timeoutSec,
	}
}

// buildBaseVars builds the scalar template variables that do not depend on
// custom.kwargs (so kwargs can themselves reference them) or on the session
// input. input_file/output_file start at their defaults and are overwritten
// once resolveCustomIOFiles has run.
func (a *CustomAgent) buildBaseVars(rt Runtime, opts ExecOptions, messages []transcript.Message, timeoutSec int) map[string]string {
	workspace := rt.Workspace()
	return map[string]string{
		"workspace":       workspace,
		"prompt":          singleTurnPrompt(messages),
		"input_file":      filepath.Join(workspace, customDefaultInputFile),
		"output_file":     filepath.Join(workspace, customDefaultOutputFile),
		"model":           formatAgentModel(a.Cfg.ModelProvider, a.Cfg.ModelName),
		"model_provider":  a.Cfg.ModelProvider,
		"model_name":      a.Cfg.ModelName,
		"api_key":         a.Cfg.APIKey,
		"case_id":         opts.CaseID,
		"variant":         opts.Variant,
		"max_turns":       strconv.Itoa(opts.MaxTurns),
		"timeout_seconds": strconv.Itoa(timeoutSec),
	}
}

// completeTemplateVars extends the base variables with the rendered kwargs and
// the session-input-derived structured values.
func (a *CustomAgent) completeTemplateVars(baseVars, renderedKwargs map[string]string, sess sessionInput) (map[string]string, error) {
	sessJSON, err := json.Marshal(sess)
	if err != nil {
		return nil, fmt.Errorf("marshal session input: %w", err)
	}
	msgsJSON, err := json.Marshal(sess.Messages)
	if err != nil {
		return nil, fmt.Errorf("marshal messages: %w", err)
	}
	kwargsJSON, err := json.Marshal(orEmptyMap(renderedKwargs))
	if err != nil {
		return nil, fmt.Errorf("marshal kwargs: %w", err)
	}

	vars := make(map[string]string)
	maps.Copy(vars, baseVars)
	vars["messages"] = string(msgsJSON)
	vars["messages_json"] = string(msgsJSON)
	vars["session_input"] = string(sessJSON)
	vars["session_input_json"] = string(sessJSON)
	vars["kwargs"] = string(kwargsJSON)
	vars["kwargs_json"] = string(kwargsJSON)
	for k, v := range renderedKwargs {
		vars["kwargs."+k] = v
	}
	return vars, nil
}

// resolveCustomIOFiles renders the input/output file paths, applying defaults.
// A non-absolute configured path is resolved against the runtime workspace so
// it is consistent regardless of local.cwd (the command sees the same path the
// runtime upload/download APIs key on).
func resolveCustomIOFiles(rt Runtime, custom *config.CustomEngineConfig, vars map[string]string) (inputFile, outputFile string, err error) {
	inputFile = filepath.Join(rt.Workspace(), customDefaultInputFile)
	if custom.Local.InputFile != "" {
		rendered, rErr := renderTemplate(custom.Local.InputFile, vars)
		if rErr != nil {
			return "", "", fmt.Errorf("render local.input_file: %w", rErr)
		}
		inputFile = workspacePath(rt, rendered)
	}
	outputFile = filepath.Join(rt.Workspace(), customDefaultOutputFile)
	if custom.Local.OutputFile != "" {
		rendered, rErr := renderTemplate(custom.Local.OutputFile, vars)
		if rErr != nil {
			return "", "", fmt.Errorf("render local.output_file: %w", rErr)
		}
		outputFile = workspacePath(rt, rendered)
	}
	return inputFile, outputFile, nil
}

// workspacePath resolves a non-absolute path against the runtime workspace.
func workspacePath(rt Runtime, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(rt.Workspace(), p)
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
