package jobs

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/fleet"
)

func TestArtifactIdentityTransitions(t *testing.T) {
	a, b, branch, other := strings.Repeat("a", 40), strings.Repeat("b", 40), "work", "other"
	job := &Job{Generation: 1, View: map[string]any{}}
	set := func(host string, r Report, allowed bool) {
		t.Helper()
		c, err := updateReportArtifact(job, host, r, false)
		if (err == nil) != allowed {
			t.Fatalf("host %s: %v", host, err)
		}
		if err != nil {
			return
		}
		if c != nil {
			job.View["continuation"] = c
		}
		if r.Branch.Present {
			job.Branch = r.Branch.Value
		}
		if r.HeadSHA.Present {
			job.HeadSHA = r.HeadSHA.Value
		}
	}
	set("one", Report{Branch: Optional[string]{Present: true, Value: &branch}, HeadSHA: Optional[string]{Present: true, Value: &a}}, true)
	set("two", Report{HeadSHA: Optional[string]{Present: true, Value: &a}}, true)
	set("two", Report{}, true)
	set("two", Report{HeadSHA: Optional[string]{Present: true, Value: &b}}, false)
	set("two", Report{HeadSHA: Optional[string]{Present: true}}, false)
	set("two", Report{Branch: Optional[string]{Present: true, Value: &other}}, false)
	set("one", Report{Branch: Optional[string]{Present: true, Value: &other}}, true)
	set("one", Report{HeadSHA: Optional[string]{Present: true, Value: &b}}, true)
	identity, ok := artifactOf(job)
	if !ok || identity.Owner != "one" || identity.Head != b {
		t.Fatalf("identity: %+v", identity)
	}
	delete(job.View, "continuation")
	set("two", Report{HeadSHA: Optional[string]{Present: true, Value: &a}}, false)
	job.Generation++
	job.View["retryFromBase"] = map[string]any{"generation": job.Generation, "by": "dispatch"}
	set("two", Report{HeadSHA: Optional[string]{Present: true, Value: &a}}, false)
	job.View["retryFromBase"] = map[string]any{"generation": job.Generation, "by": "issue-screen"}
	set("two", Report{HeadSHA: Optional[string]{Present: true, Value: &a}}, true)
	identity, ok = artifactOf(job)
	if !ok || identity.Owner != "two" {
		t.Fatalf("reset identity: %+v", identity)
	}
	set("one", Report{HeadSHA: Optional[string]{Present: true, Value: &b}}, false)
}

func TestArtifactHandoffAndPublication(t *testing.T) {
	head, branch := strings.Repeat("a", 40), "work"
	job := &Job{Generation: 1, HeadSHA: &head, Branch: &branch, View: map[string]any{"continuation": map[string]any{
		"artifact":    map[string]any{"generation": 1, "branch": branch, "headSha": head, "owner": "one"},
		"publication": map[string]any{"generation": 1, "branch": branch, "headSha": head, "verified": true},
	}}}
	a, ok := artifactOf(job)
	if !ok || !artifactPublished(job, a) || !artifactAvailable(a, "two", artifactPublished(job, a)) {
		t.Fatal("verified publication unavailable")
	}
	handoff := carryArtifact(job, map[string]any{"pending": true})
	if handoff["artifact"] == nil || handoff["publication"] == nil {
		t.Fatal("handoff lost evidence")
	}
	next := strings.Repeat("b", 40)
	c, err := updateReportArtifact(job, "two", Report{HeadSHA: Optional[string]{Present: true, Value: &next}}, false)
	if err != nil || c["publication"] != nil {
		t.Fatalf("old publication retained: %v %v", c, err)
	}
	job.HeadSHA = &next
	if carryArtifact(job, nil)["artifact"] != nil {
		t.Fatal("mismatched tuple transferred")
	}
}

func TestPostgresArtifactReportProtection(t *testing.T) {
	db, ctx := testPostgres(t)
	id := seedJob(t, ctx, db, "owner/repo", "one", 55, 7)
	s, err := New(db, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	a, b, branch := strings.Repeat("a", 40), strings.Repeat("b", 40), "work"
	r := Report{Epoch: 7, Status: "running", Branch: Optional[string]{Present: true, Value: &branch}, HeadSHA: Optional[string]{Present: true, Value: &a}, View: map[string]any{"continuation": map[string]any{"artifact": map[string]any{"owner": "fake"}}}}
	job, err := s.Report(ctx, id, "one", r)
	if err != nil {
		t.Fatal(err)
	}
	identity, ok := artifactOf(job)
	if !ok || identity.Owner != "one" {
		t.Fatalf("identity: %+v", identity)
	}
	if _, err = db.ExecContext(ctx, "UPDATE jobs SET lease_host='two', lease_epoch=8 WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	for _, report := range []Report{{Epoch: 8, Status: "running"}, {Epoch: 8, Status: "running", HeadSHA: Optional[string]{Present: true, Value: &a}}} {
		if _, err = s.Report(ctx, id, "two", report); err != nil {
			t.Fatal(err)
		}
	}
	for _, report := range []Report{{Epoch: 8, Status: "running", HeadSHA: Optional[string]{Present: true, Value: &b}}, {Epoch: 8, Status: "running", HeadSHA: Optional[string]{Present: true}}, {Epoch: 8, Status: "running", Branch: Optional[string]{Present: true}}} {
		_, err = s.Report(ctx, id, "two", report)
		code(t, err, 409, "artifact_unavailable")
	}
	job, err = s.Report(ctx, id, "two", Report{Epoch: 8, Status: "running"})
	if err != nil {
		t.Fatal(err)
	}
	identity, ok = artifactOf(job)
	if !ok || identity.Owner != "one" || identity.Head != a || identity.Branch != branch {
		t.Fatalf("state changed: %+v", identity)
	}
	// The original owner may produce a new head and change the branch.
	if _, err = db.ExecContext(ctx, "UPDATE jobs SET lease_host='one', lease_epoch=9 WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	other := "new-branch"
	job, err = s.Report(ctx, id, "one", Report{Epoch: 9, Status: "running", Branch: Optional[string]{Present: true, Value: &other}, HeadSHA: Optional[string]{Present: true, Value: &b}})
	if err != nil {
		t.Fatal(err)
	}
	identity, ok = artifactOf(job)
	if !ok || identity.Owner != "one" || identity.Head != b {
		t.Fatalf("owner update: %+v", identity)
	}
	// Old rows do not acquire an owner from a later lease.
	if _, err = db.ExecContext(ctx, `UPDATE jobs SET lease_host='two', lease_epoch=10, view=view-'continuation' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	_, err = s.Report(ctx, id, "two", Report{Epoch: 10, Status: "running", HeadSHA: Optional[string]{Present: true, Value: &a}})
	code(t, err, 409, "artifact_unavailable")
	// The explicit control reset permits a new generation, then protects its owner.
	if _, err = db.ExecContext(ctx, `UPDATE jobs SET generation=generation+1, view=view || jsonb_build_object('retryFromBase',jsonb_build_object('generation',generation+1,'by','issue-screen')) WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	job, err = s.Report(ctx, id, "two", Report{Epoch: 10, Status: "running", HeadSHA: Optional[string]{Present: true, Value: &a}})
	if err != nil {
		t.Fatal(err)
	}
	identity, ok = artifactOf(job)
	if !ok || identity.Owner != "two" {
		t.Fatalf("reset: %+v", identity)
	}
}

func TestPostgresControlRetryPreservesArtifact(t *testing.T) {
	db, ctx := testPostgres(t)
	s, err := New(db, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	id := seedJob(t, ctx, db, "owner/repo", "one", 56, 7)
	a, b, branch := strings.Repeat("a", 40), strings.Repeat("b", 40), "work"
	_, err = s.Report(ctx, id, "one", Report{Epoch: 7, Status: "failed", Branch: Optional[string]{Present: true, Value: &branch}, HeadSHA: Optional[string]{Present: true, Value: &a}})
	if err != nil {
		t.Fatal(err)
	}
	// Publication is normally recorded by a later component; exercise the persisted shape.
	_, err = db.ExecContext(ctx, `UPDATE jobs SET view=jsonb_set(view,'{continuation,publication}', jsonb_build_object('generation',generation,'branch',branch,'headSha',head_sha,'verified',true)) WHERE id=$1`, id)
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.Control(ctx, id, ControlInput{ID: "22222222-2222-4222-8222-222222222222", Action: "retry", By: "issue-screen"}, ControlOptions{})
	if err != nil {
		t.Fatal(err)
	}
	identity, ok := artifactOf(job)
	if !ok || identity.Owner != "one" || !artifactPublished(job, identity) {
		t.Fatalf("retry lost identity or evidence: %+v %#v", identity, job.View)
	}
	// Remove publication to model a private artifact, then simulate a lease to another PC.
	_, err = db.ExecContext(ctx, `UPDATE jobs SET status='running',lease_host='two',lease_epoch=8,lease_expires_at=$2,view=view #- '{continuation,publication}' WHERE id=$1`, id, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, report := range []Report{{Epoch: 8, Status: "running", HeadSHA: Optional[string]{Present: true, Value: &b}}, {Epoch: 8, Status: "running", HeadSHA: Optional[string]{Present: true}}, {Epoch: 8, Status: "running", Branch: Optional[string]{Present: true}}} {
		_, err = s.Report(ctx, id, "two", report)
		code(t, err, 409, "artifact_unavailable")
		saved, e := s.Get(ctx, id)
		if e != nil {
			t.Fatal(e)
		}
		got, valid := artifactOf(saved)
		if !valid || got != identity || stringValue(saved.HeadSHA) != a || stringValue(saved.Branch) != branch {
			t.Fatalf("rejected report changed state: %+v", saved)
		}
	}
	_, err = db.ExecContext(ctx, `UPDATE jobs SET lease_host='one',lease_epoch=9 WHERE id=$1`, id)
	if err != nil {
		t.Fatal(err)
	}
	job, err = s.Report(ctx, id, "one", Report{Epoch: 9, Status: "running", HeadSHA: Optional[string]{Present: true, Value: &b}})
	if err != nil {
		t.Fatal(err)
	}
	identity, ok = artifactOf(job)
	if !ok || identity.Owner != "one" || identity.Head != b || artifactPublished(job, identity) {
		t.Fatalf("owner update: %+v", identity)
	}
}

func TestPostgresControlArtifactHandoff(t *testing.T) {
	for _, mode := range []string{"fromBase", "resume", "instruction"} {
		t.Run(mode, func(t *testing.T) {
			db, ctx, s, assignments, id := legacyArtifactFixture(t, 0, "queued")
			job := takeLegacyArtifact(t, ctx, s, assignments, id, "one")
			r := legacyArtifactReport(job.LeaseEpoch, "held")
			reason := "question"
			if mode == "fromBase" {
				r.Status = "failed"
			} else {
				r.HoldReason = &reason
			}
			if _, err := s.Report(ctx, id, "one", r); err != nil {
				t.Fatal(err)
			}
			original := assertLegacyArtifactSaved(t, ctx, s, id, r)
			identity, ok := artifactOf(original)
			if !ok {
				t.Fatal("missing initial artifact")
			}
			input := ControlInput{ID: "22222222-2222-4222-8222-222222222222", By: "issue-screen", Action: mode, Text: "追加の指示"}
			if mode == "fromBase" {
				input.Action, input.FromBase = "retry", true
			}
			job, err := s.Control(ctx, id, input, ControlOptions{})
			if err != nil || job.Status != "queued" {
				t.Fatalf("control: %+v %v", job, err)
			}
			saved, err := s.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			entries := fleet.List(saved.View["instructions"])
			if len(entries) != 1 || fleet.Object(entries[0])["text"] != input.Text {
				t.Fatalf("instruction not saved: %#v", saved.View)
			}
			if mode == "fromBase" {
				if _, ok := artifactOf(saved); ok || !artifactFromBase(saved) || saved.Generation != original.Generation+1 {
					t.Fatalf("reset: %+v", saved)
				}
				if _, err = db.ExecContext(ctx, `UPDATE hosts SET ready=false WHERE host='one'`); err != nil {
					t.Fatal(err)
				}
			} else {
				got, ok := artifactOf(saved)
				if !ok || got != identity || saved.Generation != original.Generation {
					t.Fatalf("handoff: %+v %#v", got, saved.View)
				}
				if _, err = db.ExecContext(ctx, `UPDATE hosts SET ready=false WHERE host='one'`); err != nil {
					t.Fatal(err)
				}
				other, _, err := s.Next(ctx, "two", 1, assignments)
				if err != nil || other != nil {
					t.Fatalf("private artifact dispatched to another host: %+v %v", other, err)
				}
				if _, err = db.ExecContext(ctx, `UPDATE hosts SET ready=true WHERE host='one'`); err != nil {
					t.Fatal(err)
				}
			}
			host := "one"
			if mode == "fromBase" {
				host = "two"
			}
			job, lease, err := s.Next(ctx, host, 1, assignments)
			if err != nil || job == nil || lease == nil || job.ID != id {
				t.Fatalf("dispatch: %+v %v", job, err)
			}
			head := strings.Repeat("c", 40)
			report := Report{Epoch: job.LeaseEpoch, Status: "running", HeadSHA: Optional[string]{Present: true, Value: &head}}
			if mode == "fromBase" {
				head = identity.Head
				branch := fmt.Sprintf("codex/issue-%d-job-%d-retry-%d", job.Issue, job.ID, job.Generation)
				report.Branch = Optional[string]{Present: true, Value: &branch}
				identity.Branch = branch
			}
			job, err = s.Report(ctx, id, host, report)
			if err != nil {
				t.Fatal(err)
			}
			saved, err = s.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			got, ok := artifactOf(saved)
			if !ok || got.Owner != host || got.Head != head || got.Branch != identity.Branch || got.Generation != saved.Generation {
				t.Fatalf("reported artifact: %+v job=%+v", got, job)
			}
		})
	}
}

func TestPostgresRetryFromBaseSameHeadArtifactReport(t *testing.T) {
	db, ctx := testPostgres(t)
	s, err := New(db, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	for i, tc := range []struct {
		name, by                            string
		resetGeneration, artifactGeneration int64
		branch                              string
		epoch                               int64
		accept                              bool
		code                                string
	}{
		{"authorized", "issue-screen", 2, 1, "canonical", 8, true, ""},
		{"normal continuation non-owner", "dispatch", 2, 1, "canonical", 8, false, "artifact_unavailable"},
		{"other generation", "issue-screen", 1, 1, "canonical", 8, false, "artifact_unavailable"},
		{"other branch", "issue-screen", 2, 1, "other", 8, false, "artifact_unavailable"},
		{"established artifact", "issue-screen", 2, 2, "canonical", 8, false, "artifact_unavailable"},
		{"old lease", "issue-screen", 2, 1, "canonical", 7, false, "lease_lost"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := seedJob(t, ctx, db, "owner/repo", "two", int64(55+i), 8)
			head, oldBranch := strings.Repeat("a", 40), "old-branch"
			_, err := db.ExecContext(ctx, `UPDATE jobs SET generation=2, branch=$2, head_sha=$3, pr=42,
view=jsonb_build_object('retryFromBase',jsonb_build_object('generation',$4::int,'by',$5::text),
'continuation',jsonb_build_object('artifact',jsonb_build_object('generation',$6::int,'branch',$2::text,'headSha',$3::text,'owner','one'),
'publication',jsonb_build_object('generation',$6::int,'branch',$2::text,'headSha',$3::text,'verified',$7::boolean,'pr',42))) WHERE id=$1`, id, oldBranch, head, tc.resetGeneration, tc.by, tc.artifactGeneration, tc.accept)
			if err != nil {
				t.Fatal(err)
			}
			before, err := s.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			branch := fmt.Sprintf("codex/issue-%d-job-%d-retry-2", before.Issue, id)
			if tc.branch == "other" {
				branch += "-other"
			}
			report := Report{Epoch: tc.epoch, Status: "running", Branch: Optional[string]{Present: true, Value: &branch}, HeadSHA: Optional[string]{Present: true, Value: &head}}
			got, err := s.Report(ctx, id, "two", report)
			if !tc.accept {
				code(t, err, 409, tc.code)
				saved, e := s.Get(ctx, id)
				if e != nil {
					t.Fatal(e)
				}
				if stringValue(saved.Branch) != oldBranch || stringValue(saved.HeadSHA) != head || saved.PR == nil || *saved.PR != 42 || !reflect.DeepEqual(saved.View["continuation"], before.View["continuation"]) {
					t.Fatalf("rejection changed artifact: %+v", saved)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			check := func(job *Job) {
				t.Helper()
				a, ok := artifactOf(job)
				if !ok || a.Generation != 2 || a.Branch != branch || a.Head != head || a.Owner != "two" || job.PR != nil || fleet.Object(job.View["continuation"])["publication"] != nil {
					t.Fatalf("wrong reset artifact: %+v job=%+v", a, job)
				}
			}
			check(got)
			saved, err := s.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			check(saved)
			replay, err := s.Report(ctx, id, "two", report)
			if err != nil {
				t.Fatal(err)
			}
			check(replay)
			saved, err = s.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			check(saved)
		})
	}
}
