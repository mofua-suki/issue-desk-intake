package consultations

import "issue-desk-intake/server/internal/fleet"

func pendingRead(progress any) []any {
	out := []any{}
	for _, value := range fleet.Items(record(progress)["pending"]) {
		r := record(value)
		file := text(value)
		if file == "" {
			file = text(r["file"])
		}
		if file == "" {
			continue
		}
		mode := text(r["mode"])
		if mode == "" {
			mode = "full"
		}
		out = append(out, Record{"path": prefix(MaskCredentials(file), 300), "mode": mode})
		if len(out) == 12 {
			break
		}
	}
	return out
}
func unreadRecord(progress any) []any {
	out := []any{}
	p := record(progress)
	add := func(project string, item any) {
		if len(out) >= 40 {
			return
		}
		r := record(item)
		if text(r["path"]) == "" {
			return
		}
		mode := text(r["kind"])
		if mode == "" {
			mode = text(record(r["request"])["mode"])
		}
		if mode == "" {
			mode = "full"
		}
		entry := Record{"path": prefix(MaskCredentials(text(r["path"])), 300), "reason": r["reason"], "mode": mode}
		if project != "" {
			entry["project"] = project
		}
		out = append(out, entry)
	}
	for _, v := range fleet.Items(p["refused"]) {
		add("", v)
	}
	for name, value := range record(p["projects"]) {
		project := record(value)
		for _, v := range fleet.Items(project["refused"]) {
			add(name, v)
		}
		for _, v := range fleet.Items(project["reads"]) {
			if text(record(v)["reason"]) != "" {
				add(name, v)
			}
		}
	}
	return out
}
