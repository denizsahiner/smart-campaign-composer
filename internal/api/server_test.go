package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPanicBecomesJSON500(t *testing.T) {
	h := logRequests(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	rec := serve(h, http.MethodGet, "/api/campaigns")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	var body map[string]apiError
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v (%q)", err, rec.Body.String())
	}
	if body["error"].Code != "internal" {
		t.Errorf("error code = %q, want internal", body["error"].Code)
	}
}

// Once the response has started the status can't change: the connection is aborted
// so the client doesn't take a cut-off response for a complete one.
func TestPanicAfterResponseStartedAborts(t *testing.T) {
	for name, start := range map[string]func(http.ResponseWriter){
		"WriteHeader": func(w http.ResponseWriter) { w.WriteHeader(http.StatusOK) },
		"Write":       func(w http.ResponseWriter) { w.Write([]byte("event: x\n\n")) },
	} {
		t.Run(name, func(t *testing.T) {
			h := logRequests(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				start(w)
				panic("boom")
			}))
			defer func() {
				if v := recover(); v != http.ErrAbortHandler {
					t.Fatalf("recovered %v, want http.ErrAbortHandler", v)
				}
			}()
			serve(h, http.MethodGet, "/api/campaigns/1/events")
			t.Fatal("no panic reached net/http")
		})
	}
}

func TestErrAbortHandlerPassesThrough(t *testing.T) {
	h := logRequests(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	defer func() {
		if v := recover(); v != http.ErrAbortHandler {
			t.Fatalf("recovered %v, want http.ErrAbortHandler", v)
		}
	}()
	serve(h, http.MethodGet, "/")
	t.Fatal("no panic reached net/http")
}

// Through a real server: the process survives and later requests are served.
func TestServerSurvivesPanic(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /panic", func(http.ResponseWriter, *http.Request) { panic("boom") })
	mux.HandleFunc("GET /ok", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	srv := httptest.NewServer(logRequests(mux))
	defer srv.Close()

	for path, want := range map[string]int{"/panic": 500, "/ok": 204} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("GET %s = %d, want %d", path, resp.StatusCode, want)
		}
	}
}
