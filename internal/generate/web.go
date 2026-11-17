package generate

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

var devicePattern = regexp.MustCompile(`[^a-zA-Z0-9-]`)

func newUUID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", fmt.Errorf("create local request UUID: %w", err)
	}
	data[6] = (data[6] & 0x0f) | 0x40
	data[8] = (data[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", data[0:4], data[4:6], data[6:8], data[8:10], data[10:16]), nil
}

func (client *Client) webDeviceID() (string, error) {
	if client.deviceID != "" {
		value := normalizeDevice(client.deviceID)
		if value == "" {
			return "", errors.New("invalid web device ID")
		}
		return value, nil
	}
	var contents string
	if client.tokenFile != "" {
		file, err := os.Open(client.tokenFile)
		if err != nil {
			return "", errors.New("cannot read Suno Cookie file for its device ID")
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, 65537))
		if err != nil || len(data) > 65536 {
			return "", errors.New("cannot read bounded Suno Cookie file")
		}
		contents = string(data)
	} else {
		contents = client.token
	}
	contents = strings.TrimSpace(contents)
	if strings.HasPrefix(strings.ToLower(contents), "cookie:") {
		contents = strings.TrimSpace(contents[7:])
	}
	request := http.Request{Header: http.Header{"Cookie": []string{contents}}}
	for _, cookie := range request.Cookies() {
		if cookie.Name == "ajs_anonymous_id" {
			value := normalizeDevice(cookie.Value)
			if value != "" {
				return value, nil
			}
		}
	}
	return "", errors.New("web protocol requires the ajs_anonymous_id from your Suno Cookie, or an explicit --device-id")
}

func normalizeDevice(value string) string {
	decoded, err := url.QueryUnescape(value)
	if err == nil {
		value = decoded
	}
	value = strings.Trim(value, `"`)
	value = devicePattern.ReplaceAllString(value, "")
	if len(value) > 128 {
		return ""
	}
	return value
}

func (client *Client) applyWebHeaders(request *http.Request) error {
	device, err := client.webDeviceID()
	if err != nil {
		return err
	}
	timestamp, _ := json.Marshal(map[string]int64{"timestamp": time.Now().UnixMilli()})
	browser, _ := json.Marshal(map[string]string{"token": base64.StdEncoding.EncodeToString(timestamp)})
	request.Header.Set("Device-Id", device)
	request.Header.Set("Browser-Token", string(browser))
	request.Header.Set("Accept-Language", "en-US,en;q=0.9")
	return nil
}

func (client *Client) webPayload(payload generationPayload, verification CaptchaCheck, userTier string) (map[string]any, error) {
	transaction, err := newUUID()
	if err != nil {
		return nil, err
	}
	metadata := map[string]any{"web_client_pathname": "/create", "create_mode": payload.Metadata.CreateMode, "create_session_token": client.createSessionToken, "disable_volume_normalization": false}
	if userTier != "" {
		metadata["user_tier"] = userTier
	}
	if payload.Metadata.CreateMode == "simple" {
		metadata["lyrics_model"] = "default"
	}
	body := map[string]any{"generation_type": payload.GenerationType, "mv": payload.Model, "prompt": payload.Prompt, "make_instrumental": payload.Instrumental, "transaction_uuid": transaction, "token": nil, "token_provider": nil, "metadata": metadata, "override_fields": []string{}, "user_uploaded_images_b64": nil, "cover_clip_id": nil, "cover_start_s": nil, "cover_end_s": nil, "persona_id": nil, "artist_clip_id": nil, "artist_start_s": nil, "artist_end_s": nil, "continue_clip_id": nil, "continued_aligned_prompt": nil, "continue_at": nil}
	if payload.Description != "" {
		body["gpt_description_prompt"] = payload.Description
	}
	if payload.Tags != "" {
		body["tags"] = payload.Tags
	}
	if payload.Title != "" {
		body["title"] = payload.Title
	}
	if payload.Token != "" {
		body["token"] = payload.Token
		provider := 1
		if verification.CaptchaVersion == 2 {
			provider = 2
		}
		body["token_provider"] = provider
	}
	return body, nil
}
