package jobs

import "strings"

// IsTestScopePath recognizes explicit test paths using the same conventions as
// the scope file budget. Declarations and audit records retain these paths;
// only collision scheduling ignores them. Unknown directories and * retain
// their production scope.
func IsTestScopePath(path string) bool {
	lower := strings.ToLower(path)
	parts := strings.Split(strings.TrimSuffix(lower, "/"), "/")
	for _, part := range parts[:len(parts)-1] {
		if oneOf(part, "test", "tests", "testdata", "__tests__") {
			return true
		}
	}
	name := parts[len(parts)-1]
	if strings.HasSuffix(path, "/") {
		return oneOf(name, "test", "tests", "testdata", "__tests__")
	}
	return strings.HasSuffix(name, "_test.go") || strings.Contains(name, ".test.") || strings.Contains(name, ".spec.") || strings.HasPrefix(name, "test_") && strings.HasSuffix(name, ".py") || strings.HasSuffix(name, "_test.py")
}
