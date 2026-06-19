package portal

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestGetBodyRetriesTransient(t *testing.T) {
	old := httpBackoff
	httpBackoff = time.Millisecond
	defer func() { httpBackoff = old }()

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) <= 2 {
			w.WriteHeader(http.StatusInternalServerError) // flake twice
			return
		}
		fmt.Fprint(w, "OK")
	}))
	defer srv.Close()

	c := &Client{HTTP: http.DefaultClient, BaseURL: srv.URL}
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

func TestGetBodyFailsFastOn4xx(t *testing.T) {
	old := httpBackoff
	httpBackoff = time.Millisecond
	defer func() { httpBackoff = old }()

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := &Client{HTTP: http.DefaultClient, BaseURL: srv.URL}
	if _, err := c.getBody(context.Background(), srv.URL); err == nil {
		t.Fatal("expected error on 404")
	}
	if calls != 1 {
		t.Errorf("4xx should not retry, got %d calls", calls)
	}
}
