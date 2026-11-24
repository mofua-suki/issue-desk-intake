package filing

import "strings"

// markdownFence mirrors the shared CLI's top-level fenced-code recognition.
type markdownFence struct {
	char   byte
	length int
}

type markdownLine struct {
	text   string
	fenced bool
}

func markdownLines(text string) []markdownLine {
	var fence markdownFence
	lines := strings.Split(text, "\n")
	rows := make([]markdownLine, 0, len(lines))
	for _, line := range lines {
		rows = append(rows, markdownLine{text: line, fenced: fence.contains(line)})
	}
	return rows
}

func (f *markdownFence) contains(line string) bool {
	line = strings.TrimSuffix(line, "\r")
	indent := len(line) - len(strings.TrimLeft(line, " "))
	if indent <= 3 {
		s := line[indent:]
		if len(s) > 0 && (s[0] == '`' || s[0] == '~') {
			n := 0
			for n < len(s) && s[n] == s[0] {
				n++
			}
			if n >= 3 {
				if f.char != 0 {
					if s[0] == f.char && n >= f.length && strings.Trim(s[n:], " \t") == "" {
						f.char = 0
						f.length = 0
					}
					return true
				}
				if s[0] != '`' || !strings.Contains(s[n:], "`") {
					f.char = s[0]
					f.length = n
					return true
				}
			}
		}
	}
	return f.char != 0
}
