package web

import (
	"encoding/json"
	"net/http"
)

// APIError is the error envelope every endpoint uses (docs/04-API.md §0).
type APIError struct {
	Error APIErrorBody `json:"error"`
}

// APIErrorBody carries a stable machine code and a human message.
type APIErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Error codes the PWA branches on.
const (
	CodeAuthRequired   = "auth_required"
	CodeStepupRequired = "stepup_required"
	CodeCSRF           = "csrf"
	CodeRateLimited    = "rate_limited"
	CodeLockedOut      = "locked_out"
	CodeBadRequest     = "bad_request"
	CodeNotFound       = "not_found"
	CodeConflict       = "conflict"
	CodeMaxSessions    = "max_sessions"
	CodeSessionsRun    = "sessions_running"
	CodeInternal       = "internal"
	CodeForbidden      = "forbidden"
	CodeTooLarge       = "too_large"
)

// WriteError sends an error envelope.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	// A failure here means the client is gone; there is nothing useful to do.
	_ = json.NewEncoder(w).Encode(APIError{Error: APIErrorBody{Code: code, Message: message}})
}

// WriteJSON sends a successful JSON response.
//
// Cache-Control defaults to no-store but is not overwritten if the handler
// already set it. /api/wake-config is the one endpoint that must be cacheable —
// it is what lets the service worker show the Wake screen while the PC, and so
// this server, is unreachable — and stamping no-store over it would break the
// offline flow silently.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	if h.Get("Cache-Control") == "" {
		h.Set("Cache-Control", "no-store")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// DecodeJSON reads a JSON body with a size cap and rejects unknown fields, so a
// client cannot smuggle an extra field past a handler that ignores it.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	const maxBody = 1 << 20 // 1 MiB is ample for every JSON endpoint
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "malformed request body")
		return false
	}
	return true
}
