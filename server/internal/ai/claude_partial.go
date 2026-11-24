package ai

import (
	"bytes"
	"encoding/json"
	"time"
)

// claudePartialWriter keeps stream events out of the captured result. A single
// incomplete line is bounded independently of the final result's capacity.
type claudePartialWriter struct {
	out     *capture
	line    []byte
	discard bool
}

func (w *claudePartialWriter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		part := p
		if i >= 0 {
			part = p[:i]
		}
		if !w.discard {
			if len(w.line)+len(part) > 8<<20 {
				w.discard = true
				w.line = nil
				w.out.mu.Lock()
				w.out.overflow = true
				w.out.mu.Unlock()
				w.out.cancel()
			} else {
				w.line = append(w.line, part...)
			}
		}
		if i < 0 {
			break
		}
		if !w.discard {
			w.accept(w.line, true)
		}
		w.line, w.discard = nil, false
		p = p[i+1:]
	}
	return n, nil
}

func (w *claudePartialWriter) flush() {
	if len(w.line) > 0 && !w.discard {
		w.accept(w.line, false)
	}
}

func (w *claudePartialWriter) accept(line []byte, newline bool) {
	var event struct {
		Type  string `json:"type"`
		Event struct {
			Type  string `json:"type"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		} `json:"event"`
	}
	valid := json.Unmarshal(line, &event) == nil
	if len(bytes.TrimSpace(line)) == 0 || bytes.Equal(bytes.TrimSpace(line), []byte("{}")) {
		return
	}
	if valid && (event.Type == "ping" || event.Type == "keep_alive") {
		return
	}
	if valid && event.Type == "stream_event" {
		d := event.Event.Delta
		if event.Event.Type == "content_block_delta" && ((d.Type == "text_delta" && d.Text != "") || (d.Type == "thinking_delta" && d.Thinking != "") || (d.Type == "input_json_delta" && d.PartialJSON != "")) {
			w.out.mu.Lock()
			w.out.last = time.Now()
			select {
			case w.out.activity <- struct{}{}:
			default:
			}
			w.out.mu.Unlock()
		}
		return
	}
	if newline {
		line = append(line, '\n')
	}
	_, _ = w.out.Write(line)
}
