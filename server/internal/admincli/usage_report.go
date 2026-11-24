package admincli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"time"

	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/startup"
	"issue-desk-intake/server/internal/statistics"
)

type singleStringFlag struct {
	value string
	seen  bool
}

func (p *singleStringFlag) String() string { return p.value }
func (p *singleStringFlag) Set(value string) error {
	if p.seen {
		return errors.New("オプションは一度だけ指定してください")
	}
	p.seen = true
	p.value = value
	return nil
}

func parseUsageReportArgs(args []string) (from, to, repo, format, population string, err error) {
	flags := flag.NewFlagSet("usage-per-issue", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	f, t, r := &singleStringFlag{}, &singleStringFlag{}, &singleStringFlag{}
	fmtFlag, p := &singleStringFlag{value: "both"}, &singleStringFlag{value: "merged"}
	flags.Var(f, "from", "")
	flags.Var(t, "to", "")
	flags.Var(r, "repo", "")
	flags.Var(fmtFlag, "format", "")
	flags.Var(p, "population", "")
	if flags.Parse(args) != nil || flags.NArg() != 0 || f.value == "" || t.value == "" || fmtFlag.value != "table" && fmtFlag.value != "json" && fmtFlag.value != "both" || p.value != "merged" && p.value != "all" {
		return "", "", "", "", "", errors.New("--from YYYY-MM-DD --to YYYY-MM-DD [--repo owner/repo] [--format table|json|both] [--population merged|all] を指定してください")
	}
	if r.value != "" && !fleet.ValidRepo(r.value) {
		return "", "", "", "", "", errors.New("--repo は owner/repo で指定してください")
	}
	return f.value, t.value, r.value, fmtFlag.value, p.value, nil
}

func usageReport(ctx context.Context, db *sql.DB, c *startup.Config, args []string, out io.Writer) error {
	from, to, repo, format, population, err := parseUsageReportArgs(args)
	if err != nil {
		return err
	}
	_, period, err := statistics.ReworkQuery(url.Values{"from": {from}, "to": {to}, "project": {repo}}, time.Now(), append(c.Repositories, repo))
	if err != nil {
		return err
	}
	// The drafts live in the application database itself.
	app, err := openAppStore(ctx, c)
	if err != nil {
		return err
	}
	defer app.Close()
	local := app.DB()
	report, err := statistics.ReadIssueUsageReportWith(ctx, db, local, repo, period, fleet.Object(c.Values["usageWeights"]), statistics.IssueUsageOptions{Detail: population == "all"})
	if err != nil {
		return errors.New("依頼別使用量を読み取れません。共有DBと下書きの保存先を確認してください")
	}
	if format != "json" {
		if _, err = fmt.Fprintln(out, usageReportTable(report)); err != nil {
			return err
		}
	}
	if format != "table" {
		encoder := json.NewEncoder(out)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}
	return nil
}
