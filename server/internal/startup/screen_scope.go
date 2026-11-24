package startup

import (
	"context"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/deskcleanup"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/issueapi"
	"issue-desk-intake/server/internal/listscope"
	"issue-desk-intake/server/internal/screenapi"
)

type scopedIssues struct {
	rows    []map[string]any
	omitted map[string]bool
	unread  []screenapi.UnreadProject
}

func (a *App) screenDraftEntries(ctx context.Context) ([]consultations.DraftFields, error) {
	return listscope.Read(ctx, "screen-drafts", func() ([]consultations.DraftFields, error) {
		entries, err := a.Records.ListDraftEntries(ctx, consultations.DraftReadScreen)
		if err != nil {
			return nil, err
		}
		entries = consultations.ScopeDraftEntries(ctx, entries, nil, nil, nil, false)
		records := make([]consultations.Record, len(entries))
		for i, e := range entries {
			records[i] = e.Record
		}
		records = a.Consultations.FamilyProgress(ctx, records)
		for i := range entries {
			entries[i].Record = records[i]
		}
		return entries, nil
	})
}
func (a *App) screenIssues(ctx context.Context) (scopedIssues, error) {
	return listscope.Read(ctx, "screen-issues", func() (scopedIssues, error) {
		result := scopedIssues{omitted: map[string]bool{}}
		if listscope.Active(ctx) {
			entries, err := a.screenDraftEntries(ctx)
			if err != nil {
				return result, err
			}
			closes, err := a.Issues.CloseRequests(ctx)
			if err != nil {
				return result, err
			}
			ctx = listscope.WithProtectionResolver(ctx, func(issues []map[string]any) listscope.Protection {
				selected := consultations.ScopeDraftEntries(ctx, entries, issues, nil, nil, true)
				protection := consultations.DraftScopeProtection(selected)
				// The client derives an open close request's followup flag from
				// its Issue row, even when Gitea already reports it closed.
				for _, close := range closes {
					protection.Issues[listscope.IssueKey(fleet.Text(close["project"]), close["number"])] = true
				}
				return protection
			})
			ctx = listscope.WithOmitted(ctx, result.omitted)
		}
		rows, unread, err := a.Issues.ListPartial(ctx)
		if err != nil {
			return result, err
		}
		if err := a.applyScreenIssueArchives(ctx, rows); err != nil {
			return result, err
		}
		result.rows = rows
		for _, u := range unread {
			result.unread = append(result.unread, screenapi.UnreadProject{Project: u.Project, Reason: u.Reason})
		}
		return result, nil
	})
}

func (a *App) applyScreenIssueArchives(ctx context.Context, rows []map[string]any) error {
	if a.SQLite == nil {
		return nil
	}
	return deskcleanup.ApplyIssueArchives(ctx, a.SQLite.DB(), rows)
}

// activeIssues reads the Issue rows of the active range for
// GET /api/issues?scope=active. It starts its own active request and reuses the
// screen's reader, so both keep the same closed rows.
func (a *App) activeIssues(ctx context.Context) ([]map[string]any, []issueapi.UnreadProject, error) {
	result, err := a.screenIssues(listscope.WithScope(ctx, "active"))
	if err != nil {
		return nil, nil, err
	}
	unread := make([]issueapi.UnreadProject, 0, len(result.unread))
	for _, u := range result.unread {
		unread = append(unread, issueapi.UnreadProject{Project: u.Project, Reason: u.Reason})
	}
	return result.rows, unread, nil
}

// unreadSet returns the projects of candidates the Issue list could not read.
func (s scopedIssues) unreadSet() map[string]bool {
	out := map[string]bool{}
	for _, u := range s.unread {
		out[u.Project] = true
	}
	return out
}
