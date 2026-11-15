package main

import (
	"bytes"
	"encoding/json"
	"math/big"
	"regexp"
	"strconv"
	"strings"
)

// Only usage objects are buffered. Text and tool payloads are skipped, including in SSE.
type usageMetrics struct {
	cacheUsed                 bool
	cacheRead, cacheWrite     *int64
	disabled                  bool
	skipLine                  bool
	started                   bool
	sse                       bool
	lineStart                 bool
	previousCR                bool
	prefix                    []byte
	input, output             *int64
	cost                      *int64
	stack                     []string
	key                       string
	word                      []byte
	quoted, escape, keyString bool
	capture                   []byte
	captureDepth              int
}

func (m *usageMetrics) feed(data []byte) {
	if !m.started {
		m.started = true
		m.lineStart = true
	}
	for _, b := range data {
		if b == '\n' && m.previousCR {
			m.previousCR = false
			continue
		}
		m.previousCR = b == '\r'
		if b == '\r' {
			b = '\n'
		}
		if m.lineStart && (b == '\r' || b == '\n') {
			if b == '\n' && m.sse {
				// Each SSE event has its own JSON document, including after a broken frame.
				m.disabled, m.quoted, m.escape, m.keyString = false, false, false, false
				m.stack, m.word, m.capture, m.prefix = nil, nil, nil, nil
				m.key, m.captureDepth = "", 0
			}
			continue
		}
		if m.skipLine {
			if b == '\n' {
				m.skipLine = false
				m.lineStart = true
			}
			continue
		}
		if m.lineStart {
			m.prefix = append(m.prefix, b)
			metadata := false
			for _, field := range []string{"event:", "id:", "retry:", ":"} {
				if bytes.HasPrefix([]byte(field), m.prefix) {
					metadata = true
					if len(m.prefix) == len(field) {
						m.prefix = nil
						m.skipLine = true
						m.lineStart = false
					}
					break
				}
			}
			if metadata {
				continue
			}
			if bytes.HasPrefix([]byte("data:"), m.prefix) {
				if len(m.prefix) == 5 {
					m.sse = true
					m.prefix = nil
					m.lineStart = false
				}
				continue
			}
			p := m.prefix
			m.prefix = nil
			m.lineStart = false
			m.scan(p)
		} else {
			m.scan([]byte{b})
		}
		if b == '\n' {
			m.lineStart = true
		}
	}
}

func (m *usageMetrics) scan(data []byte) {
	if m.disabled {
		return
	}
	for _, b := range data {
		if m.captureDepth > 0 && len(m.capture) < 65537 {
			m.capture = append(m.capture, b)
		}
		if m.quoted {
			if m.keyString && len(m.word) < 512 {
				m.word = append(m.word, b)
			}
			if m.escape {
				m.escape = false
				continue
			}
			if b == '\\' {
				m.escape = true
				continue
			}
			if b == '"' {
				m.quoted = false
				if m.keyString {
					var k string
					if json.Unmarshal(m.word, &k) == nil {
						m.key = k
					}
				}
				m.word = nil
			}
			continue
		}
		switch b {
		case '"':
			m.quoted = true
			m.keyString = m.captureDepth == 0 && len(m.stack) > 0 && m.key == ""
			if m.keyString {
				m.word = []byte{'"'}
			}
		case '{', '[':
			if b == '{' && m.key == "usage" && (len(m.stack) == 1 || len(m.stack) == 2 && m.stack[1] == "message") && m.captureDepth == 0 {
				m.capture = []byte{'{'}
				m.captureDepth = len(m.stack) + 1
			}
			if len(m.stack) >= 128 { // malformed or unsupported nesting: stop collecting this response
				m.disabled = true
				m.stack = nil
				m.capture = nil
				m.captureDepth = 0
				return
			}
			m.stack = append(m.stack, m.key)
			m.key = ""
		case '}', ']':
			if m.captureDepth == len(m.stack) && m.captureDepth > 0 {
				if len(m.capture) <= 65536 {
					m.readUsage(m.capture)
				} else {
					m.cacheUsed = true
				}
				m.capture = nil
				m.captureDepth = 0
			}
			if len(m.stack) > 0 {
				m.stack = m.stack[:len(m.stack)-1]
			}
			m.key = ""
		case ',':
			m.key = ""
		}
	}
}

func (m *usageMetrics) readUsage(data []byte) {
	var u map[string]json.RawMessage
	if json.Unmarshal(data, &u) != nil {
		return
	}
	for _, key := range []string{"cache_read_input_tokens", "cache_creation_input_tokens", "cache_creation", "server_tool_use", "prompt_tokens_details", "input_tokens_details"} {
		if raw, ok := u[key]; ok && !zeroUsageDimension(raw) {
			m.cacheUsed = true
		}
	}
	for key, dst := range map[string]**int64{"input_tokens": &m.input, "output_tokens": &m.output, "cache_read_input_tokens": &m.cacheRead, "cache_creation_input_tokens": &m.cacheWrite} {
		v, ok := u[key]
		r := metricNumber(string(v))
		if ok && r != nil && r.IsInt() && r.Num().IsInt64() {
			n := r.Num().Int64()
			if *dst == nil || n > **dst {
				v := n
				*dst = &v
			}
		}
	}
	if v, ok := u["cost"]; ok {
		if n := costNanos(string(v)); n != nil {
			m.cost = n
		}
	}
}

// Empty or all-zero breakdowns do not add a billing dimension.
func zeroUsageDimension(raw json.RawMessage) bool {
	if string(raw) == "null" {
		return true
	}
	if n := metricNumber(string(raw)); n != nil {
		return n.Sign() == 0
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return false
	}
	for _, v := range obj {
		if !zeroUsageDimension(v) {
			return false
		}
	}
	return true
}

func metricNumber(s string) *big.Rat {
	// Bound decimal work, including exponents, before parsing.
	if len(s) > 128 {
		return nil
	}
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		e, err := strconv.Atoi(s[i+1:])
		if err != nil || e > 400 || e < -400 {
			return nil
		}
	}
	if strings.HasPrefix(s, "\"") {
		return nil
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok || r.Sign() < 0 {
		return nil
	}
	return r
}

func costNanos(s string) *int64 {
	// JSON numbers only, rounded once to the nearest nanodollar.
	r := metricNumber(s)
	if r == nil {
		return nil
	}
	r.Mul(r, big.NewRat(1000000000, 1))
	q, rem := new(big.Int), new(big.Int)
	q.QuoRem(r.Num(), r.Denom(), rem)
	if new(big.Int).Lsh(rem, 1).Cmp(r.Denom()) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() {
		return nil
	}
	n := q.Int64()
	if n > maxEventCostNanos {
		return nil
	}
	return &n
}

// A reported cost above 1,000,000 USD per event is not believable; it is stored as unknown, so sums cannot overflow.
const maxEventCostNanos = 1e15

var secretRe = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]+|sk-[A-Za-z0-9_-]{8,}`)

// redactSecrets removes the configured provider key and anything shaped like a bearer token or an sk- key from upstream text.
func redactSecrets(s, key string) string {
	if key != "" {
		s = strings.ReplaceAll(s, key, "[redacted]")
	}
	return secretRe.ReplaceAllString(s, "[redacted]")
}

func spendingLimit(name string, body []byte) bool {
	if !strings.EqualFold(name, "OpenRouter") {
		return false
	}
	var e struct {
		Error struct {
			Code    json.RawMessage `json:"code"`
			Message json.RawMessage `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil {
		return false
	}
	var code, message string
	_ = json.Unmarshal(e.Error.Code, &code)
	_ = json.Unmarshal(e.Error.Message, &message)
	msg := strings.ToLower(message)
	return code == "spending_limit_reached" || code == "spending_limit_exceeded" || strings.Contains(msg, "spending limit reached") || strings.Contains(msg, "spending limit exceeded") || strings.Contains(msg, "key limit exceeded")
}
