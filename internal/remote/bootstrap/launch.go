package bootstrap

import (
	"fmt"
	"strings"
)

// StatePaths are the absolute remote-side paths for one workspace's serve
// state. All are under ~/.reasonix/remote.
type StatePaths struct {
	Dir       string // ~/.reasonix/remote
	StateJSON string
	TokenFile string
	LogFile   string
	PortFile  string
	PidFile   string
	LockDir   string
	LockOwner string
}

// shellQuote wraps s in single quotes safe for POSIX sh, escaping embedded
// single quotes as '\”. This is the only quoting used for remote command
// operands; every interpolated path/workspace passes through it.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// LaunchCommand builds the `sh -c` script that starts a detached serve in
// workspace, writing the port/pid files and appending output to the log. The
// binary path and every operand are single-quote-escaped so hostile paths
// (spaces, quotes, `; rm -rf ~`) cannot break out.
//
// Detachment: `setsid` fully divorces the process from any session, but it is
// absent on stock macOS, so it is used only when present (`$SX`); `nohup` +
// backgrounding + `</dev/null` is sufficient over a non-interactive SSH exec.
// The log is created 0600 (umask 077 + explicit chmod) so a same-machine user
// cannot read serve output; serve is launched with `--port-file`, which
// suppresses its token share line, so the token never reaches the log.
// It echoes the shell's $! so the caller can record the pid immediately.
//
// Credential-proxy mode (cred != nil): the virtual token rides the serve's
// environment (root-readable /proc only — never argv, never a file) and the
// provider flag selects the tunnel-backed provider entry.
func LaunchCommand(bin, workspace string, p StatePaths, cred *CredentialProxyOptions) string {
	envPrefix := ""
	modelFlag := ""
	if cred != nil {
		envPrefix = TokenEnvName + "=" + shellQuote(cred.Token) + " "
		modelFlag = " --model " + shellQuote(cred.Provider)
	}
	return fmt.Sprintf(
		"mkdir -p %s && cd %s && rm -f %s %s && umask 077 && : >>%s && chmod 600 %s && "+
			"SX=; command -v setsid >/dev/null 2>&1 && SX=setsid; "+
			"%s$SX nohup %s serve --addr 127.0.0.1:0 --auth token --token-file %s --port-file %s --pid-file %s%s </dev/null >>%s 2>&1 & echo $!",
		shellQuote(p.Dir),
		shellQuote(workspace),
		shellQuote(p.PortFile),
		shellQuote(p.PidFile),
		shellQuote(p.LogFile),
		shellQuote(p.LogFile),
		envPrefix,
		shellQuote(bin),
		shellQuote(p.TokenFile),
		shellQuote(p.PortFile),
		shellQuote(p.PidFile),
		modelFlag,
		shellQuote(p.LogFile),
	)
}

// StopCommand builds a script that TERMs the pid, waits up to ~5s, then KILLs
// if still alive. pid is validated numeric by the caller, and the caller has
// already confirmed (ServeAliveCommand) that the pid is our serve, so PID reuse
// cannot cause an unrelated process to be signalled.
func StopCommand(pid int, p StatePaths) string {
	return fmt.Sprintf(
		"T=%s; P=%s; ours() { A=$(ps -p %d -o args= 2>/dev/null || ps -p %d -o command= 2>/dev/null); "+
			"case \"$A\" in *reasonix*serve*\"$T\"*\"$P\"*) return 0;; *) return 1;; esac; }; "+
			"ours || exit 0; kill -TERM %d 2>/dev/null; "+
			"for i in 1 2 3 4 5; do kill -0 %d 2>/dev/null || exit 0; ours || exit 0; sleep 1; done; "+
			"ours && kill -KILL %d 2>/dev/null; exit 0",
		shellQuote(p.TokenFile), shellQuote(p.PortFile), pid, pid, pid, pid, pid,
	)
}

// ServeAliveCommand prints "1" only when pid is running AND its command line
// looks like a reasonix serve process. Checking the args (not just `kill -0`)
// prevents a recycled PID — now owned by an unrelated process — from being
// mistaken for the serve and later signalled by StopCommand. Each requireArgs
// fragment must additionally appear in the args, in order after the token and
// port files: local-proxy mode requires "--model <proxy provider>" so a serve
// launched under different settings (e.g. before the host switched credential
// modes) is not treated as reusable.
func ServeAliveCommand(pid int, p StatePaths, requireArgs ...string) string {
	decls := fmt.Sprintf("T=%s; P=%s; ", shellQuote(p.TokenFile), shellQuote(p.PortFile))
	pattern := "*reasonix*serve*\"$T\"*\"$P\"*"
	for i, arg := range requireArgs {
		decls += fmt.Sprintf("R%d=%s; ", i, shellQuote(arg))
		pattern += fmt.Sprintf("\"$R%d\"*", i)
	}
	return fmt.Sprintf(
		"%skill -0 %d 2>/dev/null || { echo 0; exit 0; }; "+
			"A=$(ps -p %d -o args= 2>/dev/null || ps -p %d -o command= 2>/dev/null); "+
			"case \"$A\" in %s) echo 1;; *) echo 0;; esac",
		decls, pid, pid, pid, pattern,
	)
}

// LogsCommand tails n lines of the log file (n<=0 => 200).
func LogsCommand(logFile string, n int) string {
	if n <= 0 {
		n = 200
	}
	return fmt.Sprintf("tail -n %d %s 2>/dev/null || true", n, shellQuote(logFile))
}

// servePortFileMarker is what LocateCommand greps for in `serve --help` to
// decide the located binary supports --port-file/--token-file. It must match
// the flag name registered in runServe.
const servePortFileMarker = "port-file"

// serveSessionEventsMarker gates on the multi-session capability: serves
// advertising --session-events tag SSE frames with sessionPath and keep
// background sessions running across switches. A located binary without it is
// treated as unusable so it is upgraded, exactly like the port-file gate.
const serveSessionEventsMarker = "session-events"

// serveDetachedHealMarker gates on the credential-heal fix: a reload retires
// background controllers instead of leaving them on a stale reverse-tunnel
// port. Serves without it are upgraded away like every other marker.
const serveDetachedHealMarker = "detached-heal"

// serveCapsToken is the rolling capability revision advertised in
// `serve --help`; bumping it retires every previously deployed serve.
const serveCapsToken = "reasonix-serve-caps-20260822c"

// LocateCommand probes for a usable reasonix binary. It prints the resolved
// path (or empty), the `--version` output, and "portfile:yes" /
// "sessionevents:yes" when `serve --help` advertises the respective flags.
// The bootstrap gates on the flags, not the version number, because
// --port-file/--token-file ship in this change: a version gate cannot know
// its own future release number, and any already-released binary would pass a
// numeric gate yet still lack the flags.
func LocateCommand(uploadedBin string) string {
	return fmt.Sprintf(
		"BIN=\"$(command -v reasonix 2>/dev/null)\"; "+
			"if [ -z \"$BIN\" ] && [ -x %s ]; then BIN=%s; fi; "+
			"if [ -z \"$BIN\" ]; then P=\"$(npm prefix -g 2>/dev/null)\"; if [ -n \"$P\" ] && [ -x \"$P/bin/reasonix\" ]; then BIN=\"$P/bin/reasonix\"; fi; fi; "+
			"echo \"$BIN\"; "+
			"if [ -n \"$BIN\" ]; then \"$BIN\" --version 2>/dev/null; "+
			"if \"$BIN\" serve --help 2>&1 | grep -q -- %s; then echo portfile:yes; else echo portfile:no; fi; "+
			"if \"$BIN\" serve --help 2>&1 | grep -q -- %s; then echo sessionevents:yes; else echo sessionevents:no; fi; "+
			"if \"$BIN\" serve --help 2>&1 | grep -q -- %s; then echo detachedheal:yes; else echo detachedheal:no; fi; "+
			"if \"$BIN\" serve --help 2>&1 | grep -q -- %s; then echo caps:yes; else echo caps:no; fi; fi",
		shellQuote(uploadedBin), shellQuote(uploadedBin), shellQuote(servePortFileMarker), shellQuote(serveSessionEventsMarker), shellQuote(serveDetachedHealMarker), shellQuote(serveCapsToken),
	)
}

// SupportsSessionEventsCommand probes a RUNNING serve process's binary for
// the --session-events capability, printing "yes" or "no". The executable is
// resolved via /proc/<pid>/exe (Linux) with a ps fallback (macOS). tryReuse
// and stopOutdatedServe use it to replace a live serve that predates
// multi-session switching.
func SupportsSessionEventsCommand(pid int) string {
	return fmt.Sprintf(
		"BIN=$(readlink /proc/%d/exe 2>/dev/null); "+
			"if [ -z \"$BIN\" ]; then BIN=$(ps -p %d -o comm= 2>/dev/null | tr -d ' '); fi; "+
			"if [ -n \"$BIN\" ] && [ -x \"$BIN\" ] && \"$BIN\" serve --help 2>&1 | grep -q -- %s && \"$BIN\" serve --help 2>&1 | grep -q -- %s && \"$BIN\" serve --help 2>&1 | grep -q -- %s; then echo yes; else echo no; fi",
		pid, pid, shellQuote(serveSessionEventsMarker), shellQuote(serveDetachedHealMarker), shellQuote(serveCapsToken),
	)
}
