package sandbox

import (
	"os/exec"
	"strings"
	"testing"
)

// The shell startup file the profile binds at /etc/bash.bashrc is the whole reason a terminal
// inside a tenant sandbox is coloured at all: /etc carries none of the host's bash startup
// files, and the tenant's HOME (its workspace) carries no ~/.bashrc, so every default a
// distribution would have provided has to be in this file. These tests pin the parts a
// terminal's colour depends on, plus the two ways the file could fail silently.

// bashrcCodeLines drops comment lines: the file explains in prose why it names no unmounted
// host path, and that prose must not be mistaken for the file itself using one.
func bashrcCodeLines() []string {
	var code []string
	for _, line := range strings.Split(Bashrc, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		code = append(code, line)
	}
	return code
}

func TestBashrcAsksProgramsForColour(t *testing.T) {
	joined := strings.Join(bashrcCodeLines(), "\n")
	// `ls` colours only when it is told to: an alias is the only way a shell can tell it, and
	// LS_COLORS is what makes the colours mean anything beyond the built-in defaults.
	for _, want := range []string{
		"alias ls='ls --color=auto'",
		"alias grep='grep --color=auto'",
		`eval "$(dircolors -b)"`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("the shell startup file does not ask for colour: missing %q\n%s", want, Bashrc)
		}
	}
	// dircolors is a coreutils binary, not a shell builtin, and the sandbox has no
	// /etc/DIR_COLORS for it to read, so the guard is what keeps the file safe on a host that
	// does not ship it at all.
	if !strings.Contains(joined, "command -v dircolors") {
		t.Errorf("dircolors is called without a presence guard:\n%s", Bashrc)
	}
}

func TestBashrcPromptIsColouredAndReadlineSafe(t *testing.T) {
	var prompt string
	for _, line := range bashrcCodeLines() {
		if strings.HasPrefix(line, "PS1=") {
			prompt = line
		}
	}
	if prompt == "" {
		t.Fatalf("the shell startup file sets no prompt:\n%s", Bashrc)
	}
	if !strings.Contains(prompt, `\[`) || !strings.Contains(prompt, `\033[`) {
		t.Errorf("the prompt carries no colour escape: %q", prompt)
	}
	// \[ \] are what keep readline's cursor arithmetic right; unbalanced ones make line
	// editing and history search wrap wrongly.
	if opens, closes := strings.Count(prompt, `\[`), strings.Count(prompt, `\]`); opens != closes {
		t.Errorf("prompt has %d \\[ and %d \\]: %q", opens, closes, prompt)
	}
}

func TestBashrcHasNoEarlyReturnAndNoUnmountedPath(t *testing.T) {
	joined := strings.Join(bashrcCodeLines(), "\n")
	// This file is sourced by bash for interactive shells only. An early-return guard would be
	// dead weight at best and, if it ever judged wrong, exactly the silent failure this file
	// removes: a terminal with no colour and no error to explain it.
	for _, forbidden := range []string{"return", "exit"} {
		for _, line := range bashrcCodeLines() {
			if strings.TrimSpace(line) == forbidden || strings.HasPrefix(strings.TrimSpace(line), forbidden+" ") {
				t.Errorf("the shell startup file bails out with %q: %q", forbidden, line)
			}
		}
	}
	// Every path the code names must be one the profile mounts. /etc/profile, /etc/profile.d,
	// /etc/skel and /etc/DIR_COLORS are all outside the sandbox's /etc whitelist.
	for _, unmounted := range []string{"/etc/profile", "/etc/skel", "/etc/DIR_COLORS", "/etc/bash.bashrc.local", "/etc/inputrc"} {
		if strings.Contains(joined, unmounted) {
			t.Errorf("the shell startup file reads %s, which the profile does not mount:\n%s", unmounted, joined)
		}
	}
}

func TestBashrcIsValidShellSyntax(t *testing.T) {
	// The file is bash's (/etc/bash.bashrc), but it must also parse as plain sh: it is read on
	// a shell that may not be bash at all, and a syntax error there greets every session.
	for _, shell := range []string{"/bin/sh", "/usr/bin/bash", "/bin/bash"} {
		path, err := exec.LookPath(shell)
		if err != nil {
			continue
		}
		cmd := exec.Command(path, "-n")
		cmd.Stdin = strings.NewReader(Bashrc)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s -n rejected the startup file (%v: %s)", path, err, strings.TrimSpace(string(out)))
		}
	}
}
