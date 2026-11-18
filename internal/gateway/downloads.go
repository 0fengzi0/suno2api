package gateway

import (
	"context"
	"errors"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type downloadInput struct {
	URL       string `json:"url"`
	AccountID string `json:"account_id,omitempty"`
}

func (gateway *Gateway) downloadAsset(ctx context.Context, input downloadInput) (FileView, error) {
	if strings.TrimSpace(input.URL) == "" {
		return FileView{}, errors.New("missing share URL")
	}
	gateway.mu.Lock()
	id, runtime, err := gateway.reserveDownloadAccountLocked(input.AccountID)
	gateway.mu.Unlock()
	if err != nil {
		return FileView{}, err
	}
	defer gateway.releaseAccount(id, runtime)
	fileID, err := randomID()
	if err != nil {
		return FileView{}, err
	}
	directory := filepath.Join(gateway.downloadDir, fileID)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return FileView{}, err
	}
	operation, cancel := accountContext(ctx, runtime, gateway.config.RequestTimeout)
	defer cancel()
	result, err := runtime.client.Download(operation, input.URL, directory)
	if err != nil {
		_ = os.Remove(directory)
		gateway.logf("warn", "download", "音频保存失败", map[string]any{"account": gateway.accountName(id), "error": operationErrorMessage(err)})
		return FileView{}, err
	}
	gateway.logf("info", "download", "音频已保存", map[string]any{"account": gateway.accountName(id), "file": filepath.Base(result.Path), "bytes": result.Bytes})
	relative, err := filepath.Rel(gateway.downloadDir, result.Path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return FileView{}, errors.New("invalid downloaded path")
	}
	view := FileView{ID: fileID, AccountID: id, Filename: filepath.Base(result.Path), Bytes: result.Bytes, SHA256: result.SHA256, CreatedAt: time.Now().UTC()}
	gateway.mu.Lock()
	gateway.state.Files[fileID] = &fileRecord{FileView: view, RelativePath: relative}
	err = gateway.persistFileLocked(fileID)
	gateway.mu.Unlock()
	if err != nil {
		return FileView{}, err
	}
	return view, nil
}

// reserveDownloadAccountLocked allows an explicitly selected account even when
// it is disabled for generation: the switch only limits new songs, not balance
// checks, renewal, or saving an existing song.
func (gateway *Gateway) reserveDownloadAccountLocked(accountID string) (string, *runtimeAccount, error) {
	if gateway.closed {
		return "", nil, errors.New("gateway is closed")
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		id, runtime, err := gateway.pickAccountLocked("", "")
		if err == nil {
			return id, runtime, nil
		}
		// Downloads are not generation: when every account is disabled for new
		// songs, fall back to a maintained account with spare capacity.
		ids := make([]string, 0, len(gateway.state.Accounts))
		for id := range gateway.state.Accounts {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			record := gateway.state.Accounts[id]
			runtime := gateway.accounts[id]
			if record.DeletedAt != nil || runtime == nil || record.MaxConcurrency > 0 && record.Inflight >= record.MaxConcurrency {
				continue
			}
			runtime.inflight++
			record.Inflight++
			return id, runtime, nil
		}
		return "", nil, errors.New("account unavailable")
	}
	record := gateway.state.Accounts[accountID]
	runtime := gateway.accounts[accountID]
	if record == nil || record.DeletedAt != nil || runtime == nil {
		return "", nil, errors.New("account unavailable")
	}
	if record.MaxConcurrency > 0 && record.Inflight >= record.MaxConcurrency {
		return "", nil, errors.New("account concurrency limit reached")
	}
	runtime.inflight++
	record.Inflight++
	return accountID, runtime, nil
}

func (gateway *Gateway) downloadJSON(writer http.ResponseWriter, request *http.Request) {
	var input downloadInput
	if !decodeJSON(writer, request, &input) {
		return
	}
	view, err := gateway.downloadAsset(request.Context(), input)
	if err != nil {
		status, code, message := operationError(err)
		writeError(writer, status, code, message)
		return
	}
	writeJSON(writer, 200, map[string]any{"file": view})
}
func (gateway *Gateway) downloadFileHTTP(writer http.ResponseWriter, request *http.Request) {
	view, err := gateway.downloadAsset(request.Context(), downloadInput{URL: request.URL.Query().Get("url"), AccountID: request.URL.Query().Get("account_id")})
	if err != nil {
		status, code, message := operationError(err)
		writeError(writer, status, code, message)
		return
	}
	gateway.serveFile(writer, request, view.ID)
}
func (gateway *Gateway) fileHTTP(writer http.ResponseWriter, request *http.Request) {
	gateway.serveFile(writer, request, request.PathValue("id"))
}
func (gateway *Gateway) serveFile(writer http.ResponseWriter, request *http.Request, id string) {
	gateway.mu.Lock()
	record := gateway.state.Files[id]
	if record == nil {
		gateway.mu.Unlock()
		writeError(writer, 404, "not_found", "音频文件不存在")
		return
	}
	path := filepath.Join(gateway.downloadDir, record.RelativePath)
	filename := record.Filename
	gateway.mu.Unlock()
	relative, err := filepath.Rel(gateway.downloadDir, path)
	if err != nil || strings.HasPrefix(relative, "..") || filepath.IsAbs(relative) {
		writeError(writer, 404, "not_found", "音频文件不存在")
		return
	}
	file, err := os.Open(path)
	if err != nil {
		writeError(writer, 404, "not_found", "音频文件不存在")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		writeError(writer, 404, "not_found", "音频文件不存在")
		return
	}
	writer.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	http.ServeContent(writer, request, filename, info.ModTime(), file)
}
