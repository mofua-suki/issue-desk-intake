package statistics

import (
	"context"
	"issue-desk-intake/server/internal/fleet"
)

func filed(d map[string]any) []map[string]any {
	out := []map[string]any{}
	seen := map[string]bool{}
	add := func(raw any, investigation bool) {
		i := fleet.Object(raw)
		number := fleet.Number(i["number"])
		if number < 1 {
			return
		}
		repo := fleet.Text(i["project"])
		if repo == "" {
			repo = fleet.Text(d["project"])
		}
		key := identity(repo, number)
		if seen[key] {
			if investigation {
				for _, row := range out {
					if row["project"] == repo && row["number"] == number {
						row["investigation"] = true
					}
				}
			}
			return
		}
		seen[key] = true
		out = append(out, map[string]any{"project": repo, "number": number, "investigation": investigation})
	}
	list := fleet.List(d["issues"])
	if len(list) > 0 {
		for _, raw := range list {
			add(raw, false)
		}
	} else {
		add(d["issue"], false)
	}
	for _, raw := range fleet.List(d["investigations"]) {
		add(raw, true)
	}
	return out
}
func (s *Service) draftUsageOutcomes(ctx context.Context) map[string]map[string]any {
	out := map[string]map[string]any{}
	s.filedOutcomes(ctx, out)
	if s.AppDB != nil {
		s.withdrawnOutcomes(ctx, out)
		s.returnedTodoOutcomes(ctx, out)
	}
	return out
}

// filedOutcomes records whether each consultation filed an issue or an investigation.
func (s *Service) filedOutcomes(ctx context.Context, out map[string]map[string]any) {
	if s.Drafts != nil {
		if drafts, e := s.Drafts(ctx); e == nil {
			for _, d := range drafts {
				id := fleet.Text(d["id"])
				if id == "" {
					continue
				}
				status := "pending"
				if d["status"] == "submitted" {
					status = "finished"
				}
				desks := []any{}
				ordinary, investigation := false, false
				for _, i := range filed(d) {
					if i["investigation"] == true {
						investigation = true
					} else {
						ordinary = true
					}
					if s.Desk != nil {
						if desk := s.Desk(fleet.Text(i["project"]), int64(fleet.Number(i["number"]))); desk != nil {
							desks = append(desks, desk)
						}
					}
				}
				if ordinary {
					status = "filed"
				} else if investigation {
					status = "investigation"
				}
				out[id] = map[string]any{"outcome": status, "desks": desks}
			}
		}
	}
}

// withdrawnOutcomes records the consultations discarded or returned to TODO.
func (s *Service) withdrawnOutcomes(ctx context.Context, out map[string]map[string]any) {
	var exists int
	if e := s.AppDB.QueryRowContext(ctx, tableExistsSQL(), "draft_withdrawals").Scan(&exists); e == nil && exists > 0 {
		if rows, e := s.AppDB.QueryContext(ctx, bindSQL("SELECT draft,operation FROM draft_withdrawals ORDER BY at,sequence")); e == nil {
			withdrawals := map[string]string{}
			for rows.Next() {
				var id, operation string
				if rows.Scan(&id, &operation) == nil {
					withdrawals[id] = operation
				}
			}
			rows.Close()
			for id, operation := range withdrawals {
				if out[id] != nil {
					continue
				}
				status := "unknown"
				if operation == "discard" {
					status = "discarded"
				}
				if operation == "todo" {
					status = "returned_to_todo"
				}
				out[id] = map[string]any{"outcome": status, "desks": []any{}}
			}
		}
	}
}

// returnedTodoOutcomes records the TODOs returned from a consultation.
func (s *Service) returnedTodoOutcomes(ctx context.Context, out map[string]map[string]any) {
	if todos, e := s.localRows(ctx, "todos"); e == nil {
		for _, todo := range todos {
			id := fleet.Text(todo["id"])
			if out[id] == nil && todo["returnedAt"] != nil {
				out[id] = map[string]any{"outcome": "returned_to_todo", "desks": []any{}}
			}
		}
	}
}
