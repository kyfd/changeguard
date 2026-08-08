package agentgateway

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type AuditRecord struct {
	Sequence           uint64    `json:"sequence"`
	Timestamp          time.Time `json:"timestamp"`
	Operation          string    `json:"operation"`
	ChangeID           string    `json:"change_id,omitempty"`
	PrincipalHash      string    `json:"principal_hash,omitempty"`
	RequestSHA256      string    `json:"request_sha256,omitempty"`
	HTTPStatus         int       `json:"http_status"`
	Outcome            string    `json:"outcome"`
	DurationMS         int64     `json:"duration_ms"`
	TraceID            string    `json:"trace_id,omitempty"`
	Provider           string    `json:"provider,omitempty"`
	Model              string    `json:"model,omitempty"`
	Risk               string    `json:"risk,omitempty"`
	ToolCalls          int       `json:"tool_calls,omitempty"`
	InjectionSuspected bool      `json:"injection_suspected,omitempty"`
	PrevHash           string    `json:"prev_hash,omitempty"`
	Hash               string    `json:"hash"`
}

type AuditState struct {
	Verified       bool   `json:"verified"`
	Events         uint64 `json:"events"`
	FileBytes      int64  `json:"file_bytes"`
	LastHashPrefix string `json:"last_hash_prefix,omitempty"`
}

// AuditEvent is the deliberately sanitized representation exposed to
// authorized operators. Principal/request hashes and chain MACs never leave
// the gateway process through the runtime API.
type AuditEvent struct {
	Sequence           uint64    `json:"sequence"`
	Timestamp          time.Time `json:"timestamp"`
	Operation          string    `json:"operation"`
	ChangeID           string    `json:"change_id,omitempty"`
	HTTPStatus         int       `json:"http_status"`
	Outcome            string    `json:"outcome"`
	DurationMS         int64     `json:"duration_ms"`
	TraceID            string    `json:"trace_id,omitempty"`
	Provider           string    `json:"provider,omitempty"`
	Model              string    `json:"model,omitempty"`
	Risk               string    `json:"risk,omitempty"`
	ToolCalls          int       `json:"tool_calls,omitempty"`
	InjectionSuspected bool      `json:"injection_suspected,omitempty"`
}

type AuditEventsPage struct {
	Events    []AuditEvent `json:"events"`
	Total     uint64       `json:"total"`
	Truncated bool         `json:"truncated"`
	Verified  bool         `json:"verified"`
}

type AuditLog struct {
	mu       sync.Mutex
	file     *os.File
	key      []byte
	sequence uint64
	prevHash string
	verified bool
}

func OpenAuditLog(path string, key []byte) (*AuditLog, error) {
	if len(key) < 32 {
		return nil, errors.New("audit key must contain at least 32 bytes")
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return nil, fmt.Errorf("create audit directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o640)
	if err != nil {
		return nil, fmt.Errorf("open audit file: %w", err)
	}
	if err := file.Chmod(0o640); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("secure audit file: %w", err)
	}
	log := &AuditLog{file: file, key: append([]byte(nil), key...), verified: true}
	if err := log.verifyExisting(); err != nil {
		_ = file.Close()
		return nil, err
	}
	if _, err := file.Seek(0, 2); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("seek audit file: %w", err)
	}
	return log, nil
}

func (l *AuditLog) verifyExisting() error {
	if _, err := l.file.Seek(0, 0); err != nil {
		return fmt.Errorf("seek audit file: %w", err)
	}
	scanner := bufio.NewScanner(l.file)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	var expectedSequence uint64 = 1
	previous := ""
	line := 0
	for scanner.Scan() {
		line++
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var record AuditRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return fmt.Errorf("audit chain line %d is not valid JSON: %w", line, err)
		}
		if record.Sequence != expectedSequence {
			return fmt.Errorf("audit chain line %d has sequence %d, expected %d", line, record.Sequence, expectedSequence)
		}
		if record.PrevHash != previous {
			return fmt.Errorf("audit chain line %d has an invalid previous hash", line)
		}
		expectedHash, err := l.sign(record)
		if err != nil {
			return fmt.Errorf("hash audit chain line %d: %w", line, err)
		}
		if !hmac.Equal([]byte(record.Hash), []byte(expectedHash)) {
			return fmt.Errorf("audit chain line %d failed HMAC verification", line)
		}
		previous = record.Hash
		expectedSequence++
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan audit file: %w", err)
	}
	l.sequence = expectedSequence - 1
	l.prevHash = previous
	return nil
}

func (l *AuditLog) Append(record AuditRecord) (AuditRecord, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		l.verified = false
		return AuditRecord{}, errors.New("audit log is closed")
	}
	record.Sequence = l.sequence + 1
	if record.Timestamp.IsZero() {
		record.Timestamp = time.Now().UTC()
	} else {
		record.Timestamp = record.Timestamp.UTC()
	}
	record.PrevHash = l.prevHash
	hash, err := l.sign(record)
	if err != nil {
		l.verified = false
		return AuditRecord{}, err
	}
	record.Hash = hash
	encoded, err := json.Marshal(record)
	if err != nil {
		l.verified = false
		return AuditRecord{}, fmt.Errorf("encode audit record: %w", err)
	}
	encoded = append(encoded, '\n')
	if _, err := l.file.Write(encoded); err != nil {
		l.verified = false
		return AuditRecord{}, fmt.Errorf("append audit record: %w", err)
	}
	if err := l.file.Sync(); err != nil {
		l.verified = false
		return AuditRecord{}, fmt.Errorf("sync audit record: %w", err)
	}
	l.sequence = record.Sequence
	l.prevHash = record.Hash
	return record, nil
}

func (l *AuditLog) sign(record AuditRecord) (string, error) {
	record.Hash = ""
	encoded, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, l.key)
	_, _ = mac.Write(encoded)
	return hex.EncodeToString(mac.Sum(nil)), nil
}

func (l *AuditLog) State() AuditState {
	l.mu.Lock()
	defer l.mu.Unlock()
	var fileBytes int64
	if l.file == nil {
		l.verified = false
	} else if stat, err := l.file.Stat(); err != nil {
		l.verified = false
	} else {
		fileBytes = stat.Size()
	}
	prefix := l.prevHash
	if len(prefix) > 16 {
		prefix = prefix[:16]
	}
	return AuditState{Verified: l.verified, Events: l.sequence, FileBytes: fileBytes, LastHashPrefix: prefix}
}

// Recent returns newest-first, sanitized events from a bounded tail window.
// Every parsed record is rechecked against its HMAC so recent external
// tampering also degrades the audit state before data is exposed.
func (l *AuditLog) Recent(limit int, includeStarted bool) (AuditEventsPage, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		l.verified = false
		return AuditEventsPage{}, errors.New("audit log is closed")
	}
	if limit < 1 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	stat, err := l.file.Stat()
	if err != nil {
		l.verified = false
		return AuditEventsPage{}, fmt.Errorf("stat audit log: %w", err)
	}
	if stat.Size() == 0 {
		return AuditEventsPage{Events: []AuditEvent{}, Total: l.sequence, Verified: l.verified}, nil
	}
	const maxTailBytes int64 = 4 << 20
	start := int64(0)
	if stat.Size() > maxTailBytes {
		start = stat.Size() - maxTailBytes
	}
	content := make([]byte, stat.Size()-start)
	read, err := l.file.ReadAt(content, start)
	if err != nil && !errors.Is(err, io.EOF) {
		l.verified = false
		return AuditEventsPage{}, fmt.Errorf("read audit tail: %w", err)
	}
	content = content[:read]
	if start > 0 {
		newline := bytes.IndexByte(content, '\n')
		if newline < 0 {
			return AuditEventsPage{Events: []AuditEvent{}, Total: l.sequence, Truncated: true, Verified: l.verified}, nil
		}
		content = content[newline+1:]
	}
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) == 0 {
		return AuditEventsPage{Events: []AuditEvent{}, Total: l.sequence, Truncated: start > 0, Verified: l.verified}, nil
	}
	lines := bytes.Split(trimmed, []byte{'\n'})
	records := make([]AuditRecord, 0, len(lines))
	for index, line := range lines {
		var record AuditRecord
		if err := json.Unmarshal(line, &record); err != nil {
			l.verified = false
			return AuditEventsPage{}, fmt.Errorf("audit tail line %d is invalid: %w", index+1, err)
		}
		expectedHash, err := l.sign(record)
		if err != nil || !hmac.Equal([]byte(record.Hash), []byte(expectedHash)) {
			l.verified = false
			return AuditEventsPage{}, fmt.Errorf("audit tail line %d failed HMAC verification", index+1)
		}
		if index > 0 {
			previous := records[index-1]
			if record.Sequence != previous.Sequence+1 || record.PrevHash != previous.Hash {
				l.verified = false
				return AuditEventsPage{}, fmt.Errorf("audit tail line %d broke chain continuity", index+1)
			}
		}
		records = append(records, record)
	}
	events := make([]AuditEvent, 0, limit)
	for index := len(records) - 1; index >= 0 && len(events) < limit; index-- {
		record := records[index]
		if !includeStarted && strings.HasSuffix(record.Operation, "_started") {
			continue
		}
		events = append(events, AuditEvent{
			Sequence: record.Sequence, Timestamp: record.Timestamp, Operation: record.Operation,
			ChangeID: record.ChangeID, HTTPStatus: record.HTTPStatus, Outcome: record.Outcome,
			DurationMS: record.DurationMS, TraceID: record.TraceID, Provider: record.Provider,
			Model: record.Model, Risk: record.Risk, ToolCalls: record.ToolCalls,
			InjectionSuspected: record.InjectionSuspected,
		})
	}
	return AuditEventsPage{Events: events, Total: l.sequence, Truncated: start > 0, Verified: l.verified}, nil
}

func (l *AuditLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}
