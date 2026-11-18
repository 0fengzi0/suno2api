package generate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const DefaultBaseURL = "https://studio-api-prod.suno.com/api/"

const WebBaseURL = "https://suno.com/a/api/"

const DefaultModel = "chirp-goose"

var clipIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type Config struct {
	Protocol      string
	DeviceID      string
	Token         string
	TokenFile     string
	TokenProvider func(context.Context) (string, error)
	BaseURL       string
	HTTPClient    *http.Client
	PollInterval  time.Duration
	WaitTimeout   time.Duration
	DefaultModel  string
	ProxyURL      string
}

type Client struct {
	protocol           string
	deviceID           string
	createSessionToken string
	baseURL            *url.URL
	httpClient         *http.Client
	token              string
	tokenFile          string
	tokenProvider      func(context.Context) (string, error)
	pollInterval       time.Duration
	waitTimeout        time.Duration
	defaultModel       string
}

func NewClient(config Config) (*Client, error) {
	protocol := strings.ToLower(strings.TrimSpace(config.Protocol))
	if protocol == "" {
		// The Android app protocol is the primary client shape; the website
		// protocol stays available as an explicit fallback.
		protocol = "android"
	}
	if protocol != "web" && protocol != "android" {
		return nil, errors.New("protocol must be web or android")
	}
	base := config.BaseURL
	if base == "" {
		base = DefaultBaseURL
		if protocol == "web" {
			base = WebBaseURL
		}
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("invalid Suno API base URL")
	}
	host := strings.ToLower(parsed.Hostname())
	expectedHost, expectedPath := "studio-api-prod.suno.com", "/api/"
	if protocol == "web" {
		expectedHost = "suno.com"
		expectedPath = "/a/api/"
	}
	if host != expectedHost || parsed.Scheme != "https" || (parsed.Port() != "" && parsed.Port() != "443") || strings.TrimRight(parsed.Path, "/")+"/" != expectedPath {
		address := net.ParseIP(host)
		if config.HTTPClient == nil || address == nil || !address.IsLoopback() || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return nil, errors.New("Suno credentials may only be sent to the selected official protocol host and API prefix; loopback URLs require an explicit test HTTP client")
		}
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/"
	client := http.Client{Timeout: 45 * time.Second}
	if config.HTTPClient != nil {
		client = *config.HTTPClient
	}
	if client.Timeout == 0 {
		client.Timeout = 45 * time.Second
	}
	proxyAddress := config.ProxyURL
	if proxyAddress == "" && config.HTTPClient == nil {
		proxyAddress = os.Getenv("SUNO_PROXY")
	}
	if proxyAddress == "" && config.HTTPClient == nil && os.Getenv("HTTPS_PROXY") == "" && os.Getenv("https_proxy") == "" && os.Getenv("HTTP_PROXY") == "" && os.Getenv("http_proxy") == "" {
		proxyAddress = os.Getenv("ALL_PROXY")
		if proxyAddress == "" {
			proxyAddress = os.Getenv("all_proxy")
		}
	}
	if proxyAddress != "" {
		proxy, err := url.Parse(proxyAddress)
		if err != nil || proxy.Host == "" || (proxy.Scheme != "http" && proxy.Scheme != "https" && proxy.Scheme != "socks5" && proxy.Scheme != "socks5h") {
			return nil, errors.New("invalid proxy URL; expected http://, https:// or socks5://")
		}
		var transport *http.Transport
		if client.Transport == nil {
			transport = http.DefaultTransport.(*http.Transport).Clone()
		} else {
			configured, ok := client.Transport.(*http.Transport)
			if !ok {
				return nil, errors.New("explicit proxy requires an http.Transport")
			}
			transport = configured.Clone()
		}
		transport.Proxy = http.ProxyURL(proxy)
		client.Transport = transport
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if config.PollInterval <= 0 {
		config.PollInterval = 15 * time.Second
	}
	if config.WaitTimeout <= 0 {
		config.WaitTimeout = 10 * time.Minute
	}
	if config.DefaultModel == "" {
		config.DefaultModel = DefaultModel
	}
	if config.Token == "" && config.TokenFile == "" {
		config.TokenFile = os.Getenv("SUNO_TOKEN_FILE")
		if config.TokenFile == "" {
			config.Token = os.Getenv("SUNO_TOKEN")
		}
	}
	session, err := newUUID()
	if err != nil {
		return nil, err
	}
	return &Client{protocol: protocol, deviceID: config.DeviceID, createSessionToken: session, baseURL: parsed, httpClient: &client, token: config.Token, tokenFile: config.TokenFile, tokenProvider: config.TokenProvider, pollInterval: config.PollInterval, waitTimeout: config.WaitTimeout, defaultModel: config.DefaultModel}, nil
}

func (client *Client) currentToken(ctx context.Context) (string, error) {
	if client.tokenProvider != nil {
		value, err := client.tokenProvider(ctx)
		if err != nil {
			return "", err
		}
		return normalizeToken(value)
	}
	if client.tokenFile != "" {
		return readTokenFile(client.tokenFile)
	}
	return normalizeToken(client.token)
}

func (client *Client) Generate(ctx context.Context, request Request) (Response, error) {
	if request.Model == "" {
		request.Model = client.defaultModel
	}
	payload, err := client.payload(request)
	if err != nil {
		return Response{}, err
	}
	verification, err := client.CheckCaptcha(ctx)
	if err != nil {
		return Response{}, err
	}
	if verification.Required && strings.TrimSpace(request.CaptchaToken) == "" {
		return Response{}, fmt.Errorf("%w: complete the generation verification in Suno and provide its actual token; no generation was submitted", ErrCaptchaRequired)
	}
	var response Response
	path := "generate/v2/"
	var body any = payload
	if client.protocol == "web" {
		billing, err := client.Billing(ctx)
		if err != nil {
			return Response{}, err
		}
		body, err = client.webPayload(payload, verification, billing.userTier())
		if err != nil {
			return Response{}, err
		}
		path = "generate/v2-web/"
	}
	if err := client.call(ctx, http.MethodPost, path, body, &response, request.CaptchaToken); err != nil {
		return Response{}, err
	}
	response.RequestedModel = payload.Model
	if len(response.Clips) == 0 {
		return response, errors.New("Suno accepted the request but returned no clips; do not resubmit automatically because the request may have consumed credits")
	}
	for _, clip := range response.Clips {
		if !clipIDPattern.MatchString(clip.ID) {
			return response, errors.New("Suno returned an invalid clip ID; preserve the response and check the original request before submitting again")
		}
	}
	return response, nil
}

func (client *Client) CheckCaptcha(ctx context.Context) (CaptchaCheck, error) {
	var response struct {
		Required       *bool `json:"required"`
		CaptchaVersion int   `json:"captcha_version"`
	}
	if err := client.call(ctx, http.MethodPost, "c/check", map[string]string{"ctype": "generation"}, &response, ""); err != nil {
		return CaptchaCheck{}, err
	}
	if response.Required == nil {
		return CaptchaCheck{}, errors.New("Suno verification check omitted the required field; generation was not submitted")
	}
	return CaptchaCheck{Required: *response.Required, CaptchaVersion: response.CaptchaVersion}, nil
}

func (client *Client) Billing(ctx context.Context) (BillingInfo, error) {
	var info BillingInfo
	err := client.call(ctx, http.MethodGet, "billing/info/", nil, &info, "")
	return info, err
}

func (client *Client) Get(ctx context.Context, ids []string) ([]Clip, error) {
	ids, err := validateIDs(ids)
	if err != nil {
		return nil, err
	}
	clips := make([]Clip, 0, len(ids))
	if client.protocol == "web" {
		var response struct {
			Clips []Clip `json:"clips"`
		}
		body := map[string]any{"filters": map[string]any{"ids": map[string]any{"presence": "True", "clipIds": ids}}, "limit": len(ids)}
		if err := client.call(ctx, http.MethodPost, "feed/v3", body, &response, ""); err != nil {
			return nil, err
		}
		byID := make(map[string]Clip, len(response.Clips))
		for _, clip := range response.Clips {
			byID[clip.ID] = clip
		}
		for _, id := range ids {
			clip, ok := byID[id]
			if !ok {
				return clips, errors.New("Suno feed omitted a requested clip ID")
			}
			clips = append(clips, clip)
		}
		return clips, nil
	}
	for _, id := range ids {
		var clip Clip
		if err := client.call(ctx, http.MethodGet, "clip/"+id+"/", nil, &clip, ""); err != nil {
			var apiError *APIError
			if !errors.As(err, &apiError) || apiError.StatusCode != http.StatusNotFound {
				return clips, err
			}
			if err := client.call(ctx, http.MethodGet, "generate/clip/"+id, nil, &clip, ""); err != nil {
				return clips, err
			}
		}
		if clip.ID != id {
			return clips, errors.New("Suno returned a clip that does not match the requested ID")
		}
		clips = append(clips, clip)
	}
	return clips, nil
}

func (client *Client) Wait(ctx context.Context, ids []string) ([]Clip, error) {
	ids, err := validateIDs(ids)
	if err != nil {
		return nil, err
	}
	waitContext, cancel := context.WithTimeout(ctx, client.waitTimeout)
	defer cancel()
	var latest []Clip
	for {
		delay := client.pollInterval
		clips, err := client.Get(waitContext, ids)
		if len(clips) > 0 {
			latest = clips
		}
		if err != nil {
			var apiError *APIError
			if !(errors.As(err, &apiError) && (apiError.StatusCode == http.StatusTooManyRequests || apiError.StatusCode >= 500)) {
				return latest, err
			}
			if apiError.RetryAfter > delay {
				delay = apiError.RetryAfter
			}
		} else {
			complete := len(clips) == len(ids)
			var failures []Clip
			for _, clip := range clips {
				switch strings.ToLower(clip.Status) {
				case "error", "failed":
					failures = append(failures, clip)
				case "complete":
					if !clip.hasAudio() {
						complete = false
					}
				default:
					complete = false
				}
			}
			if len(failures) > 0 {
				return clips, &GenerationError{Clips: failures}
			}
			if complete {
				return clips, nil
			}
		}
		timer := time.NewTimer(delay)
		select {
		case <-waitContext.Done():
			timer.Stop()
			return latest, fmt.Errorf("wait for Suno clips: %w", waitContext.Err())
		case <-timer.C:
		}
	}
}

func validateIDs(ids []string) ([]string, error) {
	if len(ids) == 0 || len(ids) > 20 {
		return nil, errors.New("provide between 1 and 20 Suno clip IDs")
	}
	seen := make(map[string]bool, len(ids))
	result := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.ToLower(strings.TrimSpace(id))
		if !clipIDPattern.MatchString(id) {
			return nil, errors.New("invalid Suno clip ID; expected a UUID")
		}
		if !seen[id] {
			result = append(result, id)
			seen[id] = true
		}
	}
	return result, nil
}

func (client *Client) call(ctx context.Context, method, path string, body any, result any, extraSecret string) error {
	token, err := client.currentToken(ctx)
	if err != nil {
		return err
	}
	var data []byte
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode Suno request: %w", err)
		}
	}
	endpoint := client.baseURL.ResolveReference(&url.URL{Path: path})
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create Suno request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "suno2api/1.0")
	if client.protocol == "web" {
		if err := client.applyWebHeaders(request); err != nil {
			return err
		}
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("Suno request failed: %w", err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 4*1024*1024+1))
	if err != nil {
		return fmt.Errorf("read Suno response: %w", err)
	}
	if len(payload) > 4*1024*1024 {
		return errors.New("Suno response exceeds 4 MiB")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		apiError := parseAPIError(response.StatusCode, payload, token, extraSecret)
		if seconds, err := strconv.Atoi(response.Header.Get("Retry-After")); err == nil && seconds > 0 && seconds <= 86400 {
			apiError.RetryAfter = time.Duration(seconds) * time.Second
		} else if until, err := http.ParseTime(response.Header.Get("Retry-After")); err == nil {
			apiError.RetryAfter = time.Until(until)
		}
		return apiError
	}
	if err := json.Unmarshal(payload, result); err != nil {
		return fmt.Errorf("decode Suno response (HTTP %d): invalid JSON", response.StatusCode)
	}
	return nil
}

func parseAPIError(status int, payload []byte, secrets ...string) *APIError {
	var object map[string]json.RawMessage
	_ = json.Unmarshal(payload, &object)
	message := ""
	code := ""
	for _, key := range []string{"error_type", "code", "error_code"} {
		if json.Unmarshal(object[key], &code) == nil && code != "" {
			break
		}
	}
	for _, key := range []string{"detail", "error_message", "message", "error"} {
		if json.Unmarshal(object[key], &message) == nil && message != "" {
			break
		}
		var nested struct {
			Message string `json:"message"`
			Detail  string `json:"detail"`
			Code    string `json:"code"`
		}
		if json.Unmarshal(object[key], &nested) == nil {
			message = nested.Message
			if message == "" {
				message = nested.Detail
			}
			if code == "" {
				code = nested.Code
			}
			if message != "" {
				break
			}
		}
	}
	if message == "" {
		message = http.StatusText(status)
	}
	for _, secret := range secrets {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[redacted]")
			code = strings.ReplaceAll(code, secret, "[redacted]")
		}
	}
	if len(message) > 512 {
		message = message[:512]
	}
	if len(code) > 128 {
		code = code[:128]
	}
	var cause error
	lower := strings.ToLower(message + " " + code)
	switch {
	case status == http.StatusUnauthorized:
		cause = ErrAuthentication
		message += "; refresh your Suno session token"
	case strings.Contains(lower, "captcha") || strings.Contains(lower, "human verification"):
		cause = ErrCaptchaRequired
		message += "; complete verification in Suno and supply the resulting verification token if supported"
	case status == http.StatusTooManyRequests:
		cause = ErrRateLimited
	case status == http.StatusPaymentRequired || strings.Contains(lower, "insufficient credit"):
		cause = ErrInsufficientCredits
	}
	return &APIError{StatusCode: status, Code: code, Message: message, Cause: cause}
}
