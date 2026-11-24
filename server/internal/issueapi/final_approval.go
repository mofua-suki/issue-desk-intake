package issueapi

import (
	"context"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// approveFinal records the user's answer to a pending final approval. The screen sends the answer the user pressed;
// a delegated PC token sends the user's chat answer (the shown question and the choice) with the time the user answered and must name the approval ID,
// so an AI cannot approve on its own judgement or reuse an answer for another approval. The sending route and PC are recorded.
func (s *Service) approveFinal(ctx context.Context, repo string, number int64, data Object, p auth.Principal) (Object, error) {
	statement := fleet.Text(data["statement"])
	via, pc, instructionAt := "screen", "", ""
	if p.Kind == auth.PC {
		via, pc = "token", p.Name
		statement = strings.TrimSpace(statement)
		approvalID := strings.TrimSpace(fleet.Text(data["approvalId"]))
		if !jobs.MeaningfulApproval(statement) || !namesApproval(statement, approvalID, fleet.Text(data["head"])) {
			return nil, fail(400, "利用者の承認の発言（承認IDとhead SHAを含む質問文と回答）を指定してください。")
		}
		at, ok := contract.InstructionAt(data["instructionAt"], s.o.Now())
		if !ok {
			return nil, fail(400, contract.InstructionAtProblem)
		}
		instructionAt = at
	}
	job, err := s.o.Jobs.ByIssue(ctx, repo, number)
	if err != nil {
		return nil, err
	}
	if job == nil {
		return nil, fail(404, "承認対象のジョブがありません。")
	}
	input := jobs.FinalApprovalRecord{ID: fleet.Text(data["approvalId"]), HeadSHA: fleet.Text(data["head"]), MessageID: fleet.Text(data["id"]), Quote: statement, Fingerprint: fleet.Text(data["approvalFingerprint"]), Via: via, PC: pc, InstructionAt: instructionAt}
	saved, err := s.o.Jobs.RecordFinalApproval(ctx, job.ID, input)
	if err != nil {
		return nil, err
	}
	return Object{"jobId": saved.ID, "approvalId": input.ID, "head": input.HeadSHA}, nil
}

// namesApproval reports whether the statement names the approval ID as a whole word and the full head SHA,
// so an answer cannot be reused for another approval (deploy vs deployment_both_sha) or a later head.
func namesApproval(statement, approvalID, head string) bool {
	text, id := norm.NFKC.String(statement), norm.NFKC.String(approvalID)
	if id == "" || !sha.MatchString(head) || !strings.Contains(strings.ToLower(text), head) {
		return false
	}
	// 承認IDは英数字・_・-で書かれるので、その前後が同じ種類の文字なら別のIDの一部とみなす（日本語の文字は区切り）。
	word := func(r rune) bool {
		return r == '_' || r == '-' || r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r))
	}
	for from := 0; ; {
		i := strings.Index(text[from:], id)
		if i < 0 {
			return false
		}
		start, end := from+i, from+i+len(id)
		before, _ := utf8.DecodeLastRuneInString(text[:start])
		after, _ := utf8.DecodeRuneInString(text[end:])
		if (start == 0 || !word(before)) && (end == len(text) || !word(after)) {
			return true
		}
		from = start + 1
	}
}
