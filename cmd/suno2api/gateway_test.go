package main

import (
	"context"
	"errors"
	"flag"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGatewayRemoteBindingRequiresExplicitListenAndOwnAdministratorKey(t *testing.T) {
	key := strings.Repeat("k", 32)
	for _, address := range []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080"} {
		if err := validateGatewayListen(address, "", false, false); err != nil {
			t.Fatalf("loopback gateway rejected: %v", err)
		}
	}
	for _, test := range []struct {
		address                     string
		key                         string
		explicitKey, explicitListen bool
	}{
		{"0.0.0.0:8080", "", false, true},
		{"0.0.0.0:8080", key, false, true},
		{"0.0.0.0:8080", key, true, false},
		{"[::]:8080", "short", true, true},
		{"bad-address", key, true, true},
	} {
		if validateGatewayListen(test.address, test.key, test.explicitKey, test.explicitListen) == nil {
			t.Fatal("gateway accepted an implicit or unprotected remote listener")
		}
	}
	if err := validateGatewayListen("0.0.0.0:8080", key, true, true); err != nil {
		t.Fatal(err)
	}
}

func TestGatewayAdministratorKeyNeverUsesLegacyPublicAPIKey(t *testing.T) {
	t.Setenv("SUNO_API_KEY", strings.Repeat("legacy", 8))
	t.Setenv("SUNO_ADMIN_KEY", "")
	key, source, err := gatewayAdminKey("")
	if err != nil || key != "" || source != "generated" {
		t.Fatal("gateway incorrectly used the old single-account public API key")
	}
	path := filepath.Join(t.TempDir(), "admin.key")
	if err := os.WriteFile(path, []byte("short-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := gatewayAdminKey(path); err == nil || strings.Contains(err.Error(), "short-secret") {
		t.Fatal("weak administrator key accepted or exposed")
	}
	fileKey := strings.Repeat("a", 40)
	if err := os.WriteFile(path, []byte(fileKey+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUNO_ADMIN_KEY", strings.Repeat("b", 40))
	key, source, err = gatewayAdminKey(path)
	if err != nil || key != fileKey || source != "administrator key file" {
		t.Fatal("explicit administrator key file did not take precedence")
	}
}

func TestGatewayConfigPathsAndHelpDoNotImportOrStartTheService(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"gateway_data_dir":"managed","gateway_admin_key_file":"admin.key","gateway_listen":"127.0.0.1:8181"}`), 0600); err != nil {
		t.Fatal(err)
	}
	config, _, err := readConfig(configPath)
	if err != nil || config.GatewayDataDir != filepath.Join(root, "managed") || config.GatewayAdminKeyFile != filepath.Join(root, "admin.key") || config.GatewayListen != "127.0.0.1:8181" {
		t.Fatalf("gateway configuration did not resolve: %v", err)
	}
	if err := run([]string{"gateway", "--config", configPath, "--help"}); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("gateway help failed: %v", err)
	}
	if _, err := os.Stat(config.GatewayDataDir); !os.IsNotExist(err) {
		t.Fatal("help created managed credentials or initialized the service")
	}
}

func TestGatewayExplicitImportReadsOnlyTheSelectedBoundedFiles(t *testing.T) {
	root := t.TempDir()
	cookieFile := filepath.Join(root, "session.cookie")
	authFile := filepath.Join(root, "auth.cookie")
	if err := os.WriteFile(cookieFile, []byte("Cookie: __session=fixture-session; ajs_anonymous_id=fixture-device"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(authFile, []byte("__client=fixture-client"), 0600); err != nil {
		t.Fatal(err)
	}
	input, err := currentGatewayAccount(commonFlags{tokenFile: cookieFile, authCookieFile: authFile, protocol: "web", defaultModel: "v6-mini"})
	if err != nil || !strings.Contains(input.Cookie, "fixture-session") || input.AuthCookie != "__client=fixture-client" || input.Protocol != "web" || input.Model != "v6-mini" {
		t.Fatalf("explicit import lost selected input: %v", err)
	}
	if err := os.WriteFile(authFile, []byte(strings.Repeat("x", 65537)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := currentGatewayAccount(commonFlags{tokenFile: cookieFile, authCookieFile: authFile}); err == nil {
		t.Fatal("import accepted a credential file over 64 KiB")
	}
}

type gatewayCancellationFixture struct {
	started chan struct{}
	stopped chan struct{}
	closed  chan struct{}
}

func (service *gatewayCancellationFixture) Handler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { writer.WriteHeader(http.StatusOK) })
}
func (service *gatewayCancellationFixture) Run(ctx context.Context) error {
	close(service.started)
	<-ctx.Done()
	close(service.stopped)
	return ctx.Err()
}
func (service *gatewayCancellationFixture) Close() error {
	close(service.closed)
	return nil
}

func TestGatewayShutdownCancelsAccountLifecycleAndClosesHTTP(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service := &gatewayCancellationFixture{started: make(chan struct{}), stopped: make(chan struct{}), closed: make(chan struct{})}
	result := make(chan error, 1)
	go func() { result <- serveGateway(ctx, listener, service, time.Second) }()
	select {
	case <-service.started:
	case <-time.After(time.Second):
		t.Fatal("gateway account lifecycle did not start")
	}
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("normal gateway cancellation failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("gateway did not stop after cancellation")
	}
	select {
	case <-service.stopped:
	case <-time.After(time.Second):
		t.Fatal("account lifecycle remained running")
	}
	select {
	case <-service.closed:
	default:
		t.Fatal("gateway managed resources were not closed")
	}
	if connection, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second); err == nil {
		connection.Close()
		t.Fatal("gateway HTTP listener remained open")
	}
}
