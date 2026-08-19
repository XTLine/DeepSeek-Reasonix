package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"

	"reasonix/internal/remote/sftpfs"
)

// CredentialProxyOptions configures local-proxy credential mode: the remote
// serve's model calls route back to the desktop over the SSH reverse tunnel,
// so the real provider key never leaves the desktop. The bootstrap installs a
// provider entry pointing at the tunnel and injects a virtual token into the
// serve environment; the desktop-side proxy validates the token and swaps in
// the real key.
type CredentialProxyOptions struct {
	// BaseURL is the loopback URL on the REMOTE host that tunnels back to the
	// desktop's credential proxy, e.g. http://127.0.0.1:18999.
	BaseURL string
	// Token is the virtual token the serve presents; it travels in the
	// process environment (root-readable only), never in argv or files.
	Token string
	// Provider is the provider name installed into the remote config; the
	// serve is launched with --model <Provider> so it selects this entry.
	Provider string
	// Model is the model name the provider entry carries (the desktop's
	// current default model, resolved by the caller).
	Model string
}

// TokenEnvName is the environment variable the launch command sets and the
// installed provider entry reads (api_key_env).
const TokenEnvName = "REASONIX_PROXY_TOKEN"

// isRemoteMissing reports whether err is the SFTP "no such file" condition
// (pkg/sftp maps it onto os.ErrNotExist; the text match covers older wraps).
func isRemoteMissing(err error) bool {
	return err != nil && (errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), "no such file"))
}

// remoteConfigPath is ~/.reasonix/config.toml on the remote host.
func remoteConfigPath(home string) string {
	return path.Join(home, ".reasonix", "config.toml")
}

// tomlString renders s as a basic TOML string.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// credentialProviderBlock renders the provider entry appended to the remote
// config. base_url rides the reverse tunnel; the token arrives via the
// REASONIX_PROXY_TOKEN environment variable the launch command sets.
func credentialProviderBlock(opts *CredentialProxyOptions) string {
	var b strings.Builder
	b.WriteString("\n[[providers]]\n")
	b.WriteString("# managed by the Reasonix desktop credential proxy — safe to delete\n")
	b.WriteString("name = " + tomlString(opts.Provider) + "\n")
	b.WriteString("kind = \"openai\"\n")
	b.WriteString("base_url = " + tomlString(opts.BaseURL) + "\n")
	b.WriteString("model = " + tomlString(opts.Model) + "\n")
	b.WriteString("api_key_env = \"" + TokenEnvName + "\"\n")
	return b.String()
}

// ensureCredentialProvider installs (idempotently) the desktop-proxy provider
// entry into the remote user config. A block with the provider name that
// already points at opts.BaseURL is left untouched; one pointing elsewhere is
// rewritten in place so a port change propagates.
func ensureCredentialProvider(ctx context.Context, fs *sftpfs.FS, home string, opts *CredentialProxyOptions) error {
	if opts == nil || strings.TrimSpace(opts.BaseURL) == "" || strings.TrimSpace(opts.Token) == "" ||
		strings.TrimSpace(opts.Provider) == "" || strings.TrimSpace(opts.Model) == "" {
		return fmt.Errorf("bootstrap: credential proxy options are incomplete")
	}
	cfgPath := remoteConfigPath(home)
	data, _, _, rerr := fs.ReadFile(ctx, cfgPath, 1<<20)
	if rerr != nil && !isRemoteMissing(rerr) {
		return fmt.Errorf("bootstrap: read remote config: %w", rerr)
	}
	existing := string(data)
	if idx := providerBlockIndex(existing, opts.Provider); idx >= 0 {
		if providerBlockHasBaseURL(existing[idx:], opts.BaseURL) {
			return nil
		}
		updated, ok := replaceProviderBaseURL(existing, idx, opts.BaseURL)
		if !ok {
			return fmt.Errorf("bootstrap: remote config provider %q needs a manual base_url update", opts.Provider)
		}
		existing = updated
	} else {
		existing += credentialProviderBlock(opts)
	}
	if err := fs.MkdirAll(ctx, path.Dir(cfgPath)); err != nil {
		return err
	}
	return fs.WriteFileAtomic(ctx, cfgPath, []byte(existing), 0o600)
}

// providerBlockIndex finds the start of the [[providers]] block whose name
// equals provider, or -1. Blocks are scanned line-wise; a block ends at the
// next table header.
func providerBlockIndex(text, provider string) int {
	want := "name = " + tomlString(provider)
	lines := strings.Split(text, "\n")
	offset := 0
	inBlock := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[[") || strings.HasPrefix(trimmed, "[") {
			inBlock = strings.HasPrefix(trimmed, "[[providers]]")
		} else if inBlock && trimmed == want {
			return offset
		}
		offset += len(line) + 1
	}
	return -1
}

// providerBlockHasBaseURL reports whether the block starting at idx contains
// the given base_url assignment before its next table header.
func providerBlockHasBaseURL(block, baseURL string) bool {
	want := "base_url = " + tomlString(baseURL)
	for _, line := range strings.Split(block, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			return false
		}
		if trimmed == want {
			return true
		}
	}
	return false
}

// replaceProviderBaseURL swaps the base_url line inside the block starting at
// idx, preserving everything else byte-for-byte.
func replaceProviderBaseURL(text string, idx int, baseURL string) (string, bool) {
	rest := text[idx:]
	lines := strings.Split(rest, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if i > 0 && strings.HasPrefix(trimmed, "[") {
			break
		}
		if strings.HasPrefix(trimmed, "base_url") && strings.Contains(trimmed, "=") {
			indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
			lines[i] = indent + "base_url = " + tomlString(baseURL)
			return text[:idx] + strings.Join(lines, "\n"), true
		}
	}
	return text, false
}
