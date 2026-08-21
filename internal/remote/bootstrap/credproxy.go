package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"

	"reasonix/internal/config"
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
	// Kind is the provider kind the entry carries ("openai" or "anthropic"):
	// the serve formats its model requests per kind, so it must match the
	// desktop provider behind the proxy. Empty reads as "openai".
	Kind string
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

// credentialProxyKind normalizes the options' provider kind.
func credentialProxyKind(opts *CredentialProxyOptions) string {
	kind := strings.TrimSpace(opts.Kind)
	if kind == "" {
		kind = "openai"
	}
	return kind
}

// credentialProviderBlock renders the provider entry appended to the remote
// config. base_url rides the reverse tunnel; the token arrives via the
// REASONIX_PROXY_TOKEN environment variable the launch command sets.
func credentialProviderBlock(opts *CredentialProxyOptions) string {
	var b strings.Builder
	b.WriteString("\n[[providers]]\n")
	b.WriteString("# managed by the Reasonix desktop credential proxy — safe to delete\n")
	b.WriteString("name = " + tomlString(opts.Provider) + "\n")
	b.WriteString("kind = " + tomlString(credentialProxyKind(opts)) + "\n")
	b.WriteString("base_url = " + tomlString(opts.BaseURL) + "\n")
	b.WriteString("model = " + tomlString(opts.Model) + "\n")
	b.WriteString("api_key_env = \"" + TokenEnvName + "\"\n")
	return b.String()
}

// ensureCredentialProvider installs (idempotently) the desktop-proxy provider
// entry into the remote user config. A block with the provider name that
// already points at opts.BaseURL AND carries the same kind is left untouched;
// one pointing elsewhere or carrying a stale kind is rewritten in place so
// port and kind changes propagate.
// ensureCredentialProvider installs or heals the desktop-proxy provider entry
// in the remote config. The returned bool reports whether anything was
// rewritten: the desktop uses it to decide that RUNNING serves (whose
// in-memory providers were built from the previous config) must reload.
func ensureCredentialProvider(ctx context.Context, fs *sftpfs.FS, home string, opts *CredentialProxyOptions) (bool, error) {
	if opts == nil || strings.TrimSpace(opts.BaseURL) == "" || strings.TrimSpace(opts.Token) == "" ||
		strings.TrimSpace(opts.Provider) == "" || strings.TrimSpace(opts.Model) == "" {
		return false, fmt.Errorf("bootstrap: credential proxy options are incomplete")
	}
	kind := credentialProxyKind(opts)
	cfgPath := remoteConfigPath(home)
	data, _, _, rerr := fs.ReadFile(ctx, cfgPath, 1<<20)
	if rerr != nil && !isRemoteMissing(rerr) {
		return false, fmt.Errorf("bootstrap: read remote config: %w", rerr)
	}
	existing := string(data)
	// Defining ANY [[providers]] in the file replaces the built-in defaults,
	// so appending ours would leave a preset-relied default_model dangling
	// and the serve would crash at startup. Materialize the builtin provider
	// default_model refers to as an explicit entry first; default_model
	// itself is never rewritten.
	existing = materializeDefaultProvider(existing)
	if idx := providerBlockIndex(existing, opts.Provider); idx >= 0 {
		if providerBlockHasBaseURL(existing[idx:], opts.BaseURL) && providerBlockHasKind(existing[idx:], kind) {
			// Config is already current, but the .env token is healed
			// independently — an unchanged base_url must not skip it.
			envChanged, err := ensureCredentialToken(ctx, fs, home, opts.Token)
			if err != nil {
				return false, err
			}
			return envChanged, nil
		}
		if !providerBlockHasBaseURL(existing[idx:], opts.BaseURL) {
			updated, ok := replaceProviderBaseURL(existing, idx, opts.BaseURL)
			if !ok {
				return false, fmt.Errorf("bootstrap: remote config provider %q needs a manual base_url update", opts.Provider)
			}
			existing = updated
		}
		if !providerBlockHasKind(existing[idx:], kind) {
			updated, ok := replaceProviderKind(existing, idx, kind)
			if !ok {
				return false, fmt.Errorf("bootstrap: remote config provider %q needs a manual kind update", opts.Provider)
			}
			existing = updated
		}
	} else {
		existing += credentialProviderBlock(opts)
	}
	if err := fs.MkdirAll(ctx, path.Dir(cfgPath)); err != nil {
		return false, err
	}
	if err := fs.WriteFileAtomic(ctx, cfgPath, []byte(existing), 0o600); err != nil {
		return false, err
	}
	// Runtime credential resolution reads only the global .env file — never
	// the process environment the launch command seeds — so the virtual token
	// must live there or every provider call sends an empty key (401).
	if _, err := ensureCredentialToken(ctx, fs, home, opts.Token); err != nil {
		return false, err
	}
	return true, nil
}

// ensureCredentialToken idempotently writes the credential-proxy token into
// the remote global .env, preserving every other line. Reports whether the
// value was written or already current.
func ensureCredentialToken(ctx context.Context, fs *sftpfs.FS, home, token string) (bool, error) {
	envPath := path.Join(home, ".reasonix", ".env")
	data, _, _, rerr := fs.ReadFile(ctx, envPath, 1<<20)
	if rerr != nil && !isRemoteMissing(rerr) {
		return false, fmt.Errorf("bootstrap: read remote .env: %w", rerr)
	}
	lines := strings.Split(string(data), "\n")
	prefix := TokenEnvName + "="
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), prefix) {
			if strings.TrimSpace(line) == prefix+token {
				return false, nil
			}
			lines[i] = prefix + token
			updated := strings.Join(lines, "\n")
			return true, fs.WriteFileAtomic(ctx, envPath, []byte(updated), 0o600)
		}
	}
	// Append (creating the file when missing). Keep the trailing-newline
	// convention so later manual edits stay clean.
	content := string(data)
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += prefix + token + "\n"
	return true, fs.WriteFileAtomic(ctx, envPath, []byte(content), 0o600)
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

// providerBlockHasKind reports whether the block starting at idx contains the
// given kind assignment before its next table header.
func providerBlockHasKind(block, kind string) bool {
	want := "kind = " + tomlString(kind)
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

// replaceProviderKind swaps the kind line inside the block starting at idx,
// preserving everything else byte-for-byte.
func replaceProviderKind(text string, idx int, kind string) (string, bool) {
	rest := text[idx:]
	lines := strings.Split(rest, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if i > 0 && strings.HasPrefix(trimmed, "[") {
			break
		}
		if strings.HasPrefix(trimmed, "kind") && strings.Contains(trimmed, "=") {
			indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
			lines[i] = indent + "kind = " + tomlString(kind)
			return text[:idx] + strings.Join(lines, "\n"), true
		}
	}
	return text, false
}

// materializeDefaultProvider appends an explicit [[providers]] entry for the
// provider the top-level default_model refers to when that provider currently
// resolves only through the built-in defaults. Returns the text unchanged
// when default_model is absent, already defined in the file, or not a
// builtin. default_model itself is never rewritten — the remote's model
// choice stays exactly as the user configured it.
func materializeDefaultProvider(existing string) string {
	name := defaultModelProvider(existing)
	if name == "" || providerBlockIndex(existing, name) >= 0 {
		return existing
	}
	entry, ok := config.BuiltinProviderEntry(name)
	if !ok {
		return existing
	}
	return existing + providerEntryBlock(entry)
}

// defaultModelProvider extracts the provider part of the top-level
// default_model assignment: "deepseek-flash" → "deepseek-flash",
// "deepseek/deepseek-v4-flash" → "deepseek". Empty when absent. Scanning
// stops at the first table header — default_model is only meaningful at the
// top of the file.
func defaultModelProvider(text string) string {
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			return ""
		}
		after, ok := strings.CutPrefix(trimmed, "default_model")
		if !ok || !strings.HasPrefix(strings.TrimSpace(after), "=") {
			continue
		}
		value := firstQuoted(after)
		if value == "" {
			return ""
		}
		provider, _, _ := strings.Cut(value, "/")
		return strings.TrimSpace(provider)
	}
	return ""
}

// firstQuoted returns the first double-quoted substring of s, skipping a
// trailing inline comment.
func firstQuoted(s string) string {
	i := strings.Index(s, `"`)
	if i < 0 {
		return ""
	}
	j := strings.Index(s[i+1:], `"`)
	if j < 0 {
		return ""
	}
	return s[i+1 : i+1+j]
}

// providerEntryBlock renders a builtin ProviderEntry as a TOML block with the
// connection fields the serve needs (name/kind/base_url/model/api_key_env,
// plus the models list form); secrets stay in api_key_env as everywhere else.
func providerEntryBlock(p config.ProviderEntry) string {
	var b strings.Builder
	b.WriteString("\n[[providers]]\n")
	b.WriteString("# materialized from the built-in defaults by the desktop credential proxy — safe to delete\n")
	fmt.Fprintf(&b, "name = %s\n", tomlString(p.Name))
	fmt.Fprintf(&b, "kind = %s\n", tomlString(p.Kind))
	fmt.Fprintf(&b, "base_url = %s\n", tomlString(p.BaseURL))
	if p.Model != "" {
		fmt.Fprintf(&b, "model = %s\n", tomlString(p.Model))
	}
	if len(p.Models) > 0 {
		quoted := make([]string, len(p.Models))
		for i, m := range p.Models {
			quoted[i] = tomlString(m)
		}
		fmt.Fprintf(&b, "models = [%s]\n", strings.Join(quoted, ", "))
		if p.Default != "" {
			fmt.Fprintf(&b, "default = %s\n", tomlString(p.Default))
		}
	}
	if p.APIKeyEnv != "" {
		fmt.Fprintf(&b, "api_key_env = %s\n", tomlString(p.APIKeyEnv))
	}
	if p.BalanceURL != "" {
		fmt.Fprintf(&b, "balance_url = %s\n", tomlString(p.BalanceURL))
	}
	return b.String()
}
