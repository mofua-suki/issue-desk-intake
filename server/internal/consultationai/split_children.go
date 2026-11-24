package consultationai

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"time"

	"issue-desk-intake/server/internal/completionplan"
	"issue-desk-intake/server/internal/contract"
)

// SplitChildren writes stable child IDs before creation. The caller starts each returned thinking child.
func (s *Service) SplitChildren(ctx context.Context, parent Record, parts []string) (Record, error) {
	if len(parts) < 2 || len(parts) > 25 || int(fleet.Number(parent["consultationDepth"])) >= 6 {
		return nil, errors.New("子の相談をこれ以上作れません")
	}
	children, texts, err := childConsultationIDs(parent, parts)
	if err != nil {
		return nil, err
	}
	if err := s.save(ctx, parent, Record{"consultationParts": texts, "consultationChildren": children}); err != nil {
		return nil, err
	}
	for index, rawID := range children {
		id := fleet.Text(rawID)
		if !uuid.MatchString(id) {
			return nil, errors.New("子相談のIDが不正です")
		}
		_, err := s.options.Records.Change(ctx, "drafts", id, func(_ *sql.Tx, current Record) (Record, error) {
			if current != nil {
				if fleet.Text(current["parentConsultation"]) != fleet.Text(parent["id"]) || current["project"] != parent["project"] || current["owner"] != parent["owner"] {
					return nil, errors.New("子相談のIDが別の依頼で使用されています")
				}
				return current, nil
			}
			messageID, err := newID()
			if err != nil {
				return nil, err
			}
			return childConsultation(parent, id, parts[index], messageID, s.options.Now().UTC().Format(time.RFC3339Nano)), nil
		})
		if err != nil {
			return nil, err
		}
	}
	return childrenReply(children, texts, len(parts)), nil
}

// Resume a saved split before reading materials or sending the parent to AI again.
func (s *Service) resumeSplitChildren(ctx context.Context, parent Record) (Record, error) {
	parts := fleet.Strings(parent["consultationParts"])
	children := fleet.Elements(parent["consultationChildren"])
	if s.options.Split == nil || len(parts) < 2 || len(parts) != len(children) {
		return nil, errors.New("保存された子相談の本文とIDを確認できません")
	}
	for _, child := range children {
		if !uuid.MatchString(fleet.Text(child)) {
			return nil, errors.New("保存された子相談のIDが不正です")
		}
	}
	return s.options.Split(ctx, parent, parts)
}

// childrenReply is the answer that hands the consultation over to its children.
func childrenReply(children, texts []any, count int) Record {
	lines := []string{}
	for index, id := range children {
		lines = append(lines, fmt.Sprintf("%d. [%s](#/drafts/%s)", index+1, id, id))
	}
	return Record{"ready": false, "questions": []any{}, "consultationParts": texts, "consultationChildren": children, "reply": fmt.Sprintf("資料の量に合わせて%d件の子の相談に分けました。返信・操作は子の相談で続けます。\n%s", count, strings.Join(lines, "\n"))}
}

// childConsultationIDs keeps the saved child IDs, adds IDs for new parts and validates the
// part texts against the saved split.
func childConsultationIDs(parent Record, parts []string) ([]any, []any, error) {
	children := append([]any{}, fleet.Elements(parent["consultationChildren"])...)
	if prior := fleet.Elements(parent["consultationParts"]); len(prior) > 0 {
		if string(marshal(prior)) != string(marshal(parts)) {
			return nil, nil, errors.New("保存済みの子相談の本文と一致しません")
		}
	}
	for len(children) < len(parts) {
		id, err := newID()
		if err != nil {
			return nil, nil, err
		}
		children = append(children, id)
	}
	if len(children) != len(parts) {
		return nil, nil, errors.New("子相談の件数が保存記録と一致しません")
	}
	texts := []any{}
	for _, part := range parts {
		if strings.TrimSpace(part) == "" || contract.UTF16Length(part) > 8000 {
			return nil, nil, errors.New("子相談の本文は8000文字以内です")
		}
		texts = append(texts, part)
	}
	return children, texts, nil
}

// childConsultation builds a child draft that carries the parent's conversation and settings.
func childConsultation(parent Record, id, part, messageID, at string) Record {
	messages := []any{Record{"id": messageID, "role": "user", "text": part, "at": at}}
	// Keep the original presentation and source statements together. The
	// child request precedes them so it cannot interrupt a saved reply.
	for _, item := range fleet.Elements(parent["messages"]) {
		message := fleet.Object(item)
		if len(fleet.Elements(message["images"])) > 0 {
			copy := copyRecord(message)
			copy["imageScope"] = "共通の添付画像です。対象はこの子の依頼文に限ります。"
			messages = append(messages, copy)
		} else {
			messages = append(messages, item)
		}
	}
	child := Record{"id": id, "parentConsultation": parent["id"], "consultationDepth": int(fleet.Number(parent["consultationDepth"])) + 1, "status": "thinking", "ready": false, "queued": true, "revision": 0, "createdAt": at, "updatedAt": at, "waitStartedAt": at, "waitSource": "accepted", "questions": []any{}, "messages": messages, "title": "", "body": "", "imageSourceId": parent["id"]}
	for _, key := range []string{"project", "provider", "consultationProvider", "owner", "tier", "requestedTier", "requestedImplementationTier", "priority", "prioritySource", "replaceIssues", "completionPlanVersion", "completionPlan", "completionApproval", "completionProposal", "completionComparison", "completionReconfirmation"} {
		if value, exists := parent[key]; exists {
			child[key] = value
		}
	}
	if parent["completionReconfirmationAfter"] != nil {
		child["completionReconfirmationAfter"] = completionplan.PendingAfter(parent["messages"], parent["completionReconfirmationAfter"]) + 1
	}
	if source := fleet.Text(parent["imageSourceId"]); source != "" {
		child["imageSourceId"] = source
	}
	return child
}
