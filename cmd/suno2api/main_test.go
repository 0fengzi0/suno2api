package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"suno2api/internal/generate"
	"suno2api/internal/session"
)

func TestSubmittedRequestIDIsPrintedEvenWhenNoClipsWereReturned(t *testing.T) {
	var output bytes.Buffer
	response := generate.Response{ID: "accepted-request-id", RequestedModel: "chirp-goose"}
	if err := writeSubmittedResponse(&output, response); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Submitted bool              `json:"submitted"`
		Response  generate.Response `json:"response"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Submitted || result.Response.ID != response.ID {
		t.Fatal("accepted generation request ID was lost, making recovery impossible")
	}
	output.Reset()
	if err := writeSubmittedResponse(&output, generate.Response{}); err != nil || output.Len() != 0 {
		t.Fatal("a pre-submission failure was incorrectly reported as submitted")
	}
}

type canceledKeepalive struct{ sawDeadline bool }

func (runner *canceledKeepalive) Run(ctx context.Context, event func(session.Event)) error {
	_, runner.sawDeadline = ctx.Deadline()
	return ctx.Err()
}

func TestKeepaliveUsesLifetimeContextAndTreatsCtrlCAsNormalStop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runner := &canceledKeepalive{}
	if err := runKeepalive(ctx, runner, nil); err != nil {
		t.Fatal("canceled foreground keepalive was treated as a failure")
	}
	if runner.sawDeadline {
		t.Fatal("ordinary command timeout shortened keepalive lifetime")
	}
}

func TestSessionStatusDoesNotRefreshOrExposeSessionDetails(t *testing.T) {
	root := t.TempDir()
	claims := map[string]any{"iss": "https://auth.suno.com", "sid": "private-session-id", "sub": "private-user-id", "iat": time.Now().Add(-time.Minute).Unix(), "exp": time.Now().Add(time.Hour).Unix()}
	encoded, _ := json.Marshal(claims)
	token := "eyJhbGciOiJIUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(encoded) + ".signature"
	tokenPath := filepath.Join(root, "session.token")
	cookie := "ajs_anonymous_id=device-id; __session=" + token
	if err := os.WriteFile(tokenPath, []byte(cookie), 0600); err != nil {
		t.Fatal(err)
	}
	configuration, _ := json.Marshal(diskConfig{TokenFile: tokenPath, Proxy: "direct"})
	configPath := filepath.Join(root, "config.json")
	if err := os.WriteFile(configPath, configuration, 0600); err != nil {
		t.Fatal(err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = writer
	runError := run([]string{"session", "status", "--config", configPath})
	writer.Close()
	os.Stdout = original
	output, readError := io.ReadAll(reader)
	reader.Close()
	if runError != nil || readError != nil {
		t.Fatalf("local status failed: %v %v", runError, readError)
	}
	var status session.Status
	if err := json.Unmarshal(output, &status); err != nil || status.ExpiresAt.IsZero() {
		t.Fatalf("missing safe status: %v", err)
	}
	for _, private := range []string{token, "private-session-id", "private-user-id", "ajs_anonymous_id", "Authorization"} {
		if bytes.Contains(output, []byte(private)) {
			t.Fatal("session status exposed a private value")
		}
	}
	after, _ := os.ReadFile(tokenPath)
	if string(after) != cookie {
		t.Fatal("session status changed the Cookie file")
	}
}

func TestSessionConfigCanDisableAutomaticRefresh(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.json")
	if err := os.WriteFile(path, []byte(`{"auto_refresh":false,"refresh_before":"90s","token_file":"session.token","auth_cookie_file":"auth.cookie"}`), 0600); err != nil {
		t.Fatal(err)
	}
	configuration, _, err := readConfig(path)
	if err != nil || configuration.AutoRefresh == nil || *configuration.AutoRefresh || configuration.RefreshBefore != "90s" {
		t.Fatalf("session config ignored: %v", err)
	}
	if configuration.AuthCookieFile != filepath.Join(root, "auth.cookie") {
		t.Fatal("authentication Cookie path did not resolve relative to its config file")
	}
	clients, err := createClients(commonFlags{timeout: time.Second, proxy: "direct", tokenFile: configuration.TokenFile, autoRefresh: false, refreshBefore: 90 * time.Second})
	if err != nil || clients.sessionManager != nil {
		t.Fatalf("manual-only mode created an active session manager: %v", err)
	}
}

func TestSharedSessionProviderPreservesAuthenticationAndRateLimitMeaning(t *testing.T) {
	for _, test := range []struct {
		source, target error
	}{
		{session.ErrAuthentication, generate.ErrAuthentication},
		{session.ErrRefreshCredentialsRequired, generate.ErrAuthentication},
		{session.ErrUnsupportedCredential, generate.ErrAuthentication},
		{session.ErrRateLimited, generate.ErrRateLimited},
		{context.Canceled, context.Canceled},
	} {
		provider := apiTokenProvider(func(context.Context) (string, error) { return "", test.source })
		token, err := provider(context.Background())
		if token != "" || !errors.Is(err, test.source) || !errors.Is(err, test.target) {
			t.Fatal("session failure lost its API status or underlying cause")
		}
	}
}

func TestInterspersedFlagsSupportShareURLFirst(t *testing.T) {
	flags := flag.NewFlagSet("download", flag.ContinueOnError)
	var output string
	var authorize bool
	flags.StringVar(&output, "o", "", "")
	flags.BoolVar(&authorize, "authorize-download", false, "")
	if err := parseFlags(flags, []string{"https://suno.com/s/example", "-o", "my music/", "--authorize-download"}); err != nil {
		t.Fatal(err)
	}
	if output != "my music/" || !authorize || flags.NArg() != 1 {
		t.Fatal("URL-first download options lost")
	}
}

func TestConfiguredDownloadDirectoryIsCreatedButExplicitFileIsNot(t *testing.T) {
	root := t.TempDir()
	configured := filepath.Join(root, "not-yet-created", "downloads")
	output, err := downloadOutput("", configured)
	if err != nil || output != configured {
		t.Fatalf("configured output failed: %s %v", output, err)
	}
	info, err := os.Stat(configured)
	if err != nil || !info.IsDir() {
		t.Fatal("configured download path was not kept as a directory")
	}
	explicit := filepath.Join(root, "song.mp3")
	output, err = downloadOutput(explicit, configured)
	if err != nil || output != explicit {
		t.Fatal("explicit filename changed")
	}
	if _, err := os.Stat(explicit); !os.IsNotExist(err) {
		t.Fatal("explicit output file was created as a directory")
	}
}

func TestRemoteListenerRequiresSufficientAPIKey(t *testing.T) {
	for _, address := range []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080"} {
		if err := validateListen(address, ""); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []string{"", "short"} {
		if validateListen("0.0.0.0:8080", key) == nil {
			t.Fatal("remote listener without strong key accepted")
		}
	}
	if err := validateListen("0.0.0.0:8080", "sufficient-test-api-key"); err != nil {
		t.Fatal(err)
	}
}

func clearEnv(t *testing.T, key string) {
	t.Helper()
	original, existed := os.LookupEnv(key)
	os.Unsetenv(key)
	t.Cleanup(func() {
		if existed {
			os.Setenv(key, original)
		} else {
			os.Unsetenv(key)
		}
	})
}

func TestEnvFileConfiguresGatewayAndProcessEnvironmentWins(t *testing.T) {
	root := t.TempDir()
	envPath := filepath.Join(root, ".env")
	content := "# gateway defaults\nexport SUNO_GATEWAY_LISTEN=127.0.0.1:8181\nSUNO_GATEWAY_API_STYLE=\"openai\"\nSUNO_GATEWAY_DATA_DIR=data/gw # relative to the working directory\nSUNO_GATEWAY_ADMIN_KEY_FILE=admin.key\nSUNO_MODEL=v6-mini\nSUNO_REQUEST_TIMEOUT=90s\n"
	if err := os.WriteFile(envPath, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"SUNO_GATEWAY_LISTEN", "SUNO_GATEWAY_API_STYLE", "SUNO_API_STYLE", "SUNO_GATEWAY_DATA_DIR", "SUNO_GATEWAY_ADMIN_KEY_FILE", "SUNO_REQUEST_TIMEOUT"} {
		clearEnv(t, key)
	}
	t.Setenv("SUNO_MODEL", "v6")
	used, err := loadEnvFile(envPath, "")
	if err != nil || used != envPath {
		t.Fatalf("env file not loaded: %v", err)
	}
	var configuration diskConfig
	if err := applyEnvironment(&configuration); err != nil {
		t.Fatal(err)
	}
	if configuration.GatewayListen != "127.0.0.1:8181" || configuration.GatewayAPIStyle != "openai" || configuration.GatewayDataDir != "data/gw" || configuration.GatewayAdminKeyFile != "admin.key" || configuration.RequestTimeout != "90s" {
		t.Fatalf("env configuration not applied: %#v", configuration)
	}
	if configuration.Model != "v6" {
		t.Fatal("the process environment must win over .env")
	}
	timeout, err := configuredTimeout(configuration.RequestTimeout)
	if err != nil || timeout != 90*time.Second {
		t.Fatalf("request timeout not parsed: %v", err)
	}
	if _, err := configuredTimeout("not-a-duration"); err == nil {
		t.Fatal("invalid request timeout accepted")
	}
}

func TestEnvFileParsingHandlesQuotesCommentsAndExports(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	content := "A=plain value\nB=\"quoted value\"\nC=value # comment\nexport D='single value'\nE=\"line\\nbreak\"\ninvalid line\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	values, err := parseEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"A": "plain value", "B": "quoted value", "C": "value", "D": "single value", "E": "line\nbreak"} {
		if values[key] != want {
			t.Fatalf("%s = %q, want %q", key, values[key], want)
		}
	}
}
