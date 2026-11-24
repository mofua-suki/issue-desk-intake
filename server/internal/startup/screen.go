package startup

import (
	"context"
	"errors"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/hosts"
	"issue-desk-intake/server/internal/listscope"
	"issue-desk-intake/server/internal/operations"
	"issue-desk-intake/server/internal/screenapi"
	"issue-desk-intake/server/internal/statistics"
	"issue-desk-intake/server/internal/usage"
)

func (a *App) registerScreen() error {
	config, e := a.Values()
	if e != nil {
		return e
	}
	lists := map[string]screenapi.ListReader{
		"issue": func(ctx context.Context, _ string) ([]map[string]any, error) {
			result, err := a.screenIssues(ctx)
			if err == nil {
				screenapi.ReportUnreadProjects(ctx, result.unread)
			}
			return result.rows, err
		},
		"closeRequest": func(ctx context.Context, _ string) ([]map[string]any, error) { return a.Issues.CloseRequests(ctx) },
	}
	entries := map[string]screenapi.EntryReader{
		"issue": func(ctx context.Context, id map[string]any, _ string) (map[string]any, error) {
			row, err := a.Issues.ListEntry(ctx, fleet.Text(id["project"]), int64(fleet.Number(id["number"])))
			if err != nil {
				return nil, err
			}
			rows := []map[string]any{row}
			err = a.applyScreenIssueArchives(ctx, rows)
			return rows[0], err
		},
		"closeRequest": func(ctx context.Context, id map[string]any, _ string) (map[string]any, error) {
			return a.Issues.CloseRequest(ctx, fleet.Text(id["id"]))
		},
	}
	a.registerConsultationReaders(lists, entries)
	_, assignments, e := a.Fleet.Load()
	if e != nil {
		return e
	}
	stats := &statistics.Service{DB: a.DB, AppDB: a.SQLite.DB(), Assignments: assignments, RuntimeDir: a.Config.RuntimeDir, Weights: fleet.Object(config["usageWeights"]), Desk: a.DeskNumber, TokenNames: a.SQLite.TokenNames, Drafts: func(ctx context.Context) ([]map[string]any, error) {
		records, e := a.Records.List(ctx, "drafts")
		out := []map[string]any{}
		for _, r := range records {
			out = append(out, map[string]any(r))
		}
		return out, e
	}, DraftIssue: a.firstDraftIssue}
	// The list polls these alerts; keep their heavy parts across the copies below.
	stats.InitUsageReviewCache()

	startedAt := time.Now()
	a.SizeReference = func(ctx context.Context, project string) (string, error) {
		values, e := a.Values()
		if e != nil {
			return "", e
		}
		copy := *stats
		copy.Weights = fleet.Object(values["usageWeights"])
		_, assignments, e := a.Fleet.Load()
		if e != nil {
			return "", e
		}
		copy.Risk = fleet.Object(assignments.Output["intakeSplit"])
		return copy.SizeReference(ctx, project)
	}
	a.MeasurementReference = func(ctx context.Context, project string) (string, error) {
		values, e := a.Values()
		if e != nil {
			return "", e
		}
		copy := *stats
		copy.Weights = fleet.Object(values["usageWeights"])
		return copy.MeasurementReference(ctx, project)
	}
	ops := &operations.Service{PublicLink: a.Link, SystemAlerts: a.systemAlerts, AIAlerts: a.AIFailures.Alerts, ContractMismatches: a.ContractMismatches, Fleet: a.Fleet.Load, Gitea: a.Gitea, Drift: a.Drift, DraftStallAlerts: a.draftStallAlerts, ReadComments: a.Gitea.Comments, TrustedWriter: a.TrustedWriter, LiveAIAlerts: a.liveAIAlerts, Now: time.Now, Statistics: stats, Assignments: assignments, StartedAt: startedAt, DraftAlerts: a.draftAlerts, DB: a.SQLite.DB(), PostgreSQL: a.DB, RuntimeDir: a.Config.RuntimeDir, Repositories: func() ([]string, error) { return a.Repositories(), nil }, Desk: a.DeskNumber}
	ops.InitUrgentIssueCache()
	ops.InitHeavyReworkCache()
	loadOps := func() (*operations.Service, error) {
		_, assignments, e := a.Fleet.Load()
		if e != nil {
			return nil, e
		}
		values, e := a.Values()
		if e != nil {
			return nil, e
		}
		copy := *ops
		statisticsCopy := *stats
		statisticsCopy.Assignments = assignments
		statisticsCopy.Weights = fleet.Object(values["usageWeights"])
		copy.Assignments = assignments
		copy.Statistics = &statisticsCopy
		return &copy, nil
	}
	a.Operations = loadOps
	a.Screen, e = screenapi.Register(a.Mux, screenapi.Options{
		Auth: a.Auth, AppDB: a.SQLite.DB(), Postgres: a.DB, RuntimeDir: a.Config.RuntimeDir, RepoRoot: a.Config.ApplicationDir,
		RuntimeMeasurements: a.RuntimeMeasurements.View, Config: a.Values, Lists: lists, Entries: entries, UsageWeights: fleet.Object(config["usageWeights"]),
		Providers: func(ctx context.Context) (any, error) { return a.Providers(ctx) },
		Aux: map[string]func(context.Context) (any, error){"reviews": func(ctx context.Context) (any, error) {
			source, e := loadOps()
			if e != nil {
				return nil, e
			}
			return source.UsageReviewAlerts(ctx)
		}, "stalls": func(ctx context.Context) (any, error) {
			source, e := loadOps()
			if e != nil {
				return nil, e
			}
			return source.Stalls(ctx)
		}, "fullTests": func(ctx context.Context) (any, error) {
			source, e := loadOps()
			if e != nil {
				return nil, e
			}
			return source.FullTests(ctx)
		}, "attentionAlerts": func(ctx context.Context) (any, error) {
			source, e := loadOps()
			if e != nil {
				return nil, e
			}
			return source.AttentionWithTiming(ctx, func(name string, duration time.Duration) {
				screenapi.ReportAttentionTiming(ctx, name, duration)
			})
		}, "hosts": func(ctx context.Context) (any, error) {
			_, assignments, e := a.Fleet.Load()
			if e != nil {
				return nil, e
			}
			value, e := (&hosts.Service{DB: a.DB, Now: time.Now, StartedAt: startedAt}).View(ctx, assignments)
			if e != nil {
				return nil, e
			}
			if e = a.Drift.Decorate(ctx, assignments, fleet.Object(value)); e != nil {
				return nil, e
			}
			return value, nil
		}},
		Usage: func(ctx context.Context, from, to time.Time) ([]map[string]any, error) {
			rows, skipped, e := (&usage.Store{DB: a.DB}).Read(ctx, "", from, to)
			if e != nil {
				return nil, e
			}
			if skipped > 0 {
				return nil, errors.New("使用量に読み取れない行があります")
			}
			return rows, nil
		},
	})
	return e
}

func (a *App) registerConsultationReaders(lists map[string]screenapi.ListReader, entries map[string]screenapi.EntryReader) {
	screen := auth.Principal{Kind: auth.Screen}
	view := func(kind string, record consultations.Record, origin string) map[string]any {
		if kind == "draft" {
			return map[string]any(a.Consultations.DraftView(record, screen, origin))
		}
		return map[string]any(a.Consultations.TodoView(record, screen, origin))
	}
	for kind, table := range map[string]string{"draft": "drafts", "todo": "todos"} {
		lists[kind] = func(ctx context.Context, origin string) ([]map[string]any, error) {
			var records []consultations.Record
			var draftEntries []consultations.DraftFields
			var e error
			if kind == "draft" {
				draftEntries, e = a.screenDraftEntries(ctx)
			} else {
				records, e = a.Records.List(ctx, table)
			}
			if e != nil {
				return nil, e
			}
			if kind == "draft" && listscope.Active(ctx) && a.Issues != nil {
				issueRows, err := a.screenIssues(ctx)
				if err != nil {
					return nil, err
				}
				draftEntries = consultations.ScopeDraftEntries(ctx, draftEntries, issueRows.rows, issueRows.omitted, issueRows.unreadSet(), true)
			}
			out := []map[string]any{}
			if kind == "draft" {
				for _, record := range a.Consultations.DraftScreenViews(draftEntries, screen, origin) {
					out = append(out, map[string]any(record))
				}
			} else {
				for _, record := range records {
					out = append(out, view(kind, record, origin))
				}
			}
			return out, nil
		}
		entries[kind] = func(ctx context.Context, id map[string]any, origin string) (map[string]any, error) {
			record, found, e := a.Records.Get(ctx, table, fleet.Text(id["id"]))
			if e != nil || !found {
				return nil, e
			}
			if kind == "draft" && record["parentSplit"] == nil && record["splitChildren"] != nil {
				record = a.withFamilyProgress(ctx, record)
			}
			return view(kind, record, origin), nil
		}
	}
}

// withFamilyProgress は分割の親の相談に、子の相談の進み具合を付けた最新の記録を返す。読めなければ元の記録のまま返す。
func (a *App) withFamilyProgress(ctx context.Context, record consultations.Record) consultations.Record {
	entries, err := a.Records.ListDraftEntries(ctx, consultations.DraftReadPage)
	if err == nil {
		// A projection does not preserve arbitrary JSON identifiers.  Only
		// use it when each key identifies exactly the same full record;
		// otherwise retain the legacy ordered candidate selection.
		valid := true
		seen := make(map[string]bool, len(entries))
		records := make([]consultations.Record, len(entries))
		for i, entry := range entries {
			id, ok := entry.Record["id"].(string)
			if !ok || id == "" || id != entry.Key || seen[id] {
				valid = false
			}
			seen[id] = true
			records[i] = entry.Record
		}
		if valid {
			for _, candidate := range a.Consultations.FamilyProgress(ctx, records) {
				if candidate["id"] == record["id"] {
					current, found, getErr := a.Records.Get(ctx, "drafts", fleet.Text(candidate["id"]))
					currentID, isString := current["id"].(string)
					if getErr == nil && found && isString && currentID == candidate["id"] {
						delete(current, "familyProgress")
						if progress, ok := candidate["familyProgress"]; ok {
							current["familyProgress"] = progress
						}
						record = current
					} else {
						// The selected row disappeared, changed identity, or could
						// not be reread. Retry the legacy ordered selection.
						valid = false
					}
					break
				}
			}
		}
		if !valid {
			if records, err := a.Records.List(ctx, "drafts"); err == nil {
				for _, candidate := range a.Consultations.FamilyProgress(ctx, records) {
					if candidate["id"] == record["id"] {
						record = candidate
						break
					}
				}
			}
		}
	}
	return record
}
