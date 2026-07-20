package handlers

import (
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
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
