package handlers

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// fakeCall stands in for the composed GET /call chain: it asserts the
// export-specific params were stripped and serves a canned envelope.
func fakeCall(t *testing.T, status int, body string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(wri http.ResponseWriter, req *http.Request) {
		for _, p := range exportParams {
			if req.URL.Query().Get(p) != "" {
				t.Errorf("export param %q leaked into the /call re-dispatch", p)
			}
		}
		wri.Header().Set("Content-Type", "application/json")
		wri.WriteHeader(status)
		_, _ = wri.Write([]byte(body))
	})
}

func doExport(t *testing.T, inner http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	Export(inner).ServeHTTP(rec, req)
	return rec
}

func TestExportCSVFromItems(t *testing.T) {
	body := `{"items":[
		{"name":"alpha","health":"OK","usage":{"cpu":2}},
		{"name":"beta","health":"Critical","usage":{"cpu":7}}
	]}`
	rec := doExport(t, fakeCall(t, http.StatusOK, body),
		"/export?apiVersion=templates.krateo.io/v1&resource=restactions&name=demo&namespace=ns&format=csv")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("content-type = %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("content-disposition = %q", cd)
	}

	rows, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("invalid csv: %v", err)
	}
	wantHeader := []string{"health", "name", "usage.cpu"}
	if !reflect.DeepEqual(rows[0], wantHeader) {
		t.Errorf("header = %v, want %v", rows[0], wantHeader)
	}
	if len(rows) != 3 {
		t.Fatalf("row count = %d, want 3", len(rows))
	}
	if !reflect.DeepEqual(rows[1], []string{"OK", "alpha", "2"}) {
		t.Errorf("row 1 = %v", rows[1])
	}
	if !reflect.DeepEqual(rows[2], []string{"Critical", "beta", "7"}) {
		t.Errorf("row 2 = %v", rows[2])
	}
}

// TestExportCSVFormulaInjectionNeutralized is the PR #116 arch-blocker RED arm:
// a /call row carrying spreadsheet-formula-trigger fields (=, +, -, @, and a
// leading tab/CR) must be emitted as NEUTRALIZED CSV cells — each prefixed with
// a single quote so a spreadsheet renders it as literal text, never an executable
// formula. RED = remove neutralizeCSVCell → the raw formula is emitted → the
// leading-quote assertions fail. Benign leading chars (a plain word, a numeric
// string) must pass through UNCHANGED (the guard keys on the first char only).
func TestExportCSVFormulaInjectionNeutralized(t *testing.T) {
	// Each field's first char is a formula trigger except `safe`/`num`.
	body := `{"items":[{` +
		`"eq":"=1+1",` +
		`"plus":"+cmd",` +
		`"minus":"-2+3",` +
		`"at":"@SUM(A1:A9)",` +
		`"tab":"\t=evil()",` +
		`"cr":"\r=evil()",` +
		`"safe":"alpha",` +
		`"num":"42"` +
		`}]}`
	rec := doExport(t, fakeCall(t, http.StatusOK, body),
		"/export?resource=restactions&name=demo&format=csv&fields=eq,plus,minus,at,tab,cr,safe,num")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	// Parse WITHOUT the csv reader's own trimming — we assert on the raw emitted
	// cell bytes via the csv reader (encoding/csv preserves a leading quote as a
	// literal char inside a quoted field; the neutralization quote is DATA).
	rows, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("invalid csv: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("row count = %d, want 2 (header + 1 data)", len(rows))
	}
	// Map header→value for the single data row.
	got := map[string]string{}
	for i, col := range rows[0] {
		got[col] = rows[1][i]
	}

	// Every formula-trigger cell must be neutralized (leading single quote +
	// the original text preserved after it).
	wantNeutralized := map[string]string{
		"eq":    "'=1+1",
		"plus":  "'+cmd",
		"minus": "'-2+3",
		"at":    "'@SUM(A1:A9)",
		"tab":   "'\t=evil()",
		"cr":    "'\r=evil()",
	}
	for col, want := range wantNeutralized {
		if got[col] != want {
			t.Errorf("CSV-injection VIOLATED for %q: cell = %q, want %q (leading quote neutralization missing — a spreadsheet would execute the formula)", col, got[col], want)
		}
	}
	// Benign cells pass through unchanged (guard keys on the first char only).
	if got["safe"] != "alpha" {
		t.Errorf("benign string cell mangled: %q, want %q", got["safe"], "alpha")
	}
	if got["num"] != "42" {
		t.Errorf("benign numeric-string cell mangled: %q, want %q", got["num"], "42")
	}
}

// TestNeutralizeCSVCell is the focused unit for the guard predicate.
func TestNeutralizeCSVCell(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"=1+1", "'=1+1"},
		{"+x", "'+x"},
		{"-x", "'-x"},
		{"@x", "'@x"},
		{"\tx", "'\tx"},
		{"\rx", "'\rx"},
		{"alpha", "alpha"},
		{"42", "42"},
		{"a=b", "a=b"}, // trigger not in first position → unchanged
	}
	for _, c := range cases {
		if got := neutralizeCSVCell(c.in); got != c.want {
			t.Errorf("neutralizeCSVCell(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestExportCSVFieldSelection(t *testing.T) {
	body := `{"items":[{"name":"alpha","health":"OK","noise":"x"}]}`
	rec := doExport(t, fakeCall(t, http.StatusOK, body),
		"/export?resource=restactions&name=demo&fields=name,health")

	rows, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("invalid csv: %v", err)
	}
	if !reflect.DeepEqual(rows[0], []string{"name", "health"}) {
		t.Errorf("header = %v", rows[0])
	}
	if !reflect.DeepEqual(rows[1], []string{"alpha", "OK"}) {
		t.Errorf("row = %v", rows[1])
	}
}

func TestExportJSONWithJQPath(t *testing.T) {
	body := `{"status":{"services":[{"name":"a"},{"name":"b"}]}}`
	rec := doExport(t, fakeCall(t, http.StatusOK, body),
		"/export?resource=restactions&name=demo&format=json&path=.status.services")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if len(rows) != 2 || rows[0]["name"] != "a" || rows[1]["name"] != "b" {
		t.Errorf("rows = %v", rows)
	}
}

func TestExportAutoDetectStatusArray(t *testing.T) {
	body := `{"status":{"records":[{"id":1}],"scalar":"x"}}`
	rec := doExport(t, fakeCall(t, http.StatusOK, body),
		"/export?resource=restactions&name=demo&format=json")

	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if len(rows) != 1 || rows[0]["id"] != float64(1) {
		t.Errorf("rows = %v", rows)
	}
}

// setExportPaging shrinks the internal full-list pagination bounds for a
// test and restores them on cleanup.
func setExportPaging(t *testing.T, pageSize, maxPages int) {
	t.Helper()
	origSize, origMax := exportPageSize, exportMaxPages
	exportPageSize, exportMaxPages = pageSize, maxPages
	t.Cleanup(func() { exportPageSize, exportMaxPages = origSize, origMax })
}

// pagedCall simulates a well-behaved /call target that honors the
// page/perPage window over a fixed dataset of `total` rows and records
// every dispatched (page, perPage) pair.
func pagedCall(t *testing.T, total int, calls *[][2]int) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(wri http.ResponseWriter, req *http.Request) {
		page, _ := strconv.Atoi(req.URL.Query().Get("page"))
		perPage, _ := strconv.Atoi(req.URL.Query().Get("perPage"))
		*calls = append(*calls, [2]int{page, perPage})

		start, end := (page-1)*perPage, page*perPage
		if start > total {
			start = total
		}
		if end > total {
			end = total
		}
		items := make([]string, 0, end-start)
		for i := start; i < end; i++ {
			items = append(items, fmt.Sprintf(`{"id":%d}`, i))
		}
		wri.Header().Set("Content-Type", "application/json")
		wri.WriteHeader(http.StatusOK)
		fmt.Fprintf(wri, `{"items":[%s]}`, strings.Join(items, ","))
	})
}

// TestExportPaginateAndConcatFullSet: an /export with NO page/perPage must
// export the FULL list by paginating the /call lane internally — not the
// single default page (the silent-truncation review fix).
func TestExportPaginateAndConcatFullSet(t *testing.T) {
	setExportPaging(t, 3, 100)
	var calls [][2]int
	rec := doExport(t, pagedCall(t, 7, &calls),
		"/export?resource=restactions&name=demo&format=json")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if len(rows) != 7 {
		t.Fatalf("rows = %d, want the full 7 (silent truncation?)", len(rows))
	}
	for i, r := range rows {
		if r["id"] != float64(i) {
			t.Errorf("row %d = %v, want id=%d (order must be preserved)", i, r, i)
		}
	}
	want := [][2]int{{1, 3}, {2, 3}, {3, 3}}
	if !reflect.DeepEqual(calls, want) {
		t.Errorf("inner /call dispatches = %v, want %v", calls, want)
	}
	if h := rec.Header().Get(exportTruncatedHeader); h != "" {
		t.Errorf("%s = %q on a complete export, want unset", exportTruncatedHeader, h)
	}
}

// TestExportCallerPaginationRespected: an explicit page/perPage exports
// exactly that /call window with a single dispatch — no internal loop.
func TestExportCallerPaginationRespected(t *testing.T) {
	setExportPaging(t, 3, 100)
	var calls [][2]int
	rec := doExport(t, pagedCall(t, 7, &calls),
		"/export?resource=restactions&name=demo&format=json&page=2&perPage=2")

	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if len(rows) != 2 || rows[0]["id"] != float64(2) || rows[1]["id"] != float64(3) {
		t.Errorf("rows = %v, want exactly the caller's window [2,3]", rows)
	}
	if want := [][2]int{{2, 2}}; !reflect.DeepEqual(calls, want) {
		t.Errorf("inner /call dispatches = %v, want the single caller window %v", calls, want)
	}
}

// TestExportTruncationCap: hitting the exportMaxPages bound truncates the
// export and flags it via the X-Export-Truncated header.
func TestExportTruncationCap(t *testing.T) {
	setExportPaging(t, 3, 2)
	var calls [][2]int
	rec := doExport(t, pagedCall(t, 100, &calls),
		"/export?resource=restactions&name=demo&format=json")

	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if len(rows) != 6 {
		t.Errorf("rows = %d, want the 2-page cap of 6", len(rows))
	}
	if len(calls) != 2 {
		t.Errorf("inner /call dispatches = %d, want the exportMaxPages bound of 2", len(calls))
	}
	if h := rec.Header().Get(exportTruncatedHeader); h != "true" {
		t.Errorf("%s = %q, want \"true\" on a capped export", exportTruncatedHeader, h)
	}
}

// TestExportNonPaginatingTargetNoDuplication: a target that IGNORES the
// injected pagination and always returns the same full set — exactly
// exportPageSize rows, the pathological shape — must be exported ONCE
// (fingerprint guard), not duplicated per page.
func TestExportNonPaginatingTargetNoDuplication(t *testing.T) {
	setExportPaging(t, 3, 100)
	var calls int
	ignoring := http.HandlerFunc(func(wri http.ResponseWriter, req *http.Request) {
		calls++
		wri.Header().Set("Content-Type", "application/json")
		wri.WriteHeader(http.StatusOK)
		_, _ = wri.Write([]byte(`{"items":[{"id":0},{"id":1},{"id":2}]}`))
	})
	rec := doExport(t, ignoring, "/export?resource=restactions&name=demo&format=json")

	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if len(rows) != 3 {
		t.Errorf("rows = %d, want 3 (identical pages must be collapsed, not concatenated)", len(rows))
	}
	if calls != 2 {
		t.Errorf("inner /call dispatches = %d, want 2 (page 2 detects the duplicate and stops)", calls)
	}
	if h := rec.Header().Get(exportTruncatedHeader); h != "" {
		t.Errorf("%s = %q, want unset", exportTruncatedHeader, h)
	}
}

// TestExportOverfullPageStopsLoop: a target that ignores the slice and
// returns MORE than exportPageSize rows in one shot is exported as-is
// with a single extra-free dispatch (row count != pageSize ⇒ exhausted).
func TestExportOverfullPageStopsLoop(t *testing.T) {
	setExportPaging(t, 2, 100)
	var calls int
	overfull := http.HandlerFunc(func(wri http.ResponseWriter, req *http.Request) {
		calls++
		wri.Header().Set("Content-Type", "application/json")
		wri.WriteHeader(http.StatusOK)
		_, _ = wri.Write([]byte(`{"items":[{"id":0},{"id":1},{"id":2},{"id":3},{"id":4}]}`))
	})
	rec := doExport(t, overfull, "/export?resource=restactions&name=demo&format=json")

	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if len(rows) != 5 {
		t.Errorf("rows = %d, want the full 5", len(rows))
	}
	if calls != 1 {
		t.Errorf("inner /call dispatches = %d, want 1", calls)
	}
}

func TestExportPassesThroughCallFailure(t *testing.T) {
	rec := doExport(t, fakeCall(t, http.StatusForbidden, `{"kind":"Status","status":"Failure","code":403}`),
		"/export?resource=restactions&name=demo")

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Failure") {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestExportRejectsUnknownFormat(t *testing.T) {
	rec := doExport(t, fakeCall(t, http.StatusOK, `{}`),
		"/export?resource=restactions&name=demo&format=xml")

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestAttachmentNameSanitized(t *testing.T) {
	got := attachmentName("../../evil name", "res", "csv")
	if strings.ContainsAny(got, "/\\ ") {
		t.Errorf("unsafe filename %q", got)
	}
	if !strings.HasSuffix(got, ".csv") {
		t.Errorf("missing extension: %q", got)
	}
}
