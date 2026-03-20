package cw

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// Message is a parsed AWS EventStream message.
type Message struct {
	EventType   string
	ContentType string
	MessageType string
	Payload     map[string]interface{}
}

// parseHeaders parses EventStream binary headers.
func parseHeaders(data []byte) map[string]string {
	headers := make(map[string]string)
	offset := 0
	for offset < len(data) {
		if offset >= len(data) {
			break
		}
		nameLen := int(data[offset])
		offset++
		if offset+nameLen > len(data) {
			break
		}
		name := string(data[offset : offset+nameLen])
		offset += nameLen

		if offset >= len(data) {
			break
		}
		valueType := data[offset]
		offset++

		if valueType == 7 { // string
			if offset+2 > len(data) {
				break
			}
			valueLen := int(binary.BigEndian.Uint16(data[offset : offset+2]))
			offset += 2
			if offset+valueLen > len(data) {
				break
			}
			headers[name] = string(data[offset : offset+valueLen])
			offset += valueLen
		} else {
			break // unknown type, stop
		}
	}
	return headers
}

// parseMessage parses a single complete EventStream message from buf.
func parseMessage(buf []byte) (*Message, error) {
	if len(buf) < 16 {
		return nil, fmt.Errorf("message too short")
	}
	totalLen := binary.BigEndian.Uint32(buf[0:4])
	headersLen := binary.BigEndian.Uint32(buf[4:8])

	if int(totalLen) > len(buf) {
		return nil, fmt.Errorf("incomplete message")
	}

	headers := parseHeaders(buf[12 : 12+headersLen])

	payloadStart := 12 + headersLen
	payloadEnd := totalLen - 4 // minus message CRC
	var payload map[string]interface{}

	payloadBytes := buf[payloadStart:payloadEnd]
	if len(payloadBytes) > 0 {
		if err := json.Unmarshal(payloadBytes, &payload); err != nil {
			// Non-JSON payload
			payload = map[string]interface{}{"raw": string(payloadBytes)}
		}
	}

	return &Message{
		EventType:   headers[":event-type"],
		ContentType: headers[":content-type"],
		MessageType: headers[":message-type"],
		Payload:     payload,
	}, nil
}

// Reader reads EventStream messages from an io.Reader.
type Reader struct {
	r   io.Reader
	buf []byte
}

func NewReader(r io.Reader) *Reader {
	return &Reader{r: r}
}

// Next reads the next complete EventStream message.
// Returns io.EOF when the stream ends.
func (r *Reader) Next() (*Message, error) {
	for {
		// Try to parse from buffer
		if len(r.buf) >= 12 {
			totalLen := int(binary.BigEndian.Uint32(r.buf[0:4]))
			if totalLen >= 16 && len(r.buf) >= totalLen {
				msg, err := parseMessage(r.buf[:totalLen])
				r.buf = r.buf[totalLen:]
				return msg, err
			}
		}

		// Read more data
		chunk := make([]byte, 32*1024)
		n, err := r.r.Read(chunk)
		if n > 0 {
			r.buf = append(r.buf, chunk[:n]...)
		}
		if err != nil {
			if err == io.EOF && len(r.buf) == 0 {
				return nil, io.EOF
			}
			if err == io.EOF {
				// Try to parse remaining buffer
				if len(r.buf) >= 12 {
					totalLen := int(binary.BigEndian.Uint32(r.buf[0:4]))
					if totalLen >= 16 && len(r.buf) >= totalLen {
						msg, parseErr := parseMessage(r.buf[:totalLen])
						r.buf = r.buf[totalLen:]
						return msg, parseErr
					}
				}
				return nil, io.EOF
			}
			return nil, err
		}
	}
}
