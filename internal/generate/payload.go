package generate

import (
	"errors"
	"strings"
)

type generationPayload struct {
	Prompt         string             `json:"prompt"`
	Model          string             `json:"mv"`
	Tags           string             `json:"tags,omitempty"`
	Title          string             `json:"title,omitempty"`
	Description    string             `json:"gpt_description_prompt,omitempty"`
	Instrumental   bool               `json:"make_instrumental"`
	GenerationType string             `json:"generation_type"`
	Stream         bool               `json:"stream"`
	BatchSize      int                `json:"batch_size"`
	Token          string             `json:"token,omitempty"`
	Metadata       generationMetadata `json:"metadata"`
}

type generationMetadata struct {
	CreateMode string `json:"create_mode"`
}

func (client *Client) payload(request Request) (generationPayload, error) {
	model := strings.TrimSpace(request.Model)
	if model == "" {
		model = strings.TrimSpace(client.defaultModel)
	}
	if model == "" {
		return generationPayload{}, errors.New("a Suno model is required")
	}
	if strings.EqualFold(model, "v6-mini") {
		model = DefaultModel
	}
	if len(model) > 128 || strings.ContainsAny(model, "\r\n\x00") {
		return generationPayload{}, errors.New("invalid Suno model key")
	}
	batchSize := request.BatchSize
	if batchSize == 0 {
		batchSize = 2
	}
	if batchSize < 1 || batchSize > 2 {
		return generationPayload{}, errors.New("batch_size must be 1 or 2")
	}
	payload := generationPayload{Model: model, Instrumental: request.Instrumental, GenerationType: "TEXT", Stream: false, BatchSize: batchSize, Token: request.CaptchaToken}
	custom := request.Custom || request.Lyrics != "" || request.Style != "" || request.Title != ""
	if custom {
		payload.Metadata.CreateMode = "custom"
		payload.Prompt = request.Lyrics
		if payload.Prompt == "" {
			payload.Prompt = request.Prompt
		}
		payload.Tags = request.Style
		payload.Title = request.Title
		if strings.TrimSpace(payload.Prompt) == "" && (!request.Instrumental || strings.TrimSpace(payload.Tags) == "") {
			return generationPayload{}, errors.New("custom generation requires lyrics, or a style for an instrumental song")
		}
	} else {
		payload.Metadata.CreateMode = "simple"
		if strings.TrimSpace(request.Prompt) == "" {
			return generationPayload{}, errors.New("a song description is required")
		}
		payload.Description = request.Prompt
	}
	return payload, nil
}
