package adminapi

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Whole-table exclusion also blocks *, aliases, row values and JSON payloads.
// ssh_tickets is intentionally excluded in full: filtering requested_token alone
// cannot make a lexical guard safe for whole-row projections. Settings holds
// sessions/CSRF and host credentials; temporary_code_records holds issued codes.
// Keep all credential exclusions here, including names hidden inside JSON.
const dbQueryDeniedTables = `settings host_image_tokens ssh_tickets api_tokens api_token_grants cli_keys temporary_code_records`
const dbQueryDeniedColumns = `requested_token hash password secret credential csrf session`

const dbQueryFunctions = `count sum min max avg abs ceil ceiling floor round lower upper length char_length octet_length substr substring trim ltrim rtrim btrim coalesce nullif greatest least date_trunc date_part extract now age generate_series repeat left right replace concat concat_ws split_part jsonb_typeof json_typeof jsonb_array_length json_array_length jsonb_extract_path_text json_extract_path_text`
const dbQueryForbiddenWords = `insert update delete merge create alter drop truncate copy into for grant revoke call do execute prepare deallocate vacuum analyze refresh reindex cluster comment security lock set reset discard listen unlisten notify load import export table tablesample collate operator cast`

// Only reserved grammar words (or EXISTS, which cannot name a function/type).
// Unreserved words such as OVER/FILTER must not bypass the function allowlist.
const dbQueryParenSyntax = `as in exists not and or where on from select having`

// Only fully reserved words may precede a literal without punctuation. Other
// words could be user-defined type names and invoke an input function.
const dbQueryBeforeString = `select distinct all then else when and or not where on having case`

func dbQueryListed(list, name string) bool {
	for _, item := range strings.Fields(list) {
		if item == name {
			return true
		}
	}
	return false
}

func dbQueryCredentialName(name string) bool {
	name = strings.ToLower(name)
	for _, part := range []string{"token", "hash", "secret", "key", "password", "credential", "csrf", "session"} {
		if strings.Contains(name, part) {
			return true
		}
	}
	return false
}

type dbQueryToken struct {
	raw, word string
	kind      byte // w: identifier/keyword, s: string, n: number, p: punctuation
	quoted    bool
}

// guardDBQuery is a pure, conservative gate, not a PostgreSQL parser or an
// execution permission. The caller must still enforce grants, a read-only
// transaction, pg_catalog-first search_path, result types/names and limits.
// It returns SQL with comments and the optional final semicolon removed, so it
// can safely be wrapped in a subquery. Only plain strings and ASCII identifiers
// are supported. Casts, custom operators, qualified functions and ambiguous
// qualifiers (including alias.column) are deliberately rejected. Qualified
// references must use public (shared) or issue_desk_app_migration (app).
func guardDBQuery(target, query string) (string, error) {
	schema := ""
	switch target {
	case "shared":
		schema = "public"
	case "app":
		schema = "issue_desk_app_migration"
	default:
		return "", fmt.Errorf("DB照会の対象が不正です")
	}
	tokens, err := lexDBQuery(query)
	if err != nil {
		return "", err
	}
	if len(tokens) > 0 && tokens[len(tokens)-1].raw == ";" {
		tokens = tokens[:len(tokens)-1]
	}
	if len(tokens) == 0 || tokens[0].quoted || (tokens[0].word != "select" && tokens[0].word != "with") {
		return "", fmt.Errorf("SELECTまたはWITHから始まる1文だけを許可します")
	}
	depth := 0
	for i, tok := range tokens {
		if tok.raw == ";" {
			return "", fmt.Errorf("複数の文は許可しません")
		}
		switch tok.raw {
		case "(":
			depth++
		case ")":
			depth--
			if depth < 0 {
				return "", fmt.Errorf("括弧が対応していません")
			}
		case ".":
			if !dbQueryQualifierAllowed(tokens, i, schema) {
				return "", fmt.Errorf("対象外または判別できない修飾名です")
			}
		}
		if err := checkDBQueryLiteral(tokens, i); err != nil {
			return "", err
		}
		if tok.kind == 'w' {
			if err := checkDBQueryWord(tokens, i); err != nil {
				return "", err
			}
		}
	}
	if depth != 0 {
		return "", fmt.Errorf("括弧が対応していません")
	}
	out := make([]string, len(tokens))
	for i, tok := range tokens {
		out[i] = tok.raw
	}
	return strings.Join(out, " "), nil
}

// dbQueryQualifierAllowed は tokens[i] の "." が対象のスキーマで1段だけ修飾した名前かを判定する。
// This intentionally rejects table/alias qualifiers rather than guessing
// whether they denote a different schema or a row value.
func dbQueryQualifierAllowed(tokens []dbQueryToken, i int, schema string) bool {
	return !(i == 0 || i+1 >= len(tokens) || (tokens[i-1].word != schema || (tokens[i-1].quoted && tokens[i-1].raw != `"`+schema+`"`)) || tokens[i+1].kind != 'w' ||
		(i > 1 && tokens[i-2].raw == ".") || (i+2 < len(tokens) && (tokens[i+2].raw == "." || tokens[i+2].raw == "(")))
}

// checkDBQueryLiteral は文字列の字句が型付き・接頭辞付き・連続の形でないかを検査する。
func checkDBQueryLiteral(tokens []dbQueryToken, i int) error {
	if tokens[i].kind != 's' || i == 0 {
		return nil
	}
	prev := tokens[i-1]
	if prev.kind == 'w' && (prev.quoted || !dbQueryListed(dbQueryBeforeString, prev.word)) {
		return fmt.Errorf("型付き・接頭辞付き文字列は許可しません")
	}
	if prev.kind == 's' {
		return fmt.Errorf("連続する文字列は許可しません")
	}
	return nil
}

// checkDBQueryWord は語の字句がシステムの識別子・資格情報の表と列・禁止語・許可一覧にない関数でないかを検査する。
func checkDBQueryWord(tokens []dbQueryToken, i int) error {
	tok := tokens[i]
	if strings.HasPrefix(tok.word, "pg_") || tok.word == "information_schema" {
		return fmt.Errorf("システムの識別子は許可しません")
	}
	if dbQueryListed(dbQueryDeniedTables, tok.word) || dbQueryListed(dbQueryDeniedColumns, tok.word) {
		return fmt.Errorf("資格情報の表・列は許可しません")
	}
	if !tok.quoted && dbQueryListed(dbQueryForbiddenWords, tok.word) {
		return fmt.Errorf("禁止されたSQLの語です: %s", tok.word)
	}
	if i+1 < len(tokens) && tokens[i+1].raw == "(" {
		if tok.quoted || (!dbQueryListed(dbQueryFunctions, tok.word) && !dbQueryListed(dbQueryParenSyntax, tok.word)) {
			return fmt.Errorf("許可一覧にない関数または構文です: %s", tok.word)
		}
	}
	return nil
}

func dbQueryLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' }
func dbQueryDigit(c byte) bool  { return c >= '0' && c <= '9' }

func lexDBQuery(query string) ([]dbQueryToken, error) {
	if len(query) > 65536 || !utf8.ValidString(query) || strings.IndexByte(query, 0) >= 0 {
		return nil, fmt.Errorf("SQLの長さまたは文字コードが不正です")
	}
	var tokens []dbQueryToken
	for i := 0; i < len(query); {
		c, start := query[i], i
		if strings.ContainsRune(" \t\r\n\f", rune(c)) {
			i++
			continue
		}
		if strings.HasPrefix(query[i:], "--") {
			for i < len(query) && query[i] != '\n' && query[i] != '\r' {
				i++
			}
			continue
		}
		if strings.HasPrefix(query[i:], "/*") {
			i += 2
			depth := 1
			for i < len(query) && depth > 0 {
				switch {
				case strings.HasPrefix(query[i:], "/*"):
					depth++
					i += 2
				case strings.HasPrefix(query[i:], "*/"):
					depth--
					i += 2
				default:
					i++
				}
			}
			if depth != 0 {
				return nil, fmt.Errorf("コメントが閉じていません")
			}
			continue
		}
		tok := dbQueryToken{kind: 'p'}
		switch {
		case c == '\'':
			i++
			closed := false
			for i < len(query) {
				if query[i] == '\\' {
					return nil, fmt.Errorf("バックスラッシュを含む文字列は許可しません")
				}
				if query[i] == '\'' {
					i++
					if i < len(query) && query[i] == '\'' {
						i++
						continue
					}
					closed = true
					break
				}
				i++
			}
			if !closed {
				return nil, fmt.Errorf("文字列が閉じていません")
			}
			tok.kind = 's'
		case c == '"':
			i++
			name := i
			for i < len(query) && (dbQueryLetter(query[i]) || dbQueryDigit(query[i])) {
				i++
			}
			if i == name || i-name > 63 || i == len(query) || query[i] != '"' {
				return nil, fmt.Errorf("判別できない引用識別子です")
			}
			tok.kind, tok.quoted, tok.word = 'w', true, strings.ToLower(query[name:i])
			i++
		case dbQueryLetter(c):
			i++
			for i < len(query) && (dbQueryLetter(query[i]) || dbQueryDigit(query[i])) {
				i++
			}
			if i-start > 63 {
				return nil, fmt.Errorf("識別子が長すぎます")
			}
			tok.kind, tok.word = 'w', strings.ToLower(query[start:i])
		case dbQueryDigit(c):
			for i < len(query) && dbQueryDigit(query[i]) {
				i++
			}
			if i < len(query) && query[i] == '.' {
				i++
				for i < len(query) && dbQueryDigit(query[i]) {
					i++
				}
			}
			if i < len(query) && (query[i] == 'e' || query[i] == 'E') {
				i++
				if i < len(query) && (query[i] == '+' || query[i] == '-') {
					i++
				}
				digits := i
				for i < len(query) && dbQueryDigit(query[i]) {
					i++
				}
				if i == digits {
					return nil, fmt.Errorf("数値が不正です")
				}
			}
			if i < len(query) && (dbQueryLetter(query[i]) || query[i] == '$') {
				return nil, fmt.Errorf("数値の後ろに判別できない字句があります")
			}
			tok.kind = 'n'
		case strings.ContainsRune("(),.;", rune(c)):
			i++
		case strings.ContainsRune("+-*/%=<>!~@#^&|?", rune(c)):
			i++
			for i < len(query) && strings.ContainsRune("+-*/%=<>!~@#^&|?", rune(query[i])) &&
				!strings.HasPrefix(query[i:], "--") && !strings.HasPrefix(query[i:], "/*") {
				i++
			}
			if !dbQueryListed(`+ - * / % = < > <= >= <> != || -> ->>`, query[start:i]) {
				return nil, fmt.Errorf("許可一覧にない演算子です")
			}
		default:
			return nil, fmt.Errorf("判別できない字句があります")
		}
		tok.raw = query[start:i]
		tokens = append(tokens, tok)
	}
	return tokens, nil
}
