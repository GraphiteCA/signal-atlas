package engine

import (
	"archive/zip"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Credentials live only in the running operation, never in status or settings.
type wigleClient struct {
	base    string
	http    *http.Client
	delay   time.Duration
	wait    func(context.Context, time.Duration) error
	backoff func(int, time.Time)
}

func newWiGLEClient() *wigleClient {
	return &wigleClient{base: "https://api.wigle.net/api/v2", http: &http.Client{Timeout: 180 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, delay: 2 * time.Second, wait: wigleWait}
}
func wigleWait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func (c *wigleClient) get(ctx context.Context, path, token string) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, "GET", c.base+path, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Basic "+token)
		req.Header.Set("User-Agent", "Signal-Atlas/WiGLE-import")
		res, err := c.http.Do(req)
		if err == nil && res.StatusCode == http.StatusOK {
			return res, nil
		}
		code := 0
		retryAfter := ""
		if res != nil {
			code = res.StatusCode
			retryAfter = res.Header.Get("Retry-After")
			res.Body.Close()
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if code == 401 || code == 403 {
			return nil, fmt.Errorf("WiGLE rejected the token (HTTP %d). Use the Encoded for use value from your WiGLE account", code)
		}
		if code == 429 {
			return nil, fmt.Errorf("WiGLE daily API limit reached (HTTP 429). Try again later; completed imports are retained")
		}
		if code != 0 && (code < 500 || code > 599) {
			return nil, fmt.Errorf("WiGLE request failed (HTTP %d)", code)
		}
		if attempt >= 5 {
			return nil, fmt.Errorf("WiGLE is still unavailable after 5 retries (HTTP %d). Completed imports are retained; try again later", code)
		}
		delay := 30 * time.Second * time.Duration(1<<attempt)
		if delay > 5*time.Minute {
			delay = 5 * time.Minute
		}
		if seconds, e := strconv.Atoi(retryAfter); e == nil && seconds > 0 && seconds <= 86400 {
			delay = max(delay, time.Duration(seconds)*time.Second)
		} else if date, e := http.ParseTime(retryAfter); e == nil {
			delay = max(delay, time.Until(date))
		}
		if c.backoff != nil {
			c.backoff(code, time.Now().Add(delay))
		}
		if err := c.wait(ctx, delay); err != nil {
			return nil, err
		}
		if c.backoff != nil {
			c.backoff(0, time.Time{})
		}
	}
}

type wigleUpload struct {
	ID   string `json:"transid"`
	Name string `json:"fileName"`
}

func (c *wigleClient) list(ctx context.Context, token string, progress func(int)) ([]wigleUpload, error) {
	uploads := []wigleUpload{}
	seen := map[string]bool{}
	for start := 0; ; {
		res, err := c.get(ctx, fmt.Sprintf("/file/transactions?pagestart=%d&pageend=%d", start, start+100), token)
		if err != nil {
			return uploads, err
		}
		var page struct {
			Success *bool         `json:"success"`
			Results []wigleUpload `json:"results"`
		}
		err = json.NewDecoder(io.LimitReader(res.Body, 16<<20)).Decode(&page)
		res.Body.Close()
		if err != nil {
			return uploads, fmt.Errorf("Invalid WiGLE upload listing: %w", err)
		}
		if page.Success != nil && !*page.Success {
			return uploads, fmt.Errorf("WiGLE could not list uploads")
		}
		added := 0
		for _, u := range page.Results {
			if u.ID == "" {
				return uploads, fmt.Errorf("WiGLE returned an upload without a transaction ID")
			}
			if !seen[u.ID] {
				seen[u.ID] = true
				uploads = append(uploads, u)
				added++
			}
		}
		progress(len(uploads))
		if added == 0 {
			return uploads, nil
		}
		start += len(page.Results)
		if err := c.wait(ctx, c.delay); err != nil {
			return uploads, err
		}
	}
}

const wigleFileLimit = 2_000_000_000

// Unwrap into a controlled path; archive member paths are never used on disk.
type wigleContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r wigleContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func wigleUnwrap(ctx context.Context, path, name string) (string, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	magic := make([]byte, 4)
	n, _ := f.Read(magic)
	f.Seek(0, io.SeekStart)
	var reader io.Reader = f
	var closer io.Closer
	if n >= 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, e := gzip.NewReader(f)
		if e != nil {
			return "", "", e
		}
		reader, closer = gz, gz
		if strings.HasSuffix(strings.ToLower(name), ".gz") {
			name = name[:len(name)-3]
		}
	} else if n == 4 && string(magic) == "PK\x03\x04" {
		stat, e := f.Stat()
		if e != nil {
			return "", "", e
		}
		z, e := zip.NewReader(f, stat.Size())
		if e != nil {
			return "", "", e
		}
		var member *zip.File
		for _, entry := range z.File {
			if !entry.FileInfo().IsDir() {
				if member != nil {
					return "", "", fmt.Errorf("WiGLE ZIP contains multiple files; import them manually")
				}
				member = entry
			}
		}
		if member == nil {
			return "", "", fmt.Errorf("WiGLE ZIP is empty")
		}
		r, e := member.Open()
		if e != nil {
			return "", "", e
		}
		reader, closer = r, r
		name = filepath.Base(strings.ReplaceAll(member.Name, "\\", "/"))
	}
	if closer == nil {
		return path, name, nil
	}
	defer closer.Close()
	target := path + ".unwrapped"
	out, err := os.Create(target)
	if err != nil {
		return "", "", err
	}
	size, err := io.Copy(out, io.LimitReader(wigleContextReader{ctx, reader}, wigleFileLimit+1))
	closeErr := out.Close()
	if err == nil {
		err = closeErr
	}
	if size > wigleFileLimit {
		err = fmt.Errorf("WiGLE file exceeds 2 GB after decompression")
	}
	return target, name, err
}
func (s *Server) adminWiGLE(ctx context.Context, token string, skip bool, client *wigleClient) (any, error) {
	// Check availability before spending API calls. The administration gate blocks new jobs.
	_, jobs, err := s.freezeSurvey(ctx, false)
	if err != nil {
		return nil, err
	}
	defer jobs.Resume()
	found, captured, failed := 0, 0, 0
	publish := func() {
		s.admin.mu.Lock()
		s.admin.state["wigle"] = map[string]any{"found": found, "captured": captured, "remaining": found - captured, "failed": failed}
		s.admin.mu.Unlock()
	}
	publish()
	client.backoff = func(code int, until time.Time) {
		s.admin.mu.Lock()
		if until.IsZero() {
			delete(s.admin.state, "retry_at")
			delete(s.admin.state, "retry_reason")
		} else {
			s.admin.state["retry_at"] = until.UnixMilli()
			reason := fmt.Sprintf("WiGLE HTTP %d — backing off", code)
			if code == 0 {
				reason = "WiGLE connection failed — backing off"
			}
			s.admin.state["retry_reason"] = reason
		}
		s.admin.mu.Unlock()
	}
	defer client.backoff(0, time.Time{})
	s.admin.progress("Finding WiGLE uploads", 0, 0, true)
	uploads, err := client.list(ctx, token, func(n int) { found = n; publish() })
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "signal-atlas-wigle-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	results := []map[string]any{}
	result := func() map[string]any { return map[string]any{"files": results} }
	for index, upload := range uploads {
		if err := ctx.Err(); err != nil {
			return result(), err
		}
		name := sanitizeName(upload.Name)
		if name == "" {
			name = "upload.csv"
		}
		s.admin.progress(fmt.Sprintf("Capturing WiGLE upload %d of %d: %s", index+1, found, name), int64(captured), int64(found), true)
		res, err := client.get(ctx, "/file/csv/"+url.PathEscape(upload.ID), token)
		if err != nil {
			return result(), err
		}
		path := filepath.Join(dir, "download")
		f, err := os.Create(path)
		if err != nil {
			res.Body.Close()
			return result(), err
		}
		size, copyErr := io.Copy(f, io.LimitReader(res.Body, wigleFileLimit+1))
		res.Body.Close()
		closeErr := f.Close()
		if copyErr != nil {
			return result(), fmt.Errorf("WiGLE download interrupted; completed imports are retained")
		}
		if closeErr != nil {
			return result(), closeErr
		}
		if size > wigleFileLimit {
			return result(), fmt.Errorf("WiGLE download exceeds 2 GB")
		}
		source, _, unwrapErr := wigleUnwrap(ctx, path, name)
		var rows []map[string]any
		if unwrapErr == nil {
			// Reuse all supported scan formats and the existing content-based deduplication.
			sourceName := sanitizeName(upload.ID) + "__" + sanitizeName(name)
			s.uploadMu.Lock()
			if s.uploads == nil {
				s.uploads = map[string]string{}
			}
			s.uploads[source] = sourceName
			s.uploadMu.Unlock()
			imported, importErr := s.adminImportsWithSources(ctx, []string{source}, skip, map[string]string{source: path})
			if imported != nil {
				rows, _ = imported.(map[string]any)["files"].([]map[string]any)
			}
			if importErr != nil {
				results = append(results, rows...)
				return result(), importErr
			}
		} else {
			rows = []map[string]any{{"filename": name, "error": unwrapErr.Error()}}
		}
		results = append(results, rows...)
		if len(rows) == 1 && rows[0]["error"] == nil {
			captured++
		} else {
			failed++
		}
		publish()
		os.Remove(path)
		os.Remove(path + ".unwrapped")
		if index+1 < len(uploads) {
			if err := client.wait(ctx, client.delay); err != nil {
				return result(), err
			}
		}
	}
	s.admin.progress("WiGLE import finished", int64(captured), int64(found), false)
	if failed > 0 {
		return result(), fmt.Errorf("%d WiGLE uploads could not be imported; see file outcomes. Completed imports are retained", failed)
	}
	return result(), nil
}
