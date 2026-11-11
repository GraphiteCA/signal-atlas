package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (s *Server) adminExportCSV(ctx context.Context, path, format string) (any, error) {
	a, j, err := s.freezeSurvey(ctx, false)
	if err != nil {
		return nil, err
	}
	defer j.Resume()
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	path = filepath.Join(parent, filepath.Base(path))
	root, err := filepath.EvalSymlinks(a.Root)
	if err != nil {
		return nil, err
	}
	rel, relErr := filepath.Rel(root, path)
	if relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return nil, fmt.Errorf("Choose an export destination outside the active workspace")
	}
	days := format == "days"
	if !days && !strings.EqualFold(filepath.Ext(path), ".csv") {
		return nil, fmt.Errorf("Choose a .csv destination file")
	}
	if days {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			return nil, fmt.Errorf("Choose a new folder for daily CSV files")
		}
	}
	staging, err := os.MkdirTemp(parent, ".survey-csv-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(staging)
	target := staging
	if !days {
		target = filepath.Join(staging, "survey.csv")
	}
	rows, files, err := a.ExportSurveyCSV(ctx, target, s.o.Version, days, func(done, total int64) { s.admin.progress("Exporting survey observations", done, total, true) })
	if err != nil {
		return nil, err
	}
	if err = s.admin.beginCommit(ctx, "Publishing survey CSV export"); err != nil {
		return nil, err
	}
	// A daily export is published as one new directory, so cancellation or a
	// failed write cannot leave a mixture of old and new day files.
	if days {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			return nil, fmt.Errorf("Export destination already exists")
		}
	}
	if err = os.Rename(target, path); err != nil {
		return nil, err
	}
	return map[string]any{"saved": true, "path": path, "format": format, "observations": rows, "day_files": files}, nil
}
