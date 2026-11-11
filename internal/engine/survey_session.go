package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"signal-atlas/internal/atlas"
)

// Only survey requests hold a lease. Compare long polls, Drive jobs, tiles and
// app settings remain independent of a survey swap.
func surveyRequest(p string) bool {
	if !strings.HasPrefix(p, "/api/") {
		return false
	}
	for _, prefix := range []string{"/api/app/compare/", "/api/app/drives/", "/api/app/admin/", "/api/app/update/"} {
		if strings.HasPrefix(p, prefix) {
			return false
		}
	}
	switch p {
	case "/api/app/sql/cancel", "/api/app/settings", "/api/app/path", "/api/app/pick-files", "/api/app/save-export", "/api/app/save-bytes", "/api/app/open-url", "/api/app/show-data", "/api/app/save-atlas", "/api/app/open-database", "/api/app/database-kind":
		return false
	}
	return true
}
func acquireSurveyLock(root string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(root, ".signal-atlas.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := lockSurveyFile(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("Survey workspace is already open in another process: %s", root)
	}
	return f, nil
}
func (s *Server) rememberSurvey(root string) error {
	return s.set.set(func(st *Settings) {
		out := []string{root}
		for _, p := range st.RecentSurveys {
			if !strings.EqualFold(p, root) && len(out) < 10 {
				out = append(out, p)
			}
		}
		st.LastSurvey = root
		st.RecentSurveys = out
	})
}
func (s *Server) expectedSurvey(expected string) error {
	s.surveyMu.RLock()
	defer s.surveyMu.RUnlock()
	if expected != "" && expected != s.surveyID {
		return herr(409, "The active survey changed; reload this page")
	}
	return nil
}

func (s *Server) handoffLock(ctx context.Context) (func(), error) {
	s.handoff.Store(true)
	s.sql.stop("")
	for !s.surveyMu.TryLock() {
		select {
		case <-ctx.Done():
			s.handoff.Store(false)
			return nil, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
	return func() { s.surveyMu.Unlock(); s.handoff.Store(false) }, nil
}

// Reserve the job runner, join workers and drain requests. The runner is resumed
// if the operation fails; a committed replacement retires it permanently.
func (s *Server) freezeSurvey(ctx context.Context, cancelJobs bool) (*atlas.Atlas, *Jobs, error) {
	// Administration already owns the operation gate, preventing a new prep batch.
	if err := s.preparation.join(ctx); err != nil {
		return nil, nil, err
	}
	unlock, err := s.handoffLock(ctx)
	if err != nil {
		return nil, nil, err
	}
	a, j := s.Atlas, s.Jobs
	if !cancelJobs && j.Running() {
		unlock()
		return nil, nil, herr(409, "Finish or cancel the active survey job first")
	}
	j.Retire()
	unlock()
	if err := j.Wait(ctx); err != nil {
		j.Resume()
		return nil, nil, err
	}
	unlock, err = s.handoffLock(ctx)
	if err != nil {
		j.Resume()
		return nil, nil, err
	}
	defer unlock()
	if err := ctx.Err(); err != nil {
		j.Resume()
		return nil, nil, err
	}
	return a, j, nil
}
func (s *Server) installSurvey(ctx context.Context, candidate *atlas.Atlas, lock *os.File, generation string) error {
	unlock, err := s.handoffLock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if err := s.admin.beginCommit(ctx, "Publishing complete survey"); err != nil {
		return err
	}
	old, j, oldLock := s.Atlas, s.Jobs, s.surveyLock
	root := candidate.Root
	if generation != "" {
		root = old.Root
	}
	if err := s.rememberSurvey(root); err != nil {
		return err
	}
	if generation != "" {
		p, err := atlas.ReadWorkspacePointer(old.Root)
		if err != nil {
			return err
		}
		if err := atlas.PublishWorkspace(old.Root, generation, p.Active); err != nil {
			return err
		}
		candidate.Root = old.Root
		lock = oldLock
	}
	s.Atlas = candidate
	s.Jobs = newJobs(candidate)
	s.Jobs.Threads = j.Threads
	s.surveyID = newHex()
	s.surveyLock = lock
	s.mapPoints = mapPointsCache{}
	s.preparation.close()
	s.preparation = newPreparation(s)
	s.events.publish(JobEvent{Kind: "survey-changed", SurveyID: s.surveyID, JobID: s.preparation.id, Reconcile: true})
	old.Close()
	if generation == "" && oldLock != nil {
		oldLock.Close()
	}
	return nil
}

// Only directories created by this operation may be removed on failure.
func removeOwnedCandidate(path, owner string) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return
	}
	base, err := filepath.Abs(owner)
	if err != nil {
		return
	}
	rel, err := filepath.Rel(base, absolute)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return
	}
	// Refuse traversal through a symlink/reparse point, even during error recovery.
	if actual, err := filepath.EvalSymlinks(absolute); err == nil {
		realBase, e := filepath.EvalSymlinks(base)
		if e != nil {
			return
		}
		r, e := filepath.Rel(realBase, actual)
		if e != nil || r == ".." || strings.HasPrefix(r, ".."+string(os.PathSeparator)) {
			return
		}
	}
	os.RemoveAll(absolute)
}
