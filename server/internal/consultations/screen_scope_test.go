package consultations

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/listscope"
)

func TestScopeDraftFamiliesAndViews(t *testing.T) {
	raw, err := os.ReadFile("../../../test/fixtures/list-scope-cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var data struct {
		Cases []struct {
			Name         string
			Drafts       []Record
			Issues       []Record
			ActiveDrafts []string
			ActiveIssues []int
		}
	}
	if err = json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	for _, c := range data.Cases {
		t.Run(c.Name, func(t *testing.T) {
			s := &Service{}
			s.options.FamilyJobs = func(context.Context, []FamilyIssue) ([]FamilyJob, error) {
				out := []FamilyJob{}
				for _, i := range c.Issues {
					if status := text(i["jobStatus"]); status != "" {
						n, _ := integer(i["number"])
						out = append(out, FamilyJob{Project: "owner/app", Number: n, Status: status})
					}
				}
				return out, nil
			}
			s.options.FamilyIssues = func(context.Context, string) (map[string]string, error) {
				out := map[string]string{}
				for _, i := range c.Issues {
					out[listscope.IssueKey("owner/app", i["number"])] = text(i["state"])
				}
				return out, nil
			}
			records := s.FamilyProgress(context.Background(), c.Drafts)
			entries := make([]DraftFields, len(records))
			for i, d := range records {
				entries[i] = DraftFields{Key: text(d["id"]), Record: d}
			}
			full := s.DraftScreenViews(entries, auth.Principal{Kind: auth.Screen}, "")
			listed := []map[string]any{}
			omitted := map[string]bool{}
			for _, i := range c.Issues {
				keep := false
				for _, n := range c.ActiveIssues {
					if float64(n) == i["number"] {
						keep = true
					}
				}
				if keep {
					listed = append(listed, map[string]any{"project": "owner/app", "number": i["number"]})
				} else {
					omitted[listscope.IssueKey("owner/app", i["number"])] = true
				}
			}
			activeCtx := listscope.WithScope(context.Background(), "active")
			selected := ScopeDraftEntries(activeCtx, entries, listed, omitted, nil, true)
			got := []string{}
			for _, e := range selected {
				got = append(got, e.Key)
			}
			want := c.ActiveDrafts
			if want == nil {
				want = []string{}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("drafts=%v want=%v", got, want)
			}
			views := s.DraftScreenViews(selected, auth.Principal{Kind: auth.Screen}, "")
			for _, v := range views {
				for _, f := range full {
					if f["id"] == v["id"] && !reflect.DeepEqual(v, f) {
						t.Fatalf("family/actions changed: active=%v full=%v", v, f)
					}
				}
			}
			if got := ScopeDraftEntries(context.Background(), entries, nil, nil, nil, true); !reflect.DeepEqual(got, entries) {
				t.Fatal("default scope changed")
			}
		})
	}
}

func TestScopeDraftMalformedIdentityIsConservative(t *testing.T) {
	ctx := listscope.WithScope(context.Background(), "active")
	entries := []DraftFields{{Key: "saved-key", Record: Record{"id": "other-id", "status": "submitted", "issue": Record{"number": 1}}}}
	if got := ScopeDraftEntries(ctx, entries, nil, map[string]bool{"owner/app#1": true}, nil, true); !reflect.DeepEqual(got, entries) {
		t.Fatal("malformed identity discarded")
	}
	if !DraftScopeProtection(entries).All {
		t.Fatal("malformed snapshot must retain Issue rows too")
	}
}
