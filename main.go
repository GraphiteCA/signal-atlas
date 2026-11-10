// Signal Atlas: a desktop app (Windows, macOS, Linux) for wardriving data too big for the browser. It holds two
// tools, Wardrive Compare (compare WiGLE CSV drives on the road they share) and Graphite's Signal
// Atlas explorer (fit access-point locations from whole surveys), both run by a Go engine.
//
// The screens are web pages shown in a native window (Wails: WebView2 on Windows, WKWebView on
// macOS, WebKitGTK on Linux); no browser is opened. They talk to the engine (internal/engine) over the window's own
// origin. The engine holds Signal Atlas (internal/atlas, a port of Graphite's Python, checked result
// for result against it) and Wardrive Compare's analysis (internal/compare, a port of the page's own
// worker, checked against it).
//
//	signal-atlas                        the window
//	signal-atlas -serve 127.0.0.1:8790  the same engine for a browser (testing; no file dialogs)
package main

import (
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"signal-atlas/internal/engine"
)

//go:embed ui
var uiFiles embed.FS

//go:embed web
var webFiles embed.FS

// Graphite's explorer pages, used as he delivered them (python/templates/).
//
//go:embed python/templates
var explorerFiles embed.FS

// Version is stamped at build time (-ldflags "-X main.Version=1.0.0").
var Version = "dev"

func defaultData() string {
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "Signal Atlas", "data")
	}
	return "signal-atlas-data"
}

func main() {
	serve := flag.String("serve", "", "serve the engine to a browser at this address instead of opening the window, e.g. 127.0.0.1:8790")
	data := flag.String("data", defaultData(), "folder for the Signal Atlas store, exports and the map tile cache")
	flag.Parse()
	ui, _ := fs.Sub(uiFiles, "ui")
	web, _ := fs.Sub(webFiles, "web")
	explorer, _ := fs.Sub(explorerFiles, "python/templates")
	opts := engine.Options{UI: ui, Compare: web, Explorer: explorer, Data: *data, Version: Version}
	if *serve != "" {
		s, err := engine.New(opts)
		if err != nil {
			fail(err)
		}
		ln, err := net.Listen("tcp", *serve)
		if err != nil {
			fail(err)
		}
		addr := ln.Addr().String()
		s.AllowHost(addr)
		if host, port, _ := net.SplitHostPort(addr); host == "127.0.0.1" {
			s.AllowHost("localhost:" + port)
		}
		fmt.Printf("Signal Atlas engine %s at http://%s/  (data: %s)\n", Version, addr, *data)
		fail(http.Serve(ln, s))
	}
	if err := runWindow(opts); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "Signal Atlas:", err)
	os.Exit(1)
}
