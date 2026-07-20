package handlers

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/jqutil"
	"github.com/krateoplatformops/snowplow/internal/support/audit"
	jqsupport "github.com/krateoplatformops/snowplow/internal/support/jq"
)

// exportParams are the /export-specific query parameters, stripped from
// the query before the request is re-dispatched through the /call lane.
var exportParams = []string{"format", "path", "fields", "filename"}

// Export returns the GET /export handler: a GENERIC serializer layered on
// top of the /call resolve lane. It re-dispatches the request through the
// given /call handler chain in-process (same RBAC gate, same serve-time
// user-aware filtering, same cache), extracts the row set from the
// resolved envelope and streams it as a CSV or JSON attachment. Because
// it is content-agnostic, ANY list/table widget or RESTAction resolvable
// via /call is exportable — nothing per-widget, nothing per-service.
//
// Query parameters (in addition to the /call ones):
//
//   - format:   "csv" (default) or "json"
//   - path:     optional jq expression selecting the rows within the
//     resolved envelope (e.g. ".status.items"); when omitted the
//     rows are auto-detected (top-level array, then .items, then
//     the first array under .status / .status.widgetData)
//   - fields:   optional comma-separated list of dot-paths selecting and
//     ordering the CSV columns (default: union of flattened keys)
//   - filename: optional attachment filename (sanitized; extension added)
//
// Every export emits an AuditEvent (action=export) so data egress is
// correlated end-to-end like any other action.
func Export(call http.Handler) http.Handler {
	return &exportHandler{call: call}
}

var _ http.Handler = (*exportHandler)(nil)

type exportHandler struct {
	// call is the composed GET /call handler chain (dispatcher included);
	// the export handler is a pure serializer on top of it.
	call http.Handler
}

// @Summary Export Endpoint
// @Description Export any /call-resolvable list as CSV or JSON
// @ID export
// @Param  apiVersion  query  string  true   "Resource API Group and Version"
// @Param  resource    query  string  true   "Resource Plural"
// @Param  name        query  string  true   "Resource name"
// @Param  namespace   query  string  true   "Resource namespace"
// @Param  format      query  string  false  "Export format: csv (default) or json"
// @Param  path        query  string  false  "JQ expression selecting the rows in the resolved envelope"
// @Param  fields      query  string  false  "Comma separated list of column dot-paths"
// @Param  filename    query  string  false  "Attachment file name"
// @Produce  text/csv
// @Produce  json
// @Success 200 {string} string
// @Failure 400 {object} response.Status
// @Failure 401 {object} response.Status
// @Failure 404 {object} response.Status
// @Failure 500 {object} response.Status
// @Router /export [get]
func (r *exportHandler) ServeHTTP(wri http.ResponseWriter, req *http.Request) {
	log := xcontext.Logger(req.Context())

	format := strings.ToLower(req.URL.Query().Get("format"))
	if format == "" {
		format = "csv"
	}
	if format != "csv" && format != "json" {
		writeExportError(wri, http.StatusBadRequest,
			fmt.Errorf("unsupported export format %q (want csv or json)", format))
		return
	}

	rowsPath := req.URL.Query().Get("path")
	fields := parseFields(req.URL.Query().Get("fields"))

	// Re-dispatch through the /call lane in-process: identical auth,
	// RBAC and resolve semantics — export can never see more than the
	// caller's own /call would return.
	inner := req.Clone(req.Context())
	q := inner.URL.Query()
	for _, p := range exportParams {
		q.Del(p)
	}
	inner.URL.RawQuery = q.Encode()

	rec := newExportRecorder()
	r.call.ServeHTTP(rec, inner)

	if rec.status != http.StatusOK {
		// Pass the /call failure envelope through untouched.
		for k, vv := range rec.header {
			for _, v := range vv {
				wri.Header().Add(k, v)
			}
		}
		wri.WriteHeader(rec.status)
		_, _ = wri.Write(rec.body.Bytes())
		return
	}

	var doc any
	if err := json.Unmarshal(rec.body.Bytes(), &doc); err != nil {
		writeExportError(wri, http.StatusInternalServerError,
			fmt.Errorf("resolved envelope is not JSON: %w", err))
		return
	}

	rows, err := extractRows(req.Context(), doc, rowsPath)
	if err != nil {
		writeExportError(wri, http.StatusBadRequest, err)
		return
	}

	name := req.URL.Query().Get("name")
	filename := attachmentName(req.URL.Query().Get("filename"), name, format)
	wri.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))

	switch format {
	case "json":
		wri.Header().Set("Content-Type", "application/json")
		wri.WriteHeader(http.StatusOK)
		enc := json.NewEncoder(wri)
		enc.SetIndent("", "  ")
		err = enc.Encode(rows)
	default:
		wri.Header().Set("Content-Type", "text/csv; charset=utf-8")
		wri.WriteHeader(http.StatusOK)
		err = writeCSV(wri, rows, fields)
	}
	if err != nil {
		log.Error("unable to serialize export", slog.Any("err", err))
	}

	outcome := "success"
	if err != nil {
		outcome = "failure"
	}
	audit.Emit(req.Context(), audit.Event{
		Action:    "export",
		Verb:      http.MethodGet,
		Resource:  req.URL.Query().Get("resource"),
		Name:      name,
		Namespace: req.URL.Query().Get("namespace"),
		Outcome:   outcome,
		Code:      http.StatusOK,
		Message:   fmt.Sprintf("format=%s rows=%d", format, len(rows)),
	})
}

// extractRows locates the row set inside the resolved envelope: an
// explicit jq path wins; otherwise common list shapes are auto-detected.
func extractRows(ctx context.Context, doc any, rowsPath string) ([]any, error) {
	if rowsPath != "" {
		s, err := jqutil.Eval(ctx, jqutil.EvalOptions{
			Query:        rowsPath,
			Data:         doc,
			ModuleLoader: jqsupport.ModuleLoader(),
		})
		if err != nil {
			return nil, fmt.Errorf("unable to evaluate rows path %q: %w", rowsPath, err)
		}
		var v any
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			v = s // bare string result
		}
		return normalizeRows(v), nil
	}

	return autoDetectRows(doc), nil
}

// autoDetectRows walks the conventional resolved shapes looking for the
// row array: top-level array → .items → .status (array, .items, or the
// first array found under .status / .status.widgetData in sorted key
// order). Falls back to a single-row export of the whole document.
func autoDetectRows(doc any) []any {
	if rows, ok := doc.([]any); ok {
		return rows
	}

	m, ok := doc.(map[string]any)
	if !ok {
		return []any{doc}
	}

	if rows, ok := m["items"].([]any); ok {
		return rows
	}

	if status, ok := m["status"]; ok {
		if rows, ok := status.([]any); ok {
			return rows
		}
		if sm, ok := status.(map[string]any); ok {
			if rows, ok := sm["items"].([]any); ok {
				return rows
			}
			if wd, ok := sm["widgetData"].(map[string]any); ok {
				if rows, ok := firstArray(wd); ok {
					return rows
				}
			}
			if rows, ok := firstArray(sm); ok {
				return rows
			}
		}
	}

	return []any{doc}
}

// firstArray returns the value of the first (sorted-key order,
// deterministic) top-level key holding a non-empty array.
func firstArray(m map[string]any) ([]any, bool) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if rows, ok := m[k].([]any); ok && len(rows) > 0 {
			return rows, true
		}
	}
	return nil, false
}

func normalizeRows(v any) []any {
	switch t := v.(type) {
	case nil:
		return []any{}
	case []any:
		return t
	default:
		return []any{v}
	}
}

// writeCSV flattens each row (nested maps become dot-path columns,
// arrays are JSON-encoded) and writes an RFC 4180 CSV. Column set/order:
// the explicit fields list when given, else the sorted union of the
// flattened keys across all rows.
func writeCSV(wri http.ResponseWriter, rows []any, fields []string) error {
	flat := make([]map[string]string, 0, len(rows))
	colSet := map[string]struct{}{}
	for _, row := range rows {
		fr := map[string]string{}
		flattenInto(fr, "", row)
		for k := range fr {
			colSet[k] = struct{}{}
		}
		flat = append(flat, fr)
	}

	cols := fields
	if len(cols) == 0 {
		cols = make([]string, 0, len(colSet))
		for k := range colSet {
			cols = append(cols, k)
		}
		sort.Strings(cols)
	}

	w := csv.NewWriter(wri)
	if err := w.Write(cols); err != nil {
		return err
	}
	rec := make([]string, len(cols))
	for _, fr := range flat {
		for i, c := range cols {
			rec[i] = fr[c]
		}
		if err := w.Write(rec); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

// flattenInto flattens nested maps to dot-path keys; scalars are
// stringified and composite leaves (arrays, empty maps) JSON-encoded.
func flattenInto(out map[string]string, prefix string, v any) {
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 0 {
			out[keyOrRoot(prefix)] = "{}"
			return
		}
		for k, cv := range t {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			flattenInto(out, p, cv)
		}
	case nil:
		out[keyOrRoot(prefix)] = ""
	case string:
		out[keyOrRoot(prefix)] = t
	case bool:
		out[keyOrRoot(prefix)] = fmt.Sprintf("%t", t)
	case float64:
		out[keyOrRoot(prefix)] = strconv64(t)
	default:
		dat, err := json.Marshal(t)
		if err != nil {
			out[keyOrRoot(prefix)] = fmt.Sprintf("%v", t)
			return
		}
		out[keyOrRoot(prefix)] = string(dat)
	}
}

func keyOrRoot(prefix string) string {
	if prefix == "" {
		return "value"
	}
	return prefix
}

// strconv64 renders a JSON number without a spurious trailing ".000000".
func strconv64(f float64) string {
	if f == float64(int64(f)) {
		return fmt.Sprintf("%d", int64(f))
	}
	return fmt.Sprintf("%g", f)
}

func parseFields(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

var unsafeFilenameChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func attachmentName(requested, resource, format string) string {
	base := unsafeFilenameChars.ReplaceAllString(requested, "-")
	base = strings.Trim(base, "-.")
	if base == "" {
		base = unsafeFilenameChars.ReplaceAllString(resource, "-")
		base = strings.Trim(base, "-.")
		if base == "" {
			base = "export"
		}
		base = fmt.Sprintf("%s-%s", base, time.Now().UTC().Format("20060102-150405"))
	}
	if !strings.HasSuffix(base, "."+format) {
		base += "." + format
	}
	return base
}

func writeExportError(wri http.ResponseWriter, code int, err error) {
	wri.Header().Set("Content-Type", "application/json")
	wri.WriteHeader(code)
	_ = json.NewEncoder(wri).Encode(map[string]any{
		"kind":    "Status",
		"status":  "Failure",
		"code":    code,
		"message": err.Error(),
	})
}

// exportRecorder is a minimal in-process http.ResponseWriter capturing the
// /call lane response for post-serialization.
type exportRecorder struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func newExportRecorder() *exportRecorder {
	return &exportRecorder{header: http.Header{}, status: http.StatusOK}
}

func (r *exportRecorder) Header() http.Header { return r.header }

func (r *exportRecorder) WriteHeader(code int) { r.status = code }

func (r *exportRecorder) Write(p []byte) (int, error) { return r.body.Write(p) }
