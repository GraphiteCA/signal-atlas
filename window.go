package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"signal-atlas/internal/engine"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/linux"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
	wr "github.com/wailsapp/wails/v2/pkg/runtime"
)

// host gives the engine the window's native dialogs.
type host struct{ ctx context.Context }

func (h *host) PickFiles(title string, exts []string, multiple bool) ([]string, error) {
	var filters []wr.FileFilter
	if len(exts) > 0 {
		pat := make([]string, len(exts))
		for i, e := range exts {
			pat[i] = "*." + e
		}
		filters = append(filters, wr.FileFilter{DisplayName: strings.ToUpper(strings.Join(exts, ", ")) + " files", Pattern: strings.Join(pat, ";")})
	}
	filters = append(filters, wr.FileFilter{DisplayName: "All files", Pattern: "*.*"})
	o := wr.OpenDialogOptions{Title: title, Filters: filters}
	if multiple {
		return wr.OpenMultipleFilesDialog(h.ctx, o)
	}
	p, err := wr.OpenFileDialog(h.ctx, o)
	if p == "" {
		return nil, err
	}
	return []string{p}, err
}

func (h *host) SavePath(title, name, ext string) (string, bool, error) {
	p, err := wr.SaveFileDialog(h.ctx, wr.SaveDialogOptions{Title: title, DefaultFilename: name, Filters: []wr.FileFilter{{DisplayName: strings.ToUpper(ext), Pattern: "*." + ext}}})
	return p, p != "", err
}

func (h *host) ShowFolder(dir string) {
	switch runtime.GOOS {
	case "windows":
		exec.Command("explorer.exe", dir).Start()
	case "darwin":
		exec.Command("open", dir).Start()
	default:
		exec.Command("xdg-open", dir).Start()
	}
}

func (h *host) OpenURL(url string) { wr.BrowserOpenURL(h.ctx, url) }

func (h *host) PickFolder(title string) (string, error) {
	return wr.OpenDirectoryDialog(h.ctx, wr.OpenDialogOptions{Title: title, CanCreateDirectories: true})
}

// logTo sends the program's messages, and a crash report if it ever crashes, to signal-atlas.log
// in the data folder: a windowed program has no console to show them.
func logTo(dir string) {
	os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, "signal-atlas.log")
	if st, err := os.Stat(path); err == nil && st.Size() > 4<<20 {
		os.Rename(path, path+".old")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	fmt.Fprintf(f, "\n==== Signal Atlas %s started %s\n", Version, time.Now().Format(time.RFC3339))
	os.Stderr = f
	log.SetOutput(f)
	debug.SetCrashOutput(f, debug.CrashOptions{})
}

func runWindow(opts engine.Options) error {
	logTo(opts.Data)
	h := &host{}
	opts.Host = h
	// the pages' origin: http://wails.localhost on Windows (WebView2), wails://wails on macOS
	// (WKWebView) and Linux (WebKitGTK)
	opts.Hosts = []string{"wails.localhost", "wails.localhost:80", "wails"}
	opts.Origins = []string{"wails://wails"}
	s, err := engine.New(opts)
	if err != nil {
		return err
	}
	defer s.Close()
	s.OpenURL = h.OpenURL
	return wails.Run(&options.App{
		Title:            "Signal Atlas",
		Width:            1400,
		Height:           950,
		MinWidth:         800,
		MinHeight:        560,
		BackgroundColour: &options.RGBA{R: 17, G: 22, B: 20, A: 255},
		AssetServer:      &assetserver.Options{Handler: s},
		OnStartup: func(ctx context.Context) {
			h.ctx = ctx
			wr.OnFileDrop(ctx, func(x, y int, paths []string) { s.RecordDrop(paths) })
		},
		DragAndDrop: &options.DragAndDrop{EnableFileDrop: true, DisableWebViewDrop: false},
		// always dark, whatever the system setting: the title bar and border match the pages
		// (on Linux the title bar comes from GTK, set dark in main_linux.go)
		Mac: &mac.Options{Appearance: mac.NSAppearanceNameDarkAqua, About: &mac.AboutInfo{Title: "Signal Atlas " + Version,
			Message: "Wardrive Compare by Champ, Signal Atlas by Graphite"}},
		Linux: &linux.Options{ProgramName: "signal-atlas", WebviewGpuPolicy: linux.WebviewGpuPolicyOnDemand},
		Windows: &windows.Options{Theme: windows.Dark, CustomTheme: &windows.ThemeSettings{
			DarkModeTitleBar: windows.RGB(17, 22, 20), DarkModeTitleBarInactive: windows.RGB(17, 22, 20),
			DarkModeTitleText: windows.RGB(232, 238, 234), DarkModeTitleTextInactive: windows.RGB(162, 174, 168),
			DarkModeBorder: windows.RGB(43, 52, 48), DarkModeBorderInactive: windows.RGB(43, 52, 48),
			LightModeTitleBar: windows.RGB(17, 22, 20), LightModeTitleBarInactive: windows.RGB(17, 22, 20),
			LightModeTitleText: windows.RGB(232, 238, 234), LightModeTitleTextInactive: windows.RGB(162, 174, 168),
			LightModeBorder: windows.RGB(43, 52, 48), LightModeBorderInactive: windows.RGB(43, 52, 48),
		}},
	})
}
