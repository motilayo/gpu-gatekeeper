package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleDashboardUI(t *testing.T) {
	handler := handleDashboardUI()

	// 1. Root / should return 200 OK and HTML
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("unexpected Content-Type: %q", ct)
	}
	body := rec.Body.String()
	if !containsStr(body, "G-Gate Live GPU QoS") {
		t.Errorf("body missing expected title string")
	}

	// 2. Non-root path should return 404 Not Found
	req404 := httptest.NewRequest(http.MethodGet, "/random-path", nil)
	rec404 := httptest.NewRecorder()
	handler.ServeHTTP(rec404, req404)

	if rec404.Code != http.StatusNotFound {
		t.Fatalf("expected status 404 for non-root, got %d", rec404.Code)
	}
}

func TestHandleTenantsAPINilMap(t *testing.T) {
	handler := handleTenantsAPI(nil, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tenants", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("unexpected Content-Type: %q", ct)
	}

	var resp TenantsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal JSON: %v", err)
	}

	if resp.Tenants == nil || len(resp.Tenants) != 0 {
		t.Errorf("expected empty tenants list, got %v", resp.Tenants)
	}
	if resp.Timestamp == "" {
		t.Errorf("expected non-empty timestamp")
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
