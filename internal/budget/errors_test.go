package budget

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

type errorBody struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

func TestExhaustedError(t *testing.T) {
	err := &ExhaustedError{
		Label:      "team-x",
		Pattern:    "claude-fable-*",
		Window:     "week",
		ResetAt:    utc("2026-11-16T00:00:00Z"),
		RetryAfter: 90*time.Minute + time.Millisecond,
	}
	if got := err.StatusCode(); got != http.StatusTooManyRequests {
		t.Fatalf("StatusCode() = %d, want 429", got)
	}
	if got := err.SafeResponseHeaders().Get("Retry-After"); got != "5401" {
		t.Fatalf("Retry-After = %q, want 5401 (ceil of 5400.001s)", got)
	}
	var body errorBody
	if jsonErr := json.Unmarshal([]byte(err.Error()), &body); jsonErr != nil {
		t.Fatalf("Error() must be a JSON error body, got %q: %v", err.Error(), jsonErr)
	}
	if body.Error.Type != "rate_limit_error" || body.Error.Code != "budget_exhausted" {
		t.Fatalf("type/code = %q/%q", body.Error.Type, body.Error.Code)
	}
	for _, want := range []string{"team-x", "claude-fable-*", "week", "2026-11-16T00:00:00Z"} {
		if !strings.Contains(body.Error.Message, want) {
			t.Fatalf("message %q must mention %q", body.Error.Message, want)
		}
	}
}

func TestExhaustedErrorRetryAfterIsAtLeastOneSecond(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second, time.Nanosecond} {
		err := &ExhaustedError{RetryAfter: d}
		if got := err.SafeResponseHeaders().Get("Retry-After"); got != "1" {
			t.Fatalf("RetryAfter %v -> %q, want 1", d, got)
		}
	}
	if got := (&ExhaustedError{RetryAfter: 3 * time.Second}).SafeResponseHeaders().Get("Retry-After"); got != "3" {
		t.Fatalf("exact seconds must not round up, got %q", got)
	}
}

func TestExhaustedErrorEscapesAndSurvivesWrapping(t *testing.T) {
	err := &ExhaustedError{Label: `a"b\c`, Pattern: `m"*`, Window: "day", ResetAt: utc("2026-11-15T00:00:00Z")}
	var body errorBody
	if jsonErr := json.Unmarshal([]byte(err.Error()), &body); jsonErr != nil {
		t.Fatalf("special characters must stay valid JSON: %v", jsonErr)
	}
	if !strings.Contains(body.Error.Message, `a"b\c`) {
		t.Fatalf("message lost the label: %q", body.Error.Message)
	}
	wrapped := fmt.Errorf("outer: %w", err)
	var target *ExhaustedError
	if !errors.As(wrapped, &target) || target != err {
		t.Fatal("errors.As must find the ExhaustedError through wrapping")
	}
	var nilErr *ExhaustedError
	if nilErr.SafeResponseHeaders() != nil {
		t.Fatal("nil receiver must not produce headers")
	}
}

func TestTicketContext(t *testing.T) {
	if TicketFromContext(context.Background()) != nil {
		t.Fatal("no ticket expected")
	}
	if got := WithTicket(context.Background(), nil); TicketFromContext(got) != nil {
		t.Fatal("nil ticket must not be stored")
	}
	ticket := &Ticket{label: "team-x"}
	ctx := WithTicket(context.Background(), ticket)
	if got := TicketFromContext(ctx); got != ticket || got.Label() != "team-x" {
		t.Fatalf("round trip failed: %+v", got)
	}
	if (*Ticket)(nil).Label() != "" {
		t.Fatal("nil ticket label must be empty")
	}
}
