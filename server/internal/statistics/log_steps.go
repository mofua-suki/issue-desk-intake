package statistics

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

type loggedStep struct {
	Key, Repo, Label, Outcome string
	Start, End                time.Time
	Logged                    bool
	Parent                    *loggedStep
}

var stepPattern = regexp.MustCompile(`^工程(開始|終了|中断) ((\S+)#\d+) (.+?)(?: (\d+)秒)?$`)

func logSteps(lines []logLine, now time.Time, alive bool) []*loggedStep {
	out := []*loggedStep{}
	stacks := map[string][]*loggedStep{}
	lose := func(stack []*loggedStep) {
		for _, s := range stack {
			s.Outcome = "unknown"
		}
	}
	for _, line := range lines {
		if strings.HasPrefix(line.Body, "起動 ") {
			for _, stack := range stacks {
				lose(stack)
			}
			stacks = map[string][]*loggedStep{}
			continue
		}
		m := stepPattern.FindStringSubmatch(line.Body)
		if m == nil || (m[1] == "開始") != (m[5] == "") {
			continue
		}
		key, repo, label := m[2], m[3], m[4]
		stack := stacks[key]
		same := -1
		for i, s := range stack {
			if s.Label == label {
				same = i
			}
		}
		if m[1] == "開始" {
			if same >= 0 {
				lose(stack[same:])
				stack = stack[:same]
			}
			s := &loggedStep{Key: key, Repo: repo, Label: label, Start: line.At, Logged: true}
			if len(stack) > 0 {
				s.Parent = stack[len(stack)-1]
			}
			out = append(out, s)
			stacks[key] = append(stack, s)
		} else if same >= 0 {
			s := stack[same]
			lose(stack[same+1:])
			s.End, s.Outcome = line.At, m[1]
			stacks[key] = stack[:same]
		} else {
			seconds, _ := strconv.Atoi(m[5])
			out = append(out, &loggedStep{Key: key, Repo: repo, Label: label, Start: line.At.Add(-time.Duration(seconds) * time.Second), End: line.At, Outcome: m[1], Logged: false})
		}
	}
	for _, stack := range stacks {
		for _, s := range stack {
			if alive {
				s.Outcome = "running"
				s.End = now
			} else {
				s.Outcome = "unknown"
			}
		}
	}
	return out
}
func stepMeasured(s *loggedStep) bool {
	return s.Logged && (s.Outcome == "終了" || s.Outcome == "中断") && !s.End.Before(s.Start)
}
func stepPlaced(s *loggedStep) time.Time {
	if s.Logged {
		return s.Start
	}
	return s.End
}
func stepNested(s *loggedStep, labels []string) bool {
	for parent := s.Parent; parent != nil; parent = parent.Parent {
		if oneOf(parent.Label, labels) {
			return true
		}
	}
	return false
}
