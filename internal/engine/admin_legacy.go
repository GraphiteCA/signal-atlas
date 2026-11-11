package engine

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"signal-atlas/internal/atlas"
	"signal-atlas/internal/drivedb"
	"signal-atlas/internal/py"
)

func generationPath(root string) (string, string, error) {
	parent := filepath.Join(root, "generations")
	if err := os.Mkdir(parent, 0755); err != nil && !os.IsExist(err) {
		return "", "", err
	}
	actual, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", "", err
	}
	base, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", "", err
	}
	r, err := filepath.Rel(base, actual)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(os.PathSeparator)) {
		return "", "", py.Errorf("Generation directory escapes the workspace")
	}
	return parent, filepath.Join(parent, "g-"+newHex()), nil
}

func (s *Server) legacyDatabase(w http.ResponseWriter, r *http.Request, p string) error {
	body, err := readJSON(r, 1<<16)
	if err != nil {
		return err
	}
	path, _ := body["path"].(string)
	if path == "" {
		return py.Errorf("No database file was given")
	}
	kind, err := atlas.DatabaseKind(path)
	if err != nil {
		if k, e := drivedb.Detect(path); e == nil && (k == drivedb.KindTables || k == drivedb.KindDrive) {
			kind = KindLogger
		} else {
			return err
		}
	}
	s.surveyMu.RLock()
	expected := s.surveyID
	have := s.Atlas.Obs.State().N
	s.surveyMu.RUnlock()
	if h := r.Header.Get("X-Survey-ID"); h != "" {
		expected = h
	}
	if err := s.expectedSurvey(expected); err != nil {
		return err
	}
	if p == "/api/app/database-kind" {
		sendGoJSON(w, map[string]any{"kind": kind, "name": filepath.Base(path), "observations": atlas.Count(path, kind), "store_observations": have}, 200)
		return nil
	}
	if kind != atlas.KindAtlas {
		skip, _ := body["skip_invalid"].(bool)
		return s.startAdmin(w, "import", expected, func(ctx context.Context) (any, error) { return s.adminImports(ctx, []string{path}, skip) })
	}
	if err := portableSurvey(path); err != nil {
		return err
	}
	return s.startAdmin(w, "convert", expected, func(ctx context.Context) (any, error) {
		s.surveyMu.RLock()
		root := s.Atlas.Root
		s.surveyMu.RUnlock()
		parent, stage, err := generationPath(root)
		if err != nil {
			return nil, err
		}
		candidate, err := atlas.Open(stage)
		if err != nil {
			removeOwnedCandidate(stage, parent)
			return nil, err
		}
		consumed := false
		defer func() {
			if !consumed {
				candidate.Close()
				removeOwnedCandidate(stage, parent)
			}
		}()
		result, err := candidate.OpenDatabase(path, func(n int64) error { s.admin.progress("Converting saved SQLite", n, 0, true); return ctx.Err() }, nil)
		if err != nil {
			return nil, err
		}
		if err := candidate.Validate(ctx); err != nil {
			return nil, err
		}
		_, j, err := s.freezeSurvey(ctx, true)
		if err != nil {
			return nil, err
		}
		relative, _ := filepath.Rel(root, stage)
		if err := s.installSurvey(ctx, candidate, nil, relative); err != nil {
			j.Resume()
			return nil, err
		}
		consumed = true
		result["reload"] = true
		return result, nil
	})
}
