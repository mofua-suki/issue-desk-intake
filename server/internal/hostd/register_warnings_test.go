package hostd

import (
	"strings"
	"testing"
)

func TestOverrideWarningNamesEveryOverridingHostSorted(t *testing.T) {
	w := overrideWarning("-python", []string{"nas", "a9max", "gt12"})
	if !strings.Contains(w, "a9max・gt12・nas") || !strings.Contains(w, "client-python") || !strings.Contains(w, "優先") {
		t.Fatalf("warning = %q", w)
	}
}

func TestOverrideWarningIsEmptyWithoutOverridingHosts(t *testing.T) {
	if w := overrideWarning("", nil); w != "" {
		t.Fatalf("warning = %q, want empty", w)
	}
}

func TestRegistryChangeWarningOnlyWhenRegistryDiffersFromPrevious(t *testing.T) {
	if w := registryChangeWarning("nas", "", "127.0.0.1:5000"); w != "" {
		t.Fatalf("first registration warned: %q", w)
	}
	if w := registryChangeWarning("nas", "127.0.0.1:5000", "127.0.0.1:5000"); w != "" {
		t.Fatalf("same registry warned: %q", w)
	}
	w := registryChangeWarning("nas", "127.0.0.1:5000", "ugreen-nas.example.ts.net:5443")
	for _, want := range []string{"nas", "127.0.0.1:5000", "ugreen-nas.example.ts.net:5443", "REGISTRY", "再試行できません"} {
		if !strings.Contains(w, want) {
			t.Fatalf("warning %q lacks %q", w, want)
		}
	}
}
