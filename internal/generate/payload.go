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
	// The song description and the lyrics are different fields upstream. Style or title
	// alone must never switch a simple description into custom lyrics: that used to send
	// the description as the sung text even when instrumental was selected.
	description := strings.TrimSpace(request.Prompt)
	lyrics := strings.TrimSpace(request.Lyrics)
	style := strings.TrimSpace(request.Style)
	title := strings.TrimSpace(request.Title)
	custom := request.Custom || lyrics != ""
	if !custom && request.Instrumental && description == "" && style != "" {
		// A style-only instrumental request can only be expressed in custom mode.
		custom = true
	}
	payload := generationPayload{Model: model, Instrumental: request.Instrumental, GenerationType: "TEXT", Stream: false, BatchSize: batchSize, Token: request.CaptchaToken}
	if custom {
		payload.Metadata.CreateMode = "custom"
		payload.Tags = style
		payload.Title = title
		if request.Instrumental {
			// Instrumental custom mode has no vocal track, so it never carries lyrics.
			payload.Prompt = ""
			if style == "" {
				return generationPayload{}, errors.New("instrumental custom generation requires a musical style")
			}
		} else {
			payload.Prompt = lyrics
			if payload.Prompt == "" {
				return generationPayload{}, errors.New("custom generation requires lyrics; clear the custom flag to use a song description")
			}
		}
	} else {
		payload.Metadata.CreateMode = "simple"
		if description == "" {
			return generationPayload{}, errors.New("a song description is required")
		}
		payload.Description = description
	}
	return payload, nil
}
