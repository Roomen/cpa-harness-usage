//go:build !darwin || !cgo

package quota

// systemKeychain is unavailable: Claude Code stores credentials in
// $CLAUDE_CONFIG_DIR/.credentials.json or ~/.claude/.credentials.json here.
var systemKeychain keychainReader
