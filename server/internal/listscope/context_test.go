package listscope

import (
	"context"
	"testing"
)

func TestIssueIdentityAcrossNumberTypes(t *testing.T) {
	const project = "owner/app"
	jsonNumber, storedNumber := float64(1000000), int64(1000000)
	if IssueKey(project, jsonNumber) != IssueKey(project, storedNumber) {
		t.Fatal("JSON and stored issue identities differ")
	}
	ctx := WithProtection(context.Background(), Protection{Issues: map[string]bool{IssueKey(project, jsonNumber): true}})
	if !Protected(ctx, project, storedNumber, "") || Protected(ctx, "other/app", storedNumber, "") || Protected(ctx, project, storedNumber+1, "") {
		t.Fatal("protection must match only the same repository and issue")
	}
	omitted := map[string]bool{}
	Omit(WithOmitted(ctx, omitted), project, storedNumber)
	if !omitted[IssueKey(project, jsonNumber)] {
		t.Fatal("JSON reference did not match omitted stored issue")
	}
}
