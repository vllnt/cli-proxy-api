package budget

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/MoeForge/moego/ojson"
)

const (
	errorType = "rate_limit_error"
	errorCode = "budget_exhausted"
	// fallbackBody is returned if building the detailed body fails.
	fallbackBody = `{"error":{"message":"token budget exhausted","type":"rate_limit_error","code":"budget_exhausted"}}`
)

// ExhaustedError rejects a request because an enforced budget rule is used up.
// It names the label and model pattern, never the client key. Error() is a JSON
// error body so it reaches non-stream, stream and websocket clients unchanged, and
// SafeResponseHeaders() lets the handlers send Retry-After.
type ExhaustedError struct {
	Label   string
	Pattern string
	Window  string
	// ResetAt is when the exhausted window ends.
	ResetAt time.Time
	// RetryAfter is the time left until ResetAt when the request was rejected.
	RetryAfter time.Duration
}

// Error returns the JSON error body.
func (e *ExhaustedError) Error() string {
	message := "token budget exhausted"
	if e != nil {
		message = fmt.Sprintf("token budget exhausted for label %s (models %s, window %s); resets at %s",
			e.Label, e.Pattern, e.Window, e.ResetAt.UTC().Format(time.RFC3339))
	}
	doc := ojson.NewDocString(`{"error":{}}`)
	if err := doc.Set("error.message", message); err != nil {
		return fallbackBody
	}
	if err := doc.Set("error.type", errorType); err != nil {
		return fallbackBody
	}
	if err := doc.Set("error.code", errorCode); err != nil {
		return fallbackBody
	}
	return doc.String()
}

// StatusCode reports HTTP 429.
func (e *ExhaustedError) StatusCode() int { return http.StatusTooManyRequests }

// SafeResponseHeaders returns the locally computed Retry-After (whole seconds, rounded up, at least 1).
func (e *ExhaustedError) SafeResponseHeaders() http.Header {
	if e == nil {
		return nil
	}
	seconds := int64(e.RetryAfter / time.Second)
	if e.RetryAfter%time.Second > 0 {
		seconds++
	}
	if seconds < 1 {
		seconds = 1
	}
	return http.Header{"Retry-After": []string{strconv.FormatInt(seconds, 10)}}
}
