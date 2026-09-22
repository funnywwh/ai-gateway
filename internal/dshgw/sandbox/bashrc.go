// Interactive-shell startup file for a tenant sandbox. This file is pure: the content is a
// constant, so what a tenant's shell runs is reviewable without building a deployment.
package sandbox

// Bashrc is the interactive-shell startup file the profile binds at /etc/bash.bashrc.
//
// Why it exists: /etc inside the sandbox is a whitelist (bubblewrap starts from an empty tmpfs
// root), so the host's own bash startup files are not part of the tenant's view, and the
// tenant's HOME — its workspace — carries no ~/.bashrc either. `bash -i` therefore starts with
// no aliases, no LS_COLORS and bash's bare default prompt, and a terminal that renders colour
// perfectly (web-tty hands it TERM=xterm-256color, COLORTERM=truecolor and 256-colour
// terminfo) still shows a monochrome `ls` and a monochrome prompt: `ls` only colours when it is
// asked to, and nothing in that view ever asked it to.
//
// Why this path: the distro's bash is built to read /etc/bash.bashrc for interactive shells
// before ~/.bashrc (SYS_BASHRC; `strace -f -e trace=openat bash -ic true` shows that open, and
// the order), so one bound file covers every `bash -i` in the sandbox — the web terminal's
// shell included — while a tenant that writes its own ~/.bashrc still overrides every default
// below. Nothing here is read by a non-interactive shell, which is why the agent's own bash
// tool (TERM=dumb, NO_COLOR=1) keeps its plain, unprompted output.
//
// The content deliberately carries no early-return guard: bash sources this file for
// interactive shells only, and a guard that guessed wrong would fail silently by leaving the
// terminal colourless — the exact symptom this file removes. It also names no path the profile
// does not mount: dircolors falls back to its built-in database without /etc/DIR_COLORS.
const Bashrc = `# Interactive-shell defaults for a tenant sandbox.
#
# The profile binds this file at /etc/bash.bashrc (a per-tenant view the gateway renders to
# <DshHome>/sandbox/bashrc). /etc inside the sandbox is a whitelist: the host's own bash
# startup files are not part of it, and the tenant's HOME — the workspace — carries no
# ~/.bashrc either. Without this file an interactive bash starts with no aliases, no
# LS_COLORS and bash's bare default prompt, so a terminal that renders colour perfectly
# (TERM=xterm-256color, COLORTERM=truecolor, 256 colours) still shows a monochrome 'ls' and a
# monochrome prompt: 'ls' only colours when it is asked to, and nothing asked it to.
#
# bash sources this file for interactive shells only, before ~/.bashrc, so a tenant that
# writes its own ~/.bashrc still overrides everything below.

# File-type colours. dircolors ships with coreutils and falls back to its built-in database;
# the sandbox never has the /etc/DIR_COLORS it would otherwise read.
if command -v dircolors >/dev/null 2>&1; then
    eval "$(dircolors -b)"
fi

# The aliases a distribution's default ~/.bashrc would have provided.
alias ls='ls --color=auto'
alias ll='ls -alF'
alias la='ls -A'
alias l='ls -CF'
alias grep='grep --color=auto'

# Green user@host, blue working directory. The \[ \] escapes keep readline's cursor
# arithmetic right, so line editing, history search and wrapping still behave.
PS1='\[\033[01;32m\]\u@\h\[\033[00m\]:\[\033[01;34m\]\w\[\033[00m\]\$ '

# History and geometry knobs an interactive shell expects.
HISTCONTROL=ignoreboth
HISTSIZE=1000
HISTFILESIZE=2000
shopt -s checkwinsize
`
