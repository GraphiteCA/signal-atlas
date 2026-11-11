package engine

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testWiGLEClient(t *testing.T, handler http.HandlerFunc) *wigleClient {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	c := newWiGLEClient()
	c.base = server.URL
	c.delay = 0
	return c
}
func TestWiGLERetry(t *testing.T) {
	calls := 0
	c := testWiGLEClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Basic secret" {
			t.Error("missing auth")
		}
		calls++
		if calls <= 3 {
			w.WriteHeader(500)
			return
		}
		fmt.Fprint(w, "ok")
	})
	var delays []time.Duration
	c.wait = func(ctx context.Context, d time.Duration) error { delays = append(delays, d); return nil }
	notices := 0
	c.backoff = func(code int, until time.Time) {
		if !until.IsZero() {
			notices++
			if code != 500 || !until.After(time.Now()) {
				t.Error("missing retry deadline")
			}
		}
	}
	res, err := c.get(context.Background(), "/file/csv/id", "secret")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if calls != 4 || notices != 3 || fmt.Sprint(delays) != "[30s 1m0s 2m0s]" {
		t.Fatalf("calls=%d notices=%d delays=%v", calls, notices, delays)
	}
}
func TestWiGLERetryLimitAndTerminalErrors(t *testing.T) {
	for _, code := range []int{500, 401, 403, 429, 404} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			calls := 0
			c := testWiGLEClient(t, func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(code); fmt.Fprint(w, "secret") })
			c.wait = func(context.Context, time.Duration) error { return nil }
			_, err := c.get(context.Background(), "/test", "secret")
			want := 1
			if code == 500 {
				want = 6
			}
			if err == nil || strings.Contains(err.Error(), "secret") || calls != want {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
		})
	}
}
func TestWiGLECancelBackoff(t *testing.T) {
	c := testWiGLEClient(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.backoff = func(int, time.Time) { cancel() }
	start := time.Now()
	_, err := c.get(ctx, "/test", "secret")
	if err != context.Canceled || time.Since(start) > time.Second {
		t.Fatalf("cancel: %v", err)
	}
}
func TestWiGLEPagination(t *testing.T) {
	c := testWiGLEClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("pagestart") {
		case "0":
			fmt.Fprint(w, `{"success":true,"results":[{"transid":"a"},{"transid":"b"}]}`)
		case "2":
			fmt.Fprint(w, `{"success":true,"results":[{"transid":"b"},{"transid":"c"}]}`)
		case "4":
			fmt.Fprint(w, `{"success":true,"results":[{"transid":"c"}]}`)
		default:
			t.Error(r.URL)
			fmt.Fprint(w, `{"results":[]}`)
		}
	})
	found := 0
	uploads, err := c.list(context.Background(), "secret", func(n int) { found = n })
	if err != nil || len(uploads) != 3 || found != 3 {
		t.Fatalf("uploads=%v found=%d err=%v", uploads, found, err)
	}
}
func TestWiGLEUnwrap(t *testing.T) {
	for _, kind := range []string{"plain", "gzip", "zip"} {
		t.Run(kind, func(t *testing.T) {
			content := []byte("original bytes\r\n")
			var b bytes.Buffer
			name := "original.csv"
			switch kind {
			case "gzip":
				z := gzip.NewWriter(&b)
				z.Write(content)
				z.Close()
				name += ".gz"
			case "zip":
				z := zip.NewWriter(&b)
				f, _ := z.Create("../../original.csv")
				f.Write(content)
				z.Close()
				name = "wrapper.zip"
			default:
				b.Write(content)
			}
			path := filepath.Join(t.TempDir(), "download")
			os.WriteFile(path, b.Bytes(), 0600)
			out, n, err := wigleUnwrap(context.Background(), path, name)
			if err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(out)
			if !bytes.Equal(got, content) || n != "original.csv" || filepath.Dir(out) != filepath.Dir(path) {
				t.Fatalf("%s %s %q", out, n, got)
			}
		})
	}
}
func TestWiGLEImportAndRepeat(t *testing.T) {
	s := adminServer(t)
	csv := "MAC,SSID,FirstSeen,Frequency,RSSI,CurrentLatitude,CurrentLongitude,AccuracyMeters,Type\n02:50:00:00:00:01,Net,2026-11-01 00:00:00,2412,-60,48.4,-123.4,3,WIFI\n"
	c := testWiGLEClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/file/csv/") {
			fmt.Fprint(w, csv)
			return
		}
		if r.URL.Query().Get("pagestart") == "0" {
			fmt.Fprint(w, `{"results":[{"transid":"abc","fileName":"scan.csv"}]}`)
		} else {
			fmt.Fprint(w, `{"results":[]}`)
		}
	})
	for i := 0; i < 2; i++ {
		_, err := s.admin.start("wigle", func(ctx context.Context) (any, error) { return s.adminWiGLE(ctx, "secret", false, c) })
		if err != nil {
			t.Fatal(err)
		}
		s.admin.mu.Lock()
		done := s.admin.done
		s.admin.mu.Unlock()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("timed out")
		}
		state := s.admin.snapshot()
		requireComplete(t, state)
		counts := state["wigle"].(map[string]any)
		if counts["found"] != 1 || counts["captured"] != 1 || counts["remaining"] != 0 {
			t.Fatal(counts)
		}
		encoded, _ := json.Marshal(state)
		if bytes.Contains(encoded, []byte("secret")) {
			t.Fatal("token leaked")
		}
		if s.Atlas.Obs.State().N != 1 {
			t.Fatal("duplicate observations")
		}
	}
}

func TestWiGLEPartialImportCancelledDuringBackoff(t *testing.T) {
	s := adminServer(t)
	csv := "MAC,SSID,FirstSeen,Frequency,RSSI,CurrentLatitude,CurrentLongitude,AccuracyMeters,Type\n02:50:00:00:00:01,Net,2026-11-01 00:00:00,2412,-60,48.4,-123.4,3,WIFI\n"
	c := testWiGLEClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/file/csv/a":
			fmt.Fprint(w, csv)
		case "/file/csv/b":
			w.WriteHeader(500)
		default:
			if r.URL.Query().Get("pagestart") == "0" {
				fmt.Fprint(w, `{"results":[{"transid":"a"},{"transid":"b"}]}`)
			} else {
				fmt.Fprint(w, `{"results":[]}`)
			}
		}
	})
	c.wait = func(ctx context.Context, d time.Duration) error {
		if d == 0 {
			return ctx.Err()
		}
		state := s.admin.snapshot()
		if state["retry_at"] == nil || state["retry_reason"] == nil {
			t.Error("backoff not published")
		}
		s.admin.requestCancel(state["id"].(string))
		return wigleWait(ctx, d)
	}
	_, err := s.admin.start("wigle", func(ctx context.Context) (any, error) { return s.adminWiGLE(ctx, "secret", false, c) })
	if err != nil {
		t.Fatal(err)
	}
	s.admin.mu.Lock()
	done := s.admin.done
	s.admin.mu.Unlock()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out")
	}
	state := s.admin.snapshot()
	counts := state["wigle"].(map[string]any)
	if state["status"] != "cancelled" || counts["captured"] != 1 || counts["remaining"] != 1 || state["retry_at"] != nil || s.Atlas.Obs.State().N != 1 {
		t.Fatal(state)
	}
	files := state["result"].(map[string]any)["files"].([]map[string]any)
	if len(files) != 1 {
		t.Fatal(files)
	}
}
func TestWiGLERetryAfter(t *testing.T) {
	c := testWiGLEClient(t, func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Retry-After", "120"); w.WriteHeader(503) })
	c.wait = func(ctx context.Context, d time.Duration) error {
		if d != 2*time.Minute {
			t.Errorf("delay %s", d)
		}
		return context.Canceled
	}
	_, err := c.get(context.Background(), "/test", "secret")
	if err != context.Canceled {
		t.Fatal(err)
	}
}
