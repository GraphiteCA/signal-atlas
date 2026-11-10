package engine

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// OpenStreetMap's tile servers refuse requests from an embedded window whose origin is not a
// website ("App is not following the tile usage policy"). Their policy asks an application to
// identify itself in the User-Agent and to cache, so the desktop app fetches tiles here, names
// itself, keeps each tile on disk for a week and fetches at most four at a time. The pages' Leaflet
// maps are pointed at /tiles/osm/{z}/{x}/{y}.png by ui/bridge.js.

// ProjectURL identifies the app to OpenStreetMap's tile servers.
const ProjectURL = "https://github.com/olosnoks/signal-atlas"

var tilePath = regexp.MustCompile(`^/tiles/osm/(\d{1,2})/(\d{1,8})/(\d{1,8})\.png$`)

var tileSlots = make(chan struct{}, 4)

var tileClient = &http.Client{Timeout: 30 * time.Second}

const tileMaxAge = 7 * 24 * time.Hour

func (s *Server) tile(w http.ResponseWriter, r *http.Request, m []string) error {
	z, x, y := m[1], m[2], m[3]
	file := filepath.Join(s.o.Data, "tiles", "osm", z, x, y+".png")
	if st, err := os.Stat(file); err == nil && time.Since(st.ModTime()) < tileMaxAge {
		return serveTile(w, file)
	}
	tileSlots <- struct{}{}
	defer func() { <-tileSlots }()
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, fmt.Sprintf("https://tile.openstreetmap.org/%s/%s/%s.png", z, x, y), nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "SignalAtlas/"+s.o.Version+" (+"+ProjectURL+"; wardriving map viewer)")
	resp, err := tileClient.Do(req)
	if err != nil {
		if _, e2 := os.Stat(file); e2 == nil { // offline: an old tile beats none
			return serveTile(w, file)
		}
		return herr(502, "map tile unavailable: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		if _, e2 := os.Stat(file); e2 == nil {
			return serveTile(w, file)
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	os.MkdirAll(filepath.Dir(file), 0o755)
	tmp := file + ".part"
	if os.WriteFile(tmp, data, 0o644) == nil {
		os.Rename(tmp, file)
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "max-age=86400")
	w.Write(data)
	return nil
}

func serveTile(w http.ResponseWriter, file string) error {
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "max-age=86400")
	w.Write(data)
	return nil
}
