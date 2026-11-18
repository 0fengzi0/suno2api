package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"suno2api/internal/gateway"
	"suno2api/internal/gateway/web"
	"suno2api/internal/netconfig"
)

func gatewayCommand(ctx context.Context, flags *flag.FlagSet, arguments []string, configuration diskConfig, common *commonFlags) error {
	listen := configuration.GatewayListen
	if listen == "" {
		listen = "127.0.0.1:8080"
	}
	dataDir := configuration.GatewayDataDir
	if dataDir == "" {
		dataDir = "data/gateway"
	}
	adminKeyFile := configuration.GatewayAdminKeyFile
	apiStyle := configuration.GatewayAPIStyle
	importCurrent := false
	flags.StringVar(&listen, "listen", listen, "gateway HTTP listen address; remote binding requires explicit --listen and an administrator key")
	flags.StringVar(&dataDir, "data-dir", dataDir, "gateway private account, key, and job storage directory")
	flags.StringVar(&adminKeyFile, "admin-key-file", adminKeyFile, "file containing an existing administrator key; otherwise SUNO_ADMIN_KEY or a generated key is used")
	flags.StringVar(&apiStyle, "api-style", apiStyle, "public API style: suno for /api/*, openai for /v1/*, or both")
	flags.BoolVar(&importCurrent, "import-current", false, "explicitly import the currently configured Suno Cookie files, preserving an existing matching account")
	if err := parseFlags(flags, arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("gateway accepts named flags only")
	}
	if common.timeout <= 0 {
		return errors.New("timeout must be positive")
	}
	if strings.TrimSpace(dataDir) == "" {
		return errors.New("data-dir must not be empty")
	}
	key, keySource, err := gatewayAdminKey(adminKeyFile)
	if err != nil {
		return err
	}
	listenExplicit := false
	flags.Visit(func(selected *flag.Flag) {
		if selected.Name == "listen" {
			listenExplicit = true
		}
	})
	if err := validateGatewayListen(listen, key, keySource != "generated", listenExplicit); err != nil {
		return err
	}
	httpClient, proxySource, err := netconfig.NewHTTPClient(common.proxy)
	if err != nil {
		return err
	}
	service, err := gateway.New(gateway.Config{DataDir: dataDir, HTTPClient: httpClient, AdminKey: key, RequestTimeout: common.timeout, DefaultModel: common.defaultModel, DefaultProtocol: common.protocol, APIStyle: apiStyle, WebHandler: web.Handler()})
	if err != nil {
		return err
	}
	closeOnReturn := true
	defer func() {
		if closeOnReturn {
			_ = service.Close()
		}
	}()
	if importCurrent {
		input, err := currentGatewayAccount(*common)
		if err != nil {
			return err
		}
		operation, cancel := context.WithTimeout(ctx, common.timeout)
		_, err = service.ImportAccount(operation, input)
		cancel()
		if err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "Current account imported into gateway-managed storage.")
	}
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	if keySource == "generated" {
		fmt.Fprintf(os.Stderr, "Gateway administrator key file: %s\n", service.AdminKeyPath())
	} else {
		fmt.Fprintf(os.Stderr, "Gateway administrator key source: %s\n", keySource)
	}
	fmt.Fprintf(os.Stderr, "Suno gateway listening on http://%s (%s). API style: %s. Data: %s\n", listener.Addr(), proxySource, service.APIStyle(), service.DataDir())
	fmt.Fprintln(os.Stderr, "Open the address above, log in with the administrator key, then add accounts and API keys.")
	closeOnReturn = false
	return serveGateway(ctx, listener, service, common.timeout)
}

func gatewayAdminKey(path string) (string, string, error) {
	var key, source string
	if path != "" {
		data, err := readSmallFile(path, 4096)
		if err != nil {
			return "", "", errors.New("cannot read administrator key file")
		}
		key = strings.TrimSpace(string(data))
		source = "administrator key file"
	} else {
		key = strings.TrimSpace(os.Getenv("SUNO_ADMIN_KEY"))
		if key == "" {
			return "", "generated", nil
		}
		source = "SUNO_ADMIN_KEY"
	}
	if len(key) < 32 || len(key) > 4096 || strings.ContainsAny(key, "\r\n\t ") {
		return "", "", errors.New("administrator key must contain at least 32 characters without whitespace")
	}
	return key, source, nil
}

func validateGatewayListen(address, adminKey string, explicitKey, explicitListen bool) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("gateway listen must be host:port")
	}
	ip := net.ParseIP(host)
	loopback := strings.EqualFold(host, "localhost") || ip != nil && ip.IsLoopback()
	if !loopback && (!explicitListen || !explicitKey || len(adminKey) < 32) {
		return errors.New("remote gateway binding requires explicit --listen and --admin-key-file or SUNO_ADMIN_KEY with at least 32 characters")
	}
	return nil
}

func currentGatewayAccount(common commonFlags) (gateway.AccountInput, error) {
	path := common.tokenFile
	if path == "" {
		path = os.Getenv("SUNO_TOKEN_FILE")
	}
	if path == "" {
		return gateway.AccountInput{}, errors.New("--import-current requires --token-file, configured token_file, or SUNO_TOKEN_FILE")
	}
	cookie, err := readSmallFile(path, 64<<10)
	if err != nil {
		return gateway.AccountInput{}, errors.New("cannot read the current Suno Cookie file")
	}
	authPath := common.authCookieFile
	if authPath == "" {
		authPath = os.Getenv("SUNO_AUTH_COOKIE_FILE")
	}
	var auth []byte
	if authPath != "" {
		auth, err = readSmallFile(authPath, 64<<10)
		if err != nil {
			return gateway.AccountInput{}, errors.New("cannot read the current authentication Cookie file")
		}
	}
	return gateway.AccountInput{Name: "导入的当前账号", Cookie: string(cookie), AuthCookie: string(auth), Protocol: common.protocol, Model: common.defaultModel}, nil
}

type gatewayLifecycle interface {
	Handler() http.Handler
	Run(context.Context) error
	Close() error
}

func serveGateway(parent context.Context, listener net.Listener, service gatewayLifecycle, timeout time.Duration) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	server := &http.Server{Handler: service.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: timeout + 30*time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 32 << 10}
	background := make(chan error, 1)
	go func() {
		background <- service.Run(ctx)
	}()
	serving := make(chan error, 1)
	go func() {
		serving <- server.Serve(listener)
	}()
	var result error
	backgroundFinished := false
	select {
	case <-parent.Done():
	case err := <-serving:
		if !errors.Is(err, http.ErrServerClosed) {
			result = err
		}
	case err := <-background:
		backgroundFinished = true
		if err != nil && !errors.Is(err, context.Canceled) {
			result = err
		} else if parent.Err() == nil {
			result = errors.New("gateway account lifecycle stopped unexpectedly")
		}
	}
	cancel()
	shutdown, release := context.WithTimeout(context.Background(), 10*time.Second)
	defer release()
	if err := server.Shutdown(shutdown); err != nil {
		_ = server.Close()
		if result == nil {
			result = err
		}
	}
	if err := service.Close(); err != nil && result == nil {
		result = err
	}
	if !backgroundFinished {
		select {
		case err := <-background:
			if err != nil && !errors.Is(err, context.Canceled) && result == nil {
				result = err
			}
		case <-shutdown.Done():
			if result == nil {
				result = errors.New("gateway account lifecycle did not stop before shutdown completed")
			}
		}
	}
	return result
}
