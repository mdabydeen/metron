package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInstalledModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			t.Errorf("asked for %s, want /api/tags", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"models":[{"name":"a:latest","model":"a:latest"},{"name":"b:7b","model":""},{"name":"","model":"c:1b"}]}`))
	}))
	defer srv.Close()

	got, err := installedModels(t.Context(), srv.URL+"/api/chat")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"a:latest", "b:7b", "c:1b"} {
		if !got[want] {
			t.Fatalf("%q missing from %v", want, got)
		}
	}
}

func TestInstalledModelsErrors(t *testing.T) {
	t.Run("bad url", func(t *testing.T) {
		if _, err := installedModels(t.Context(), "http://\x7f/api/chat"); err == nil ||
			!strings.Contains(err.Error(), "model list") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("unreachable", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := srv.URL + "/api/chat"
		srv.Close()
		if _, err := installedModels(t.Context(), url); err == nil {
			t.Fatal("want an error")
		}
	})

	t.Run("http error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()
		if _, err := installedModels(t.Context(), srv.URL+"/api/chat"); err == nil ||
			!strings.Contains(err.Error(), "HTTP 500") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("malformed body", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"models":`))
		}))
		defer srv.Close()
		if _, err := installedModels(t.Context(), srv.URL+"/api/chat"); err == nil {
			t.Fatal("want an error")
		}
	})
}
