package thinking

import "strings"

type SegmentType int

const (
	SegmentThinking SegmentType = iota
	SegmentText
)

type Segment struct {
	Type SegmentType
	Text string
}

type parseState int

const (
	stateInitial parseState = iota
	stateInThinking
	stateAfterThinking
	statePassthrough
)

const (
	openTag  = "<thinking>"
	closeTag = "</thinking>"
)

var quoteChars = []byte{'`', '"', '\''}

// Parser is an incremental streaming parser for <thinking>...</thinking> tags.
type Parser struct {
	state             parseState
	buf               strings.Builder
	thinkingExtracted bool
	stripLeadingNL    bool
}

func NewParser() *Parser {
	return &Parser{state: stateInitial}
}

func (p *Parser) IsThinkingMode() bool {
	return p.state == stateInThinking || p.state == stateAfterThinking
}

func (p *Parser) HasExtractedThinking() bool {
	return p.thinkingExtracted
}

// Push feeds incoming text and returns parsed segments.
func (p *Parser) Push(incoming string) []Segment {
	p.buf.WriteString(incoming)
	var segments []Segment

	for {
		seg, cont := p.step()
		if seg != nil {
			if seg.Text != "" {
				segments = append(segments, *seg)
			}
		}
		if !cont {
			break
		}
	}
	return segments
}

// Flush should be called at stream end to emit any remaining buffered content.
func (p *Parser) Flush() []Segment {
	var segments []Segment
	text := p.buf.String()
	p.buf.Reset()

	switch p.state {
	case stateInitial:
		text = strings.TrimLeft(text, " \t\n\r")
		if text != "" {
			segments = append(segments, Segment{Type: SegmentText, Text: text})
		}
	case stateInThinking:
		if text != "" {
			segments = append(segments, Segment{Type: SegmentThinking, Text: text})
		}
		p.thinkingExtracted = true
	case stateAfterThinking:
		text = stripLeadingNewlines(text)
		if text != "" {
			segments = append(segments, Segment{Type: SegmentText, Text: text})
		}
	case statePassthrough:
		if text != "" {
			segments = append(segments, Segment{Type: SegmentText, Text: text})
		}
	}
	return segments
}

func (p *Parser) step() (*Segment, bool) {
	switch p.state {
	case stateInitial:
		return p.handleInitial()
	case stateInThinking:
		return p.handleInThinking()
	case stateAfterThinking:
		return p.handleAfterThinking()
	case statePassthrough:
		return p.handlePassthrough()
	}
	return nil, false
}

func (p *Parser) handleInitial() (*Segment, bool) {
	text := p.buf.String()
	trimmed := strings.TrimLeft(text, " \t\n\r")

	if strings.HasPrefix(trimmed, openTag) {
		// Consume the open tag
		idx := strings.Index(text, openTag)
		rest := text[idx+len(openTag):]
		p.buf.Reset()
		p.buf.WriteString(rest)
		p.state = stateInThinking
		return nil, true
	}

	// Check if buffer could still become <thinking> with more data
	if len(trimmed) < len(openTag) && strings.HasPrefix(openTag, trimmed) {
		return nil, false // need more data
	}

	// Not a thinking response
	p.buf.Reset()
	p.buf.WriteString(trimmed)
	p.state = statePassthrough
	return nil, true
}

func (p *Parser) handleInThinking() (*Segment, bool) {
	text := p.buf.String()
	pos := p.findRealCloseTag(text)
	if pos < 0 {
		// Keep potential tag fragment in buffer
		fragLen := len(closeTag) - 1
		if len(text) <= fragLen {
			return nil, false
		}
		safe := text[:len(text)-fragLen]
		p.buf.Reset()
		p.buf.WriteString(text[len(text)-fragLen:])
		return &Segment{Type: SegmentThinking, Text: safe}, false
	}

	thinkingText := text[:pos]
	rest := text[pos+len(closeTag):]
	// Strip one trailing newline after close tag
	rest = strings.TrimLeft(rest, "\n\r")
	p.buf.Reset()
	p.buf.WriteString(rest)
	p.state = stateAfterThinking
	p.thinkingExtracted = true
	p.stripLeadingNL = true

	if thinkingText != "" {
		return &Segment{Type: SegmentThinking, Text: thinkingText}, true
	}
	return nil, true
}

func (p *Parser) handleAfterThinking() (*Segment, bool) {
	text := p.buf.String()
	if text == "" {
		return nil, false
	}
	if p.stripLeadingNL {
		text = stripLeadingNewlines(text)
		p.stripLeadingNL = false
		if text == "" {
			p.buf.Reset()
			return nil, false
		}
	}
	p.buf.Reset()
	return &Segment{Type: SegmentText, Text: text}, false
}

func (p *Parser) handlePassthrough() (*Segment, bool) {
	text := p.buf.String()
	if text == "" {
		return nil, false
	}
	p.buf.Reset()
	return &Segment{Type: SegmentText, Text: text}, false
}

// findRealCloseTag finds the position of a real </thinking> tag,
// skipping quoted/code-block occurrences.
func (p *Parser) findRealCloseTag(text string) int {
	searchFrom := 0
	for {
		idx := strings.Index(text[searchFrom:], closeTag)
		if idx < 0 {
			return -1
		}
		absPos := searchFrom + idx
		if !p.isQuotedTag(text, absPos) {
			return absPos
		}
		searchFrom = absPos + len(closeTag)
	}
}

func (p *Parser) isQuotedTag(text string, tagPos int) bool {
	if tagPos == 0 {
		return false
	}
	prev := text[tagPos-1]
	for _, q := range quoteChars {
		if prev == q {
			return true
		}
	}
	// Count backticks before tag position
	backticks := 0
	for i := tagPos - 1; i >= 0; i-- {
		if text[i] == '`' {
			backticks++
		} else {
			break
		}
	}
	return backticks%2 == 1
}

func stripLeadingNewlines(s string) string {
	i := 0
	for i < len(s) && (s[i] == '\n' || s[i] == '\r') {
		i++
	}
	return s[i:]
}
