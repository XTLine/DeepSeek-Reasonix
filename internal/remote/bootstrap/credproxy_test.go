package bootstrap

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"reasonix/internal/remote"
)

func credProxyOpts(baseURL string) *CredentialProxyOptions {
	return &CredentialProxyOptions{
		BaseURL:  baseURL,
		Token:    "virtual-token-123",
		Provider: "reasonix-desktop-proxy",
		Model:    "deepseek-v4-flash",
	}
}

// TestEnsureCredentialProviderAppendsAndIsIdempotent: a fresh remote gains the
// provider block; a second run with the same options leaves the file
// byte-identical; a base_url change rewrites just that line.
func TestEnsureCredentialProviderAppendsAndIsIdempotent(t *testing.T) {
	skipOnWindows(t)
	root := t.TempDir()
	conn := newFakeConn(t, root, func(string) (remote.ExecResult, error) { return ok("") })
	fs, err := conn.SFTP()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if _, err := ensureCredentialProvider(ctx, fs, root, credProxyOpts("http://127.0.0.1:18999")); err != nil {
		t.Fatalf("first install: %v", err)
	}
	first, rerr := os.ReadFile(filepath.Join(root, ".reasonix", "config.toml"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	for _, want := range []string{
		`[[providers]]`,
		`name = "reasonix-desktop-proxy"`,
		`base_url = "http://127.0.0.1:18999"`,
		`api_key_env = "REASONIX_PROXY_TOKEN"`,
		`model = "deepseek-v4-flash"`,
	} {
		if !strings.Contains(string(first), want) {
			t.Fatalf("config missing %q:\n%s", want, first)
		}
	}

	// Idempotent: same options ⇒ no rewrite.
	if _, err := ensureCredentialProvider(ctx, fs, root, credProxyOpts("http://127.0.0.1:18999")); err != nil {
		t.Fatalf("second install: %v", err)
	}
	second, _ := os.ReadFile(filepath.Join(root, ".reasonix", "config.toml"))
	if string(first) != string(second) {
		t.Fatalf("idempotent run rewrote the config:\n%s\n---\n%s", first, second)
	}

	// A base_url change (tunnel port moved) rewrites only that assignment.
	if _, err := ensureCredentialProvider(ctx, fs, root, credProxyOpts("http://127.0.0.1:19000")); err != nil {
		t.Fatalf("port change: %v", err)
	}
	third, _ := os.ReadFile(filepath.Join(root, ".reasonix", "config.toml"))
	if !strings.Contains(string(third), `base_url = "http://127.0.0.1:19000"`) {
		t.Fatalf("port change not applied:\n%s", third)
	}
	if strings.Count(string(third), "[[providers]]") != 1 {
		t.Fatalf("port change duplicated the block:\n%s", third)
	}
}

// TestEnsureCredentialProviderPreservesUserConfig: an existing user config
// keeps its content; the block appends at the end.
func TestEnsureCredentialProviderPreservesUserConfig(t *testing.T) {
	skipOnWindows(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".reasonix"), 0o755); err != nil {
		t.Fatal(err)
	}
	existing := "default_model = \"deepseek/deepseek-v4-flash\"\n\n[[providers]]\nname = \"mine\"\nkind = \"openai\"\nbase_url = \"https://api.deepseek.com\"\napi_key_env = \"MY_KEY\"\n"
	if err := os.WriteFile(filepath.Join(root, ".reasonix", "config.toml"), []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	conn := newFakeConn(t, root, func(string) (remote.ExecResult, error) { return ok("") })
	fs, err := conn.SFTP()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ensureCredentialProvider(context.Background(), fs, root, credProxyOpts("http://127.0.0.1:18999")); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(root, ".reasonix", "config.toml"))
	if !strings.HasPrefix(string(got), existing) {
		t.Fatalf("user config prefix disturbed:\n%s", got)
	}
	if strings.Count(string(got), "[[providers]]") != 2 {
		t.Fatalf("expected two provider blocks:\n%s", got)
	}
	if idx := providerBlockIndex(string(got), "mine"); idx < 0 {
		t.Fatalf("user provider block lost:\n%s", got)
	}
}

// TestLaunchCommandCredentialInjection: the virtual token rides the
// environment (quoted) and the serve selects the tunnel-backed provider.
func TestLaunchCommandCredentialInjection(t *testing.T) {
	paths := StatePaths{Dir: "/d", TokenFile: "/d/t", PortFile: "/d/p", PidFile: "/d/i", LogFile: "/d/l"}
	cmd := LaunchCommand("/usr/bin/reasonix", "/ws", paths, &CredentialProxyOptions{
		BaseURL: "http://127.0.0.1:18999", Token: "to'ken $x", Provider: "reasonix-desktop-proxy", Model: "m",
	})
	for _, want := range []string{
		`REASONIX_PROXY_TOKEN='to'\''ken $x'`,
		`--model 'reasonix-desktop-proxy'`,
		`nohup '/usr/bin/reasonix' serve`,
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("LaunchCommand missing %q:\n%s", want, cmd)
		}
	}
	// Token must not appear unquoted anywhere else (never bare in argv).
	if strings.Contains(cmd, " to'ken") && !strings.Contains(cmd, "REASONIX_PROXY_TOKEN=") {
		t.Errorf("token leaked outside the env assignment:\n%s", cmd)
	}
}

// TestEnsureCredentialProviderMaterializesBuiltinDefault: a remote whose
// default_model resolves only through the built-in defaults gains an explicit
// entry for it before ours — appending ours alone would disable the builtins
// and crash the serve at startup. default_model is never rewritten and the
// second run is byte-stable.
func TestEnsureCredentialProviderMaterializesBuiltinDefault(t *testing.T) {
	skipOnWindows(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".reasonix"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "config_version = 6\n\ndefault_model = \"deepseek-flash\"   # user's choice stays\n\n[ui]\ntheme = \"dark\"\n"
	if err := os.WriteFile(filepath.Join(root, ".reasonix", "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	conn := newFakeConn(t, root, func(string) (remote.ExecResult, error) { return ok("") })
	fs, err := conn.SFTP()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := ensureCredentialProvider(ctx, fs, root, credProxyOpts("http://127.0.0.1:18999")); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(filepath.Join(root, ".reasonix", "config.toml"))
	got := string(first)
	for _, want := range []string{
		`default_model = "deepseek-flash"   # user's choice stays`,
		`name = "deepseek-flash"`,
		`api_key_env = "DEEPSEEK_API_KEY"`,
		`name = "reasonix-desktop-proxy"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("config missing %q:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "[[providers]]"); n != 2 {
		t.Fatalf("provider blocks = %d, want 2 (materialized + ours):\n%s", n, got)
	}
	// The materialized entry must come BEFORE ours so file order reads
	// user-default then desktop-proxy.
	if strings.Index(got, `name = "deepseek-flash"`) > strings.Index(got, `name = "reasonix-desktop-proxy"`) {
		t.Fatalf("materialized entry should precede ours:\n%s", got)
	}

	// Idempotent.
	if _, err := ensureCredentialProvider(ctx, fs, root, credProxyOpts("http://127.0.0.1:18999")); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(filepath.Join(root, ".reasonix", "config.toml"))
	if string(first) != string(second) {
		t.Fatalf("second run rewrote the config:\n%s\n---\n%s", first, second)
	}
}

// TestMaterializeDefaultProviderSkipsNonBuiltin: a default_model whose
// provider is neither in the file nor a builtin stays untouched — the gap is
// user-owned, not ours to invent.
func TestMaterializeDefaultProviderSkipsNonBuiltin(t *testing.T) {
	before := "default_model = \"custom/pro-model\"\n"
	after := materializeDefaultProvider(before)
	if before != after {
		t.Fatalf("non-builtin default was rewritten:\n%s", after)
	}
	if got := defaultModelProvider("default_model = \"deepseek/deepseek-v4-flash\"\n"); got != "deepseek" {
		t.Fatalf("provider extraction = %q, want deepseek", got)
	}
	if got := defaultModelProvider("[ui]\ndefault_model = \"deepseek-flash\"\n"); got != "" {
		t.Fatalf("table-scoped default_model leaked: %q", got)
	}
}

// TestEnsureCredentialProviderRewritesKindDrift: an existing block whose kind
// no longer matches the desktop provider behind the proxy (the desktop
// switched from an openai-kind to an anthropic-kind provider) is rewritten
// in place; a matching re-run stays byte-identical.
func TestEnsureCredentialProviderRewritesKindDrift(t *testing.T) {
	skipOnWindows(t)
	root := t.TempDir()
	conn := newFakeConn(t, root, func(string) (remote.ExecResult, error) { return ok("") })
	fs, err := conn.SFTP()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if _, err := ensureCredentialProvider(ctx, fs, root, credProxyOpts("http://127.0.0.1:18999")); err != nil {
		t.Fatal(err)
	}
	installed, _ := os.ReadFile(filepath.Join(root, ".reasonix", "config.toml"))
	if !strings.Contains(string(installed), "kind = \"openai\"") {
		t.Fatalf("default install missing the openai kind:\n%s", installed)
	}

	switched := credProxyOpts("http://127.0.0.1:18999")
	switched.Kind = "anthropic"
	if _, err := ensureCredentialProvider(ctx, fs, root, switched); err != nil {
		t.Fatal(err)
	}
	rewritten, _ := os.ReadFile(filepath.Join(root, ".reasonix", "config.toml"))
	if !strings.Contains(string(rewritten), "kind = \"anthropic\"") || strings.Contains(string(rewritten), "kind = \"openai\"") {
		t.Fatalf("kind drift not rewritten:\n%s", rewritten)
	}
	if !strings.Contains(string(rewritten), "base_url = \"http://127.0.0.1:18999\"") {
		t.Fatalf("base_url lost in the kind rewrite:\n%s", rewritten)
	}

	// Idempotent once the kind matches.
	if _, err := ensureCredentialProvider(ctx, fs, root, switched); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(filepath.Join(root, ".reasonix", "config.toml"))
	if string(again) != string(rewritten) {
		t.Fatalf("matching re-run rewrote the config:\n%s\n---\n%s", rewritten, again)
	}
}
