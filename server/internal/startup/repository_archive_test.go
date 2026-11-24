package startup

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"issue-desk-intake/server/internal/contract"
)

func TestRepositoryArchiveWiringAndEntrypoints(t *testing.T) {
	var calls atomic.Int32
	app := openLifecycleApp(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/owner/repo" {
			calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"archived":true}`)
			return
		}
		lifecycleGitea(w, r)
	}, nil)
	checked, err := app.Consultations.CheckRepositoryArchive(context.Background(), "owner/repo")
	var problem *contract.Problem
	if !checked || !errors.As(err, &problem) || problem.Status != 409 || problem.Code != "repository_archived" {
		t.Fatalf("checked=%v error=%v", checked, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("archive requests = %d, want 1", calls.Load())
	}
}
