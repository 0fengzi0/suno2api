package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"suno2api/internal/download"
	"suno2api/internal/generate"
)

type Downloader interface {
	Download(context.Context, string, string) (download.Result, error)
}
type Generator interface {
	Generate(context.Context, generate.Request) (generate.Response, error)
	Get(context.Context, []string) ([]generate.Clip, error)
}

type Config struct {
	Downloader     Downloader
	Generator      Generator
	DownloadDir    string
	APIKey         string
	RequestTimeout time.Duration
}

type handler struct {
	config Config
	mux    *http.ServeMux
}

func NewHandler(config Config) (http.Handler, error) {
	if config.Downloader == nil || config.Generator == nil {
		return nil, errors.New("download and generation clients are required")
	}
	if config.DownloadDir == "" {
		config.DownloadDir = "downloads"
	}
	root, err := filepath.Abs(config.DownloadDir)
	if err != nil {
		return nil, err
	}
	config.DownloadDir = root
	if config.RequestTimeout <= 0 {
		config.RequestTimeout = 2 * time.Minute
	}
	service := &handler{config: config, mux: http.NewServeMux()}
	service.mux.HandleFunc("GET /health", service.health)
	service.mux.HandleFunc("GET /download", service.downloadFile)
	service.mux.HandleFunc("POST /api/download", service.downloadJSON)
	service.mux.HandleFunc("POST /api/generate", service.generate)
	service.mux.HandleFunc("GET /api/songs", service.songs)
	return service, nil
}

func (service *handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Referrer-Policy", "no-referrer")
	if request.URL.Path == "/health" {
		service.mux.ServeHTTP(writer, request)
		return
	}
	if strings.EqualFold(request.Header.Get("Sec-Fetch-Site"), "cross-site") {
		writeError(writer, http.StatusForbidden, "cross-site requests are not allowed")
		return
	}
	if origin := request.Header.Get("Origin"); origin != "" {
		parsed, err := url.Parse(origin)
		if err != nil || parsed.Host != request.Host || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			writeError(writer, http.StatusForbidden, "cross-origin requests are not allowed")
			return
		}
	}
	if service.config.APIKey != "" {
		provided := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(provided), []byte(service.config.APIKey)) != 1 {
			writeError(writer, http.StatusUnauthorized, "local API key required")
			return
		}
	} else {
		host := request.Host
		if name, _, err := net.SplitHostPort(host); err == nil {
			host = name
		}
		address := net.ParseIP(strings.Trim(host, "[]"))
		if !strings.EqualFold(host, "localhost") && (address == nil || !address.IsLoopback()) {
			writeError(writer, http.StatusForbidden, "use localhost or configure a local API key")
			return
		}
	}
	ctx, cancel := context.WithTimeout(request.Context(), service.config.RequestTimeout)
	defer cancel()
	service.mux.ServeHTTP(writer, request.WithContext(ctx))
}

func (service *handler) health(writer http.ResponseWriter, request *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]any{"status": "ok", "service": "suno2api"})
}

func (service *handler) downloadFile(writer http.ResponseWriter, request *http.Request) {
	shareURL := strings.TrimSpace(request.URL.Query().Get("url"))
	if shareURL == "" {
		writeError(writer, http.StatusBadRequest, "url is required")
		return
	}
	directory, err := os.MkdirTemp("", "suno2api-download-")
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "cannot create a temporary download directory")
		return
	}
	defer os.Remove(directory)
	result, err := service.config.Downloader.Download(request.Context(), shareURL, directory)
	if err != nil {
		service.upstreamError(writer, err)
		return
	}
	path, err := filepath.Abs(result.Path)
	if err != nil || !within(directory, path) {
		writeError(writer, http.StatusInternalServerError, "download produced an invalid output path")
		return
	}
	defer os.Remove(path)
	file, err := os.Open(path)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "downloaded file is unavailable")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		writeError(writer, http.StatusInternalServerError, "downloaded file is unavailable")
		return
	}
	writer.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filepath.Base(path)}))
	if contentType := mime.TypeByExtension(filepath.Ext(path)); contentType != "" {
		writer.Header().Set("Content-Type", contentType)
	}
	http.ServeContent(writer, request, filepath.Base(path), info.ModTime(), file)
}

func (service *handler) downloadJSON(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	if !decodeBody(writer, request, &body) {
		return
	}
	if strings.TrimSpace(body.URL) == "" {
		writeError(writer, http.StatusBadRequest, "url is required")
		return
	}
	if err := os.MkdirAll(service.config.DownloadDir, 0755); err != nil {
		writeError(writer, http.StatusInternalServerError, "download directory is unavailable")
		return
	}
	directory, err := os.MkdirTemp(service.config.DownloadDir, "song-")
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "download directory is unavailable")
		return
	}
	result, err := service.config.Downloader.Download(request.Context(), body.URL, directory)
	if err != nil {
		os.Remove(directory)
		service.upstreamError(writer, err)
		return
	}
	if !within(directory, result.Path) {
		writeError(writer, http.StatusInternalServerError, "download produced an invalid output path")
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (service *handler) generate(writer http.ResponseWriter, request *http.Request) {
	var body generate.Request
	if !decodeBody(writer, request, &body) {
		return
	}
	if strings.TrimSpace(body.Prompt) == "" && strings.TrimSpace(body.Lyrics) == "" && !(body.Instrumental && strings.TrimSpace(body.Style) != "") {
		writeError(writer, http.StatusBadRequest, "prompt or lyrics are required")
		return
	}
	if body.BatchSize < 0 || body.BatchSize > 2 {
		writeError(writer, http.StatusBadRequest, "batch_size must be 1 or 2")
		return
	}
	response, err := service.config.Generator.Generate(request.Context(), body)
	if err != nil {
		if response.ID != "" || len(response.Clips) > 0 {
			status, message := upstreamFailure(err)
			writeJSON(writer, status, map[string]any{"error": message + "; Suno returned a submitted request, do not resubmit automatically", "submitted": true, "response": response, "ids": response.IDs(), "requested_ids": response.RequestedIDs()})
			return
		}
		service.upstreamError(writer, err)
		return
	}
	writeJSON(writer, http.StatusAccepted, map[string]any{"ids": response.IDs(), "requested_ids": response.RequestedIDs(), "response": response})
}

func (service *handler) songs(writer http.ResponseWriter, request *http.Request) {
	value := strings.TrimSpace(request.URL.Query().Get("ids"))
	if value == "" {
		writeError(writer, http.StatusBadRequest, "ids is required")
		return
	}
	ids := strings.Split(value, ",")
	if len(ids) > 20 {
		writeError(writer, http.StatusBadRequest, "at most 20 clip IDs are allowed")
		return
	}
	clips, err := service.config.Generator.Get(request.Context(), ids)
	if err != nil {
		service.upstreamError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"clips": clips})
}

func decodeBody(writer http.ResponseWriter, request *http.Request, destination any) bool {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(writer, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return false
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 1<<20)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid JSON body or body exceeds 1 MiB")
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		writeError(writer, http.StatusBadRequest, "only one JSON object is allowed")
		return false
	} else if !errors.Is(err, io.EOF) {
		writeError(writer, http.StatusBadRequest, "invalid trailing JSON data")
		return false
	}
	return true
}

func (service *handler) upstreamError(writer http.ResponseWriter, err error) {
	status, message := upstreamFailure(err)
	writeError(writer, status, message)
}

func upstreamFailure(err error) (int, string) {
	status := http.StatusBadGateway
	message := "Suno request failed; check your session and upstream connectivity"
	switch {
	case errors.Is(err, generate.ErrAuthentication), errors.Is(err, download.ErrAuthenticationRequired):
		status = http.StatusUnauthorized
		message = "Suno session is missing or expired; update the configured token file"
	case errors.Is(err, generate.ErrCaptchaRequired):
		status = http.StatusForbidden
		message = "Suno requires user verification in its own app or website"
	case errors.Is(err, generate.ErrInsufficientCredits):
		status = http.StatusPaymentRequired
		message = "Suno generation credits are insufficient"
	case errors.Is(err, generate.ErrRateLimited):
		status = http.StatusTooManyRequests
		message = "Suno rate limit reached; retry later"
	case errors.Is(err, download.ErrNotReady):
		status = http.StatusConflict
		message = "song is not ready for download"
	case errors.Is(err, context.DeadlineExceeded):
		status = http.StatusGatewayTimeout
		message = "Suno request timed out; generation may still be running, so check its ID before resubmitting"
	case errors.Is(err, download.ErrAuthorizationRequired):
		status = http.StatusForbidden
		message = "Suno requires download authorization"
	}
	var downloadError *download.APIError
	if errors.As(err, &downloadError) && (downloadError.Status == 401 || downloadError.Status == 403) {
		status = downloadError.Status
		message = "Suno denied access to this song"
	}
	return status, message
}

func within(directory, path string) bool {
	relative, err := filepath.Rel(directory, path)
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func writeError(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, map[string]string{"error": message})
}
func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
