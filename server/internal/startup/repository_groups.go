package startup

import (
	"log/slog"
	"strings"
	"sync"
)

// disabledRepositoryGroup is a configured group that this intake cannot use
// because some members are not among its repository candidates.
type disabledRepositoryGroup struct {
	Group   []string
	Missing []string
}

// filterRepositoryGroups keeps groups whose members are all candidates, in
// their original order and spelling. The distributed assignments and each
// intake's local configuration are updated separately, so a mismatch disables
// only the affected group instead of stopping the intake.
func filterRepositoryGroups(groups [][]string, candidates []string) ([][]string, []disabledRepositoryGroup) {
	allowed := make(map[string]bool, len(candidates))
	for _, name := range candidates {
		allowed[strings.ToLower(name)] = true
	}
	valid := make([][]string, 0, len(groups))
	var disabled []disabledRepositoryGroup
	for _, group := range groups {
		var missing []string
		for _, name := range group {
			if !allowed[strings.ToLower(name)] {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			disabled = append(disabled, disabledRepositoryGroup{Group: append([]string{}, group...), Missing: missing})
			continue
		}
		valid = append(valid, append([]string{}, group...))
	}
	return valid, disabled
}

// repositoryGroupWarnings records a disabled-group warning once per distinct
// mismatch. It keeps only the latest mismatch, so repeated reads do not fill
// the log and the state does not grow.
type repositoryGroupWarnings struct {
	mu   sync.Mutex
	last string
	// emit replaces the logger in tests.
	emit func(message string)
}

// fallbackGroupWarnings serves Configs built without Load.
var fallbackGroupWarnings = &repositoryGroupWarnings{}

func (c *Config) groupWarningState() *repositoryGroupWarnings {
	if c.groupWarnings != nil {
		return c.groupWarnings
	}
	return fallbackGroupWarnings
}

func (w *repositoryGroupWarnings) report(disabled []disabledRepositoryGroup) {
	key := describeDisabledGroups(disabled)
	w.mu.Lock()
	defer w.mu.Unlock()
	if key == w.last {
		return
	}
	w.last = key
	if key == "" {
		return
	}
	message := "受付候補に無いリポジトリを含む組を無効にしました: " + key
	if w.emit != nil {
		w.emit(message)
		return
	}
	slog.Warn(message)
}

func describeDisabledGroups(disabled []disabledRepositoryGroup) string {
	parts := make([]string, 0, len(disabled))
	for _, d := range disabled {
		parts = append(parts, "組["+strings.Join(d.Group, ", ")+"]（候補に無い: "+strings.Join(d.Missing, ", ")+"）")
	}
	return strings.Join(parts, "、")
}
