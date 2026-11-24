package usage

import (
	"regexp"
	"strings"
)

// quotedCredentialValue は引用符の中を空白ごと1つの値として読む。JSONの中のJSON（\"...\"）と「」も同じ。
// 「」は次の同じ文字で閉じると限らないので長さを限る。閉じない値の繰り返しで走査が2乗にならない。
const quotedCredentialValue = `"(?:[^"\\\n]|\\.)*"|'[^'\n]*'|\\"(?:[^"\\\n]|\\[^"\n])*\\"|「[^」\n]{0,200}」`

// credentialKeyStart は access_token・x-api-key のような接頭辞と、JSONの鍵の引用符も一緒に伏せる。
const credentialKeyStart = `(?:\\?["'])?[\w-]*`
const credentialKeyEnd = `(?:\\?["'])?[\s　]*[:=：][\s　]*`

// credentialPattern は contract.MaskCredentials と違い、40桁の16進を伏せない。
// ここを通る文にはコミットSHAが入り、依頼者が読むため。
var credentialPattern = regexp.MustCompile(`(?i)` + strings.Join([]string{
	`\b(?:idk_|idh_|sk-|gh[pousr]_|github_pat_|glpat-|xox[abprs]-)[A-Za-z0-9_-]+`,
	`(?-i:\bAKIA[0-9A-Z]{16}\b)`,
	// URLの user:password@ と https://<token>@。https:\/\/ の形も読む。
	// ssh://git@ や docker://foo@ のようなパスワードの無いものは伏せない。
	`https?:(?:\\?/){2}[^/\\\s:@?#]+@`,
	`(?:\\?/){2}[^/\\\s:@]+:[^/\\\s@?#]+@`,
	// 値は空白・句読点で止め、日本語の文の残りまで消さない。
	credentialKeyStart + `(?:password|passwd|secret[_-]?key|private[_-]?key|secret|token|api[_-]?key|access[_-]?key|パスワード|トークン|合言葉)` + credentialKeyEnd + `(?:` + quotedCredentialValue + `|[^\s　,;、。，；！？）」]+)`,
}, "|"))

// headerPattern は値が複数語になる見出し。先に書いた選択肢が優先する。
//  1. 認証方式で始まる値は、改行か非ASCIIの文字まで伏せる。
//  2. 残す文（第1グループ）: 小文字だけの2語か、JSONの true・false・null。短く止めて後ろは読み進める。
//  3. それ以外は引用符の中か、改行か非ASCIIの文字まで伏せる。
//
// 残す文を短く止めるので、全体を1回で走査でき、処理時間は入力の長さに比例する。
var headerPattern = regexp.MustCompile(`(?i)` + credentialKeyStart + `(?:authorization|cookie)` + credentialKeyEnd + `(?:` +
	`(?:bearer|basic|digest|token|negotiate|ntlm|hawk|apikey|oauth|key)[ \t]+[\x21-\x7e][\x20-\x7e]*` +
	`|((?-i:[a-z]{1,19} [a-z]{1,19})(?:[\s,.;)]|$)|(?:true|false|null)(?:[,}\]\r\n]|$))` +
	`|` + quotedCredentialValue + `|[\x21-\x7e][\x20-\x7e]*)`)

func redactHeaders(value string) string {
	var out strings.Builder
	last := 0
	for _, match := range headerPattern.FindAllStringSubmatchIndex(value, -1) {
		out.WriteString(value[last:match[0]])
		if match[2] >= 0 {
			out.WriteString(value[match[0]:match[1]])
		} else {
			out.WriteString("[REDACTED]")
		}
		last = match[1]
	}
	out.WriteString(value[last:])
	return out.String()
}

var privateKeyBegin = regexp.MustCompile(`(?i)-----BEGIN [A-Z ]*PRIVATE KEY-----`)
var privateKeyEnd = regexp.MustCompile(`(?i)-----END [A-Z ]*PRIVATE KEY-----`)

// privateKeyBody は END の無い鍵の本体。Base64の語が続く範囲で止め、後ろの文を消さない。
var privateKeyBody = regexp.MustCompile(`^(?:(?:\s|\\[nr])+[A-Za-z0-9+/=]+)*`)

func redactPrivateKeys(value string) string {
	var out strings.Builder
	hasEnd := true
	for {
		begin := privateKeyBegin.FindStringIndex(value)
		if begin == nil {
			out.WriteString(value)
			return out.String()
		}
		out.WriteString(value[:begin[0]])
		out.WriteString("[REDACTED]")
		value = value[begin[1]:]
		// END が無いと分かったら探し直さない。閉じない鍵の繰り返しで走査が2乗にならない。
		if hasEnd {
			if end := privateKeyEnd.FindStringIndex(value); end != nil {
				value = value[end[1]:]
				continue
			}
			hasEnd = false
		}
		value = value[len(privateKeyBody.FindString(value)):]
	}
}

// schemeCredential は見出しのない Bearer・Basic の値。
var schemeCredential = regexp.MustCompile(`(?i)\b(?:bearer|basic)\s+([A-Za-z0-9._~+/=-]{8,})`)
var englishWord = regexp.MustCompile(`^[A-Z]?[a-z-]*$`)

const longestPlainWord = 19

// redactSchemes は「Bearer authentication required」のような英語の文を残す。
// 長い小文字だけの値は語と見なさず伏せる。
func redactSchemes(value string) string {
	return schemeCredential.ReplaceAllStringFunc(value, func(text string) string {
		fields := strings.Fields(text)
		credential := fields[len(fields)-1]
		if len(credential) <= longestPlainWord && englishWord.MatchString(credential) {
			return text
		}
		return "[REDACTED]"
	})
}

var pathPattern = regexp.MustCompile(`(?:[A-Za-z]:[\\/]|\\\\|/)[^\s）)\]"']*`)

func SafeText(value string) string {
	return pathPattern.ReplaceAllString(Redact(value), "[path]")
}

func Redact(value string) string {
	return redactSchemes(credentialPattern.ReplaceAllString(redactHeaders(redactPrivateKeys(value)), "[REDACTED]"))
}
