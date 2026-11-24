package issueapi

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/numbers"
	"regexp"
	"strconv"
	"strings"
)

var proposalBatchSourcePattern = regexp.MustCompile(`(?m)^- (\S+#[1-9][0-9]*)（PR:`)

var followupSourcePattern = regexp.MustCompile(`<!-- issue-desk-followup:([0-9]+):[a-f0-9]{64} -->`)

var reviewSourcePattern = regexp.MustCompile(`(?:^|\n)#([1-9][0-9]*) の独立レビューで、`)

func (s *Service) issueOrigin(ctx context.Context, repo, body string, draft Object) (Object, error) {
	sources, e := s.originSources(ctx, repo, body, draft)
	if e != nil {
		return nil, e
	}
	if len(sources) == 0 {
		return nil, nil
	}
	if e = s.attachOriginDeskNumbers(ctx, sources); e != nil {
		return nil, e
	}
	return Object{"sources": sources}, nil
}

// originSources は依頼の起点になった依頼を、改善案・要対応・止まりの直しの記録と、フォローアップの本文から集める。
func (s *Service) originSources(ctx context.Context, repo, body string, draft Object) ([]Object, error) {
	sources := []Object{}
	seen := map[string]bool{}
	add := func(project string, n int64) {
		key := fmt.Sprintf("%s#%d", project, n)
		if project == "" || n < 1 || seen[key] {
			return
		}
		seen[key] = true
		sources = append(sources, Object{"project": project, "number": n})
	}
	addRef := func(value string) {
		at := strings.LastIndex(value, "#")
		if at < 1 {
			return
		}
		project := value[:at]
		n, e := strconv.ParseInt(value[at+1:], 10, 64)
		if e == nil && repoPattern.MatchString(project) && n <= 9007199254740991 {
			add(project, n)
		}
	}
	proposal := fleet.ObjectOrEmpty(draft["proposal"])
	if source := fleet.Text(proposal["source"]); source != "" {
		batch := []string{}
		if proposal["kind"] == "improvements" {
			record, e := s.readListRecord(ctx, "proposals", source+":"+fleet.Text(proposal["id"]))
			if e != nil {
				return nil, e
			}
			purpose := fleet.Text(fleet.ObjectOrEmpty(record["proposal"])["purpose"])
			for _, m := range proposalBatchSourcePattern.FindAllStringSubmatch(purpose, -1) {
				batch = append(batch, m[1])
			}
		}
		if len(batch) > 0 {
			for _, source := range batch {
				addRef(source)
			}
		} else {
			addRef(source)
		}
	}
	for _, entry := range []struct {
		value Object
		key   string
	}{{fleet.ObjectOrEmpty(draft["attention"]), "number"}, {fleet.ObjectOrEmpty(draft["stallFix"]), "issue"}} {
		if n, ok := number(entry.value[entry.key]); ok {
			add(fleet.Text(entry.value["project"]), n)
		}
	}
	if len(sources) == 0 {
		for _, m := range followupSourcePattern.FindAllStringSubmatch(body, -1) {
			n, e := strconv.ParseInt(m[1], 10, 64)
			if e == nil {
				add(repo, n)
			}
		}
		if strings.Contains(body, "<!-- issue-desk:followup -->") {
			m := reviewSourcePattern.FindStringSubmatch(body)
			if len(m) == 2 {
				n, e := strconv.ParseInt(m[1], 10, 64)
				if e == nil {
					add(repo, n)
				}
			}
		}
	}
	return sources, nil
}

// attachOriginDeskNumbers は起点の依頼に受付の共通番号を付ける。番号が無ければ nil にする。
func (s *Service) attachOriginDeskNumbers(ctx context.Context, sources []Object) error {
	for _, source := range sources {
		var desk int64
		var found bool
		var e error
		if snapshot, ok := ctx.Value(listSnapshotKey{}).(*listSnapshot); ok {
			desk, found = snapshot.numbers[numbers.Issue{Project: fleet.Text(source["project"]), Number: int64Number(source["number"])}]
		} else {
			desk, found, e = s.o.Numbers.Number(ctx, fleet.Text(source["project"]), int64Number(source["number"]))
		}
		if e != nil {
			return e
		}
		source["desk"] = nil
		if found {
			source["desk"] = desk
		}
	}
	return nil
}
