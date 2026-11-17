package download

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"suno2api/internal/generate"
	"time"
	"unicode"
)

const apiBase = "https://studio-api-prod.suno.com/api/"

var (
	ErrAuthenticationRequired = errors.New("this song requires a Suno login to obtain a downloadable audio file")
	ErrAuthorizationRequired  = errors.New("download authorization is required; enabling it may consume your Suno download quota")
	ErrEncodedMedia           = errors.New("Suno returned encoded playback media rather than a downloadable audio file")
	ErrNotReady               = errors.New("the song is not ready for download")
	uuidPattern               = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	shortcodePattern          = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	metaPattern               = regexp.MustCompile(`(?is)<(?:meta|link)\b[^>]*>`)
	attributePattern          = regexp.MustCompile(`(?is)([a-z_:][a-z0-9_:.-]*)\s*=\s*(?:"([^"]*)"|'([^']*)')`)
	flightPattern             = regexp.MustCompile(`self\.__next_f\.push\(\[1,("(?:\\.|[^"\\])*")\]\)`)
)

type Options struct {
	HTTPClient     *http.Client
	ProxyURL       string
	Token          string
	TokenFile      string
	TokenProvider  func(context.Context) (string, error)
	MaxBytes       int64
	AllowAuthorize bool
	Format         string
}

type Client struct {
	httpClient     *http.Client
	token          string
	tokenFile      string
	tokenProvider  func(context.Context) (string, error)
	maxBytes       int64
	allowAuthorize bool
	format         string
}

type Track struct {
	ID             string `json:"id"`
	Title          string `json:"title"`
	ShareURL       string `json:"share_url"`
	AudioURL       string `json:"-"`
	Format         string `json:"format"`
	Source         string `json:"source,omitempty"`
	Encoding       string `json:"encoding,omitempty"`
	CreditDeducted bool   `json:"credit_deducted,omitempty"`
}

type Result struct {
	Track  Track  `json:"track"`
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type APIError struct {
	Operation string
	Status    int
	Reason    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("Suno %s failed (HTTP %d, reason %s)", e.Operation, e.Status, e.Reason)
}

type requestKind uint8

const (
	pageRequest requestKind = iota
	apiRequest
	audioRequest
)

type requestKindKey struct{}

func NewClient(options Options) *Client {
	client := http.Client{Timeout: 2 * time.Minute, Transport: safeTransport()}
	if options.ProxyURL != "" {
		proxyURL, err := url.Parse(options.ProxyURL)
		if err == nil && (proxyURL.Scheme == "http" || proxyURL.Scheme == "https" || proxyURL.Scheme == "socks5" || proxyURL.Scheme == "socks5h") && proxyURL.Hostname() != "" {
			transport := http.DefaultTransport.(*http.Transport).Clone()
			transport.Proxy = http.ProxyURL(proxyURL)
			client.Transport = transport
		} else {
			client.Transport = invalidProxyTransport{}
		}
	}
	if options.HTTPClient != nil {
		client = *options.HTTPClient
		if client.Timeout == 0 {
			client.Timeout = 2 * time.Minute
		}
	}
	client.Jar = nil
	previousRedirect := client.CheckRedirect
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		kind, _ := request.Context().Value(requestKindKey{}).(requestKind)
		if err := validateURL(request.URL, kind); err != nil {
			return err
		}
		if kind != apiRequest {
			request.Header.Del("Authorization")
			request.Header.Del("Cookie")
		}
		if previousRedirect != nil {
			return previousRedirect(request, via)
		}
		return nil
	}
	limit := options.MaxBytes
	if limit <= 0 {
		limit = 256 << 20
	}
	format := strings.ToLower(options.Format)
	if format == "" {
		format = "mp3"
	}
	return &Client{httpClient: &client, token: options.Token, tokenFile: options.TokenFile, tokenProvider: options.TokenProvider,
		maxBytes: limit, allowAuthorize: options.AllowAuthorize, format: format}
}

type invalidProxyTransport struct{}

func (invalidProxyTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("invalid proxy URL; use an http, https, socks5, or socks5h proxy")
}

func safeTransport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		var lastError error
		for _, address := range addresses {
			ip := address.IP
			if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
				return nil, errors.New("refusing a non-public network address")
			}
			dialer := net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
			connection, dialError := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if dialError == nil {
				return connection, nil
			}
			lastError = dialError
		}
		if lastError == nil {
			lastError = errors.New("no public network address resolved")
		}
		return nil, lastError
	}
	return transport
}

func validateURL(parsed *url.URL, kind requestKind) error {
	if parsed.Scheme != "https" || parsed.User != nil || parsed.Hostname() == "" ||
		(parsed.Port() != "" && parsed.Port() != "443") {
		return errors.New("only HTTPS Suno URLs without credentials or custom ports are accepted")
	}
	host := strings.ToLower(parsed.Hostname())
	allowed := false
	switch kind {
	case pageRequest:
		allowed = host == "suno.com" || host == "www.suno.com" || host == "suno.ai" || host == "www.suno.ai"
	case apiRequest:
		allowed = host == "studio-api-prod.suno.com"
	case audioRequest:
		switch host {
		case "cdn.suno.ai", "cdn1.suno.ai", "cdn2.suno.ai", "cdn.suno.com", "cdn1.suno.com", "cdn2.suno.com", "cdn-o.suno.com", "d2lwuy8qc234o3.cloudfront.net":
			allowed = true
		}
	}
	if !allowed {
		return errors.New("URL host is not an approved Suno host for this operation")
	}
	return nil
}

func (client *Client) tokenValue(ctx context.Context) (string, error) {
	if client.tokenProvider != nil {
		value, err := client.tokenProvider(ctx)
		if err != nil {
			return "", err
		}
		return generate.NormalizeToken(value)
	}
	token := strings.TrimSpace(client.token)
	if client.tokenFile != "" {
		contents, err := os.ReadFile(client.tokenFile)
		if err != nil {
			return "", fmt.Errorf("read Suno token file: %w", err)
		}
		if len(contents) > 64<<10 {
			return "", errors.New("Suno token file is unexpectedly large")
		}
		token = strings.TrimSpace(string(contents))
	}
	if token == "" {
		return "", nil
	}
	return generate.NormalizeToken(token)
}

func (client *Client) request(ctx context.Context, method, address string, body io.Reader, kind requestKind, authenticated bool) (*http.Response, error) {
	parsed, err := url.Parse(address)
	if err != nil || validateURL(parsed, kind) != nil {
		return nil, errors.New("refusing an invalid or unapproved URL")
	}
	ctx = context.WithValue(ctx, requestKindKey{}, kind)
	request, err := http.NewRequestWithContext(ctx, method, address, body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "suno2api/1.0")
	if kind == apiRequest {
		request.Header.Set("Accept", "application/json")
		if body != nil {
			request.Header.Set("Content-Type", "application/json")
		}
	}
	if authenticated && kind == apiRequest {
		token, err := client.tokenValue(ctx)
		if err != nil {
			return nil, err
		}
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
	}
	response, err := client.httpClient.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("Suno network request failed")
	}
	if response.Request != nil && validateURL(response.Request.URL, kind) != nil {
		response.Body.Close()
		return nil, errors.New("response redirected to an unapproved host")
	}
	return response, nil
}

func (client *Client) apiJSON(ctx context.Context, method, path string, body any, destination any, authenticated bool) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	response, err := client.request(ctx, method, apiBase+path, reader, apiRequest, authenticated)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	contents, err := readLimited(response.Body, 4<<20)
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		reason := http.StatusText(response.StatusCode)
		var failure struct {
			Reason string `json:"reason"`
		}
		if json.Unmarshal(contents, &failure) == nil && failure.Reason != "" && len(failure.Reason) < 80 {
			reason = failure.Reason
		}
		return &APIError{Operation: strings.Split(path, "?")[0], Status: response.StatusCode, Reason: reason}
	}
	if err := json.Unmarshal(contents, destination); err != nil {
		return errors.New("Suno returned an invalid JSON response")
	}
	return nil
}

func readLimited(reader io.Reader, limit int64) ([]byte, error) {
	contents, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(contents)) > limit {
		return nil, errors.New("Suno response exceeded the size limit")
	}
	return contents, nil
}

func ParseShareURL(address string) (clipID, shortcode string, err error) {
	parsed, err := url.Parse(strings.TrimSpace(address))
	if err != nil || validateURL(parsed, pageRequest) != nil {
		return "", "", errors.New("enter a valid HTTPS Suno song or share URL")
	}
	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(segments) != 2 {
		return "", "", errors.New("Suno URL must contain /song/<song-id> or /s/<share-code>")
	}
	if segments[0] == "song" && uuidPattern.MatchString(segments[1]) {
		return strings.ToLower(segments[1]), "", nil
	}
	if segments[0] == "s" && shortcodePattern.MatchString(segments[1]) {
		return "", segments[1], nil
	}
	return "", "", errors.New("unsupported Suno share URL")
}

func (client *Client) clipID(ctx context.Context, address string) (string, error) {
	clipID, shortcode, err := ParseShareURL(address)
	if err != nil || clipID != "" {
		return clipID, err
	}
	var unwrapped struct {
		Success     bool   `json:"success"`
		ContentID   string `json:"content_id"`
		ContentType string `json:"content_type"`
	}
	err = client.apiJSON(ctx, http.MethodGet, "share/code/"+url.PathEscape(shortcode), nil, &unwrapped, false)
	if err == nil && unwrapped.Success {
		if unwrapped.ContentType != "song" && unwrapped.ContentType != "clip" {
			return "", errors.New("this Suno share code does not refer to a song")
		}
		if !uuidPattern.MatchString(unwrapped.ContentID) {
			return "", errors.New("Suno returned an invalid song ID")
		}
		return strings.ToLower(unwrapped.ContentID), nil
	}
	page, finalURL, pageError := client.readPage(ctx, address)
	if pageError != nil {
		return "", fmt.Errorf("resolve Suno share code: %w", pageError)
	}
	if id, _, parseError := ParseShareURL(finalURL); parseError == nil && id != "" {
		return id, nil
	}
	for _, element := range metaPattern.FindAllString(string(page), -1) {
		attributes := elementAttributes(element)
		if attributes["rel"] == "canonical" || attributes["property"] == "og:url" {
			candidate := attributes["href"]
			if candidate == "" {
				candidate = attributes["content"]
			}
			if id, _, parseError := ParseShareURL(candidate); parseError == nil && id != "" {
				return id, nil
			}
		}
	}
	return "", errors.New("Suno share code did not resolve to a song")
}

func (client *Client) readPage(ctx context.Context, address string) ([]byte, string, error) {
	response, err := client.request(ctx, http.MethodGet, address, nil, pageRequest, false)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, "", &APIError{Operation: "share page", Status: response.StatusCode, Reason: http.StatusText(response.StatusCode)}
	}
	contents, err := readLimited(response.Body, 8<<20)
	finalURL := address
	if response.Request != nil {
		finalURL = response.Request.URL.String()
	}
	return contents, finalURL, err
}

type mediaURL struct {
	URL         string `json:"url"`
	ContentType string `json:"content_type"`
	Delivery    string `json:"delivery"`
	Encoding    string `json:"encoding"`
}

type clipResponse struct {
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	Status    string     `json:"status"`
	AudioURL  string     `json:"audio_url"`
	MediaURLs []mediaURL `json:"media_urls"`
	IsTrashed bool       `json:"is_trashed"`
}

func (client *Client) Resolve(ctx context.Context, shareURL string) (Track, error) {
	clipID, err := client.clipID(ctx, shareURL)
	if err != nil {
		return Track{}, err
	}
	track := Track{ID: clipID, Title: clipID, ShareURL: "https://suno.com/song/" + clipID}
	var clip clipResponse
	apiError := client.apiJSON(ctx, http.MethodGet, "clip/"+clipID+"/", nil, &clip, true)
	if apiError != nil {
		var failure *APIError
		if errors.As(apiError, &failure) && (failure.Status == 401 || failure.Status == 403) {
			return Track{}, apiError
		}
		page, _, pageError := client.readPage(ctx, track.ShareURL)
		if pageError != nil {
			return Track{}, apiError
		}
		clip = parsePageClip(page, clipID)
	}
	if clip.ID != clipID {
		return Track{}, errors.New("Suno did not return the requested song")
	}
	if clip.IsTrashed {
		return Track{}, errors.New("the Suno song has been deleted")
	}
	if clip.Status != "complete" {
		return Track{}, ErrNotReady
	}
	if strings.TrimSpace(clip.Title) != "" {
		track.Title = strings.TrimSpace(clip.Title)
	}
	if validAudioAddress(clip.AudioURL) {
		track.AudioURL = clip.AudioURL
		track.Format = addressFormat(clip.AudioURL)
		track.Source = "public_audio_url"
		return track, nil
	}
	for _, media := range clip.MediaURLs {
		if media.Delivery != "" && media.Delivery != "progressive" {
			continue
		}
		if validAudioAddress(media.URL) {
			if media.Encoding != "" && media.Encoding != "1.0.0" {
				continue
			}
			track.AudioURL = media.URL
			track.Format = addressFormat(media.URL)
			track.Source = "public_media_url"
			track.Encoding = media.Encoding
			if media.Encoding != "" {
				track.Source = "playback_media"
			}
			return track, nil
		}
	}
	token, err := client.tokenValue(ctx)
	if err != nil {
		return Track{}, err
	}
	if token == "" {
		return Track{}, ErrAuthenticationRequired
	}
	return client.authorizedTrack(ctx, track)
}

func (client *Client) authorizedTrack(ctx context.Context, track Track) (Track, error) {
	if client.format != "mp3" && client.format != "wav" && client.format != "m4a" {
		return Track{}, errors.New("download format must be mp3, wav, or m4a")
	}
	path := "download/clip/" + track.ID + "?format=" + client.format
	var response struct {
		OK          bool   `json:"ok"`
		DownloadURL string `json:"download_url"`
		Status      string `json:"status"`
		Reason      string `json:"reason"`
	}
	err := client.apiJSON(ctx, http.MethodGet, path, nil, &response, true)
	needsAuthorization := response.Reason == "not_authorized"
	var failure *APIError
	if errors.As(err, &failure) && failure.Reason == "not_authorized" {
		needsAuthorization = true
	}
	if needsAuthorization {
		if !client.allowAuthorize {
			return Track{}, ErrAuthorizationRequired
		}
		var authorization struct {
			OK              bool   `json:"ok"`
			AlreadyUnlocked bool   `json:"already_unlocked"`
			CreditDeducted  bool   `json:"credit_deducted"`
			Reason          string `json:"reason"`
		}
		body := map[string]string{"item_id": track.ID, "item_type": "clip", "surface": "share"}
		if err := client.apiJSON(ctx, http.MethodPost, "download/authorize", body, &authorization, true); err != nil {
			return Track{}, err
		}
		if !authorization.OK {
			return Track{}, &APIError{Operation: "download/authorize", Status: 200, Reason: authorization.Reason}
		}
		track.CreditDeducted = authorization.CreditDeducted
		response = struct {
			OK          bool   `json:"ok"`
			DownloadURL string `json:"download_url"`
			Status      string `json:"status"`
			Reason      string `json:"reason"`
		}{}
		err = client.apiJSON(ctx, http.MethodGet, path, nil, &response, true)
	}
	if err != nil {
		return Track{}, err
	}
	if !response.OK {
		return Track{}, &APIError{Operation: "download/clip", Status: 200, Reason: response.Reason}
	}
	if !validAudioAddress(response.DownloadURL) {
		return Track{}, errors.New("Suno did not provide an approved downloadable audio URL")
	}
	track.AudioURL = response.DownloadURL
	track.Format = client.format
	track.Source = "authorized_download"
	return track, nil
}

func validAudioAddress(address string) bool {
	parsed, err := url.Parse(address)
	return err == nil && validateURL(parsed, audioRequest) == nil && !strings.Contains(parsed.Path, "sil-100.mp3")
}

func addressFormat(address string) string {
	parsed, _ := url.Parse(address)
	if parsed == nil {
		return ""
	}
	return strings.ToLower(strings.TrimPrefix(filepath.Ext(parsed.Path), "."))
}

func elementAttributes(element string) map[string]string {
	result := map[string]string{}
	for _, match := range attributePattern.FindAllStringSubmatch(element, -1) {
		value := match[2]
		if value == "" {
			value = match[3]
		}
		result[strings.ToLower(match[1])] = html.UnescapeString(value)
	}
	return result
}

func parsePageClip(contents []byte, clipID string) clipResponse {
	var joined strings.Builder
	joined.Write(contents)
	for _, match := range flightPattern.FindAllSubmatch(contents, -1) {
		var value string
		if json.Unmarshal(match[1], &value) == nil {
			joined.WriteString(value)
		}
	}
	text := joined.String()
	for pass := 0; pass < 3; pass++ {
		for position := 0; position < len(text); position++ {
			if text[position] != '{' {
				continue
			}
			decoder := json.NewDecoder(strings.NewReader(text[position:]))
			var value any
			if decoder.Decode(&value) == nil {
				if clip, ok := findClip(value, clipID, 0); ok {
					return clip
				}
			}
		}
		unescaped := strings.ReplaceAll(text, `\"`, `"`)
		if text == unescaped {
			break
		}
		text = unescaped
	}
	return clipResponse{}
}

func findClip(value any, clipID string, depth int) (clipResponse, bool) {
	if depth > 32 {
		return clipResponse{}, false
	}
	switch value := value.(type) {
	case map[string]any:
		if value["id"] == clipID && value["status"] != nil {
			contents, _ := json.Marshal(value)
			var clip clipResponse
			if json.Unmarshal(contents, &clip) == nil {
				return clip, true
			}
		}
		for _, child := range value {
			if clip, ok := findClip(child, clipID, depth+1); ok {
				return clip, true
			}
		}
	case []any:
		for _, child := range value {
			if clip, ok := findClip(child, clipID, depth+1); ok {
				return clip, true
			}
		}
	case string:
		if strings.Contains(value, clipID) {
			for position := 0; position < len(value); position++ {
				if value[position] == '{' {
					decoder := json.NewDecoder(strings.NewReader(value[position:]))
					var nested any
					if decoder.Decode(&nested) == nil {
						if clip, ok := findClip(nested, clipID, depth+1); ok {
							return clip, true
						}
					}
				}
			}
		}
	}
	return clipResponse{}, false
}

func (client *Client) Download(ctx context.Context, shareURL, outputPath string) (Result, error) {
	track, err := client.Resolve(ctx, shareURL)
	if err != nil {
		return Result{}, err
	}
	return client.DownloadTrack(ctx, track, outputPath)
}

func (client *Client) DownloadTrack(ctx context.Context, track Track, outputPath string) (Result, error) {
	if !uuidPattern.MatchString(track.ID) || !validAudioAddress(track.AudioURL) {
		return Result{}, errors.New("invalid song ID or unapproved audio URL")
	}
	var license *playbackLicense
	if track.Encoding != "" {
		if track.Encoding != "1.0.0" {
			return Result{}, errors.New("unsupported Suno playback encoding")
		}
		var err error
		license, err = client.playbackLicense(ctx, track.ID, track.Encoding)
		if err != nil {
			return Result{}, err
		}
		defer license.clear()
	}
	response, err := client.request(ctx, http.MethodGet, track.AudioURL, nil, audioRequest, false)
	if err != nil {
		return Result{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Result{}, &APIError{Operation: "audio download", Status: response.StatusCode, Reason: http.StatusText(response.StatusCode)}
	}
	if response.ContentLength > client.maxBytes {
		return Result{}, errors.New("audio file exceeds the download size limit")
	}
	var audio io.Reader = response.Body
	if license != nil {
		audio, err = license.reader(response.Body, 0)
		if err != nil {
			return Result{}, err
		}
	}
	header := make([]byte, 512)
	count, err := io.ReadFull(audio, header)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return Result{}, err
	}
	header = header[:count]
	format := detectAudioFormat(header)
	if format == "" {
		return Result{}, ErrEncodedMedia
	}
	track.Format = format
	path, err := outputFilename(track, outputPath)
	if err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return Result{}, err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".suno-download-*.part")
	if err != nil {
		return Result{}, err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	defer temporary.Close()
	hash := sha256.New()
	writer := io.MultiWriter(temporary, hash)
	total, err := io.Copy(writer, io.LimitReader(io.MultiReader(bytes.NewReader(header), audio), client.maxBytes+1))
	if err != nil {
		return Result{}, err
	}
	if total > client.maxBytes {
		return Result{}, errors.New("audio file exceeds the download size limit")
	}
	if response.ContentLength >= 0 && total != response.ContentLength {
		return Result{}, errors.New("audio download was incomplete")
	}
	if err := temporary.Sync(); err != nil {
		return Result{}, err
	}
	if err := temporary.Close(); err != nil {
		return Result{}, err
	}
	if err := os.Link(temporaryName, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return Result{}, errors.New("output file already exists; choose another path")
		}
		return Result{}, fmt.Errorf("publish completed download without overwriting files: %w", err)
	}
	return Result{Track: track, Path: path, Bytes: total, SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}

func detectAudioFormat(header []byte) string {
	if len(header) >= 3 && string(header[:3]) == "ID3" {
		return "mp3"
	}
	if len(header) >= 3 && header[0] == 0xff && header[1]&0xe0 == 0xe0 &&
		header[1]&0x18 != 0x08 && header[1]&0x06 != 0 &&
		header[2]&0xf0 != 0 && header[2]&0xf0 != 0xf0 && header[2]&0x0c != 0x0c {
		return "mp3"
	}
	if len(header) >= 12 && string(header[4:8]) == "ftyp" {
		return "m4a"
	}
	if len(header) >= 12 && string(header[:4]) == "RIFF" && string(header[8:12]) == "WAVE" {
		return "wav"
	}
	if len(header) >= 4 && string(header[:4]) == "OggS" {
		return "ogg"
	}
	if len(header) >= 4 && string(header[:4]) == "fLaC" {
		return "flac"
	}
	return ""
}

func outputFilename(track Track, outputPath string) (string, error) {
	filename := safeFilename(track.Title) + "-" + track.ID + "." + track.Format
	if outputPath == "" {
		outputPath = filepath.Join("downloads", filename)
	} else if information, err := os.Stat(outputPath); err == nil && information.IsDir() {
		outputPath = filepath.Join(outputPath, filename)
	} else if strings.HasSuffix(outputPath, "/") || strings.HasSuffix(outputPath, `\`) {
		outputPath = filepath.Join(outputPath, filename)
	} else if extension := filepath.Ext(outputPath); extension == "" {
		outputPath += "." + track.Format
	} else if !strings.EqualFold(extension, "."+track.Format) {
		return "", fmt.Errorf("audio is %s; choose an output path ending in .%s", track.Format, track.Format)
	}
	return filepath.Abs(outputPath)
}

func safeFilename(title string) string {
	var result strings.Builder
	count := 0
	for _, character := range strings.TrimSpace(title) {
		if count >= 80 {
			break
		}
		if unicode.IsControl(character) || strings.ContainsRune(`<>:"/\|?*`, character) {
			result.WriteRune('_')
		} else {
			result.WriteRune(character)
		}
		count++
	}
	name := strings.Trim(result.String(), " .")
	if name == "" {
		name = "song"
	}
	return name
}
