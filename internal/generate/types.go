package generate

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrCaptchaRequired     = errors.New("Suno requires user verification")
	ErrRateLimited         = errors.New("Suno rate limit reached")
	ErrInsufficientCredits = errors.New("Suno credits are insufficient")
	ErrGenerationFailed    = errors.New("Suno generation failed")
)

type Request struct {
	Prompt       string `json:"prompt"`
	Lyrics       string `json:"lyrics,omitempty"`
	Style        string `json:"style,omitempty"`
	Title        string `json:"title,omitempty"`
	Model        string `json:"model,omitempty"`
	CaptchaToken string `json:"captcha_token,omitempty"`
	Instrumental bool   `json:"instrumental"`
	Custom       bool   `json:"custom"`
	BatchSize    int    `json:"batch_size,omitempty"`
}

type Clip struct {
	ID        string          `json:"id"`
	Status    string          `json:"status"`
	Title     string          `json:"title"`
	AudioURL  string          `json:"audio_url"`
	VideoURL  string          `json:"video_url,omitempty"`
	ImageURL  string          `json:"image_url,omitempty"`
	ModelName string          `json:"model_name,omitempty"`
	Metadata  json.RawMessage `json:"metadata,omitempty"`
	MediaURLs []MediaURL      `json:"media_urls,omitempty"`
}

type MediaURL struct {
	URL         string `json:"url"`
	ContentType string `json:"content_type,omitempty"`
	Delivery    string `json:"delivery,omitempty"`
	Encoding    string `json:"encoding,omitempty"`
}

func (clip Clip) hasAudio() bool {
	if clip.AudioURL != "" {
		return true
	}
	for _, media := range clip.MediaURLs {
		contentType := strings.ToLower(strings.TrimSpace(media.ContentType))
		bareAudio := contentType == "m4a-opus" || contentType == "m4a" || contentType == "mp3" || contentType == "wav" || contentType == "ogg" || contentType == "flac"
		if media.URL != "" && (contentType == "" || strings.HasPrefix(contentType, "audio/") || bareAudio) {
			return true
		}
	}
	return false
}

type Response struct {
	RequestedModel    string `json:"requested_model,omitempty"`
	ID                string `json:"id,omitempty"`
	Clips             []Clip `json:"clips"`
	Status            string `json:"status,omitempty"`
	CreatedAt         string `json:"created_at,omitempty"`
	MajorModelVersion string `json:"major_model_version,omitempty"`
	BatchSize         int    `json:"batch_size,omitempty"`
}

type Model struct {
	Name               string `json:"name"`
	ExternalKey        string `json:"external_key"`
	CanUse             bool   `json:"can_use"`
	IsDefaultModel     bool   `json:"is_default_model"`
	IsDefaultFreeModel bool   `json:"is_default_free_model"`
}

type BillingInfo struct {
	Plans []struct {
		ID      string `json:"id"`
		PlanKey string `json:"plan_key"`
	} `json:"plans"`
	Plan struct {
		ID string `json:"id"`
	} `json:"plan"`
	Models           []Model `json:"models"`
	Credits          int     `json:"credits"`
	TotalCreditsLeft int     `json:"total_credits_left"`
}

func (info BillingInfo) userTier() string {
	if info.Plan.ID != "" {
		return info.Plan.ID
	}
	for _, plan := range info.Plans {
		if strings.EqualFold(plan.PlanKey, "free") {
			return plan.ID
		}
	}
	return ""
}

type CaptchaCheck struct {
	Required       bool `json:"required"`
	CaptchaVersion int  `json:"captcha_version,omitempty"`
}

func (response Response) IDs() []string {
	ids := make([]string, 0, len(response.Clips))
	for _, clip := range response.Clips {
		if clip.ID != "" {
			ids = append(ids, clip.ID)
		}
	}
	return ids
}

func (response Response) RequestedIDs() []string {
	if response.RequestedModel == "" {
		return response.IDs()
	}
	ids := make([]string, 0, len(response.Clips))
	var unknown []string
	for _, clip := range response.Clips {
		if clip.ModelName == response.RequestedModel {
			ids = append(ids, clip.ID)
		} else if clip.ModelName == "" {
			unknown = append(unknown, clip.ID)
		}
	}
	if len(ids) > 0 {
		return ids
	}
	return unknown
}

type APIError struct {
	StatusCode int           `json:"status_code"`
	Code       string        `json:"code,omitempty"`
	Message    string        `json:"message"`
	Cause      error         `json:"-"`
	RetryAfter time.Duration `json:"-"`
}

func (err *APIError) Error() string {
	if err.Code != "" {
		return fmt.Sprintf("Suno API HTTP %d (%s): %s", err.StatusCode, err.Code, err.Message)
	}
	return fmt.Sprintf("Suno API HTTP %d: %s", err.StatusCode, err.Message)
}

func (err *APIError) Unwrap() error { return err.Cause }

type GenerationError struct{ Clips []Clip }

func (err *GenerationError) Error() string {
	ids := make([]string, 0, len(err.Clips))
	for _, clip := range err.Clips {
		ids = append(ids, clip.ID)
	}
	return "Suno generation failed for clips: " + strings.Join(ids, ", ")
}

func (err *GenerationError) Unwrap() error { return ErrGenerationFailed }
