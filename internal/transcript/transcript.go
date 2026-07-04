// Package transcript reads model/token-usage information out of a Claude
// Code session transcript file (the JSONL Claude Code itself writes to
// transcript_path, which every hook payload includes). We only ever need
// the most recent assistant message, so we read backwards from the end of
// the file in growing chunks instead of parsing the whole transcript.
package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"time"
)

type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

type LatestAssistantMessage struct {
	Model string `json:"model"`
	Usage Usage  `json:"usage"`
	Text  string // concatenated text blocks of the message (the visible reply)
	// StopReason of the most recent assistant message. "tool_use" means the
	// turn's final reply hasn't been written yet (the transcript ends on a
	// message that's about to call a tool); a terminal reason (end_turn,
	// stop_sequence, max_tokens) means the final reply is present. Callers
	// closing a turn use this to poll through the write race (see the hook).
	StopReason string
}

type transcriptLine struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Message   struct {
		Model      string          `json:"model"`
		Usage      Usage           `json:"usage"`
		StopReason string          `json:"stop_reason"`
		Content    json.RawMessage `json:"content"`
	} `json:"message"`
}

// Message is one assistant text block from the transcript, tagged with the
// timestamp Claude Code recorded for the message that produced it.
type Message struct {
	Time time.Time
	Text string
}

// AssistantMessages returns every assistant text block whose recorded timestamp
// falls within (since, until], in chronological order. This recovers a turn's
// intermediate "narration" — the text Claude emits between tool calls — which
// lives only in the transcript, never in a hook event. It reads the whole file
// forward (one turn's worth of work is bounded; the session transcript is not,
// so this is O(file) per call — acceptable for a local, single-user tool).
func AssistantMessages(path string, since, until time.Time) ([]Message, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []Message
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxChunk) // transcript lines can be large
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var tl transcriptLine
		if err := json.Unmarshal(line, &tl); err != nil {
			continue
		}
		if tl.Type != "assistant" || tl.Timestamp == "" {
			continue
		}
		ts, err := time.Parse(time.RFC3339Nano, tl.Timestamp)
		if err != nil {
			continue
		}
		if !ts.After(since) || ts.After(until) {
			continue
		}
		text := extractText(tl.Message.Content)
		if strings.TrimSpace(text) == "" {
			continue
		}
		out = append(out, Message{Time: ts, Text: text})
	}
	return out, sc.Err()
}

// extractText pulls the visible reply out of an assistant message's content,
// which is normally an array of typed blocks ([{type:"text",text:...}, ...])
// but can be a bare string. Non-text blocks (tool_use, thinking) are skipped.
func extractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var b strings.Builder
		for _, bl := range blocks {
			if bl.Type == "text" && bl.Text != "" {
				if b.Len() > 0 {
					b.WriteString("\n\n")
				}
				b.WriteString(bl.Text)
			}
		}
		return b.String()
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return ""
}

var ErrNotFound = errors.New("transcript: no assistant message with usage found")

const (
	initialChunk = 64 * 1024
	maxChunk     = 4 * 1024 * 1024
)

// LatestUsage scans backwards through path for the most recent JSONL line
// with type "assistant" that carries usage info, and returns its model and
// token counts.
func LatestUsage(path string) (*LatestAssistantMessage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}

	for chunkSize := int64(initialChunk); ; chunkSize *= 4 {
		readFrom := max(size-chunkSize, 0)
		if _, err := f.Seek(readFrom, io.SeekStart); err != nil {
			return nil, err
		}
		buf := make([]byte, size-readFrom)
		if _, err := io.ReadFull(f, buf); err != nil {
			return nil, err
		}

		if msg := scanLinesReverse(buf, readFrom > 0); msg != nil {
			return msg, nil
		}

		if readFrom == 0 || chunkSize >= maxChunk {
			return nil, ErrNotFound
		}
	}
}

// scanLinesReverse walks buf's lines from the end. It takes model/usage from
// the most recent assistant message (the turn's context/token snapshot), but
// the visible reply text from the most recent assistant message that actually
// has text — these are usually the same message, but a turn that ends on a
// tool_use has usage without text on its last line, so the text is one (or
// more) messages earlier. Returns nil until a usage-bearing message is found;
// keeps scanning for text after that, so the caller's growing-chunk loop can
// reach text that lies further back than the usage line.
// If droppedFirst is true, the first line in buf is partial and is skipped.
func scanLinesReverse(buf []byte, droppedFirst bool) *LatestAssistantMessage {
	lines := bytes.Split(buf, []byte("\n"))
	start := 0
	if droppedFirst {
		start = 1
	}
	var result *LatestAssistantMessage
	for i := len(lines) - 1; i >= start; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		var tl transcriptLine
		if err := json.NewDecoder(bufio.NewReader(bytes.NewReader(line))).Decode(&tl); err != nil {
			continue
		}
		if tl.Type != "assistant" {
			continue
		}
		hasUsage := tl.Message.Model != "" || tl.Message.Usage.InputTokens != 0 || tl.Message.Usage.OutputTokens != 0
		text := extractText(tl.Message.Content)

		if result == nil {
			if !hasUsage {
				continue
			}
			result = &LatestAssistantMessage{
				Model:      tl.Message.Model,
				Usage:      tl.Message.Usage,
				Text:       text,
				StopReason: tl.Message.StopReason,
			}
		} else if result.Text == "" && text != "" {
			result.Text = text
		}
		if result != nil && result.Text != "" {
			return result
		}
	}
	// Found usage but no text within this chunk; the growing-chunk loop will
	// re-scan a larger window. Returning result here (with empty text) would
	// stop that search, so only return once text is filled — unless this was
	// already the whole file (droppedFirst == false), in which case there is
	// no more to read and usage-without-text is the final answer.
	if result != nil && !droppedFirst {
		return result
	}
	return nil
}
