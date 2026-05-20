//go:build !windows

package agent

// agentExecutablePath prepends the agent CLI's installed-from-bootstrap
// locations onto PATH so the nvm-managed node and ~/.local/bin shims win over
// any older system installs.
var agentExecutablePath = "$HOME/.local/bin:$HOME/.nvm/current/bin:$PATH"
