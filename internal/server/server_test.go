package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"suno2api/internal/download"
	"suno2api/internal/generate"
)

const fixtureID = "12345678-1234-4567-8901-123456789012"

type fakeDownloader struct {
	calls      int
	lastOutput string
}

func (fake *fakeDownloader) Download(ctx context.Context, shareURL, output string) (download.Result, error) {
	fake.calls++
	fake.lastOutput = output
	path := filepath.Join(output, "piano.m4a")
	if err := os.WriteFile(path, []byte("test-audio"), 0600); err != nil {
		return download.Result{}, err
	}
	return download.Result{Path: path, Bytes: 10, Track: download.Track{ID: fixtureID, Title: "Piano", Format: "m4a"}}, nil
}

type fakeGenerator struct {
	calls    int
	wait     bool
	fail     error
	accepted generate.Response
	last     generate.Request
}

func (fake *fakeGenerator) Generate(ctx context.Context, request generate.Request) (generate.Response, error) {
	fake.calls++
	fake.last = request
	if fake.wait {
		<-ctx.Done()
		return generate.Response{}, ctx.Err()
	}
	if fake.fail != nil {
		return fake.accepted, fake.fail
	}
	return generate.Response{ID: "generation-request", Clips: []generate.Clip{{ID: fixtureID, Status: "submitted"}}}, nil
}

func TestGenerationAcceptedResponseIsPreservedWhenValidationFails(t *testing.T) {
	handler, _, generator := fixtureHandler(t, Config{})
	generator.accepted = generate.Response{ID: "accepted-request", RequestedModel: "chirp-goose", Clips: []generate.Clip{{ID: fixtureID, ModelName: "chirp-goose", Status: "submitted"}}}
	generator.fail = errors.New("upstream response validation failed")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, localRequest("POST", "http://localhost/api/generate", `{"prompt":"piano"}`))
	var body struct {
		Submitted    bool              `json:"submitted"`
		IDs          []string          `json:"ids"`
		RequestedIDs []string          `json:"requested_ids"`
		Response     generate.Response `json:"response"`
		Error        string            `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusBadGateway || !body.Submitted || body.Response.ID != "accepted-request" || len(body.IDs) != 1 || len(body.RequestedIDs) != 1 || !strings.Contains(body.Error, "do not resubmit") {
		t.Fatalf("accepted request was lost: %d %s", response.Code, response.Body.String())
	}
	generator.accepted = generate.Response{}
	generator.fail = generate.ErrCaptchaRequired
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, localRequest("POST", "http://localhost/api/generate", `{"prompt":"piano"}`))
	if response.Code != http.StatusForbidden || strings.Contains(response.Body.String(), `"submitted":true`) {
		t.Fatal("preflight error falsely marked as submitted")
	}
}
func (fake *fakeGenerator) Get(ctx context.Context, ids []string) ([]generate.Clip, error) {
	return []generate.Clip{{ID: ids[0], Status: "submitted"}}, nil
}

func fixtureHandler(t *testing.T, config Config) (http.Handler, *fakeDownloader, *fakeGenerator) {
	t.Helper()
	downloader := &fakeDownloader{}
	generator := &fakeGenerator{}
	config.Downloader = downloader
	config.Generator = generator
	config.DownloadDir = t.TempDir()
	handler, err := NewHandler(config)
	if err != nil {
		t.Fatal(err)
	}
	return handler, downloader, generator
}
func localRequest(method, path, body string) *http.Request {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Host = "localhost"
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	return request
}

func TestDownloadStreamsOnlyGeneratedTemporaryFile(t *testing.T) {
	handler, downloader, _ := fixtureHandler(t, Config{})
	request := localRequest("GET", "http://localhost/download?url=https%3A%2F%2Fsuno.com%2Fs%2Fexample&path=C%3A%5Csecret.txt", "")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || response.Body.String() != "test-audio" {
		t.Fatalf("response %d %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Header().Get("Content-Disposition"), "attachment") {
		t.Fatal("missing download header")
	}
	if _, err := os.Stat(downloader.lastOutput); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("temporary download directory was not removed")
	}
}

func TestCrossSiteGenerationAndMissingAPIKeyAreRejected(t *testing.T) {
	for _, scenario := range []string{"origin", "fetch-site", "host", "api-key"} {
		t.Run(scenario, func(t *testing.T) {
			config := Config{}
			if scenario == "api-key" {
				config.APIKey = "local-key-for-testing"
			}
			handler, _, generator := fixtureHandler(t, config)
			request := localRequest("POST", "http://localhost/api/generate", `{"prompt":"piano"}`)
			switch scenario {
			case "origin":
				request.Header.Set("Origin", "https://other.invalid")
			case "fetch-site":
				request.Header.Set("Sec-Fetch-Site", "cross-site")
			case "host":
				request.Host = "attacker.invalid"
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != 403 && response.Code != 401 {
				t.Fatalf("unsafe request accepted: %d", response.Code)
			}
			if generator.calls != 0 {
				t.Fatal("blocked request submitted a generation")
			}
		})
	}
}

func TestGenerateAcceptsJSONAndReturnsIDsWithoutWaiting(t *testing.T) {
	handler, _, generator := fixtureHandler(t, Config{APIKey: "local-key-for-testing"})
	request := localRequest("POST", "http://localhost/api/generate", `{"prompt":"gentle piano","instrumental":true,"batch_size":1}`)
	request.Header.Set("Authorization", "Bearer local-key-for-testing")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 202 || generator.calls != 1 {
		t.Fatalf("generation response: %d %s", response.Code, response.Body.String())
	}
	var body struct {
		IDs []string `json:"ids"`
	}
	if json.Unmarshal(response.Body.Bytes(), &body) != nil || len(body.IDs) != 1 || body.IDs[0] != fixtureID {
		t.Fatal("generation IDs missing")
	}
	if generator.last.BatchSize != 1 || !generator.last.Instrumental {
		t.Fatal("generation options lost")
	}
}

func TestRejectsUnknownFieldsAndMultipleJSONDocuments(t *testing.T) {
	for _, body := range []string{`{"url":"https://suno.com/s/example","output":"C:\\secret"}`, `{"url":"https://suno.com/s/example"} {}`, `{"url":"` + strings.Repeat("a", 1<<20) + `"}`} {
		handler, downloader, _ := fixtureHandler(t, Config{})
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, localRequest("POST", "http://localhost/api/download", body))
		if response.Code != 400 || downloader.calls != 0 {
			t.Fatalf("invalid download body accepted: %d", response.Code)
		}
	}
}

func TestRequestTimeoutAndErrorRedaction(t *testing.T) {
	handler, _, generator := fixtureHandler(t, Config{RequestTimeout: time.Millisecond})
	generator.wait = true
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, localRequest("POST", "http://localhost/api/generate", `{"prompt":"piano"}`))
	if response.Code != 504 {
		t.Fatalf("timeout status: %d", response.Code)
	}
	handler, _, generator = fixtureHandler(t, Config{})
	generator.fail = errors.New("private-token-and-signed-url")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, localRequest("POST", "http://localhost/api/generate", `{"prompt":"piano"}`))
	data, _ := io.ReadAll(response.Result().Body)
	if strings.Contains(string(data), "private-token") {
		t.Fatal("upstream error exposed private data")
	}
}
