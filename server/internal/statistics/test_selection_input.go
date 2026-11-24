package statistics

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type testChoice struct {
	At                      time.Time
	Key, Repo, Kind, Reason string
	Files, Total            int
	Omitted                 []string
	NotRun                  bool
}
type fullRun struct {
	At                                    time.Time
	Repo, Version, Status                 string
	MS                                    float64
	Failed                                []string
	UnknownFiles                          bool
	Cut                                   int
	Merged, UnknownSelections, NoPrevious int
	Leaks                                 map[string][]string
}
type testFollow struct {
	At                      time.Time
	Key, Repo, Kind, Reason string
	Rechecked, SavedMS      int
}
type testMerge struct {
	At        time.Time
	Key, Repo string
}
type selectionInput struct {
	Choices []*testChoice
	Fulls   []*fullRun
	Follows []testFollow
	Merges  []testMerge
	Kinds   map[*loggedStep]string
}

var choicePattern = regexp.MustCompile(`^テスト選択 ((\S+)#\d+): 選択 (\d+)/(\d+)ファイル（[^）]*）; 省略 \d+ファイル; 基点 \S+; 選択 .*?; 省略 (.*)$`)
var wholePattern = regexp.MustCompile(`^テスト選択 ((\S+)#\d+): 全部(?: (\d+)ファイル)?; 理由 (.*)$`)
var fullPattern = regexp.MustCompile(`^全部のテスト (\S+) (\S+): (合格|失敗|実行できず|中断) (\d+)秒(?:; (.*))?$`)
var failedPattern = regexp.MustCompile(`; 失敗したファイル (?:一部のみ (\d+)/(\d+)件 (.+)|\d+件 (.+)|不明)`)
var mergePattern = regexp.MustCompile(`^マージ完了 ((\S+)#\d+)(?: |$)`)
var followUsedPattern = regexp.MustCompile(`^固定テスト省略 ((\S+)#\d+): 取り込み前の合格を使いました（.*; 一覧で読むテストだけ流した (?:(\d+)ファイル|なし); 見込み (\d+)秒）$`)
var followRefusedPattern = regexp.MustCompile(`^固定テスト省略 ((\S+)#\d+): 取り込み前の合格を使えません: (.*)$`)
var dynamicImport = regexp.MustCompile(`非リテラルの動的 import（[^）]*）\s*`)
var listTests = regexp.MustCompile(`^一覧で読むテスト（[^）]*）`)

func reasonKind(s string, follow bool) string {
	s = dynamicImport.ReplaceAllString(s, "非リテラルの動的 import ")
	if follow {
		s = listTests.ReplaceAllString(s, "一覧で読むテスト")
	}
	if at := strings.Index(s, ": "); at >= 0 {
		s = s[:at]
	}
	s = strings.TrimSpace(s)
	if s == "" {
		s = "理由不明"
	}
	return s
}
func intText(s string) int { n, _ := strconv.Atoi(s); return n }
func selectionParse(lines []logLine, steps []*loggedStep) selectionInput {
	out := selectionInput{Kinds: map[*loggedStep]string{}}
	parseSelectionLines(&out, lines)
	matchSelectionSteps(&out, steps)
	attributeLeaks(&out)
	return out
}

// parseSelectionLines reads the test choices, full runs, merges and reuses from the log.
func parseSelectionLines(out *selectionInput, lines []logLine) {
	for _, line := range lines {
		body := line.Body
		if m := choicePattern.FindStringSubmatch(body); m != nil {
			omitted := []string{}
			if m[5] != "なし" {
				omitted = strings.Split(m[5], ", ")
			}
			out.Choices = append(out.Choices, &testChoice{At: line.At, Key: m[1], Repo: m[2], Kind: "chosen", Files: intText(m[3]), Total: intText(m[4]), Omitted: omitted})
		} else if m := wholePattern.FindStringSubmatch(body); m != nil {
			out.Choices = append(out.Choices, &testChoice{At: line.At, Key: m[1], Repo: m[2], Kind: "whole", Total: intText(m[3]), Reason: m[4]})
		} else if m := fullPattern.FindStringSubmatch(body); m != nil {
			r := &fullRun{At: line.At, Repo: m[1], Version: m[2], Status: map[string]string{"合格": "passed", "失敗": "failed", "実行できず": "unrunnable", "中断": "interrupted"}[m[3]], MS: float64(intText(m[4]) * 1000), Leaks: map[string][]string{}}
			if r.Status == "failed" {
				r.UnknownFiles = true
				if f := failedPattern.FindStringSubmatch(m[5]); f != nil {
					list := f[3]
					if list == "" {
						list = f[4]
					}
					if list != "" {
						r.Failed = strings.Split(list, ", ")
						r.UnknownFiles = false
					}
					if f[1] != "" {
						r.Cut = intText(f[2]) - intText(f[1])
						if r.Cut < 0 {
							r.Cut = 0
						}
					}
				}
			}
			out.Fulls = append(out.Fulls, r)
		} else if m := mergePattern.FindStringSubmatch(body); m != nil {
			out.Merges = append(out.Merges, testMerge{line.At, m[1], m[2]})
		} else if m := followUsedPattern.FindStringSubmatch(body); m != nil {
			kind := "reused"
			if m[3] != "" {
				kind = "rechecked"
			}
			out.Follows = append(out.Follows, testFollow{At: line.At, Key: m[1], Repo: m[2], Kind: kind, Rechecked: intText(m[3]), SavedMS: intText(m[4]) * 1000})
		} else if m := followRefusedPattern.FindStringSubmatch(body); m != nil {
			out.Follows = append(out.Follows, testFollow{At: line.At, Key: m[1], Repo: m[2], Kind: "notReused", Reason: m[3]})
		}
	}
}

// matchSelectionSteps pairs each choice with the fixed test step that ran it,
// and marks the choices that never ran.
func matchSelectionSteps(out *selectionInput, steps []*loggedStep) {
	type mark struct {
		At     time.Time
		Order  int
		Choice *testChoice
		Span   *loggedStep
		Abort  bool
	}
	marks := []mark{}
	for _, c := range out.Choices {
		marks = append(marks, mark{At: c.At, Choice: c})
	}
	for _, step := range steps {
		if step.Label == "固定テスト" && step.Logged {
			marks = append(marks, mark{At: step.Start, Order: 2, Span: step})
		}
		if step.Label == "テストの順番待ち" && step.Outcome == "中断" {
			marks = append(marks, mark{At: step.End, Order: 1, Span: step, Abort: true})
		}
	}
	sort.SliceStable(marks, func(i, j int) bool {
		if marks[i].At.Equal(marks[j].At) {
			return marks[i].Order < marks[j].Order
		}
		return marks[i].At.Before(marks[j].At)
	})
	pending := map[string]*testChoice{}
	for _, m := range marks {
		if m.Choice != nil {
			if before := pending[m.Choice.Key]; before != nil {
				before.NotRun = true
			}
			pending[m.Choice.Key] = m.Choice
		} else if m.Abort {
			if before := pending[m.Span.Key]; before != nil && !m.Span.Start.Before(before.At) {
				before.NotRun = true
				delete(pending, m.Span.Key)
			}
		} else {
			if before := pending[m.Span.Key]; before != nil {
				out.Kinds[m.Span] = before.Kind
			}
			delete(pending, m.Span.Key)
		}
	}
}

// attributeLeaks links each failed full run's failing files to the merges since
// the previous run whose selection omitted them.
func attributeLeaks(out *selectionInput) {
	previous := map[string]time.Time{}
	for _, run := range out.Fulls {
		if run.Status != "passed" && run.Status != "failed" {
			continue
		}
		start := run.At.Add(-time.Duration(run.MS) * time.Millisecond)
		from, hasPrevious := previous[run.Repo]
		previous[run.Repo] = start
		if run.Status != "failed" {
			continue
		}
		if !hasPrevious {
			run.NoPrevious = 1
		}
		merged := map[string]testMerge{}
		for _, m := range out.Merges {
			if m.Repo == run.Repo && !m.At.After(start) && (!hasPrevious || m.At.After(from)) {
				merged[m.Key] = m
			}
		}
		run.Merged = len(merged)
		omitted := map[string][]string{}
		for key, m := range merged {
			var latest *testChoice
			for _, c := range out.Choices {
				if c.Key == key && !c.At.After(m.At) {
					latest = c
				}
			}
			if latest == nil {
				run.UnknownSelections++
			} else if latest.Kind == "chosen" {
				for _, file := range latest.Omitted {
					omitted[file] = append(omitted[file], key)
				}
			}
		}
		for _, file := range run.Failed {
			if keys := omitted[file]; len(keys) > 0 {
				sort.Strings(keys)
				run.Leaks[file] = keys
			}
		}
	}
}
