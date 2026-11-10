package engine

import (
	"encoding/binary"
	"math"
	"net/http"
	"strconv"

	"signal-atlas/internal/py"
)

// ---- the GPU map's data (ui/map.html): bounds, packed points, saved fits ----

// mapGet answers /api/app/map/<what>; ok is false for other paths.
func (s *Server) mapGet(w http.ResponseWriter, r *http.Request, p string, params py.Params) (bool, error) {
	switch p {
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
		pts, err := s.Atlas.PointsIn(params, max)
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
		buf := make([]byte, 4*(len(pts.XY)+len(pts.RSSI)))
		o := 0
		for _, v := range pts.XY {
			binary.LittleEndian.PutUint32(buf[o:], math.Float32bits(v))
			o += 4
		}
		for _, v := range pts.RSSI {
			binary.LittleEndian.PutUint32(buf[o:], math.Float32bits(v))
			o += 4
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
		ssid, mac := make([]string, n), make([]string, n)
		shared, edge := make([]bool, n), make([]bool, n)
		for i, f := range fits {
			lat[i], lon[i], ssid[i], mac[i], shared[i], used[i], rms[i], edge[i] = f.Lat, f.Lon, f.SSID, f.MAC, f.Shared, f.Used, f.RMS, f.BoundaryLimited
		}
		sendJSON(w, map[string]any{"count": n, "lat": lat, "lon": lon, "ssid": ssid, "mac": mac, "shared": shared, "used": used, "rms": rms, "edge": edge}, 200)
		return true, nil
	}
	return false, nil
}
