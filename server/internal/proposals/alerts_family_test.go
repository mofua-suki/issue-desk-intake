package proposals

import (
	"context"
	"testing"
)

func TestUnfiledAlertsWorkerFamily(t *testing.T) {
	const old = "worker-job-with-hyphen-1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-2"
	const newer = "worker-job-with-hyphen-1-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-2"
	tests := []struct {
		name, oldID, otherID, status, repo string
		number, comment                    int
		addProposal, wantOld               bool
	}{
		{"notified", old, newer, "notified", "owner/repo", 1, 11, false, false},
		{"discarded", old, newer, "discarded", "owner/repo", 1, 11, false, false},
		{"no later post", old, "", "", "", 0, 0, false, true},
		{"later unfiled", old, newer, "adopted", "owner/repo", 1, 11, false, true},
		{"different job", old, "worker-other-1-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-2", "notified", "owner/repo", 1, 11, false, true},
		{"different generation", old, "worker-job-with-hyphen-2-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-2", "notified", "owner/repo", 1, 11, false, true},
		{"different batch", old, "worker-job-with-hyphen-1-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-3", "notified", "owner/repo", 1, 11, false, true},
		{"different issue", old, newer, "notified", "owner/repo", 2, 11, false, true},
		{"different repository", old, newer, "notified", "other/repo", 1, 11, false, true},
		{"earlier completion", old, newer, "notified", "owner/repo", 1, 9, false, true},
		{"same comment", old, newer, "notified", "owner/repo", 1, 10, false, true},
		{"missing comment", old, newer, "notified", "owner/repo", 1, 0, false, true},
		{"legacy ID", "legacy-1", newer, "notified", "owner/repo", 1, 11, false, true},
		{"same ID completion", old, old, "notified", "owner/repo", 1, 11, false, false},
		{"adopted group remains", old, newer, "notified", "owner/repo", 1, 11, true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s, _, restart := fixture(t)
			r, err := s.read(ctx, "rejection:owner/repo#1:10")
			if err != nil {
				t.Fatal(err)
			}
			r["id"] = tc.oldID
			if err = s.save(ctx, "rejection:owner/repo#1:10", r); err != nil {
				t.Fatal(err)
			}
			if tc.otherID != "" {
				other := Object{"type": "proposal", "repo": tc.repo, "number": tc.number, "id": tc.otherID, "status": tc.status, "commentId": tc.comment, "postedAt": r["postedAt"]}
				if err = s.save(ctx, "other", other); err != nil {
					t.Fatal(err)
				}
			}
			if tc.addProposal {
				p := Object{"type": "proposal", "repo": "owner/repo", "number": 1, "id": tc.oldID, "status": "adopted", "commentId": 10, "postedAt": r["postedAt"]}
				if err = s.save(ctx, "adopted", p); err != nil {
					t.Fatal(err)
				}
			}
			restart()
			alerts, err := s.UnfiledProposalAlerts(ctx)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, raw := range alerts {
				if raw.(Object)["proposalId"] == tc.oldID {
					found = true
				}
			}
			if found != tc.wantOld {
				t.Fatalf("old proposal present = %v, want %v; alerts = %v", found, tc.wantOld, alerts)
			}
		})
	}
}
