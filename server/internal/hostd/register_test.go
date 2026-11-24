package hostd

import "testing"

func TestClassifyRelease(t *testing.T) {
	cases := []struct {
		name  string
		files []string
		want  string
	}{
		{"versions", []string{"deploy/versions.json"}, "align"},
		{"antigravity", []string{"deploy/antigravity-release.json"}, "align"},
		{"intake", []string{"internal/intake/client.go"}, "align"},
		{"execution config", []string{"internal/execution/config.go"}, "align"},
		{"internal", []string{"internal/execution/worker.go"}, "soon"},
		{"cmd", []string{"cmd/worker/main.go"}, "soon"},
		{"client deployment", []string{"deploy/client/linux-hostd/install.sh"}, "soon"},
		{"go.mod", []string{"go.mod"}, "soon"},
		{"go.sum", []string{"go.sum"}, "soon"},
		{"Dockerfile", []string{"Dockerfile"}, "soon"},
		{"docs", []string{"docs/guide.md"}, "lazy"},
		{"test", []string{"internal/x_test.go"}, "lazy"},
		{"readme", []string{"README.md"}, "lazy"},
		{"excluded align paths", []string{"internal/intake/client_test.go", "internal/intake/README.md", "docs/README.md"}, "lazy"},
		{"align after soon", []string{"internal/execution/worker.go", "internal/execution/config.go"}, "align"},
		{"align before soon", []string{"internal/intake/client.go", "cmd/worker/main.go"}, "align"},
		{"excluded with soon", []string{"internal/intake/client_test.go", "go.mod"}, "soon"},
		{"exact file names", []string{"go.mod.backup", "Dockerfile.old", "internal/execution/config.go.old"}, "soon"},
		{"old paths", []string{"server/src/jobs-foo", "client/src/jobs-client.mjs", "shared/src/desk-contract.mjs", "package.json"}, "lazy"},
		{"empty", nil, "lazy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyRelease(tc.files); got != tc.want {
				t.Errorf("classifyRelease(%q) = %q, want %q", tc.files, got, tc.want)
			}
		})
	}
}
