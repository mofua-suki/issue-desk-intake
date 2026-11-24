package httpapi

import (
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/dispatch"
	"issue-desk-intake/server/internal/fleet"
	"net/http"
	"strings"
)

func (s *WorkerRoutes) nativeRequestAccess(r *http.Request, queue dispatch.Queue, id, actor string) error {
	request, err := queue.GetRequest(r.Context(), id)
	if err != nil || request == nil || request.Kind != "native_test" {
		return err
	}
	target := fleet.Text(request.Payload["host"])
	if actor != request.Sender && actor != target {
		return contract.NewProblem(403, "ほかのPC宛てのMac検証にはアクセスできません。", "")
	}
	document, assignments, err := s.fleet.Load()
	if err != nil {
		return err
	}
	// Releasing work and cancelling an unclaimed request remain possible after reassignment.
	if strings.HasSuffix(r.URL.Path, "/release") || strings.HasSuffix(r.URL.Path, "/cancel") {
		return nil
	}
	if !assignments.CanTest(target, request.Repo) || fleet.NativeTestTarget(document, assignments, request.Sender, request.Repo) != target {
		return contract.NewProblem(409, "Mac検証の担当設定が変わりました。現在の設定で依頼し直してください。", "")
	}
	return nil
}
