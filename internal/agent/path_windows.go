//go:build windows

package agent

// agentExecutablePath is empty on Windows: the POSIX bootstrap does not run
// natively, and overriding PATH with a colon-separated string would break
// the host's `where` lookups (and conflict with case-insensitive `Path`).
// Letting the inherited environment flow through keeps preinstalled CLIs
// reachable.
var agentExecutablePath = ""
