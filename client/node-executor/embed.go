// Package nodeexecutor embeds the Node chat executor source so Pie Relay can
// install it to the user's machine (~/.kroot/chat-executor) at runtime.
package nodeexecutor

import "embed"

//go:embed executor.mjs pty-host.mjs package.json
var Files embed.FS
