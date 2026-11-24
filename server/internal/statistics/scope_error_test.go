package statistics

import (
	"context"
	"testing"
)

func TestScopeReportsMissingPostgreSQLBackend(t *testing.T) {
	result, err := (&Service{}).Scope(context.Background(), Query{})
	if err == nil || err.Error() != "PostgreSQL backend missing" {
		t.Fatalf("Scope error = %v, want PostgreSQL backend missing", err)
	}
	if result != nil {
		t.Fatalf("Scope result = %v, want nil when the backend is missing", result)
	}
}
