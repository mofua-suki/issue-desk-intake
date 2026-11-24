package usage

import (
	"strings"
	"testing"
	"time"
)

func TestRedactHidesEveryCredentialForm(t *testing.T) {
	for _, tc := range []struct{ name, input, secret string }{
		{"既存の接頭辞", "値 idk_abc123 です", "abc123"},
		{"既存の接頭辞sk", "値 sk-abc123 です", "abc123"},
		{"token=", "token=abc123 の続き", "abc123"},
		{"Authorization Bearer", "Authorization: Bearer abc123", "abc123"},
		{"Authorization Basic 小文字", "authorization=basic abc123", "abc123"},
		{"Authorization 複数語", "Authorization: Digest username=abc123, response=def456", "abc123"},
		{"Authorization 複数語の後半", "Authorization: Digest username=abc123, response=def456", "def456"},
		{"引用符のAuthorization", `{"Authorization":"Bearer abc123"}`, "abc123"},
		{"見出しのない Bearer", "curl に Bearer abc123def456 を渡す", "abc123def456"},
		{"JSONのtoken", `{"token":"abc123"}`, "abc123"},
		{"JSONのtoken 空白入り", `{"token":"abc 123"}`, "123"},
		{"JSONの接頭辞付きtoken", `{"access_token":"abc123"}`, "abc123"},
		{"JSONのpassword", `"password": "abc123"`, "abc123"},
		{"JSONのpassword 空白入り", `"password": "abc 123"`, "123"},
		{"シングルクォート", `'password': 'abc 123'`, "123"},
		{"JSONの中のJSON", `{"body":"{\"token\":\"abc 123\"}"}`, "123"},
		{"URLのパスワード", "https://user:abc123@host/path", "abc123"},
		{"URLのトークン", "https://abc123@host/path", "abc123"},
		{"URLのトークン http", "http://abc123@host/path", "abc123"},
		{"URLのパスワード エスケープ", `https:\/\/u:p4ss@h`, "p4ss"},
		{"sshのURLのパスワード", "ssh://git:p4ss@host/x.git", "p4ss"},
		{"日本語のパスワード", "パスワード: abc123", "abc123"},
		{"日本語のトークン", "トークン=abc123", "abc123"},
		{"日本語の見出し 「」", "パスワード：「abc 123」", "123"},
		{"全角コロン", "token：abc123", "abc123"},
		{"Cookie", "Cookie: session=abc123", "abc123"},
		{"Cookie 2つ目", "Cookie: a=b; session=abc123", "abc123"},
		{"Set-Cookie", "Set-Cookie: sid=abc123; Path=/", "abc123"},
		{"秘密鍵の本体", "前\n-----BEGIN PRIVATE KEY-----\nabc123\n-----END PRIVATE KEY-----\n後", "abc123"},
		{"RSA秘密鍵の本体", "-----BEGIN RSA PRIVATE KEY-----\nabc123\n-----END RSA PRIVATE KEY-----", "abc123"},
		{"見出し付きの秘密鍵", "-----BEGIN ENCRYPTED PRIVATE KEY-----\nProc-Type: 4\n\nabc123\n-----END ENCRYPTED PRIVATE KEY-----", "abc123"},
		{"途中で切れた秘密鍵", "-----BEGIN PRIVATE KEY-----\nabc123\ndef", "abc123"},
		{"secret_key", "secret_key=abc123", "abc123"},
		{"private_key", "private_key: abc123", "abc123"},
		{"AKIA", "鍵 AKIAIOSFODNN7EXAMPLE です", "AKIAIOSFODNN7EXAMPLE"},
		{"見出しのない Basic", "Basic dXNlcjpwYXNz を渡す", "dXNlcjpwYXNz"},
		{"見出しのない Bearer の長い小文字", "Bearer abcdefghijklmnopqrstuvwx", "abcdefghijklmnopqrstuvwx"},
		{"Authorization の1語", "authorization: abcdefgh", "abcdefgh"},
		{"proxy-authorization", "Proxy-Authorization: Basic abc123", "abc123"},
		{"2語目が小文字の語でない apikey", "authorization: apikey SECRETabc123XYZ", "SECRETabc123XYZ"},
		{"2語目が小文字の語でない oauth", "authorization: oauth ya29.a0AfH6SECRET", "SECRET"},
		{"2語目が小文字の語でない key", "authorization: key AIzaSyD-SECRET", "SECRET"},
		{"2語目が小文字の語でない true", "authorization: true SECRETabc", "SECRETabc"},
		{"2語目が小文字の語でない cookie", "cookie: session abcSECRET", "abcSECRET"},
		{"小文字2語の認証方式", "authorization: bearer abcdefg", "abcdefg"},
		{"文の次の見出し", "cookie: required authorization: abc123", "abc123"},
		{"文の後ろの token", "authorization: required for token=abc123", "abc123"},
		{"文の後の Authorization", `{"authorization":true,"proxy_authorization":"Bearer abc123"}`, "abc123"},
		{"ghp", "ghp_abc123", "abc123"},
		{"github_pat", "github_pat_abc123", "abc123"},
		{"api_key", "api_key=abc123", "abc123"},
		{"api-key", "x-api-key: abc123", "abc123"},
		{"apikey", `{"apiKey":"abc123"}`, "abc123"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Redact(tc.input)
			if strings.Contains(got, tc.secret) || !strings.Contains(got, "[REDACTED]") {
				t.Fatalf("%q -> %q", tc.input, got)
			}
			if safe := SafeText(tc.input); strings.Contains(safe, tc.secret) {
				t.Fatalf("SafeText %q -> %q", tc.input, safe)
			}
		})
	}
}

// 呼び出し元は報告文を読ませるので、資格情報でないものを伏せない。
func TestRedactKeepsOrdinaryText(t *testing.T) {
	for _, text := range []string{
		"コミット 0123456789abcdef0123456789abcdef01234567 を確認",
		"https://example.com/path?q=a@b",
		"http://192.168.0.10:3000/mofua-suki/issue-desk-worker/pulls/68",
		"連絡先 user@example.com",
		"input_tokens: 12, max_tokens=34",
		"<!-- gcr:issue-desk-close-request:abc -->",
		"task-123 と risk-456",
		"トークンの期限が切れました",
		"bearer token",
		"https://example.com?q=a@b",
		"https://example.com:8080?q=a@b",
		"ssh://git@192.168.0.10:2222/x.git",
		"docker://foo@sha256:abc",
		"go install example.com//cmd@latest",
		"Bearer authentication required",
		"Basic Authentication",
		"authorization: required for /api/v1/x, status=500, retry in 5s",
		`{"authorization":true,"host":"nas"}`,
		`{"cookie":false,"host":"nas"}`,
		"Authorization ヘッダの扱い",
	} {
		if got := Redact(text); got != text {
			t.Fatalf("%q -> %q", text, got)
		}
	}
	// END の無い鍵は本体までで止め、後ろの文を消さない。
	for input, want := range map[string]string{
		"-----BEGIN PRIVATE KEY-----\nabc123\ndef\n次の文は残る":         "[REDACTED]\n次の文は残る",
		"-----BEGIN PRIVATE KEY-----\\nabc123\\n<!-- 目印 -->":       "[REDACTED]\\n<!-- 目印 -->",
		"-----BEGIN PRIVATE KEY-----\nabc\n\n<!-- gcr:close:1 -->": "[REDACTED]\n\n<!-- gcr:close:1 -->",
	} {
		if got := Redact(input); got != want {
			t.Fatalf("%q -> %q", input, got)
		}
	}
	// 鍵が続いても本体を残さない。
	if got := Redact("-----BEGIN PRIVATE KEY-----\nabc123 -----BEGIN PRIVATE KEY-----\ndef456"); strings.Contains(got, "abc123") || strings.Contains(got, "def456") {
		t.Fatal(got)
	}
	// 伏せるのは値までで、後ろの別の項目は読める。
	for input, want := range map[string]string{
		`{"token":"abc 123","count":2}`:                                     `{[REDACTED],"count":2}`,
		"token=abc123, count=2":                                             "[REDACTED], count=2",
		"Authorization: Bearer abc123\n次の行":                                 "[REDACTED]\n次の行",
		"Authorization: Bearer abc123 の続き":                                  "[REDACTED]の続き",
		"パスワード: abc123。次の文は残る":                                              "[REDACTED]。次の文は残る",
		"トークン=abc123、次の文は残る":                                                "[REDACTED]、次の文は残る",
		"パスワード: abc123　全角空白の後は残る":                                           "[REDACTED]　全角空白の後は残る",
		"前\n-----BEGIN PRIVATE KEY-----\nabc\n-----END PRIVATE KEY-----\n後": "前\n[REDACTED]\n後",
		`{"authorization":true,"token":"abc"}`:                              `{"authorization":true,[REDACTED]}`,
	} {
		if got := Redact(input); got != want {
			t.Fatalf("%q -> %q", input, got)
		}
	}
}

// 一覧の行ごとに走るので、処理時間は入力の長さに比例しなければならない。
// 走査が2乗になると128KBで数十秒かかる。上限はCIの揺れを見込み、それより十分小さくする。
func TestRedactRunsInLinearTime(t *testing.T) {
	const size = 128 * 1024
	for _, unit := range []string{
		"cookie: required ",
		"authorization: required ",
		`{"authorization":true},`,
		"authorization: required for ",
		"cookie: required\n",
		"パスワード: 「",
		"token: 「",
		`token: "`,
		"-----BEGIN PRIVATE KEY----- ",
		"-----BEGIN PRIVATE KEY-----\n",
		"Bearer ",
		"https://a",
		"//a:b",
	} {
		input := strings.Repeat(unit, size/len(unit)+1)
		started := time.Now()
		Redact(input)
		SafeText(input)
		elapsed := time.Since(started)
		t.Logf("%q: %v", unit, elapsed)
		if elapsed > 3*time.Second {
			t.Fatalf("%q の繰り返しに %v かかりました", unit, elapsed)
		}
	}
}
