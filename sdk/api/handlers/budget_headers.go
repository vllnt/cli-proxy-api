package handlers

import (
	"net/http"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/gin-gonic/gin"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/budget"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/httpwire"
)

// budgetHeadersKey is the gin context key of the budgetHeaders holder of a budgeted request.
const budgetHeadersKey = "cpa.budget-response-headers"

// budgetHeaders carries the X-Budget-* headers from admission to whoever commits the response.
//
// Admission can run on any goroutine, including one that races a keep-alive writer, so it never
// touches the response: it only stores the headers here. The response writer reads them from the
// goroutine that commits the response.
type budgetHeaders struct {
	header atomic.Pointer[http.Header]
}

func (h *budgetHeaders) store(header http.Header) {
	h.header.Store(&header)
}

// BudgetHeadersMiddleware reports token budgets in X-Budget-* response headers. Register it after the
// middlewares that wrap the response writer, so those see the headers too.
//
// Without configured budgets, and for websocket upgrades, whose turns carry no headers, it does
// nothing: the response writer stays as it is.
func BudgetHeadersMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !budget.Default().Enabled() || isWebsocketUpgrade(c.Request) {
			c.Next()
			return
		}
		w := &budgetResponseWriter{ResponseWriter: c.Writer, headers: &budgetHeaders{}}
		c.Set(budgetHeadersKey, w.headers)
		c.Writer = w
		c.Next()
		// A handler that wrote nothing leaves gin to commit the empty response, which is too late
		// for the writer to see.
		w.applyBudgetHeaders()
	}
}

func isWebsocketUpgrade(req *http.Request) bool {
	return req != nil && strings.EqualFold(strings.TrimSpace(req.Header.Get("Upgrade")), "websocket")
}

// budgetResponseWriter sets the stored budget headers just before the response is committed. Headers
// that arrive later cannot change the response any more and are dropped.
type budgetResponseWriter struct {
	gin.ResponseWriter
	headers *budgetHeaders
}

func (w *budgetResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *budgetResponseWriter) WriteHeader(statusCode int) {
	w.applyBudgetHeaders()
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *budgetResponseWriter) WriteHeaderNow() {
	w.applyBudgetHeaders()
	w.ResponseWriter.WriteHeaderNow()
}

func (w *budgetResponseWriter) Write(data []byte) (int, error) {
	w.applyBudgetHeaders()
	return w.ResponseWriter.Write(data)
}

func (w *budgetResponseWriter) WriteString(data string) (int, error) {
	w.applyBudgetHeaders()
	return w.ResponseWriter.WriteString(data)
}

func (w *budgetResponseWriter) Flush() {
	_ = w.FlushError()
}

// FlushError forwards transport errors, like the writers it wraps.
func (w *budgetResponseWriter) FlushError() error {
	w.applyBudgetHeaders()
	return httpwire.FlushResponse(w.ResponseWriter)
}

func (w *budgetResponseWriter) applyBudgetHeaders() {
	if w.ResponseWriter.Written() {
		return
	}
	header := w.headers.header.Load()
	if header == nil {
		return
	}
	dst := w.ResponseWriter.Header()
	for key, values := range *header {
		dst[key] = slices.Clone(values)
	}
}
