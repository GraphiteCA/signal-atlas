package engine

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"signal-atlas/internal/atlas"
	"signal-atlas/internal/obs"
	"signal-atlas/internal/py"
)

type mapPointsCache struct {
	mu     sync.Mutex
	state  *obs.State
	key    string
	data   []byte
	points *atlas.Points
}

func paramsKey(p py.Params) string { return url.Values(p).Encode() }

func (s *Server) mapDisplayPost(w http.ResponseWriter, r *http.Request) error {
	d := atlas.DefaultMapDisplay()
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&d); err != nil {
		return err
	}
	c, put, err := s.Atlas.Conn(r.Context())
	if err != nil {
		return err
	}
	defer put()
	if err := s.Atlas.SaveMapDisplay(c, d); err != nil {
		return err
	}
	sendGoJSON(w, d, 200)
	return nil
}

// ---- the GPU map's data (ui/map.html): bounds, packed points, saved fits ----

// mapGet answers /api/app/map/<what>; ok is false for other paths.
func (s *Server) mapGet(w http.ResponseWriter, r *http.Request, p string, params py.Params) (bool, error) {
	switch p {
	case "/api/app/map/display", "/api/app/map/summary", "/api/app/map/features", "/api/app/map/detail":
		c, put, err := s.Atlas.Conn(r.Context())
		if err != nil {
			return true, err
		}
		defer put()
		var v any
		switch p {
		case "/api/app/map/display":
			v, err = s.Atlas.MapDisplay(c)
		case "/api/app/map/summary":
			var summary atlas.MapSummary
			summary, err = s.Atlas.MapSummary(c)
			summary.Revision = s.surveyID + ":" + summary.Revision + ":" + params.First("solver", "reference")
			summary.ObservationRevision = s.surveyID + ":" + summary.ObservationRevision
			summary.Solver = params.First("solver", "reference")
			v = summary
		case "/api/app/map/features":
			v, err = s.Atlas.MapFeatures(r.Context(), c, params.With("_survey", []string{s.surveyID}))
		case "/api/app/map/detail":
			v, err = s.Atlas.MapDetail(r.Context(), c, params)
		}
		if err != nil {
			if strings.Contains(err.Error(), "Map changed;") {
				err = herr(409, "%s", err)
			}
			return true, err
		}
		sendGoJSON(w, v, 200)
		return true, nil
	case "/api/app/map/bounds":
		b, ok := s.Atlas.Bounds()
		if !ok {
			sendJSON(w, map[string]any{"empty": true}, 200)
		} else {
			sendJSON(w, map[string]any{"west": b[0], "south": b[1], "east": b[2], "north": b[3]}, 200)
		}
		return true, nil
	case "/api/app/map/points":
		max := 2_000_000
		if v, err := strconv.Atoi(params.First("max", "")); err == nil && v > 0 {
			max = min(v, 8_000_000)
		}
		// Repeated requests reuse packed buffers, not just decoded points. Retain
		// one view up to 64 MiB; a larger request is served without caching it.
		cache := &s.mapPoints
		cache.mu.Lock()
		defer cache.mu.Unlock()
		key := paramsKey(params) + ":" + strconv.Itoa(max)
		state := s.Atlas.Obs.State()
		pts, buf := cache.points, cache.data
		var err error
		if cache.state != state || cache.key != key || pts == nil {
			pts, err = s.Atlas.PointsInContext(r.Context(), params, max)
			buf = nil
		}
		if err != nil {
			return true, err
		}
		// little-endian float32: n (east, north) offset pairs, then n signals
		h := w.Header()
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Cache-Control", "no-store")
		h.Set("X-Origin-Lon", strconv.FormatFloat(pts.Lon0, 'f', -1, 64))
		h.Set("X-Origin-Lat", strconv.FormatFloat(pts.Lat0, 'f', -1, 64))
		h.Set("X-Total", strconv.Itoa(pts.Total))
		h.Set("X-Shown", strconv.Itoa(pts.Shown))
		if buf == nil {
			buf = make([]byte, 4*(len(pts.XY)+len(pts.RSSI)))
			o := 0
			for _, v := range pts.XY {
				binary.LittleEndian.PutUint32(buf[o:], math.Float32bits(v))
				o += 4
			}
			for _, v := range pts.RSSI {
				binary.LittleEndian.PutUint32(buf[o:], math.Float32bits(v))
				o += 4
			}
			if len(buf) <= 64<<20 && s.Atlas.Obs.State() == state {
				cache.state = state
				cache.key = key
				cache.data = buf
				cache.points = &atlas.Points{Lon0: pts.Lon0, Lat0: pts.Lat0, Total: pts.Total, Shown: pts.Shown}
			} else {
				cache.state = nil
				cache.points = nil
				cache.data = nil
			}
		}
		h.Set("Content-Length", strconv.Itoa(len(buf)))
		w.WriteHeader(200)
		w.Write(buf)
		return true, nil
	case "/api/app/map/fits":
		c, put, err := s.Atlas.Conn(r.Context())
		if err != nil {
			return true, err
		}
		fits, err := s.Atlas.FitsIn(c, params)
		put()
		if err != nil {
			return true, err
		}
		// columns, so a few hundred thousand fits stay compact
		n := len(fits)
		lat, lon, used, rms := make([]float64, n), make([]float64, n), make([]int64, n), make([]float64, n)
		ssid, mac, ids, bands := make([]string, n), make([]string, n), make([]string, n), make([]string, n)
		shared, edge := make([]bool, n), make([]bool, n)
		for i, f := range fits {
			ids[i], bands[i] = f.ID, f.Band
			lat[i], lon[i], ssid[i], mac[i], shared[i], used[i], rms[i], edge[i] = f.Lat, f.Lon, f.SSID, f.MAC, f.Shared, f.Used, f.RMS, f.BoundaryLimited
		}
		sendGoJSON(w, map[string]any{"count": n, "id": ids, "band": bands, "lat": lat, "lon": lon, "ssid": ssid, "mac": mac, "shared": shared, "used": used, "rms": rms, "edge": edge}, 200)
		return true, nil
	}
	return false, nil
}
