// Package audit implements snowplow's end-to-end audit correlation
// mechanism (generic, use-case-agnostic):
//
//   - a correlation id (`X-Krateo-Correlation-Id`) accepted from the
//     caller (the portal injects it at the edge) or minted here when
//     absent, carried on the request context, echoed on the response,
//     and propagated into every downstream call an api-step performs
//     (see internal/resolvers/restactions/api/external_fetch.go);
//   - a normalized AuditEvent record, emitted as a structured slog
//     line on stdout. It deliberately rides the EXISTING
//     stdout -> log-collector (filelog/Vector/Fluent Bit) -> ClickHouse
//     pipeline (see internal/tracing doc contract): downstream pipeline
//     config can route records with `kind=AuditEvent` to the audit view
//     and/or an immutable WORM sink. Snowplow itself takes NO sink
//     dependency — the mechanism stays agnostic.
//
// COEXISTENCE CONTRACT: the shortid `X-Krateo-TraceId` (plumbing
// xcontext.TraceId) and the OTel W3C `traceparent` are UNTOUCHED. The
// correlation id is a THIRD, caller-owned id: it survives across many
// requests of one logical business action (trace ids are per-request),
// which is what lets an auditor link portal action -> composition ->
// adapter -> downstream object. When the caller sends nothing, the
// request's own trace id is reused so every AuditEvent always carries a
// non-empty correlation id.
package audit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
)

const (
	// HeaderCorrelationID is the wire header carrying the correlation id,
	// inbound (portal -> snowplow) and outbound (snowplow -> downstream).
	HeaderCorrelationID = "X-Krateo-Correlation-Id"

	// EventKind is the discriminator value downstream log pipelines route
	// on (e.g. `kind=AuditEvent` -> ClickHouse audit view + WORM sink).
	EventKind = "AuditEvent"

	// maxIDLen bounds an inbound correlation id; anything longer (or with
	// characters outside idPattern) is rejected and replaced, so a hostile
	// caller cannot inject log/pipeline control characters.
	maxIDLen = 128
)

// idPattern is the accepted shape of an inbound correlation id. Kept to
// header-safe token characters (no ":" — the AWS-signed outbound arm
// splits headers on the first colon).
var idPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

type contextKey struct{}

// CorrelationID returns the correlation id carried by ctx, or "" when the
// middleware did not run.
func CorrelationID(ctx context.Context) string {
	id, _ := ctx.Value(contextKey{}).(string)
	return id
}

// WithCorrelationID returns a ctx carrying the given correlation id.
func WithCorrelationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, contextKey{}, id)
}

// SanitizeID validates an inbound correlation id; it returns "" when the
// id is empty, oversized or contains unexpected characters.
func SanitizeID(id string) string {
	if id == "" || len(id) > maxIDLen || !idPattern.MatchString(id) {
		return ""
	}
	return id
}

// NewID mints a random correlation id (16 hex chars).
func NewID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Extremely unlikely; fall back to a time-derived id rather
		// than failing the request path.
		return hex.EncodeToString([]byte(time.Now().Format("150405.000")))
	}
	return hex.EncodeToString(b[:])
}

// Middleware resolves the request correlation id (inbound header ->
// request trace id -> minted), stores it on the context and echoes it on
// the response so the caller can persist/link it. It is additive-only:
// no request is ever rejected here.
func Middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(wri http.ResponseWriter, req *http.Request) {
			id := SanitizeID(req.Header.Get(HeaderCorrelationID))
			if id == "" {
				id = xcontext.TraceId(req.Context(), false)
			}
			if id == "" {
				id = NewID()
			}

			wri.Header().Set(HeaderCorrelationID, id)
			next.ServeHTTP(wri, req.WithContext(WithCorrelationID(req.Context(), id)))
		})
	}
}

// Event is the normalized platform-side audit record linking a portal/API
// action to the downstream object it touched. All fields are generic —
// nothing here is specific to any particular assembly of Krateo.
type Event struct {
	// Action is the logical operation ("call", "export", ...).
	Action string
	// Verb is the HTTP verb of the action (GET/POST/PUT/PATCH/DELETE).
	Verb string
	// Group/Version/Resource/Name/Namespace identify the object acted on.
	Group     string
	Version   string
	Resource  string
	Name      string
	Namespace string
	// Outcome is "success" or "failure".
	Outcome string
	// Code is the HTTP status code of the outcome (0 if unknown).
	Code int
	// Message is an optional human-readable detail (failure reason).
	Message string
}

// Emit writes the AuditEvent as a structured log record on the context
// logger (stdout JSON), carrying the correlation id, the acting user and
// the request trace id. Downstream shipping/immutability is pipeline
// configuration, not snowplow's concern.
func Emit(ctx context.Context, ev Event) {
	user := ""
	var groups []string
	if ui, err := xcontext.UserInfo(ctx); err == nil {
		user = ui.Username
		groups = ui.Groups
	}

	attrs := []any{
		slog.String("kind", EventKind),
		slog.String("correlationId", CorrelationID(ctx)),
		slog.String("action", ev.Action),
		slog.String("verb", ev.Verb),
		slog.String("group", ev.Group),
		slog.String("version", ev.Version),
		slog.String("resource", ev.Resource),
		slog.String("name", ev.Name),
		slog.String("namespace", ev.Namespace),
		slog.String("user", user),
		slog.Any("userGroups", groups),
		slog.String("outcome", ev.Outcome),
		slog.Int("code", ev.Code),
		slog.String("timestamp", time.Now().UTC().Format(time.RFC3339Nano)),
	}
	if ev.Message != "" {
		attrs = append(attrs, slog.String("message", ev.Message))
	}

	xcontext.Logger(ctx).Info("audit event", slog.Group("audit", attrs...))
}
