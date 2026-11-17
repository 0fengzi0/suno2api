package gateway

import (
	"context"
	"net/http"
	"strings"
	"time"

	"suno2api/internal/generate"
)

type openAIGenerationInput struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	Lyrics         string `json:"lyrics"`
	Style          string `json:"style"`
	Title          string `json:"title"`
	Instrumental   bool   `json:"instrumental"`
	Custom         bool   `json:"custom"`
	N              int    `json:"n"`
	AccountID      string `json:"account_id"`
	Wait           bool   `json:"wait"`
	ResponseFormat string `json:"response_format"`
}

func openAIModelIDs() []string {
	return []string{"v6-mini", "chirp-goose", "v6", "chirp-hawk", "v5_5", "chirp-fenix", "v5", "chirp-crow", "v4_5_plus", "chirp-bluejay", "v4_5", "chirp-auk", "v4", "chirp-v4", "v3_5", "chirp-v3-5", "v3", "chirp-v3-0"}
}

func openAIModelObject(id string) map[string]any {
	return map[string]any{"id": id, "object": "model", "created": 0, "owned_by": "suno"}
}

func (gateway *Gateway) openAIModelsHTTP(writer http.ResponseWriter, request *http.Request) {
	data := make([]map[string]any, 0, len(openAIModelIDs()))
	for _, id := range openAIModelIDs() {
		data = append(data, openAIModelObject(id))
	}
	writeJSON(writer, 200, map[string]any{"object": "list", "data": data})
}

func (gateway *Gateway) openAIModelHTTP(writer http.ResponseWriter, request *http.Request) {
	requested := strings.ToLower(strings.TrimSpace(request.PathValue("id")))
	for _, id := range openAIModelIDs() {
		if strings.ToLower(id) == requested {
			writeJSON(writer, 200, openAIModelObject(id))
			return
		}
	}
	writeOpenAIError(writer, 404, "model_not_found", "模型不存在")
}

func openAIJobStatus(state string) string {
	switch state {
	case "complete", "no_requested_model":
		return "completed"
	case "failed":
		return "failed"
	case "submission_unknown":
		return "unknown"
	case "interrupted":
		return "cancelled"
	case "submitted", "submitting":
		return "processing"
	default:
		return "queued"
	}
}

func openAIAudioObjects(job JobView, clips []generate.Clip) []map[string]any {
	data := make([]map[string]any, 0, len(clips))
	created := job.CreatedAt.Unix()
	if job.CreatedAt.IsZero() {
		created = time.Now().Unix()
	}
	for _, clip := range clips {
		if clip.ID == "" {
			continue
		}
		data = append(data, map[string]any{
			"id":       clip.ID,
			"object":   "audio",
			"created":  created,
			"model":    firstNonEmpty(clip.ModelName, job.RequestedModel),
			"title":    clip.Title,
			"status":   clip.Status,
			"url":      clipAudioURL(clip),
			"duration": nil,
		})
	}
	if len(data) == 0 {
		for _, clip := range job.Clips {
			if clip.ID == "" {
				continue
			}
			data = append(data, map[string]any{
				"id":       clip.ID,
				"object":   "audio",
				"created":  created,
				"model":    firstNonEmpty(clip.Model, job.RequestedModel),
				"title":    clip.Title,
				"status":   clip.Status,
				"url":      "",
				"duration": nil,
			})
		}
	}
	return data
}

func openAIGenerationPayload(job JobView, clips []generate.Clip) map[string]any {
	payload := map[string]any{
		"id":      job.ID,
		"object":  "audio.generation",
		"created": job.CreatedAt.Unix(),
		"model":   job.RequestedModel,
		"status":  openAIJobStatus(job.State),
		"data":    openAIAudioObjects(job, clips),
	}
	if job.Error != "" && (job.State == "failed" || job.State == "submission_unknown") {
		payload["error"] = map[string]any{"message": job.Error, "code": job.State}
	}
	return payload
}

func writeOpenAIError(writer http.ResponseWriter, status int, code, message string) {
	kind := "invalid_request_error"
	if status >= 500 {
		kind = "server_error"
	}
	writeJSON(writer, status, map[string]any{"error": map[string]any{"message": message, "type": kind, "param": nil, "code": code}})
}

func (gateway *Gateway) openAIGenerateHTTP(writer http.ResponseWriter, request *http.Request) {
	var input openAIGenerationInput
	if !decodeJSONLenient(writer, request, &input) {
		return
	}
	if input.ResponseFormat != "" && input.ResponseFormat != "url" {
		writeOpenAIError(writer, 400, "unsupported_response_format", "当前只支持 response_format=url")
		return
	}
	batch := input.N
	if batch == 0 {
		batch = 1
	}
	model := strings.TrimSpace(input.Model)
	if model == "" {
		model = gateway.config.DefaultModel
	}
	normalized, err := normalizeGeneration(generate.Request{
		Prompt:       input.Prompt,
		Lyrics:       input.Lyrics,
		Style:        input.Style,
		Title:        input.Title,
		Model:        model,
		Instrumental: input.Instrumental,
		Custom:       input.Custom,
		BatchSize:    batch,
	}, gateway.config.DefaultModel)
	if err != nil {
		writeOpenAIError(writer, 400, "invalid_request", err.Error())
		return
	}
	job, runtime, err := gateway.startJob(input.AccountID, normalized.Model)
	if err != nil {
		writeOpenAIError(writer, 503, "no_available_account", "没有可用账号，或已达到账号并发上限")
		return
	}
	settled, response, generateErr := gateway.executeJob(request.Context(), job.ID, runtime, normalized)
	if generateErr != nil && settled.State != "submitted" && settled.State != "complete" && settled.State != "no_requested_model" {
		status, code, message := operationError(generateErr)
		writeJSON(writer, status, map[string]any{
			"error":  map[string]any{"message": message, "type": "server_error", "param": nil, "code": code},
			"job_id": settled.ID,
		})
		return
	}
	clips := response.Clips
	if input.Wait {
		settled = gateway.waitForJob(request.Context(), job.ID)
		if runtime, _, lookupErr := gateway.runtimeForAccount(settled.AccountID); lookupErr == nil {
			if live := gateway.liveClips(request.Context(), settled, runtime); len(live) > 0 {
				clips = live
			}
		}
	}
	writeJSON(writer, 200, openAIGenerationPayload(settled, clips))
}

func (gateway *Gateway) openAIGenerationHTTP(writer http.ResponseWriter, request *http.Request) {
	id := strings.TrimSpace(request.PathValue("id"))
	job, ok := gateway.jobSnapshot(id)
	if !ok {
		writeOpenAIError(writer, 404, "not_found", "生成任务不存在")
		return
	}
	job, _ = gateway.refreshJob(request.Context(), id)
	runtime, _, err := gateway.runtimeForAccount(job.AccountID)
	var clips []generate.Clip
	if err == nil {
		clips = gateway.liveClips(request.Context(), job, runtime)
	}
	writeJSON(writer, 200, openAIGenerationPayload(job, clips))
}

func (gateway *Gateway) openAIContentHTTP(writer http.ResponseWriter, request *http.Request) {
	job, ok := gateway.jobSnapshot(strings.TrimSpace(request.PathValue("id")))
	if !ok {
		writeOpenAIError(writer, 404, "not_found", "生成任务不存在")
		return
	}
	clipID := ""
	if len(job.RequestedIDs) > 0 {
		clipID = job.RequestedIDs[0]
	} else if len(job.ClipIDs) > 0 {
		clipID = job.ClipIDs[0]
	}
	if clipID == "" {
		writeOpenAIError(writer, 409, "audio_not_ready", "歌曲尚未提交完成，请稍后重试")
		return
	}
	view, err := gateway.downloadAsset(request.Context(), downloadInput{URL: "https://suno.com/song/" + clipID, AccountID: job.AccountID})
	if err != nil {
		status, code, message := operationError(err)
		writeOpenAIError(writer, status, code, message)
		return
	}
	gateway.serveFile(writer, request, view.ID)
}

func (gateway *Gateway) jobSnapshot(id string) (JobView, bool) {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	job := gateway.state.Jobs[id]
	if job == nil {
		return JobView{}, false
	}
	return *job, true
}

func (gateway *Gateway) liveClips(ctx context.Context, job JobView, runtime *runtimeAccount) []generate.Clip {
	if runtime == nil {
		return nil
	}
	ids := job.RequestedIDs
	if len(ids) == 0 {
		ids = job.ClipIDs
	}
	if len(ids) == 0 {
		return nil
	}
	ctx, cancel := accountContext(ctx, runtime, time.Minute)
	defer cancel()
	clips, err := runtime.client.Get(ctx, ids)
	if err != nil {
		return nil
	}
	return clips
}
