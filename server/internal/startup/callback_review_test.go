package startup

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/filingcheck"
	"issue-desk-intake/server/internal/gitea"
	"issue-desk-intake/server/internal/numbers"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func reviewAPI(t *testing.T, send deployPollTransport) *gitea.Client {
	t.Helper()
	token := filepath.Join(t.TempDir(), "token")
	if e := os.WriteFile(token, []byte("fixture-token"), 0600); e != nil {
		t.Fatal(e)
	}
	api, e := gitea.New("http://gitea.invalid", token, send)
	if e != nil {
		t.Fatal(e)
	}
	return api
}
func TestFilingCallbacksRejectFractionalIdentifiersBeforeSending(t *testing.T) {
	calls := 0
	api := reviewAPI(t, func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{}, Request: r}, nil
	})
	a := App{Config: &Config{Repositories: []string{"owner/repo"}}, Gitea: api}
	for _, number := range []any{float64(1.5), "1", true, nil, float64(9007199254740992)} {
		calls = 0
		if _, e := a.resolvePreceding(context.Background(), consultations.Record{"project": "owner/repo", "dependsOnIssues": []any{map[string]any{"source": "gitea", "number": number}}}); e == nil || calls != 0 {
			t.Fatal("不正な先行番号を外部へ送信", number, calls, e)
		}
		calls = 0
		if _, e := a.resolvePreceding(context.Background(), consultations.Record{"project": "owner/repo", "dependsOnIssues": []any{consultations.Record{"source": "gitea", "number": number}}}); e == nil || calls != 0 {
			t.Fatal("不正なRecord型の先行番号を外部へ送信", number, calls, e)
		}
		calls = 0
		if e := a.backfillScope(context.Background(), "owner/repo", map[string]any{"number": number}, "body"); e == nil || calls != 0 {
			t.Fatal("不正なIssue番号を外部へ送信", number, calls, e)
		}
	}
}
func TestOpenRequestsUsesResolvedControllerIDsInsteadOfMatchingLogin(t *testing.T) {
	ctx := context.Background()
	local := newAppStore(t)
	records, e := consultations.OpenRecords(ctx, local.DB())
	if e != nil {
		t.Fatal(e)
	}
	index, e := numbers.OpenLedger(ctx, local.DB(), filepath.Join(t.TempDir(), "intake-desk-numbers.jsonl"))
	if e != nil {
		t.Fatal(e)
	}
	api := reviewAPI(t, func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Request: r, Body: io.NopCloser(strings.NewReader(`[{"number":1,"title":"所有者の依頼","body":"owned body","user":{"id":2,"login":"owner"}},{"number":2,"title":"別IDの依頼は参照しない","body":"foreign body","user":{"id":99,"login":"owner"}},{"number":3,"title":"ID欠落の依頼は参照しない","body":"missing id","user":{"login":"owner"}}]`))}, nil
	})
	a := App{ctx: ctx, Config: &Config{Repositories: []string{"owner/repo"}}, Gitea: api, Records: records, Numbers: index, ownIDs: map[int64]bool{1: true, 2: true}}
	text, e := a.openRequests(ctx, consultations.Record{"project": "owner/repo"})
	if e != nil || !strings.Contains(text, "所有者の依頼") || strings.Contains(text, "別IDの依頼") || strings.Contains(text, "ID欠落の依頼") {
		t.Fatal(text, e)
	}
}

func TestProviderLabelIDsRemainIntegersAndDoNotCreateOnMalformedExistingLabel(t *testing.T) {
	for _, item := range []struct {
		id string
		ok bool
	}{{"1.5", false}, {`"2"`, false}, {"2", true}} {
		posts := 0
		api := reviewAPI(t, func(r *http.Request) (*http.Response, error) {
			if r.Method != "GET" {
				posts++
			}
			return &http.Response{StatusCode: 200, Header: http.Header{}, Request: r, Body: io.NopCloser(strings.NewReader(`[{"name":"provider/claude","id":` + item.id + `}]`))}, nil
		})
		a := App{Config: &Config{Repositories: []string{"owner/repo"}}, Gitea: api, Log: func(string) {}}
		ids, e := a.providerLabels(context.Background(), "owner/repo", "claude")
		if item.ok {
			if e != nil || len(ids) != 1 || ids[0] != 2 {
				t.Fatal(ids, e)
			}
		} else if e == nil || len(ids) > 0 {
			t.Fatal("不正なlabel IDを使用", item, ids, e)
		}
		if posts != 0 {
			t.Fatal("不正な既存labelから重複作成", posts)
		}
	}
}

func TestResolvePrecedingAcceptsMapAndRecord(t *testing.T) {
	api := reviewAPI(t, func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Request: r, Body: io.NopCloser(strings.NewReader(`{"state":"open"}`))}, nil
	})
	a := App{ctx: context.Background(), Config: &Config{Repositories: []string{"owner/repo"}}, Gitea: api}
	// An unmapped Gitea number has no desk number.
	local := newAppStore(t)
	index, err := numbers.OpenLedger(context.Background(), local.DB(), filepath.Join(t.TempDir(), "numbers-desk-numbers.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	a.Numbers = index
	for _, item := range []any{map[string]any{"source": "gitea", "number": json.Number("491")}, consultations.Record{"source": "gitea", "number": json.Number("491")}} {
		got, err := a.resolvePreceding(context.Background(), consultations.Record{"project": "owner/repo", "dependsOnIssues": []any{item}})
		if err != nil || len(got) != 1 || got[0]["gitea"] != int64(491) {
			t.Fatalf("%T: %v %v", item, got, err)
		}
	}
}

func TestSavedDependenciesSurviveFilingCheckAndResolution(t *testing.T) {
	ctx := context.Background()
	local := newAppStore(t)
	records, err := consultations.OpenRecords(ctx, local.DB())
	if err != nil {
		t.Fatal(err)
	}
	index, err := numbers.OpenLedger(ctx, local.DB(), filepath.Join(t.TempDir(), "numbers-desk-numbers.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = index.Reserve(ctx, 4901); err != nil {
		t.Fatal(err)
	}
	if _, err = index.Assign(ctx, []numbers.List{{Project: "owner/repo", Issues: []numbers.Candidate{{Number: 491}}}}, []string{"owner/repo"}, true); err != nil {
		t.Fatal(err)
	}
	api := reviewAPI(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/api/v1/repos/owner/repo/issues/491" && r.URL.Path != "/api/v1/repos/owner/repo/issues/492" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Request: r, Body: io.NopCloser(strings.NewReader(`{"state":"open"}`))}, nil
	})
	a := App{ctx: ctx, Config: &Config{Repositories: []string{"owner/repo"}}, Gitea: api, Numbers: index, numberAt: time.Now()}
	d := consultations.Record{"id": "11111111-1111-4111-8111-111111111111", "project": "owner/repo", "status": "submitting", "revision": 1, "title": "修正", "body": "目的", "messages": []any{}, "dependsOnIssues": []any{map[string]any{"source": "desk", "number": 4901}, map[string]any{"source": "gitea", "number": 491}}}
	if _, err = records.Change(ctx, "drafts", d["id"].(string), func(*sql.Tx, consultations.Record) (consultations.Record, error) { return d, nil }); err != nil {
		t.Fatal(err)
	}
	d, _, err = records.Get(ctx, "drafts", d["id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	item := d["dependsOnIssues"].([]any)[0].(map[string]any)
	if _, ok := item["number"].(json.Number); !ok {
		t.Fatalf("保存後の型: %T", item["number"])
	}
	material := consultations.Record{"target": consultations.Record{"body": "目的", "dependsOnIssues": d["dependsOnIssues"]}, "requests": []any{consultations.Record{"number": 492, "title": "別件", "body": "別目的"}}, "jobs": []any{}, "consultations": []any{}}
	checker, err := filingcheck.New(filingcheck.Options{Records: records, Config: func(context.Context) (map[string]any, error) { return map[string]any{}, nil }, Project: a.Project, Material: func(context.Context, consultations.Record) (consultations.Record, error) { return material, nil }, Answer: func(context.Context, map[string]any) (map[string]any, error) {
		return map[string]any{"proceed": true, "reason": "別件", "dependsOnIssues": []any{map[string]any{"source": "gitea", "number": 492}}, "dependencyEvidence": []any{map[string]any{"candidateId": "request:492", "number": 492}}, "duplicate": nil, "evidenceRequests": []any{}}, nil
	}, Revision: strings.Repeat("a", 40), SchemaDir: filepath.Join("..", "..", "schemas")})
	if err != nil {
		t.Fatal(err)
	}
	if err = checker.Check(ctx, d); err != nil {
		t.Fatal(err)
	}
	deps := d["dependsOnIssues"].([]any)
	if len(deps) != 3 {
		t.Fatalf("dependencies: %#v", deps)
	}
	if _, ok := deps[2].(consultations.Record); !ok {
		t.Fatalf("追加項目の型: %T", deps[2])
	}
	got, err := a.resolvePreceding(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []int64{491, 491, 492} {
		if got[i]["gitea"] != want {
			t.Fatal(fmt.Sprint(got))
		}
	}
	if got[0]["desk"] != int64(4901) || got[1]["desk"] != int64(4901) {
		t.Fatal(got)
	}
}

func TestConsultationObjectsBeforeAndAfterPersistence(t *testing.T) {
	ctx := context.Background()
	local := newAppStore(t)
	records, err := consultations.OpenRecords(ctx, local.DB())
	if err != nil {
		t.Fatal(err)
	}
	d := consultations.Record{"id": "11111111-1111-4111-8111-111111111111", "status": "draft", "messages": []any{
		consultations.Record{"role": "user", "text": "共通番号 #123", "references": []any{consultations.Record{"desk": int64(123), "resolution": "resolved"}}},
		consultations.Record{"role": "assistant", "text": `{"relation":"required","evidence":"確認済み"}`},
	}}
	check := func(d consultations.Record) {
		t.Helper()
		entries := referenceEntries(d)
		if len(entries) != 2 || fmt.Sprint(entries[0]["desk"]) != "123" {
			t.Fatalf("references: %#v", entries)
		}
		refs, err := (&App{}).resolveNumbers(ctx, d)
		if err != nil || len(refs.([]map[string]any)) != 1 || refs.([]map[string]any)[0]["resolution"] != "resolved" {
			t.Fatalf("resolve: %#v %v", refs, err)
		}
		relation, evidence := transferAnswer(d)
		if relation != "required" || evidence != "確認済み" {
			t.Fatalf("transfer: %q %q", relation, evidence)
		}
	}
	check(d)
	if _, err = records.Change(ctx, "drafts", d["id"].(string), func(*sql.Tx, consultations.Record) (consultations.Record, error) { return d, nil }); err != nil {
		t.Fatal(err)
	}
	saved, ok, err := records.Get(ctx, "drafts", d["id"].(string))
	if err != nil || !ok {
		t.Fatalf("reload: %v %v", ok, err)
	}
	check(saved)
}

func TestConsultationRecordImagesAreNotSilentlySkipped(t *testing.T) {
	d := consultations.Record{"id": "draft", "messages": []any{consultations.Record{"id": "message", "images": []any{consultations.Record{}}}}}
	if _, err := (&App{}).aiImages(context.Background(), d); err == nil || !strings.Contains(err.Error(), "画像のID") {
		t.Fatalf("missing image ID: %v", err)
	}
}
