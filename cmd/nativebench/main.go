//go:build windows

// nativebench hosts the real map pages in Wails/WebView2 and records their
// Performance API diagnostics. It only accepts an owned benchmark data folder.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"signal-atlas/internal/atlas"
	"signal-atlas/internal/db"
	"signal-atlas/internal/engine"
	"signal-atlas/internal/py"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
	wr "github.com/wailsapp/wails/v2/pkg/runtime"
)

func must(e error) {
	if e != nil {
		panic(e)
	}
}
func owned(path string) bool {
	for range 4 {
		b, err := os.ReadFile(filepath.Join(path, "owner.json"))
		if err == nil {
			var marker struct {
				Purpose string `json:"purpose"`
				Schema  int    `json:"schema"`
			}
			if json.Unmarshal(b, &marker) == nil && marker.Purpose == "signal-atlas-benchmark" && marker.Schema == 1 {
				return true
			}
		}
		path = filepath.Dir(path)
	}
	return false
}
func seed(a *atlas.Atlas, n int) {
	gs, err := a.SelectedGroups(py.Params{})
	must(err)
	gs = atlas.EstimationGroups(gs, py.Params{"shared": {"1"}, "individual": {"0"}})
	if len(gs) > n {
		gs = gs[:n]
	}
	_, key, err := atlas.EstimateScope(py.Params{"fit_mode": {"sample"}})
	must(err)
	c, put, err := a.Conn(nil)
	must(err)
	defer put()
	must(db.Tx(c, func() error {
		for i, g := range gs {
			fit := map[string]any{}
			for k, v := range g {
				fit[k] = v
			}
			fit["status"] = "fitted"
			fit["latitude"] = 48.400 + float64(i/250)*.00001
			fit["longitude"] = -123.400 + float64(i%250)*.00001
			fit["used_observations"] = 10
			fit["residual_rms_db"] = 3.
			fit["boundary_limited"] = false
			fit["benchmark_synthetic_location"] = true
			text, err := json.Marshal(fit)
			if err != nil {
				return err
			}
			if _, err = db.Exec(c, "INSERT OR REPLACE INTO estimates(radio_key,scope_key,result) VALUES(?,?,?)", g["radio_key"], key, string(text)); err != nil {
				return err
			}
		}
		return nil
	}))
	fmt.Fprintln(os.Stderr, "Seeded", len(gs), "synthetic render-only AP locations; not calculated fits")
}
func main() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	data := flag.String("data", "", "owned benchmark data (contains store/)")
	repo := flag.String("repo", ".", "upstream checkout")
	donor := flag.String("donor", "..", "donor source copy")
	page := flag.String("page", "gpu", "gpu, canvas, admin, explorer or donor")
	output := flag.String("output", "", "new JSON output path")
	count := flag.Int("seed-fits", 0, "seed synthetic saved locations for rendering only")
	driver := flag.String("driver", "native.js", "diagnostic script name inside benchmarks/")
	flag.Parse()
	realData, err := filepath.EvalSymlinks(*data)
	must(err)
	*data = realData
	if !owned(*data) || *output == "" {
		panic("owned benchmark data and new output required")
	}
	if _, err := os.Stat(*output); !os.IsNotExist(err) {
		panic("output already exists")
	}
	s, err := engine.New(engine.Options{Data: *data, Version: "dev", UI: os.DirFS(filepath.Join(*repo, "ui")), Explorer: os.DirFS(filepath.Join(*repo, "python", "templates")), Compare: os.DirFS(filepath.Join(*repo, "web")), Hosts: []string{"wails.localhost", "wails.localhost:80", "wails"}, Origins: []string{"wails://wails"}})
	must(err)
	defer s.Close()
	if *count > 0 {
		seed(s.Atlas, *count)
	}
	if filepath.Base(*driver) != *driver {
		panic("driver must be a file in benchmarks/")
	}
	script, err := os.ReadFile(filepath.Join(*repo, "benchmarks", *driver))
	must(err)
	var ctx context.Context
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/__bench/report" && r.Method == "POST" {
			b, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
			must(err)
			must(os.WriteFile(*output, b, 0644))
			w.Write([]byte("ok"))
			go func() { time.Sleep(500 * time.Millisecond); wr.Quit(ctx) }()
			return
		}
		if strings.HasPrefix(r.URL.Path, "/__bench/donor/") {
			name := strings.TrimPrefix(r.URL.Path, "/__bench/donor/")
			if name != "leaflet.js" && name != "leaflet.css" && name != "nearby_canvas.js" {
				http.NotFound(w, r)
				return
			}
			http.ServeFile(w, r, filepath.Join(*donor, "templates", name))
			return
		}
		if r.URL.Path == "/__bench/native.js" {
			w.Header().Set("Content-Type", "text/javascript")
			w.Write(script)
			return
		}
		if r.URL.Path == "/" {
			if *page == "donor" {
				w.Header().Set("Content-Type", "text/html")
				fmt.Fprint(w, donorHTML)
				return
			}
			path := "/map"
			if *page == "canvas" {
				path = "/map?renderer=canvas"
			}
			if *page == "explorer" {
				path = "/atlas/"
			}
			if *page == "admin" {
				path = "/admin"
			}
			http.Redirect(w, r, path, http.StatusFound)
			return
		}
		if r.URL.Path == "/map" || r.URL.Path == "/atlas/" || r.URL.Path == "/admin" {
			out := httptest.NewRecorder()
			s.ServeHTTP(out, r)
			body := bytes.Replace(out.Body.Bytes(), []byte("</body>"), []byte(`<script src="/__bench/native.js"></script></body>`), 1)
			for k, vs := range out.Header() {
				if strings.EqualFold(k, "Content-Length") {
					continue
				}
				for _, v := range vs {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(out.Code)
			w.Write(body)
			return
		}
		s.ServeHTTP(w, r)
	})
	must(wails.Run(&options.App{Title: "Signal Atlas baseline benchmark", StartHidden: *page == "admin", Width: 1400, Height: 950, AssetServer: &assetserver.Options{Handler: h}, OnStartup: func(c context.Context) { ctx = c }, Windows: &windows.Options{Theme: windows.Dark, WebviewUserDataPath: filepath.Join(*data, "webview-benchmark-"+*page)}}))
}

const donorHTML = `<!doctype html><html><head><title>Donor canvas baseline</title><link rel="stylesheet" href="/__bench/donor/leaflet.css"><style>html,body,#map{height:100%;margin:0;background:#13212a}#bench-status{position:absolute;top:10px;left:60px;z-index:1000;color:white;background:#13212a;padding:8px}</style></head><body><div id="map"></div><div id="bench-status">Benchmarking donor canvas</div><script src="/__bench/donor/leaflet.js"></script><script src="/__bench/donor/nearby_canvas.js"></script><script>let map=L.map('map',{minZoom:2,maxZoom:22}).setView([48.401,-123.399],15);window.__donor=true;</script><script src="/__bench/native.js"></script></body></html>`
