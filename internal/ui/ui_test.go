package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUIHandlerRoutes(t *testing.T) {
	h := NewHandler()

	// Root "/" serves index.html
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / returned %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "KubePilot") || !strings.Contains(rec.Body.String(), "diagnose-form") {
		t.Fatalf("GET / missing expected HTML elements")
	}

	// Static assets "/assets/style.css" and "/assets/app.js"
	for _, asset := range []string{"/assets/style.css", "/assets/app.js"} {
		areq := httptest.NewRequest(http.MethodGet, asset, nil)
		arec := httptest.NewRecorder()
		h.ServeHTTP(arec, areq)
		if arec.Code != http.StatusOK || arec.Body.Len() == 0 {
			t.Fatalf("GET %s returned status=%d len=%d", asset, arec.Code, arec.Body.Len())
		}
	}

	// Unknown route returns 404
	nreq := httptest.NewRequest(http.MethodGet, "/nonexistent", nil)
	nrec := httptest.NewRecorder()
	h.ServeHTTP(nrec, nreq)
	if nrec.Code != http.StatusNotFound {
		t.Fatalf("GET /nonexistent returned %d, want 404", nrec.Code)
	}
}
