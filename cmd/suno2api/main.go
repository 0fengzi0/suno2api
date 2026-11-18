package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"suno2api/internal/download"
	"suno2api/internal/generate"
	"suno2api/internal/netconfig"
	"suno2api/internal/server"
	"suno2api/internal/session"
)

type diskConfig struct {
	Protocol            string `json:"protocol"`
	DeviceID            string `json:"device_id,omitempty"`
	Model               string `json:"model"`
	TokenFile           string `json:"token_file"`
	AuthCookieFile      string `json:"auth_cookie_file,omitempty"`
	Proxy               string `json:"proxy"`
	DownloadDir         string `json:"download_dir"`
	Listen              string `json:"listen"`
	RequestTimeout      string `json:"request_timeout,omitempty"`
	AutoRefresh         *bool  `json:"auto_refresh,omitempty"`
	RefreshBefore       string `json:"refresh_before,omitempty"`
	GatewayDataDir      string `json:"gateway_data_dir,omitempty"`
	GatewayListen       string `json:"gateway_listen,omitempty"`
	GatewayAdminKeyFile string `json:"gateway_admin_key_file,omitempty"`
	GatewayAPIStyle     string `json:"gateway_api_style,omitempty"`
}

type commonFlags struct {
	protocol, deviceID           string
	defaultModel                 string
	tokenFile, proxy, configPath string
	authCookieFile               string
	timeout                      time.Duration
	authorize                    bool
	autoRefresh                  bool
	refreshBefore                time.Duration
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	if len(arguments) > 0 && (arguments[0] == "help" || arguments[0] == "--help" || arguments[0] == "-h") {
		printHelp()
		return nil
	}
	// The binary is a gateway web application by default; explicit commands remain available.
	if len(arguments) == 0 {
		arguments = []string{"gateway"}
	} else if strings.HasPrefix(arguments[0], "-") {
		// Allow "suno2api --listen ..." as the gateway command.
		arguments = append([]string{"gateway"}, arguments...)
	}
	command := arguments[0]
	arguments = arguments[1:]
	if strings.HasPrefix(command, "https://") {
		arguments = append([]string{command}, arguments...)
		command = "download"
	}
	configPath := optionValue(arguments, "config")
	if configPath == "" {
		configPath = strings.TrimSpace(os.Getenv("SUNO_CONFIG"))
	}
	configuration, actualConfig, err := readConfig(configPath)
	if err != nil {
		return err
	}
	envFile, err := loadEnvFile(optionValue(arguments, "env-file"), actualConfig)
	if err != nil {
		return err
	}
	if err := applyEnvironment(&configuration); err != nil {
		return err
	}
	requestTimeout, err := configuredTimeout(configuration.RequestTimeout)
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	protocol := configuration.Protocol
	if protocol == "" {
		protocol = "android"
	}
	common := commonFlags{defaultModel: configuration.Model}
	autoRefresh := true
	if configuration.AutoRefresh != nil {
		autoRefresh = *configuration.AutoRefresh
	}
	refreshBefore := time.Minute
	if configuration.RefreshBefore != "" {
		refreshBefore, err = time.ParseDuration(configuration.RefreshBefore)
		if err != nil || refreshBefore <= 0 {
			return errors.New("refresh_before must be a positive duration, for example 60s")
		}
	}
	flags.StringVar(&common.protocol, "protocol", protocol, "Suno protocol: android for the app API (default), web for the website API fallback")
	flags.StringVar(&common.deviceID, "device-id", configuration.DeviceID, "web Device-Id; default reads ajs_anonymous_id from your Cookie")
	flags.StringVar(&common.tokenFile, "token-file", configuration.TokenFile, "file containing your Suno token or Cookie")
	flags.StringVar(&common.authCookieFile, "auth-cookie-file", configuration.AuthCookieFile, "optional Cookie file from your own Suno authentication service session")
	flags.StringVar(&common.proxy, "proxy", configuration.Proxy, "HTTP/HTTPS/SOCKS proxy URL, or direct")
	flags.StringVar(&common.configPath, "config", actualConfig, "local JSON configuration file")
	flags.StringVar(&envFile, "env-file", envFile, "optional .env file; defaults to .env beside the configuration file or executable")
	flags.DurationVar(&common.timeout, "timeout", requestTimeout, "maximum operation time")
	flags.BoolVar(&common.authorize, "authorize-download", false, "allow an explicit Suno download authorization request, which may consume quota")
	flags.BoolVar(&common.autoRefresh, "auto-refresh", autoRefresh, "refresh an eligible Cookie file shortly before session expiry")
	flags.DurationVar(&common.refreshBefore, "refresh-before", refreshBefore, "refresh this long before session expiry")
	ctx, cancelSignal := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancelSignal()
	switch command {
	case "gateway":
		return gatewayCommand(ctx, flags, arguments, configuration, &common)
	case "session":
		if err := parseFlags(flags, arguments); err != nil {
			return err
		}
		if flags.NArg() != 1 {
			return errors.New("usage: suno2api session status|refresh|keepalive [--token-file session.token]")
		}
		action := flags.Arg(0)
		if action != "status" && action != "refresh" && action != "keepalive" {
			return errors.New("session action must be status, refresh, or keepalive")
		}
		httpClient, _, err := netconfig.NewHTTPClient(common.proxy)
		if err != nil {
			return err
		}
		manager, err := createSessionManager(common, httpClient)
		if err != nil {
			return err
		}
		if action == "keepalive" {
			return runKeepalive(ctx, manager, func(event session.Event) { _ = printJSON(event) })
		}
		if common.timeout <= 0 {
			return errors.New("timeout must be positive")
		}
		operation, cancel := context.WithTimeout(ctx, common.timeout)
		defer cancel()
		if action == "refresh" {
			if _, err := manager.Refresh(operation); err != nil {
				return err
			}
		}
		status, err := manager.Status(operation)
		if err != nil {
			if action == "status" && status.State != "" {
				if outputError := printJSON(status); outputError != nil {
					return outputError
				}
			}
			return err
		}
		return printJSON(status)
	case "download":
		output := ""
		flags.StringVar(&output, "output", "", "output directory or filename; default downloads/")
		flags.StringVar(&output, "o", "", "output directory or filename")
		if err := parseFlags(flags, arguments); err != nil {
			return err
		}
		if flags.NArg() != 1 {
			return errors.New("usage: suno2api download [-o output] <Suno share URL>")
		}
		clients, err := createClients(common)
		if err != nil {
			return err
		}
		operation, cancel := context.WithTimeout(ctx, common.timeout)
		defer cancel()
		output, err = downloadOutput(output, configuration.DownloadDir)
		if err != nil {
			return err
		}
		result, err := clients.downloader.Download(operation, flags.Arg(0), output)
		if err != nil {
			return err
		}
		return printJSON(result)
	case "generate":
		var request generate.Request
		wait, save := true, false
		lyricsFile, output := "", ""
		flags.StringVar(&request.Prompt, "prompt", "", "song description for simple mode; custom mode uses --lyrics and --style")
		flags.StringVar(&request.Lyrics, "lyrics", "", "custom lyrics; enables custom mode")
		flags.StringVar(&lyricsFile, "lyrics-file", "", "UTF-8 lyrics text file")
		flags.StringVar(&request.Style, "style", "", "custom musical style")
		flags.StringVar(&request.Title, "title", "", "custom song title")
		modelDefault := configuration.Model
		if modelDefault == "" {
			modelDefault = "v6-mini"
		}
		flags.StringVar(&request.Model, "model", modelDefault, "model key or v6-mini alias; default v6-mini/chirp-goose")
		flags.StringVar(&request.CaptchaToken, "captcha-token", "", "verification token provided by your completed Suno verification")
		flags.BoolVar(&request.Instrumental, "instrumental", false, "generate instrumental music")
		flags.BoolVar(&request.Custom, "custom", false, "use custom mode; requires --lyrics, or --style for an instrumental song")
		flags.IntVar(&request.BatchSize, "batch-size", 2, "Android requested batch size: 1 or 2; web uses Suno's own batch default")
		flags.BoolVar(&wait, "wait", true, "wait for generated songs to complete")
		flags.BoolVar(&save, "download", false, "download completed songs")
		flags.StringVar(&output, "output", "", "directory for generated songs")
		flags.StringVar(&output, "o", "", "directory for generated songs")
		if err := parseFlags(flags, arguments); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("generate takes named flags; use --prompt for the song description")
		}
		if request.BatchSize < 1 || request.BatchSize > 2 {
			return errors.New("--batch-size must be 1 or 2")
		}
		if save && !wait {
			return errors.New("--download requires --wait=true")
		}
		if lyricsFile != "" {
			data, err := readSmallFile(lyricsFile, 1<<20)
			if err != nil {
				return err
			}
			request.Lyrics = string(data)
			request.Custom = true
		}
		clients, err := createClients(common)
		if err != nil {
			return err
		}
		operation, cancel := context.WithTimeout(ctx, common.timeout)
		defer cancel()
		response, err := clients.generator.Generate(operation, request)
		if err != nil {
			if outputError := writeSubmittedResponse(os.Stdout, response); outputError != nil {
				return fmt.Errorf("%w; could not output the submitted request: %v", err, outputError)
			}
			return err
		}
		if err := printJSON(map[string]any{"ids": response.IDs(), "response": response}); err != nil {
			return err
		}
		if !wait {
			return nil
		}
		requestedIDs := response.RequestedIDs()
		if len(requestedIDs) == 0 {
			return errors.New("Suno returned no songs for the requested model; inspect the submitted IDs before generating again")
		}
		clips, err := clients.generator.Wait(operation, requestedIDs)
		if err != nil {
			if len(clips) > 0 {
				printJSON(map[string]any{"clips": clips})
			}
			return fmt.Errorf("%w; use get --ids with the submitted IDs before generating again", err)
		}
		if err := printJSON(map[string]any{"clips": clips}); err != nil {
			return err
		}
		if save {
			if output == "" {
				output = configuration.DownloadDir
			}
			if output == "" {
				output = "downloads"
			}
			if err := os.MkdirAll(output, 0755); err != nil {
				return err
			}
			for _, clip := range clips {
				result, err := clients.downloader.Download(operation, "https://suno.com/song/"+clip.ID, output)
				if err != nil {
					return err
				}
				if err := printJSON(result); err != nil {
					return err
				}
			}
		}
		return nil
	case "get":
		ids := ""
		wait := false
		flags.StringVar(&ids, "ids", "", "comma-separated song IDs")
		flags.BoolVar(&wait, "wait", false, "wait for songs to complete")
		if err := parseFlags(flags, arguments); err != nil {
			return err
		}
		if ids == "" {
			return errors.New("get requires --ids")
		}
		clients, err := createClients(common)
		if err != nil {
			return err
		}
		operation, cancel := context.WithTimeout(ctx, common.timeout)
		defer cancel()
		var clips []generate.Clip
		if wait {
			clips, err = clients.generator.Wait(operation, strings.Split(ids, ","))
		} else {
			clips, err = clients.generator.Get(operation, strings.Split(ids, ","))
		}
		if len(clips) > 0 {
			if outputError := printJSON(map[string]any{"clips": clips}); outputError != nil {
				return outputError
			}
		}
		return err
	case "billing":
		if err := parseFlags(flags, arguments); err != nil {
			return err
		}
		clients, err := createClients(common)
		if err != nil {
			return err
		}
		operation, cancel := context.WithTimeout(ctx, common.timeout)
		defer cancel()
		billing, err := clients.generator.Billing(operation)
		if err != nil {
			return err
		}
		return printJSON(billing)
	case "check-captcha":
		if err := parseFlags(flags, arguments); err != nil {
			return err
		}
		clients, err := createClients(common)
		if err != nil {
			return err
		}
		operation, cancel := context.WithTimeout(ctx, common.timeout)
		defer cancel()
		check, err := clients.generator.CheckCaptcha(operation)
		if err != nil {
			return err
		}
		return printJSON(check)
	case "serve":
		listen := configuration.Listen
		if listen == "" {
			listen = "127.0.0.1:8080"
		}
		flags.StringVar(&listen, "listen", listen, "HTTP listen address; remote addresses require SUNO_API_KEY")
		if err := parseFlags(flags, arguments); err != nil {
			return err
		}
		apiKey := os.Getenv("SUNO_API_KEY")
		if err := validateListen(listen, apiKey); err != nil {
			return err
		}
		clients, err := createClients(common)
		if err != nil {
			return err
		}
		handler, err := server.NewHandler(server.Config{Downloader: clients.downloader, Generator: clients.generator, DownloadDir: configuration.DownloadDir, APIKey: apiKey, RequestTimeout: common.timeout})
		if err != nil {
			return err
		}
		httpServer := &http.Server{Addr: listen, Handler: handler, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: common.timeout + 30*time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
		listener, err := net.Listen("tcp", listen)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "suno2api listening on http://%s (%s)\n", listener.Addr(), clients.proxySource)
		if clients.sessionManager != nil {
			go func() {
				encoder := json.NewEncoder(os.Stderr)
				err := clients.sessionManager.Run(ctx, func(event session.Event) { _ = encoder.Encode(event) })
				if err != nil && ctx.Err() == nil {
					fmt.Fprintln(os.Stderr, "Suno session keepalive stopped; check session status or update the Cookie file")
				}
			}()
		}
		go func() {
			<-ctx.Done()
			shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			httpServer.Shutdown(shutdown)
		}()
		if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	default:
		return fmt.Errorf("unknown command %q; run suno2api --help", command)
	}
}

type keepaliveRunner interface {
	Run(context.Context, func(session.Event)) error
}

func runKeepalive(ctx context.Context, manager keepaliveRunner, onEvent func(session.Event)) error {
	err := manager.Run(ctx, onEvent)
	if ctx.Err() != nil && errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func apiTokenProvider(ensure func(context.Context) (string, error)) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		token, err := ensure(ctx)
		if err == nil {
			return token, nil
		}
		if errors.Is(err, session.ErrAuthentication) || errors.Is(err, session.ErrRefreshCredentialsRequired) || errors.Is(err, session.ErrUnsupportedCredential) {
			return "", fmt.Errorf("%w: %w", generate.ErrAuthentication, err)
		}
		if errors.Is(err, session.ErrRateLimited) {
			return "", fmt.Errorf("%w: %w", generate.ErrRateLimited, err)
		}
		return "", err
	}
}

type clients struct {
	downloader     *download.Client
	generator      *generate.Client
	proxySource    string
	sessionManager *session.Manager
}

func createSessionManager(common commonFlags, httpClient *http.Client) (*session.Manager, error) {
	path := common.tokenFile
	if path == "" {
		path = os.Getenv("SUNO_TOKEN_FILE")
	}
	if path == "" {
		return nil, errors.New("session management requires --token-file or SUNO_TOKEN_FILE pointing to your Suno Cookie file")
	}
	if common.refreshBefore <= 0 {
		return nil, errors.New("refresh-before must be positive")
	}
	authCookieFile := common.authCookieFile
	if authCookieFile == "" {
		authCookieFile = os.Getenv("SUNO_AUTH_COOKIE_FILE")
	}
	return session.NewManager(session.Config{TokenFile: path, AuthCookieFile: authCookieFile, HTTPClient: httpClient, RefreshBefore: common.refreshBefore})
}

func createClients(common commonFlags) (clients, error) {
	if common.timeout <= 0 {
		return clients{}, errors.New("timeout must be positive")
	}
	httpClient, source, err := netconfig.NewHTTPClient(common.proxy)
	if err != nil {
		return clients{}, err
	}
	tokenFile := common.tokenFile
	if tokenFile == "" {
		tokenFile = os.Getenv("SUNO_TOKEN_FILE")
	}
	token := ""
	if tokenFile == "" {
		token = os.Getenv("SUNO_TOKEN")
	}
	var manager *session.Manager
	var tokenProvider func(context.Context) (string, error)
	if common.autoRefresh && tokenFile != "" {
		manager, err = createSessionManager(common, httpClient)
		if err != nil {
			return clients{}, err
		}
		tokenProvider = apiTokenProvider(manager.Ensure)
	}
	generator, err := generate.NewClient(generate.Config{Token: token, TokenFile: tokenFile, TokenProvider: tokenProvider, HTTPClient: httpClient, WaitTimeout: common.timeout, DefaultModel: common.defaultModel, Protocol: common.protocol, DeviceID: common.deviceID})
	if err != nil {
		return clients{}, err
	}
	downloader := download.NewClient(download.Options{HTTPClient: httpClient, Token: token, TokenFile: tokenFile, TokenProvider: tokenProvider, AllowAuthorize: common.authorize})
	return clients{downloader: downloader, generator: generator, proxySource: source, sessionManager: manager}, nil
}

func readConfig(explicit string) (diskConfig, string, error) {
	path := explicit
	if path == "" {
		path = defaultConfigPath()
	}
	data, err := readSmallFile(path, 64<<10)
	if errors.Is(err, os.ErrNotExist) && explicit == "" {
		return diskConfig{}, path, nil
	}
	if err != nil {
		return diskConfig{}, path, fmt.Errorf("read local configuration: %w", err)
	}
	var configuration diskConfig
	if json.Unmarshal(data, &configuration) != nil {
		return diskConfig{}, path, errors.New("invalid local JSON configuration")
	}
	directory, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return diskConfig{}, path, err
	}
	if configuration.TokenFile != "" && !filepath.IsAbs(configuration.TokenFile) {
		configuration.TokenFile = filepath.Join(directory, configuration.TokenFile)
	}
	if configuration.AuthCookieFile != "" && !filepath.IsAbs(configuration.AuthCookieFile) {
		configuration.AuthCookieFile = filepath.Join(directory, configuration.AuthCookieFile)
	}
	if configuration.DownloadDir != "" && !filepath.IsAbs(configuration.DownloadDir) {
		configuration.DownloadDir = filepath.Join(directory, configuration.DownloadDir)
	}
	if configuration.GatewayDataDir != "" && !filepath.IsAbs(configuration.GatewayDataDir) {
		configuration.GatewayDataDir = filepath.Join(directory, configuration.GatewayDataDir)
	}
	if configuration.GatewayAdminKeyFile != "" && !filepath.IsAbs(configuration.GatewayAdminKeyFile) {
		configuration.GatewayAdminKeyFile = filepath.Join(directory, configuration.GatewayAdminKeyFile)
	}
	return configuration, path, nil
}

func defaultConfigPath() string {
	names := []string{"config.local.json", "config.json"}
	directories := []string{"."}
	if executable, err := os.Executable(); err == nil {
		directories = append(directories, filepath.Dir(executable))
	}
	for _, directory := range directories {
		for _, name := range names {
			path := filepath.Join(directory, name)
			if info, err := os.Stat(path); err == nil && !info.IsDir() {
				return path
			}
		}
	}
	return "config.local.json"
}

// loadEnvFile applies KEY=VALUE entries from a .env file without overwriting
// variables that are already set in the process environment.
func loadEnvFile(explicit, configPath string) (string, error) {
	candidates := []string{}
	if explicit != "" {
		candidates = append(candidates, explicit)
	} else {
		candidates = append(candidates, ".env")
		if configPath != "" {
			candidates = append(candidates, filepath.Join(filepath.Dir(configPath), ".env"))
		}
		if executable, err := os.Executable(); err == nil {
			candidates = append(candidates, filepath.Join(filepath.Dir(executable), ".env"))
		}
	}
	for _, path := range candidates {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		values, err := parseEnvFile(path)
		if err != nil {
			return "", err
		}
		for key, value := range values {
			if _, exists := os.LookupEnv(key); !exists {
				if err := os.Setenv(key, value); err != nil {
					return "", err
				}
			}
		}
		return path, nil
	}
	if explicit != "" {
		return "", errors.New("cannot read the requested .env file")
	}
	return "", nil
}

func parseEnvFile(path string) (map[string]string, error) {
	data, err := readSmallFile(path, 256<<10)
	if err != nil {
		return nil, errors.New("cannot read .env configuration")
	}
	values := make(map[string]string)
	for _, line := range strings.Split(strings.TrimPrefix(string(data), "\ufeff"), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" || strings.ContainsAny(key, " \t") {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
			quote := value[0]
			value = value[1 : len(value)-1]
			if quote == '"' {
				value = strings.NewReplacer(`\n`, "\n", `\r`, "\r", `\t`, "\t", `\"`, `"`).Replace(value)
			}
		} else if index := strings.Index(value, " #"); index >= 0 {
			value = strings.TrimSpace(value[:index])
		}
		values[key] = value
	}
	return values, nil
}

func applyEnvironment(configuration *diskConfig) error {
	apply := func(key string, target *string) {
		if value, ok := os.LookupEnv(key); ok && strings.TrimSpace(value) != "" {
			*target = strings.TrimSpace(value)
		}
	}
	apply("SUNO_PROTOCOL", &configuration.Protocol)
	apply("SUNO_MODEL", &configuration.Model)
	apply("SUNO_TOKEN_FILE", &configuration.TokenFile)
	apply("SUNO_AUTH_COOKIE_FILE", &configuration.AuthCookieFile)
	apply("SUNO_PROXY", &configuration.Proxy)
	apply("SUNO_DOWNLOAD_DIR", &configuration.DownloadDir)
	apply("SUNO_LISTEN", &configuration.Listen)
	apply("SUNO_REFRESH_BEFORE", &configuration.RefreshBefore)
	apply("SUNO_REQUEST_TIMEOUT", &configuration.RequestTimeout)
	apply("SUNO_GATEWAY_LISTEN", &configuration.GatewayListen)
	apply("SUNO_GATEWAY_DATA_DIR", &configuration.GatewayDataDir)
	apply("SUNO_GATEWAY_ADMIN_KEY_FILE", &configuration.GatewayAdminKeyFile)
	if _, ok := os.LookupEnv("SUNO_GATEWAY_API_STYLE"); ok {
		apply("SUNO_GATEWAY_API_STYLE", &configuration.GatewayAPIStyle)
	} else {
		apply("SUNO_API_STYLE", &configuration.GatewayAPIStyle)
	}
	if value, ok := os.LookupEnv("SUNO_AUTO_REFRESH"); ok && strings.TrimSpace(value) != "" {
		parsed, err := strconv.ParseBool(strings.TrimSpace(value))
		if err != nil {
			return errors.New("SUNO_AUTO_REFRESH must be true or false")
		}
		configuration.AutoRefresh = &parsed
	}
	return nil
}

func configuredTimeout(value string) (time.Duration, error) {
	if strings.TrimSpace(value) == "" {
		return 10 * time.Minute, nil
	}
	duration, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil || duration <= 0 {
		return 0, errors.New("request_timeout must be a positive duration, for example 10m")
	}
	return duration, nil
}

func readSmallFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("file exceeds size limit")
	}
	return data, nil
}

func downloadOutput(explicit, configured string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if configured != "" {
		if err := os.MkdirAll(configured, 0755); err != nil {
			return "", err
		}
	}
	return configured, nil
}

func optionValue(arguments []string, name string) string {
	for index, value := range arguments {
		if value == "--"+name || value == "-"+name {
			if index+1 < len(arguments) {
				return arguments[index+1]
			}
		}
		if strings.HasPrefix(value, "--"+name+"=") {
			return strings.TrimPrefix(value, "--"+name+"=")
		}
		if strings.HasPrefix(value, "-"+name+"=") {
			return strings.TrimPrefix(value, "-"+name+"=")
		}
	}
	return ""
}

func parseFlags(flags *flag.FlagSet, arguments []string) error {
	var options, positional []string
	for index := 0; index < len(arguments); index++ {
		value := arguments[index]
		if value == "--" {
			positional = append(positional, arguments[index+1:]...)
			break
		}
		if len(value) > 1 && value[0] == '-' {
			options = append(options, value)
			name := strings.TrimLeft(value, "-")
			if strings.Contains(name, "=") {
				continue
			}
			if selected := flags.Lookup(name); selected != nil {
				boolFlag, isBool := selected.Value.(interface{ IsBoolFlag() bool })
				if isBool && boolFlag.IsBoolFlag() {
					continue
				}
				if index+1 < len(arguments) {
					index++
					options = append(options, arguments[index])
				}
			}
		} else {
			positional = append(positional, value)
		}
	}
	return flags.Parse(append(options, positional...))
}

func validateListen(address, key string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("listen must be host:port")
	}
	if key != "" {
		if len(key) < 16 {
			return errors.New("SUNO_API_KEY must contain at least 16 characters")
		}
		return nil
	}
	ip := net.ParseIP(host)
	if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
		return errors.New("a non-loopback listener requires SUNO_API_KEY with at least 16 characters")
	}
	return nil
}

func printJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func writeSubmittedResponse(writer io.Writer, response generate.Response) error {
	if response.ID == "" && len(response.Clips) == 0 {
		return nil
	}
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(map[string]any{
		"submitted":     true,
		"ids":           response.IDs(),
		"requested_ids": response.RequestedIDs(),
		"response":      response,
	})
}

func printHelp() {
	fmt.Print(`suno2api - Suno gateway web application, download and song generation

  suno2api
      Start the gateway web application (same as "suno2api gateway").
      Default address: http://127.0.0.1:8080

  suno2api gateway [--listen 127.0.0.1:8080] [--data-dir data/gateway] [--api-style suno|openai|both]
  suno2api gateway --import-current [--token-file session.token] [--auth-cookie-file auth.cookie]
  suno2api "https://suno.com/s/SHARE_CODE"
  suno2api download URL [-o downloads/] [--token-file session.token]
  suno2api generate --prompt "gentle piano" --instrumental --batch-size 1
  suno2api generate --lyrics-file lyrics.txt --style "acoustic folk" --title "Hello" --download
  suno2api get --ids ID1,ID2 [--wait]
  suno2api billing
  suno2api check-captcha
  suno2api serve [--listen 127.0.0.1:8080]
  suno2api session status|refresh|keepalive [--token-file session.token]

Configuration: config.local.json or config.json plus an optional .env file.
Relative paths in the JSON file are resolved against that file.
Environment variables: SUNO_GATEWAY_LISTEN, SUNO_GATEWAY_DATA_DIR,
SUNO_GATEWAY_API_STYLE, SUNO_MODEL, SUNO_PROXY, SUNO_TOKEN_FILE,
SUNO_AUTH_COOKIE_FILE, SUNO_REQUEST_TIMEOUT, SUNO_ADMIN_KEY.

Common options: --config, --env-file, --token-file, --protocol, --device-id,
--proxy, --timeout.
Session options: --auto-refresh=true|false, --refresh-before 60s, --auth-cookie-file.
Gateway options: --data-dir, --admin-key-file, --api-style, --import-current.
SUNO_ADMIN_KEY overrides the generated administrator key.
API styles: suno serves /api/*, openai serves /v1/*, both serves both.
Keepalive runs until Ctrl+C; --timeout does not limit its overall lifetime.
Use COMMAND --help to show command options. Generation uses your Suno credits.
`)
}
