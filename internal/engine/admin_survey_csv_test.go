package engine

import (
	"net/http"
	"strings"
	"testing"
)

func TestAdminSurveyExportRoutes(t *testing.T) {
	s := adminServer(t)
	for _, path := range []string{"/database", "/database.html"} {
		for _, method := range []string{"GET", "HEAD"} {
			w := adminRequest(t, s, method, path, nil, "")
			if w.Code != http.StatusTemporaryRedirect || w.Header().Get("Location") != "/admin?tab=survey" {
				t.Fatalf("%s %s: status=%d location=%q", method, path, w.Code, w.Header().Get("Location"))
			}
		}
	}
	w := adminRequest(t, s, "GET", "/admin?tab=survey", nil, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `id="adminCSVExport"`) || !strings.Contains(w.Body.String(), `id="adminDeduplicate"`) {
		t.Fatalf("survey export controls missing from Admin: status=%d", w.Code)
	}
}
