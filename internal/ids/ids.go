// Package ids generates prefixed identifiers compatible with OpenAI object ids
// (resp_, msg_, fc_, rs_, ...). All randomness comes from crypto/rand.
package ids

import (
	"crypto/rand"
	"encoding/base32"
)

// entropyBytes = 15 bytes = 120 bits -> 24 base32 characters.
const entropyBytes = 15

var enc = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// New returns a random identifier with the given prefix, e.g. New("resp") => "resp_ab12...".
func New(prefix string) string {
	buf := make([]byte, entropyBytes)
	if _, err := rand.Read(buf); err != nil {
		panic("ids: crypto/rand unavailable: " + err.Error())
	}
	return prefix + "_" + enc.EncodeToString(buf)
}

// Convenience constructors for identifiers used across the gateway.
func Response() string       { return New("resp") }
func Message() string        { return New("msg") }
func FunctionCall() string   { return New("fc") }
func Reasoning() string      { return New("rs") }
func Request() string        { return New("req") }
func APIKey() string         { return New("sk-gw") }
func MCPToken() string       { return New("aigw_mcp") }
func Session() string        { return New("sess") }
func RedemptionCode() string { return New("gwrc") }
func BackupJob() string      { return New("bkp") }

// Console chat identifiers. They are prefixed like everything else so a log line or a
// support ticket says at a glance which kind of object an id names.
func ChatSession() string  { return New("chat") }
func ChatTurn() string     { return New("turn") }
func ChatMessage() string  { return New("cmsg") }
func ChatToolCall() string { return New("tcall") }
func ChatArtifact() string { return New("art") }
