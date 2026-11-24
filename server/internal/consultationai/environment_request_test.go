package consultationai

import (
	"context"
	"errors"
	"testing"
)

func TestEnvironmentNetworkIdentifiersAndHosts(t *testing.T) {
	identifiers := []string{"json.Number", "s.repair", "strings.TrimSpace", "regexp.MustCompile", "budget.check", "process.env", "formatRepairs.kinds", "fleet.ClientConfig", ".env.example", "Dockerfile.dev", ".github/workflows/ci.yml"}
	for _, identifier := range identifiers {
		for _, quoted := range []bool{false, true} {
			value := identifier
			if quoted {
				value = "`" + value + "`"
			}
			for _, action := range []string{"確認", "検証", "利用", "使用"} {
				body := value + " を" + action + "する"
				if got := environmentRequests(Record{"ready": true, "body": body}); len(got) != 0 {
					t.Errorf("%q: unexpected findings: %v", body, got)
				}
			}
		}
	}
	for _, body := range []string{
		"https://example.com/x にアクセスする",
		"`https://example.com/x` にアクセスする",
		"example.com にアクセスする",
		"huggingface.co からダウンロードする",
		"Hugging Face API で確認する",
	} {
		if got := environmentRequests(Record{"ready": true, "body": body}); len(got) == 0 {
			t.Errorf("%q: missing finding", body)
		}
	}
}

func TestEnvironmentNetworkMixedCaseHosts(t *testing.T) {
	for _, host := range []string{"Example.com", "EXAMPLE.COM", "Sub.Example.CoM"} {
		for _, field := range []string{"body", "title", "purpose", "acceptance"} {
			body := host + " にアクセスする"
			answer := Record{"ready": true}
			location := "body"
			if field == "body" {
				answer[field] = body
			} else {
				child := Record{field: body}
				location = "splitIssues[1]." + field
				if field == "acceptance" {
					child[field] = []any{body}
					location += "[1]"
				}
				answer["splitIssues"] = []any{child}
			}
			got := environmentRequests(answer)
			if len(got) != 1 || got[0].location != location || got[0].kind != "許可リスト外のネットワーク" || got[0].line != body {
				t.Errorf("%s %s: %v", field, host, got)
			}
		}
	}
	for _, body := range []string{"Example.com/path を取得する", "Dockerfile.DEV を取得する", "strings.TrimSpace を確認する"} {
		if got := environmentRequests(Record{"ready": true, "body": body}); len(got) != 0 {
			t.Errorf("%q: unexpected findings: %v", body, got)
		}
	}
}

func TestEnvironmentRequests(t *testing.T) {
	tests := []struct {
		name, body string
		want       bool
	}{
		{"main", "着手時に最新の main を取り込む", true},
		{"trunk follow", "trunk に追従する", true},
		{"huggingface", "Hugging Face API で確認する", true},
		{"git", "git fetch を実行する", true},
		{"git directory", ".git/config を編集する", true},
		{"conflict", "git の競合を確認して解消する", true},
		{"unrelated conflict", "予約の競合を確認して解消する", false},
		{"production", "本番のDBのデータを取得する", true},
		{"url", "https://example.com/api を呼び出す", true},
		{"huggingface url", "https://huggingface.co/api を確認する", true},
		{"domain", "example.com にアクセスする", true},
		{"controller", "コントローラーが git fetch を実行する", false},
		{"intake", "受付AIが最新の main を取り込む", false},
		{"monitor", "監視が git fetch を実行する", false},
		{"worker", "Worker が git fetch を実行する", false},
		{"hostd", "hostd が git fetch を実行する", false},
		{"human", "マージ後に人が git fetch を実行する", false},
		{"negative", "git fetch はしない", false},
		{"prohibition", "git fetch を禁止する", false},
		{"unnecessary", "git fetch は不要", false},
		{"not requested", "git fetch を求めない", false},
		{"not used", "Hugging Face API は使わない", false},
		{"past", "原因は git fetch を実行したこと", false},
		{"paths", ".gitignore と .gitattributes と deploy/nas/git-hooks/ と git-sync-status.ts を確認する", false},
		{"real test path", "node --test test/consultation-spec.test.ts で確認する", false},
		{"real schema path", "api/schemas/intake.schema.json を確認する", false},
		{"schema filename", "intake.schema.json を検証する", false},
		{"test filename", "server.test.ts を確認する", false},
		{"main path", "server/cmd/intake/main.go の起動処理を更新する", false},
		{"reference", "## 参照（経緯・根拠）\n最新の main を取り込む\n### 詳細\ngit fetch", false},
		{"operations", "## マージ後の運用作業（自動化の対象外）\ngit fetch", false},
		{"fence", "```sh\ngit fetch\n```\n> Hugging Face API で確認する", false},
		{"after section", "## 参照（経緯・根拠）\ngit fetch\n## 受入条件\ngit fetch", true},
		{"corrected", "固定資料 docs/spec.md の10〜20行とテストフィクスチャを確認する", false},
		{"package", "pnpm add example を実行する", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := len(environmentRequests(Record{"ready": true, "body": tc.body})) > 0
			if got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestEnvironmentRequestContext(t *testing.T) {
	for _, tc := range []struct {
		body string
		want bool
	}{
		{"既存の github.com/jackc/pgx を利用する", false},
		{"golang.org/x/net を使用する", false},
		{"golang.org/x/net の API を使用する", false},
		{"go get で github.com/jackc/pgx を取得する", false},
		{"https://example.com/api を呼び出す", true},
		{"Dockerfile.dev を取得する", false},
		{"socket.io を使用する", false},
		{"cfg.app を確認する", false}, {"x.io を使用する", false}, {"dockerfile.dev を検証する", false},
		{"example.com に接続する", true}, {"https://socket.io を確認する", true},
		{"受付の画面を確認し、git fetch を実行する", true},
		{"監視の結果に関係なく git fetch を実行する", true},
		{"worker の設定を見て git fetch を実行する", true},
		{"git fetch を実行し、結果が空なら問題ない", true},
		{"git fetch を実行するが、結果は保存しません", true},
		{"Worker が git fetch を実行する", false}, {"git fetch はしない", false},
		{"git fetch は実行しない", false}, {"git fetch を実行しません", false},
		{"本番DBにはアクセスしません", false}, {"github.com の API は呼び出さない", false},
		{"github.com には接続しません", false},
		{"本番ログの測定はコントローラーの担当とする", false},
		{"既定ブランチへの追従はコントローラーが行う", false},
		{"前回 git fetch を実行して停止した", false}, {"以前のジョブで本番DBに接続した", false},
		{"マージ後に予約の競合を確認する", false},
		{"マージの競合を解消する", true}, {"予定の競合を解消する", false},
	} {
		if got := len(environmentRequests(Record{"ready": true, "body": tc.body})) > 0; got != tc.want {
			t.Errorf("%q: got %v, want %v", tc.body, got, tc.want)
		}
	}
}

func TestEnvironmentRequestsSplitFields(t *testing.T) {
	for _, field := range []string{"title", "purpose", "acceptance"} {
		child := Record{field: "git fetch を実行する"}
		if field == "acceptance" {
			child[field] = []any{"git fetch を実行する"}
		}
		got := environmentRequests(Record{"ready": true, "splitIssues": []any{child}})
		if len(got) != 1 {
			t.Fatalf("%s: %v", field, got)
		}
	}
	if len(environmentRequests(Record{"ready": false, "body": "git fetch"})) != 0 {
		t.Fatal("未確定を検査した")
	}
}

func TestEnvironmentCapabilities(t *testing.T) {
	yes := map[string]any{"gcc": true, "cgo": true, "race": true}
	no := map[string]any{"gcc": true, "cgo": false, "race": true}
	linux := []EnvironmentHost{{"a", map[string]any{"platform": "linux", "race": false}}}
	tests := []struct {
		name, body string
		hosts      []EnvironmentHost
		want       bool
	}{
		{"不足", "go test -race ./... を実行する", []EnvironmentHost{{"a", no}}, true},
		{"充足", "go test -race ./... を実行する", []EnvironmentHost{{"a", yes}, {"b", yes}}, false},
		{"一部", "go test -race ./... を実行する", []EnvironmentHost{{"a", yes}, {"b", no}}, true},
		{"合算しない", "go test -race ./... を実行する", []EnvironmentHost{{"a", map[string]any{"gcc": true, "cgo": false, "race": true}}, {"b", map[string]any{"gcc": false, "cgo": true, "race": true}}}, true},
		{"未確認", "go test -race ./... を実行する", []EnvironmentHost{{"a", no}, {"b", map[string]any{"gcc": true}}}, false},
		{"空", "go test -race ./... を実行する", nil, false},
		{"否定", "raceは不要", []EnvironmentHost{{"a", no}}, false},
		{"Dockerの否定", "Dockerは不要でテストを実行する", []EnvironmentHost{{"a", map[string]any{"docker": false}}}, false},
		{"Dockerの否定とgccの不足", "Dockerは不要でgccを使うテストを実行する", []EnvironmentHost{{"a", map[string]any{"docker": false, "gcc": false}}}, true},
		{"Dockerの否定とgccの充足", "Dockerは不要でgccを使うテストを実行する", []EnvironmentHost{{"a", map[string]any{"docker": false, "gcc": true}}}, false},
		{"連結した否定", "gccとcgoは不要でテストを実行する", []EnvironmentHost{{"a", map[string]any{"gcc": false, "cgo": false}}}, false},
		{"否定と肯定の混在", "gccは不要でcgoを使うテストを実行する", []EnvironmentHost{{"a", map[string]any{"gcc": false, "cgo": false}}}, true},
		{"否定と肯定の混在で充足", "gccは不要でcgoを使うテストを実行する", []EnvironmentHost{{"a", map[string]any{"gcc": false, "cgo": true}}}, false},
		{"否定と別の不足", "raceは不要、gccを使ったテストを実行する", []EnvironmentHost{{"a", map[string]any{"gcc": false}}}, true},
		{"否定と別の充足", "raceは不要、gccを使ったテストを実行する", []EnvironmentHost{{"a", map[string]any{"gcc": true}}}, false},
		{"肯定の後に別の否定と不足", "gccを使ったテストを実行しraceは不要", []EnvironmentHost{{"a", map[string]any{"gcc": false}}}, true},
		{"肯定の後に別の否定と充足", "gccを使ったテストを実行しraceは不要", []EnvironmentHost{{"a", map[string]any{"gcc": true}}}, false},
		{"過去", "原因: 以前はgccが無くテストできなかった", []EnvironmentHost{{"a", map[string]any{"gcc": false}}}, false},
		{"Mac用スタブテスト名", "go test ./internal/hostd -run TestMacOSScripts/apple を実行する", linux, false},
		{"Mac用スタブのファイル", "deploy/client/macos-apple/prepare.test.sh を実行する", linux, false},
		{"Windows用ファイル", "scripts/windows/prepare.ps1 をスタブで検証する", linux, false},
		{"FlutterのWindows用テスト", "flutter test test/windows/foo_test.dart を実行する", linux, false},
		{"GoのWindows用パッケージ", "go test ./internal/windows/... を実行する", linux, false},
		{"拡張子のないパッケージ", "go test internal/windows を実行する", linux, false},
		{"Windowsとアーキテクチャ", "windows/amd64 でテストを実行する", linux, true},
		{"明示したMacとファイル", "test/common/foo_test.dart を macOS15.1/arm64 で検証する", linux, true},
		{"Windows名のサブテスト", "go test -run TestPlatform/windows を実行する", linux, false},
		{"同じ文のMac実行条件", "deploy/client/macos-apple/prepare.test.sh を Mac OS で実行する", linux, true},
		{"版付きMac実行条件", "macOS15.1でテストを実行する", linux, true},
		{"同じ文のWindows実行条件", "scripts/windows/prepare.ps1 を Windows で実行する", linux, true},
		{"Mac用スタブにもraceが必要", "go test -race ./internal/hostd -run TestMacOSScripts/apple を実行する", linux, true},
		{"Windows", "Windows専用テストを実行する", []EnvironmentHost{{"a", map[string]any{"platform": "linux"}}}, true},
		{"macOS Linux", "macOSでの確認必須", []EnvironmentHost{{"a", map[string]any{"platform": "linux"}}}, true},
		{"macOSは登録された別Macで検証", "macOSで検証する", []EnvironmentHost{{"a", map[string]any{"platform": "linux", "nativeTest": map[string]any{"platform": "darwin"}}}}, false},
		{"Macの能力をLinuxと合算しない", "macOSでgccを使い検証する", []EnvironmentHost{{"a", map[string]any{"platform": "linux", "gcc": true, "nativeTest": map[string]any{"platform": "darwin", "gcc": false}}}}, true},
		{"macOS", "macOSで検証する", []EnvironmentHost{{"a", map[string]any{"platform": "windows"}}}, true},
		{"実機", "対象実機で動作を確認する", []EnvironmentHost{{"a", map[string]any{"realDevice": false}}}, true},
		{"実機一部", "対象実機で動作を確認する", []EnvironmentHost{{"a", map[string]any{"realDevice": true}}, {"b", map[string]any{"realDevice": false}}}, false},
		{"実機未確認", "対象実機で動作を確認する", []EnvironmentHost{{"a", map[string]any{"realDevice": false}}, {"b", map[string]any{"realDevice": nil}}}, false},
		{"実機真値は未確認", "対象実機で動作を確認する", []EnvironmentHost{{"a", map[string]any{"realDevice": true}}}, false},
		{"実測", "実環境で性能を実測する", []EnvironmentHost{{"a", map[string]any{"liveMeasurement": false}}}, true},
		{"実測真値は未確認", "実環境で性能を実測する", []EnvironmentHost{{"a", map[string]any{"liveMeasurement": true}}}, false},
		{"実測未確認", "実環境で性能を実測する", []EnvironmentHost{{"a", map[string]any{"liveMeasurement": nil}}}, false},
		{"別の測定先は特定できない", "本番環境Bで性能を実測する", []EnvironmentHost{{"a", map[string]any{"liveMeasurement": true}}, {"b", map[string]any{"liveMeasurement": false}}}, false},
		{"運用節", "## マージ後の運用作業（自動化の対象外）\ngo test -race ./... を実行する", []EnvironmentHost{{"a", no}}, false},
		{"引用とコード", "> go test -race ./...\n```\ngo test -race ./...\n```", []EnvironmentHost{{"a", no}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &Service{options: Options{Environment: func(_ context.Context, _ string) ([]EnvironmentHost, error) { return tc.hosts, nil }}}
			got := len(s.environmentFindings(context.Background(), Record{"project": "owner/app"}, Record{"ready": true, "body": tc.body})) > 0
			if got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestEnvironmentCapabilitiesSplitProject(t *testing.T) {
	calls := map[string]int{}
	s := &Service{options: Options{Environment: func(_ context.Context, repo string) ([]EnvironmentHost, error) {
		calls[repo]++
		if repo == "owner/b" {
			return []EnvironmentHost{{"b", map[string]any{"gcc": false, "cgo": true, "race": true}}}, nil
		}
		return []EnvironmentHost{{"a", map[string]any{"gcc": true, "cgo": true, "race": true}}}, nil
	}}}
	raceTest := "go test -race ./..."
	answer := Record{"ready": true, "body": raceTest, "splitIssues": []any{
		Record{"project": "owner/b", "acceptance": []any{raceTest, raceTest}},
		Record{"title": raceTest, "acceptance": []any{raceTest}},
		Record{"project": "owner/c", "acceptance": []any{"go test ./..."}},
	}}
	findings := s.environmentFindings(context.Background(), Record{"project": "owner/a"}, answer)
	if len(findings) != 2 || findings[0].location != "splitIssues[1].acceptance[1]" || findings[1].location != "splitIssues[1].acceptance[2]" {
		t.Fatal(findings)
	}
	if len(calls) != 2 || calls["owner/a"] != 1 || calls["owner/b"] != 1 {
		t.Fatalf("能力の取得回数: %v", calls)
	}
	fallback := Record{"ready": true, "splitIssues": []any{Record{"acceptance": []any{raceTest}}}}
	findings = s.environmentFindings(context.Background(), Record{"project": "owner/b"}, fallback)
	if len(findings) != 1 || findings[0].location != "splitIssues[1].acceptance[1]" || calls["owner/b"] != 2 {
		t.Fatal(findings, calls)
	}
}

func TestEnvironmentCapabilitiesRetriesNextField(t *testing.T) {
	calls := 0
	s := &Service{options: Options{Environment: func(context.Context, string) ([]EnvironmentHost, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("unavailable")
		}
		return []EnvironmentHost{{"a", map[string]any{"gcc": false, "cgo": true, "race": true}}}, nil
	}}}
	raceTest := "go test -race ./..."
	answer := Record{"ready": true, "body": raceTest + "\n" + raceTest,
		"splitIssues": []any{Record{"acceptance": []any{raceTest, raceTest}}}}
	got := s.environmentFindings(context.Background(), Record{"project": "owner/a"}, answer)
	if calls != 2 || len(got) != 2 || got[0].location != "splitIssues[1].acceptance[1]" || got[1].location != "splitIssues[1].acceptance[2]" {
		t.Fatal(calls, got)
	}
}

func TestEnvironmentUnavailableKeepsProhibitedRequestChecks(t *testing.T) {
	s := &Service{options: Options{Environment: func(context.Context, string) ([]EnvironmentHost, error) { return nil, errors.New("unavailable") }}}
	got := s.environmentFindings(context.Background(), Record{"project": "owner/app"}, Record{"ready": true, "body": "go test -race ./... を実行する。git fetch を実行する"})
	if len(got) != 1 || got[0].kind != "git・既定ブランチ" {
		t.Fatal(got)
	}
}
