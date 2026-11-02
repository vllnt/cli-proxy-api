package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// ErrorCodeAPIKeyAllowlistUnavailable marks a request whose client API key is restricted to
// an account allowlist and no allowed, available credential remains.
const ErrorCodeAPIKeyAllowlistUnavailable = "api_key_allowlist_unavailable"

// newAPIKeyAllowlistUnavailableError returns the client-visible failure. It names no key,
// label or account.
func newAPIKeyAllowlistUnavailableError() *Error {
	return &Error{
		Code:       ErrorCodeAPIKeyAllowlistUnavailable,
		Message:    "no allowed account is currently available for this API key",
		HTTPStatus: http.StatusServiceUnavailable,
	}
}

// IsAPIKeyAllowlistUnavailable reports whether err (or an error it wraps) is the
// allowlist-exhausted failure.
func IsAPIKeyAllowlistUnavailable(err error) bool {
	var authErr *Error
	return errors.As(err, &authErr) && authErr != nil && authErr.Code == ErrorCodeAPIKeyAllowlistUnavailable
}

// authHasTag reports whether auth carries tag. Tags come from Attributes["tags"] or
// Metadata["tags"]; each is a comma separated string, a []string, or a []any of strings
// (the shape JSON auth files decode to). Entries are trimmed, comparison is case-sensitive
// and empty entries never match.
func authHasTag(auth *Auth, tag string) bool {
	tag = strings.TrimSpace(tag)
	if auth == nil || tag == "" {
		return false
	}
	if tagListContains(auth.Attributes["tags"], tag) {
		return true
	}
	return tagListContains(auth.Metadata["tags"], tag)
}

func tagListContains(raw any, tag string) bool {
	switch list := raw.(type) {
	case string:
		for _, item := range strings.Split(list, ",") {
			if strings.TrimSpace(item) == tag {
				return true
			}
		}
	case []string:
		for _, item := range list {
			if strings.TrimSpace(item) == tag {
				return true
			}
		}
	case []any:
		for _, item := range list {
			if text, ok := item.(string); ok && strings.TrimSpace(text) == tag {
				return true
			}
		}
	}
	return false
}

// restrictPickError maps "nothing selectable" pick failures (no candidate, all cooling down)
// of an allowlist-restricted request to the distinct allowlist error, so the client never
// sees which accounts exist. Other errors and unrestricted requests pass through unchanged.
func (e authSelectionEligibility) restrictPickError(ctx context.Context, providers []string, err error) error {
	if e.allowlist == nil || err == nil {
		return err
	}
	var authErr *Error
	if !isAuthUnavailableError(err) && !(errors.As(err, &authErr) && authErr != nil && authErr.Code == "auth_not_found") {
		return err
	}
	logEntryWithRequestID(ctx).Warnf("api-key allowlist: no allowed credential available | providers=%s", strings.Join(providers, ","))
	return newAPIKeyAllowlistUnavailableError()
}

func apiKeyAuthAllowlistFromOptions(opts cliproxyexecutor.Options) *cliproxyexecutor.APIKeyAuthAllowlist {
	return cliproxyexecutor.APIKeyAuthAllowlistFromMetadata(opts.Metadata)
}
