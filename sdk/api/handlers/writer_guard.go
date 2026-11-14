package handlers

import (
	"sync"

	"github.com/gin-gonic/gin"
)

// writerGuardKey is the gin context key of the lock that orders keep-alive writes with other writers.
const writerGuardKey = "cpa.response-writer-guard"

// guardResponseWriter returns the lock that serialises c's response writer between a keep-alive writer
// and the request goroutine, creating it on first use. Call it before starting the goroutine that
// writes keep-alives; every concurrent write holds the lock.
func guardResponseWriter(c *gin.Context) *sync.Mutex {
	if guard := responseWriterGuard(c); guard != nil {
		return guard
	}
	guard := &sync.Mutex{}
	c.Set(writerGuardKey, guard)
	return guard
}

// responseWriterGuard returns the lock guardResponseWriter created for c, or nil when no keep-alive
// goroutine can be writing the response.
func responseWriterGuard(c *gin.Context) *sync.Mutex {
	if existing, found := c.Get(writerGuardKey); found {
		if guard, isGuard := existing.(*sync.Mutex); isGuard {
			return guard
		}
	}
	return nil
}
