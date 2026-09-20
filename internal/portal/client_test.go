package portal

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/curbol/synty-sync/internal/model"
)

func TestGetBodyRetriesTransient(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) <= 2 {
			w.WriteHeader(http.StatusInternalServerError) // flake twice
			return
		}
		fmt.Fprint(w, "OK")
	}))
	defer srv.Close()

	c := &Client{Limits: testLimits(), HTTP: http.DefaultClient, BaseURL: srv.URL}
	body, err := c.getBody(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("getBody: %v", err)
	}
	if string(body) != "OK" {
		t.Errorf("body = %q", body)
	}
	if calls < 3 {
		t.Errorf("expected retries past the 500s, got %d calls", calls)
	}
}

func TestGetBodyRedactsCustomerID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	const id = "9876543"
	c := &Client{Limits: testLimits(), HTTP: http.DefaultClient, BaseURL: srv.URL, CustomerID: id}
	_, err := c.getBody(context.Background(), srv.URL+"/apps/downloads/orders/"+id+"?line_items_page=1")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), id) {
		t.Errorf("customer id leaked into error message: %q", err)
	}
}

func TestResolveReturnsClassifiableStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	c := &Client{Limits: testLimits(), HTTP: http.DefaultClient, BaseURL: srv.URL}
	_, _, err := c.Resolve(context.Background(), model.FileEntry{FileToken: "T", Variant: "Godot_4_5_1", DownloadHref: "/dl"})
	if err == nil {
		t.Fatal("expected an error on 403")
	}
	if code, ok := StatusOf(err); !ok || code != http.StatusForbidden {
		t.Errorf("StatusOf = %d,%v; want 403,true", code, ok)
	}
}

func TestResolveFilenameFromURL(t *testing.T) {
	// The signed URL answers with a bare "attachment" carrying no filename, so the
	// name has to come from the final URL path after the redirect.
	srv := downloadServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", "attachment")
		w.Write(zipBytes)
	})

	c := &Client{Limits: testLimits(), HTTP: http.DefaultClient, BaseURL: srv.URL}
	body, name, err := c.Resolve(context.Background(), model.FileEntry{FileToken: "T", Variant: "Godot_4_5_1", DownloadHref: "/dl"})
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	if name != "pack.zip" {
		t.Errorf("filename = %q, want pack.zip (from the final URL path)", name)
	}
}

func TestResolveSanitizesDispositionFilename(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No basename in the final URL path forces the Content-Disposition fallback,
		// which here carries a traversal attempt.
		w.Header().Set("Content-Disposition", `attachment; filename="../../evil.zip"`)
		w.Write(zipBytes)
	}))
	defer srv.Close()

	c := &Client{Limits: testLimits(), HTTP: http.DefaultClient, BaseURL: srv.URL}
	body, name, err := c.Resolve(context.Background(), model.FileEntry{FileToken: "T", Variant: "Godot", DownloadHref: "/"})
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	if name != "evil.zip" {
		t.Errorf("filename = %q, want the sanitized basename evil.zip", name)
	}
}
