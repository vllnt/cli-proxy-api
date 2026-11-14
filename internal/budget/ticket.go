package budget

import (
	"context"
	"time"
)

// Ticket records what Admit decided for one request: the label it was admitted under and the
// rules (with their windows) it matched. It travels in the request context so the usage record
// is charged to exactly what was admitted, even if the window rolls over or the config reloads
// while a stream is still running. A Ticket is immutable once issued.
type Ticket struct {
	label string
	rules []ticketRule
}

type ticketRule struct {
	id         string
	start, end time.Time
}

func (t *Ticket) rule(id string) (ticketRule, bool) {
	if t == nil {
		return ticketRule{}, false
	}
	for _, r := range t.rules {
		if r.id == id {
			return r, true
		}
	}
	return ticketRule{}, false
}

type ticketContextKey struct{}

// WithTicket stores the admission ticket in ctx. A nil ticket leaves ctx unchanged.
func WithTicket(ctx context.Context, t *Ticket) context.Context {
	if t == nil {
		return ctx
	}
	return context.WithValue(ctx, ticketContextKey{}, t)
}

// TicketFromContext returns the admission ticket stored by WithTicket, or nil.
func TicketFromContext(ctx context.Context) *Ticket {
	if ctx == nil {
		return nil
	}
	t, _ := ctx.Value(ticketContextKey{}).(*Ticket)
	return t
}
