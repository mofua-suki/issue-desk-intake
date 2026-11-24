package jobs

import (
	"reflect"
	"testing"

	"issue-desk-intake/server/internal/fleet"
)

func TestPostgresArtifactRetryModesKeepInvalidRecord(t *testing.T) {
	for _, mode := range []string{"normal", "retest", "autoRetest", "fromBase"} {
		for _, raw := range []string{
			`{"artifact":null}`, `{"artifact":{}}`, `"invalid"`,
			`{"artifact":{"generation":0,"owner":"one","headSha":"wrong","branch":"old-branch"}}`,
			`{"artifact":{"generation":1,"owner":"one","headSha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","branch":"old-branch"}}`,
		} {
			t.Run(mode+"/"+raw, func(t *testing.T) {
				db, ctx, s, a, id := legacyArtifactFixture(t, 2, "failed")
				// Persist the intake result shape that makes both retest modes eligible.
				if _, err := db.ExecContext(ctx, `UPDATE jobs SET view=jsonb_build_object('continuation',$2::jsonb),
result='{"status":"failed","testFailures":{"complete":true,"names":["fixed regression"]}}' WHERE id=$1`, id, raw); err != nil {
					t.Fatal(err)
				}
				beforeRetry, err := s.Get(ctx, id)
				if err != nil || !RetestEligible(beforeRetry) {
					t.Fatalf("ineligible fixture: %+v %v", beforeRetry, err)
				}
				input := ControlInput{ID: "22222222-2222-4222-8222-222222222222", Action: "retry", By: "issue-screen",
					Retest: mode == "retest", AutoRetest: mode == "autoRetest", FromBase: mode == "fromBase"}
				job, err := s.Control(ctx, id, input, ControlOptions{})
				if err != nil || job.Status != "queued" || job.Generation != 1 {
					t.Fatalf("retry: %+v %v", job, err)
				}
				if mode == "retest" || mode == "autoRetest" {
					if fleet.Object(job.View["retryRetest"])["generation"] != float64(1) || job.View["retryFromBase"] != nil {
						t.Fatalf("retry did not select retest: %#v", job.View)
					}
				}
				if mode == "fromBase" {
					if job.View["continuation"] != nil || !artifactFromBase(job) {
						t.Fatalf("explicit reset contract changed: %#v", job.View)
					}
				} else if legacyArtifactMissing(job) {
					t.Fatal("retry laundered an invalid artifact into legacy absence")
				}
				job, lease, err := s.Next(ctx, "one", 1, a)
				if err != nil || job == nil || job.ID != id || lease == nil || job.LeaseEpoch != 3 {
					t.Fatalf("dispatch: %+v %v", job, err)
				}
				if mode != "fromBase" {
					// No host can report a head change for an invalid record, so
					// dispatch restarts the generation from the default branch
					// instead of leasing it until the lease expires.
					reset := fleet.Object(job.View["retryFromBase"])
					if reset["by"] != artifactResetBy || reset["generation"] != float64(job.Generation) || !artifactFromBase(job) {
						t.Fatalf("dispatch kept an unreportable artifact: %#v", job.View)
					}
					var events int
					if err := db.QueryRowContext(ctx, `SELECT count(*) FROM job_events WHERE job_id=$1 AND kind='artifact_reset'`, id).Scan(&events); err != nil || events != 1 {
						t.Fatalf("artifact_reset events=%d err=%v", events, err)
					}
				}
				r := legacyArtifactReport(job.LeaseEpoch, "running")
				if _, err := s.Report(ctx, id, "one", r); err != nil {
					t.Fatal(err)
				}
				assertLegacyArtifactSaved(t, ctx, s, id, r)
			})
		}
	}
}

func TestPostgresArtifactRetestPreservesOwnershipAndPublication(t *testing.T) {
	for _, auto := range []bool{false, true} {
		for _, owner := range []string{"one", "two"} {
			for _, published := range []bool{false, true} {
				name := "manual/" + owner
				if auto {
					name = "auto/" + owner
				}
				if published {
					name += "/published"
				}
				t.Run(name, func(t *testing.T) {
					db, ctx, s, a, id := legacyArtifactFixture(t, 2, "failed")
					if _, err := db.ExecContext(ctx, `UPDATE jobs SET view=jsonb_build_object('continuation',jsonb_build_object(
'artifact',jsonb_build_object('generation',generation,'owner',$2::text,'headSha',head_sha,'branch',branch),
'publication',jsonb_build_object('generation',generation,'headSha',head_sha,'branch',branch,'verified',$3::boolean))),
result='{"status":"failed","testFailures":{"complete":true,"names":["fixed regression"]}}' WHERE id=$1`, id, owner, published); err != nil {
						t.Fatal(err)
					}
					job, err := s.Control(ctx, id, ControlInput{ID: "22222222-2222-4222-8222-222222222222", Action: "retry", By: "issue-screen", Retest: !auto, AutoRetest: auto}, ControlOptions{})
					if err != nil {
						t.Fatal(err)
					}
					identity, ok := artifactOf(job)
					if !ok || identity.Owner != owner || artifactPublished(job, identity) != published || fleet.Object(job.View["continuation"])["pending"] != nil {
						t.Fatalf("retest lost identity/evidence or kept dispatch hint: %+v %#v", identity, job.View)
					}
					if owner == "two" && !published {
						// Report would refuse every head change from "one", so dispatch keeps
						// the unpublished artifact off "one" instead of letting its lease expire.
						before, err := s.Get(ctx, id)
						if err != nil {
							t.Fatal(err)
						}
						job, _, err = s.Next(ctx, "one", 1, a)
						if err != nil || job != nil {
							t.Fatalf("dispatched an unavailable artifact: %+v %v", job, err)
						}
						after, err := s.Get(ctx, id)
						if err != nil || !reflect.DeepEqual(before, after) {
							t.Fatalf("refused dispatch changed saved job: %+v %v", after, err)
						}
						return
					}
					job, _, err = s.Next(ctx, "one", 1, a)
					if err != nil || job == nil {
						t.Fatalf("dispatch: %+v %v", job, err)
					}
					r := legacyArtifactReport(job.LeaseEpoch, "failed")
					if _, err := s.Report(ctx, id, "one", r); err != nil {
						t.Fatal(err)
					}
					assertLegacyArtifactSaved(t, ctx, s, id, r)
				})
			}
		}
	}
}
