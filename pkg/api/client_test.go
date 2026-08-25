package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func TestTruncate(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"short trimmed", "  err body \n", "err body"},
		{"exactly 300 kept", strings.Repeat("a", 300), strings.Repeat("a", 300)},
		{"301 cut", strings.Repeat("a", 301), strings.Repeat("a", 300) + "…"},
		// A 3-byte rune straddling the cut point must be dropped whole, not
		// split into invalid bytes.
		{"multibyte at boundary", strings.Repeat("a", 299) + "한글", strings.Repeat("a", 299) + "…"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := truncate([]byte(tt.in)); got != tt.want {
				t.Fatalf("truncate = %q, want %q", got, tt.want)
			}
		})
	}
}

func FuzzTruncate(f *testing.F) {
	f.Add("short")
	f.Add(strings.Repeat("한", 200))
	f.Add(strings.Repeat("a", 299) + "𝕏") // 4-byte rune at the cut
	f.Fuzz(func(t *testing.T, s string) {
		out := truncate([]byte(s))
		if utf8.ValidString(s) && !utf8.ValidString(out) {
			t.Fatalf("truncate broke UTF-8: %q → %q", s, out)
		}
		if len(out) > 300+len("…") {
			t.Fatalf("truncate output too long: %d bytes", len(out))
		}
	})
}

func TestClientRetriesServerErrors(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "flaky", http.StatusInternalServerError)
			return
		}
		w.Write([]byte(`{"clusters":[]}`))
	}))
	defer srv.Close()
	c, err := NewClient(srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.do(context.Background(), http.MethodGet, "/api/v1/clusters", nil, false, nil); err != nil {
		t.Fatalf("500-then-200 must succeed: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("calls = %d, want 2", got)
	}
}

func TestClientDoesNotRetryClientErrors(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "no such cluster", http.StatusNotFound)
	}))
	defer srv.Close()
	c, _ := NewClient(srv.URL, "")
	if err := c.do(context.Background(), http.MethodGet, "/x", nil, false, nil); err == nil {
		t.Fatal("404 must surface as an error")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("4xx must not be retried: %d calls", got)
	}
}

func TestClientHonorsContextDuringBackoff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer srv.Close()
	c, _ := NewClient(srv.URL, "")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := c.do(ctx, http.MethodGet, "/x", nil, false, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline error, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("backoff ignored context: took %v", elapsed)
	}
}
