package gateway

import (
	"context"
	"net/http"
	"time"

	"suno2api/internal/download"
	"suno2api/internal/generate"
	"suno2api/internal/session"
)

type AccountClient interface {
	Generate(context.Context, generate.Request) (generate.Response, error)
	Get(context.Context, []string) ([]generate.Clip, error)
	Billing(context.Context) (generate.BillingInfo, error)
	Download(context.Context, string, string) (download.Result, error)
	Status(context.Context) (session.Status, error)
	Refresh(context.Context) error
	Run(context.Context, func(session.Event)) error
}

type AccountClientConfig struct {
	ID, TokenFile, AuthCookieFile, Protocol, Model string
	HTTPClient                                     *http.Client
	OnCaptchaCheck                                 func(generate.CaptchaCheck)
}
type AccountClientFactory func(AccountClientConfig) (AccountClient, error)
type Config struct {
	DataDir              string
	Proxy                string
	ProxyExplicit        bool
	AdminKey             string
	DownloadDir          string
	RequestTimeout       time.Duration
	DefaultModel         string
	DefaultProtocol      string
	APIStyle             string
	WebHandler           http.Handler
	AccountClientFactory AccountClientFactory
}

type AccountInput struct {
	Name           string `json:"name"`
	Cookie         string `json:"cookie,omitempty"`
	AuthCookie     string `json:"auth_cookie,omitempty"`
	Enabled        *bool  `json:"enabled,omitempty"`
	Weight         int    `json:"weight,omitempty"`
	MaxConcurrency int    `json:"max_concurrency,omitempty"`
	Protocol       string `json:"protocol,omitempty"`
	Model          string `json:"model,omitempty"`
}

type BillingView struct {
	Credits          int              `json:"credits"`
	TotalCreditsLeft int              `json:"total_credits_left"`
	Models           []generate.Model `json:"models"`
}
type AccountView struct {
	ID             string          `json:"id"`
	Name           string          `json:"name"`
	Enabled        bool            `json:"enabled"`
	Weight         int             `json:"weight"`
	MaxConcurrency int             `json:"max_concurrency"`
	Protocol       string          `json:"protocol"`
	Model          string          `json:"model"`
	State          string          `json:"state"`
	Inflight       int             `json:"inflight"`
	Billing        *BillingView    `json:"billing,omitempty"`
	Session        *session.Status `json:"session,omitempty"`
	LastError      string          `json:"last_error,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
	DeletedAt      *time.Time      `json:"deleted_at,omitempty"`
}

type ClipView struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Status     string `json:"status"`
	Model      string `json:"model"`
	MediaReady bool   `json:"media_ready"`
}
type JobView struct {
	RequestedModel string     `json:"requested_model,omitempty"`
	ID             string     `json:"id"`
	AccountID      string     `json:"account_id"`
	State          string     `json:"state"`
	RequestID      string     `json:"request_id,omitempty"`
	ClipIDs        []string   `json:"clip_ids"`
	RequestedIDs   []string   `json:"requested_ids,omitempty"`
	Clips          []ClipView `json:"clips"`
	Error          string     `json:"error,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}
type FileView struct {
	ID        string    `json:"id"`
	AccountID string    `json:"account_id"`
	Filename  string    `json:"filename"`
	Bytes     int64     `json:"bytes"`
	SHA256    string    `json:"sha256"`
	CreatedAt time.Time `json:"created_at"`
}
type APIKeyView struct {
	ID         string     `json:"id"`
	Label      string     `json:"label"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	Revoked    bool       `json:"revoked"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

type accountRecord struct {
	AccountView
	IdentityHash string `json:"identity_hash,omitempty"`
}
type keyRecord struct {
	APIKeyView
	Hash string `json:"hash"`
}
type fileRecord struct {
	FileView
	RelativePath string `json:"relative_path"`
}
type persistent struct {
	Version  int                       `json:"version"`
	Accounts map[string]*accountRecord `json:"accounts"`
	Jobs     map[string]*JobView       `json:"jobs"`
	Keys     map[string]*keyRecord     `json:"keys"`
	Files    map[string]*fileRecord    `json:"files"`
}
type adminSession struct {
	CSRF      string
	ExpiresAt time.Time
}
type loginAttempt struct {
	Count int
	Since time.Time
}
