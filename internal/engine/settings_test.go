package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{{"0.2.0", "0.1.0", true}, {"0.10.0", "0.9.9", true}, {"v1.0.0", "0.9.0", true}, {"0.2.0", "0.2.0", false}, {"0.1.9", "0.2.0", false}, {"1.0.0-beta.1", "0.9.0", true}} {
		if got := newer(c.a, c.b); got != c.want {
			t.Errorf("newer(%s, %s) = %v", c.a, c.b, got)
		}
	}
}

// The update check reads the newest release and its installer; without a window (no installer
// can run) it only points at the release page.
func TestCheckUpdate(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "SignalAtlas/0.2.0" {
			t.Errorf("user agent %q", r.Header.Get("User-Agent"))
		}
		w.Write([]byte(`{"tag_name":"v0.3.0","html_url":"https://example.invalid/r","assets":[
			{"name":"signal-atlas-0.3.0-windows-amd64-setup.exe","browser_download_url":"https://example.invalid/s.exe","size":9},
			{"name":"SHA256SUMS.txt","browser_download_url":"https://example.invalid/sums","size":1}]}`))
	}))
	defer gh.Close()
	releasesURL = gh.URL
	defer func() { releasesURL = "" }()
	dir := t.TempDir()
	s := &Server{o: Options{Version: "0.2.0", Data: dir}, set: newSettings(dir)}
	if s.set.get().UpdateChecks != "ask" {
		t.Fatalf("a new install should ask first, not %q", s.set.get().UpdateChecks)
	}
	u, err := s.checkUpdate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if u["latest"] != "0.3.0" || u["newer"] != true || u["installer"] != false || u["installer_name"] != "signal-atlas-0.3.0-windows-amd64-setup.exe" {
		t.Fatalf("update %v", u)
	}
	if err := s.installUpdate(context.Background()); err == nil {
		t.Fatal("installed without a window to hand over to")
	}
	s.set.set(func(st *Settings) { st.UpdateChecks = "off"; st.FastFits = true })
	if again := newSettings(dir).get(); again.UpdateChecks != "off" || !again.FastFits {
		t.Fatalf("settings didn't persist: %+v", again)
	}
}
