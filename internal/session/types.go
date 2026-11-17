package session

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

var (
	ErrAuthentication             = errors.New("Suno session authentication is unavailable")
	ErrRefreshCredentialsRequired = errors.New("Suno session refresh requires valid authentication cookies")
	ErrUnsupportedCredential      = errors.New("unsupported Suno login credential")
	ErrInvalidResponse            = errors.New("Suno session refresh response was not valid")
	ErrConflict                   = errors.New("Suno session file changed or is locked")
	ErrRateLimited                = errors.New("Suno session refresh rate limit reached")
	ErrTemporary                  = errors.New("Suno session refresh is temporarily unavailable")
	ErrChallenge                  = errors.New("Suno authentication requires user verification")
	ErrRefreshRejected            = errors.New("Suno session refresh was blocked")
	ErrUnavailable                = errors.New("Suno session refresh retries were exhausted")
)

type Config struct {
	TokenFile      string
	AuthCookieFile string
	HTTPClient     *http.Client
	RefreshBefore  time.Duration
	AuthBaseURL    string
	RetryMin       time.Duration
	RetryMax       time.Duration
	LockTimeout    time.Duration
	MaxFailures    int
}

type Status struct {
	State                    string    `json:"state"`
	IssuedAt                 time.Time `json:"issued_at,omitempty"`
	ExpiresAt                time.Time `json:"expires_at,omitempty"`
	RefreshAt                time.Time `json:"refresh_at,omitempty"`
	CanRefresh               bool      `json:"can_refresh"`
	AuthCookieFileConfigured bool      `json:"auth_cookie_file_configured"`
	RefreshCount             int64     `json:"refresh_count"`
	LastRefresh              time.Time `json:"last_refresh,omitempty"`
	LastRefreshAttempt       time.Time `json:"last_refresh_attempt,omitempty"`
	LastRefreshError         string    `json:"last_refresh_error,omitempty"`
	ReauthenticationRequired bool      `json:"reauthentication_required"`
	NextRetryAt              time.Time `json:"next_retry_at,omitempty"`
}

type Event struct {
	State     string        `json:"state"`
	Status    Status        `json:"status"`
	ErrorCode string        `json:"error_code,omitempty"`
	RetryIn   time.Duration `json:"retry_in,omitempty"`
}

type Error struct {
	Code       string
	StatusCode int
	RetryAfter time.Duration
	cause      error
}

func (err *Error) Error() string {
	message := "Suno session operation failed"
	switch err.Code {
	case "authentication_required":
		message = "Suno refused session refresh; update the authentication cookies from your own signed-in session"
	case "refresh_credentials_required":
		message = "the token file does not contain authentication cookies for normal session refresh; update the file from your own Suno session"
	case "unsupported_credential":
		message = "the supplied credential is not a supported Suno login session; realtime subscription tokens cannot refresh login"
	case "invalid_response":
		message = "Suno did not return a matching, extended login session and valid session cookie; the token file was not changed"
	case "file_conflict":
		message = "the Suno token file changed during refresh or is locked; the newer file was preserved"
	case "rate_limited":
		message = "Suno session refresh is rate limited; retry after the recorded delay"
	case "temporary":
		message = "Suno session refresh could not complete; the existing token file was preserved"
	case "expired":
		message = "the Suno login session has expired and could not be refreshed; update the authentication cookies"
	case "verification_required":
		message = "Suno authentication requires a browser verification; complete it yourself in the official signed-in session"
	case "http_403_unclassified":
		message = "Suno blocked the refresh request with HTTP 403; the response did not establish whether authentication or another access check caused it"
	case "unavailable":
		message = "Suno keepalive stopped after bounded refresh failures; the existing token file was preserved"
	}
	if err.StatusCode != 0 {
		return fmt.Sprintf("%s (HTTP %d)", message, err.StatusCode)
	}
	return message
}

func (err *Error) Unwrap() error { return err.cause }

func errorCode(err error) string {
	var failure *Error
	if errors.As(err, &failure) {
		return failure.Code
	}
	return "temporary"
}
