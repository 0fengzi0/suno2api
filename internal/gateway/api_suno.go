package gateway

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"suno2api/internal/generate"
)

var clipIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// sunoGenerationInput mirrors the fields Suno's own web client posts to
// /api/generate/v2/. Gateway-only fields are marked in the comment.
type sunoGenerationInput struct {
	Prompt               *string `json:"prompt"`
	Tags                 *string `json:"tags"`
	Title                *string `json:"title"`
	NegativeTags         string  `json:"negative_tags"`
	MV                   string  `json:"mv"`
	Model                string  `json:"model"`
	MakeInstrumental     *bool   `json:"make_instrumental"`
	Instrumental         *bool   `json:"instrumental"`
	GenerationType       string  `json:"generation_type"`
	BatchSize            int     `json:"batch_size"`
	CreateMode           string  `json:"create_mode"`
	CustomMode           *bool   `json:"custom_mode"`
	Custom               *bool   `json:"custom"`
	GptDescriptionPrompt *string `json:"gpt_description_prompt"`
	Metadata             struct {
		CreateMode string `json:"create_mode"`
	} `json:"metadata"`
	AccountID string `json:"account_id"`
}

func optionalString(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func optionalBool(primary, secondary *bool) bool {
	if primary != nil {
		return *primary
	}
	if secondary != nil {
		return *secondary
	}
	return false
}

func (input sunoGenerationInput) toGenerate(defaultModel string) (generate.Request, error) {
	if input.GenerationType != "" && !strings.EqualFold(input.GenerationType, "TEXT") {
		return generate.Request{}, errors.New("仅支持 generation_type=TEXT 的歌曲生成")
	}
	prompt := optionalString(input.Prompt)
	description := optionalString(input.GptDescriptionPrompt)
	style := optionalString(input.Tags)
	title := optionalString(input.Title)
	createMode := input.CreateMode
	if createMode == "" {
		createMode = input.Metadata.CreateMode
	}
	custom := false
	switch {
	case createMode != "":
		switch strings.ToLower(strings.TrimSpace(createMode)) {
		case "custom":
			custom = true
		case "simple":
			custom = false
		default:
			return generate.Request{}, errors.New("create_mode 只能是 simple 或 custom")
		}
	case input.CustomMode != nil:
		custom = *input.CustomMode
	case input.Custom != nil:
		custom = *input.Custom
	case input.GptDescriptionPrompt != nil:
		custom = false
	default:
		// Suno's native custom body carries lyrics in prompt plus tags/title.
		custom = prompt != "" && (style != "" || title != "")
	}
	lyrics := ""
	if custom {
		lyrics = prompt
	} else if description == "" {
		description = prompt
	}
	model := strings.TrimSpace(input.MV)
	if model == "" {
		model = strings.TrimSpace(input.Model)
	}
	if model == "" {
		model = defaultModel
	}
	return normalizeGeneration(generate.Request{
		Prompt:       description,
		Lyrics:       lyrics,
		Style:        style,
		Title:        title,
		Model:        model,
		Instrumental: optionalBool(input.MakeInstrumental, input.Instrumental),
		Custom:       custom,
		BatchSize:    input.BatchSize,
	}, defaultModel)
}

func clipIDsFromValues(values []string) ([]string, error) {
	ids := make([]string, 0, len(values))
	seen := make(map[string]bool)
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			id := strings.ToLower(strings.TrimSpace(part))
			if id == "" {
				continue
			}
			if !clipIDPattern.MatchString(id) {
				return nil, errors.New("歌曲 ID 必须是 UUID")
			}
			if !seen[id] {
				ids = append(ids, id)
				seen[id] = true
			}
		}
	}
	if len(ids) == 0 || len(ids) > 20 {
		return nil, errors.New("请提供 1 至 20 个歌曲 ID")
	}
	return ids, nil
}

// runtimeForAccount returns a client even when the account is disabled for
// generation, because billing, renewal, feed queries, and downloads still work.
func (gateway *Gateway) runtimeForAccount(accountID string) (*runtimeAccount, string, error) {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	if strings.TrimSpace(accountID) != "" {
		record := gateway.state.Accounts[accountID]
		runtime := gateway.accounts[accountID]
		if record == nil || record.DeletedAt != nil || runtime == nil {
			return nil, "", errors.New("账号不存在或不可用")
		}
		return runtime, accountID, nil
	}
	ids := make([]string, 0, len(gateway.state.Accounts))
	for id := range gateway.state.Accounts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	fallback := ""
	for _, id := range ids {
		record := gateway.state.Accounts[id]
		if record.DeletedAt != nil || gateway.accounts[id] == nil {
			continue
		}
		if record.Enabled {
			return gateway.accounts[id], id, nil
		}
		if fallback == "" {
			fallback = id
		}
	}
	if fallback != "" {
		return gateway.accounts[fallback], fallback, nil
	}
	return nil, "", errors.New("没有可用账号")
}

func (gateway *Gateway) accountForClipIDs(ids []string) string {
	wanted := make(map[string]bool, len(ids))
	for _, id := range ids {
		wanted[id] = true
	}
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	for _, field := range []func(*JobView) []string{
		func(job *JobView) []string { return job.RequestedIDs },
		func(job *JobView) []string { return job.ClipIDs },
	} {
		for _, job := range gateway.state.Jobs {
			for _, id := range field(job) {
				if wanted[id] {
					return job.AccountID
				}
			}
		}
	}
	return ""
}

func (gateway *Gateway) sunoGenerateHTTP(writer http.ResponseWriter, request *http.Request) {
	var input sunoGenerationInput
	if !decodeJSONLenient(writer, request, &input) {
		return
	}
	generateRequest, err := input.toGenerate(gateway.config.DefaultModel)
	if err != nil {
		writeError(writer, 400, "invalid_request", err.Error())
		return
	}
	job, runtime, err := gateway.startJob(input.AccountID, generateRequest.Model)
	if err != nil {
		writeError(writer, 503, "no_available_account", "没有可用账号，或已达到账号并发上限")
		return
	}
	settled, response, generateErr := gateway.executeJob(request.Context(), job.ID, runtime, generateRequest)
	if generateErr != nil && settled.State != "submitted" && settled.State != "complete" && settled.State != "no_requested_model" {
		status, code, message := operationError(generateErr)
		writeJSON(writer, status, map[string]any{"error": message, "code": code, "job_id": settled.ID, "submitted": false})
		return
	}
	clips := response.Clips
	if clips == nil {
		clips = []generate.Clip{}
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"id":              firstNonEmpty(response.ID, settled.ID),
		"clips":           clips,
		"status":          sunoJobStatus(settled.State),
		"requested_model": firstNonEmpty(response.RequestedModel, settled.RequestedModel),
		"job_id":          settled.ID,
		"batch_size":      len(clips),
	})
}

func (gateway *Gateway) sunoFeedHTTP(writer http.ResponseWriter, request *http.Request) {
	var ids []string
	accountID := ""
	if request.Method == http.MethodPost {
		var body struct {
			IDs       []string `json:"ids"`
			AccountID string   `json:"account_id"`
			Filters   struct {
				IDs struct {
					ClipIDs []string `json:"clip_ids"`
					ClipIds []string `json:"clipIds"`
				} `json:"ids"`
			} `json:"filters"`
		}
		if !decodeJSONLenient(writer, request, &body) {
			return
		}
		ids = append(ids, body.IDs...)
		ids = append(ids, body.Filters.IDs.ClipIDs...)
		ids = append(ids, body.Filters.IDs.ClipIds...)
		accountID = body.AccountID
	} else {
		ids = append(ids, request.URL.Query()["ids"]...)
		accountID = request.URL.Query().Get("account_id")
	}
	normalized, err := clipIDsFromValues(ids)
	if err != nil {
		writeError(writer, 400, "invalid_ids", err.Error())
		return
	}
	if accountID == "" {
		accountID = gateway.accountForClipIDs(normalized)
	}
	if accountID == "" {
		writeError(writer, 404, "not_found", "未找到这些歌曲对应的原账号，请指定 account_id")
		return
	}
	runtime, _, err := gateway.runtimeForAccount(accountID)
	if err != nil {
		writeError(writer, 409, "account_unavailable", err.Error())
		return
	}
	ctx, cancel := accountContext(request.Context(), runtime, time.Minute)
	defer cancel()
	clips, err := runtime.client.Get(ctx, normalized)
	if err != nil {
		status, code, message := operationError(err)
		writeError(writer, status, code, message)
		return
	}
	writeJSON(writer, 200, map[string]any{"clips": clips})
}

func (gateway *Gateway) sunoClipHTTP(writer http.ResponseWriter, request *http.Request) {
	id := strings.ToLower(strings.TrimSpace(request.PathValue("id")))
	if !clipIDPattern.MatchString(id) {
		writeError(writer, 400, "invalid_ids", "歌曲 ID 必须是 UUID")
		return
	}
	accountID := gateway.accountForClipIDs([]string{id})
	if accountID == "" {
		writeError(writer, 404, "not_found", "未找到这首歌曲对应的原账号")
		return
	}
	runtime, _, err := gateway.runtimeForAccount(accountID)
	if err != nil {
		writeError(writer, 409, "account_unavailable", err.Error())
		return
	}
	ctx, cancel := accountContext(request.Context(), runtime, time.Minute)
	defer cancel()
	clips, err := runtime.client.Get(ctx, []string{id})
	if err != nil {
		status, code, message := operationError(err)
		writeError(writer, status, code, message)
		return
	}
	if len(clips) == 0 {
		writeError(writer, 404, "not_found", "歌曲不存在")
		return
	}
	writeJSON(writer, 200, clips[0])
}

func (gateway *Gateway) sunoBillingHTTP(writer http.ResponseWriter, request *http.Request) {
	runtime, _, err := gateway.runtimeForAccount(request.URL.Query().Get("account_id"))
	if err != nil {
		writeError(writer, 503, "no_available_account", err.Error())
		return
	}
	ctx, cancel := accountContext(request.Context(), runtime, time.Minute)
	defer cancel()
	billing, err := runtime.client.Billing(ctx)
	if err != nil {
		status, code, message := operationError(err)
		writeError(writer, status, code, message)
		return
	}
	writeJSON(writer, 200, billing)
}

func (gateway *Gateway) sunoModelsHTTP(writer http.ResponseWriter, request *http.Request) {
	runtime, _, err := gateway.runtimeForAccount(request.URL.Query().Get("account_id"))
	if err != nil {
		writeJSON(writer, 200, map[string]any{"models": sunoModelCatalog()})
		return
	}
	ctx, cancel := accountContext(request.Context(), runtime, time.Minute)
	defer cancel()
	billing, err := runtime.client.Billing(ctx)
	if err != nil || len(billing.Models) == 0 {
		writeJSON(writer, 200, map[string]any{"models": sunoModelCatalog()})
		return
	}
	writeJSON(writer, 200, map[string]any{"models": billing.Models})
}

func sunoModelCatalog() []generate.Model {
	keys := []string{"chirp-goose", "chirp-fenix", "chirp-crow", "chirp-bluejay", "chirp-auk", "chirp-v4", "chirp-v3-5", "chirp-v3-0", "chirp-hawk"}
	models := make([]generate.Model, 0, len(keys))
	for index, key := range keys {
		models = append(models, generate.Model{Name: key, ExternalKey: key, CanUse: true, IsDefaultModel: index == 0, IsDefaultFreeModel: index == 0})
	}
	return models
}

func sunoJobStatus(state string) string {
	switch state {
	case "complete", "no_requested_model":
		return "complete"
	case "failed":
		return "failed"
	case "submission_unknown":
		return "unknown"
	default:
		return "submitted"
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// waitForJob polls until the job reaches a terminal state or the request ends.
func (gateway *Gateway) waitForJob(ctx context.Context, id string) JobView {
	deadline := time.Now().Add(gateway.config.RequestTimeout)
	for {
		job, _ := gateway.refreshJob(ctx, id)
		if job.ID == "" {
			return job
		}
		switch job.State {
		case "complete", "failed", "no_requested_model", "submission_unknown", "interrupted":
			return job
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return job
		}
		timer := time.NewTimer(3 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return job
		case <-timer.C:
		}
	}
}

func clipAudioURL(clip generate.Clip) string {
	if clip.AudioURL != "" {
		return clip.AudioURL
	}
	for _, media := range clip.MediaURLs {
		contentType := strings.ToLower(strings.TrimSpace(media.ContentType))
		if media.URL != "" && (contentType == "" || strings.HasPrefix(contentType, "audio/") || contentType == "m4a" || contentType == "m4a-opus" || contentType == "mp3" || contentType == "wav" || contentType == "ogg" || contentType == "flac") {
			return media.URL
		}
	}
	return ""
}
