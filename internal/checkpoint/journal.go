// Package checkpoint stores completed campaign results in a durable JSONL journal.
package checkpoint

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"unicode/utf8"
)

const (
	version     = 1
	maxFileSize = 128 << 20
	maxLineSize = 1 << 20
	maxTextSize = 4 << 10
	maxResults  = 1 << 18
)

var (
	ErrLocked      = errors.New("checkpoint is locked by another campaign")
	ErrFingerprint = errors.New("checkpoint fingerprint does not match this campaign")
	ErrCorrupt     = errors.New("invalid checkpoint")
)

type header struct {
	Version     int    `json:"version"`
	Fingerprint string `json:"fingerprint"`
}

type record struct {
	Key    string          `json:"key"`
	Result json.RawMessage `json:"result"`
}

// Journal owns an exclusive file lock until Close. Values must support a JSON
// round trip. Get returns independent values and can run concurrently with Put.
type Journal[T any] struct {
	mu      sync.RWMutex
	file    *os.File
	size    int64
	results map[string]json.RawMessage
	failure error
}

// Open checks the file format and campaign fingerprint before restoring results.
// A result without a final newline is discarded after the complete history is
// validated. The parent directory must already exist.
func Open[T any](path, fingerprint string) (*Journal[T], error) {
	if !validText(fingerprint) {
		return nil, errors.New("checkpoint fingerprint must be nonempty UTF-8 of at most 4096 bytes")
	}
	file, err := openLocked(path)
	if err != nil {
		return nil, err
	}
	journal := &Journal[T]{file: file, results: make(map[string]json.RawMessage)}
	if err := journal.restore(fingerprint); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("checkpoint %s: %w", path, err)
	}
	return journal, nil
}

func (j *Journal[T]) restore(fingerprint string) error {
	info, err := j.file.Stat()
	if err != nil {
		return err
	}
	if info.Size() > maxFileSize {
		return fmt.Errorf("%w: file exceeds %d bytes", ErrCorrupt, maxFileSize)
	}
	if info.Size() == 0 {
		data, err := json.Marshal(header{Version: version, Fingerprint: fingerprint})
		if err != nil {
			return err
		}
		if err := j.persist(append(data, '\n')); err != nil {
			return err
		}
		return syncParent(j.file.Name())
	}

	reader := bufio.NewReaderSize(j.file, maxLineSize+1)
	first, err := reader.ReadSlice('\n')
	if err != nil || len(first) > maxLineSize {
		return fmt.Errorf("%w: incomplete or oversized header", ErrCorrupt)
	}
	var h header
	if err := decode(first, &h); err != nil || h.Version != version || !validText(h.Fingerprint) {
		return fmt.Errorf("%w: malformed or unsupported header", ErrCorrupt)
	}
	if h.Fingerprint != fingerprint {
		return ErrFingerprint
	}
	j.size = int64(len(first))
	for lineNumber := 2; ; lineNumber++ {
		line, err := reader.ReadSlice('\n')
		if len(line) > maxLineSize || errors.Is(err, bufio.ErrBufferFull) {
			return fmt.Errorf("%w: line %d exceeds %d bytes", ErrCorrupt, lineNumber, maxLineSize)
		}
		switch {
		case errors.Is(err, io.EOF):
			if len(line) == 0 {
				return nil
			}
			if err := j.file.Truncate(j.size); err != nil {
				return err
			}
			return j.file.Sync()
		case err != nil:
			return err
		}
		var r record
		if err := decode(line, &r); err != nil || !validText(r.Key) || len(r.Result) == 0 {
			return fmt.Errorf("%w: malformed record on line %d", ErrCorrupt, lineNumber)
		}
		if _, err := decodeResult[T](r.Result); err != nil {
			return fmt.Errorf("%w: result type on line %d: %v", ErrCorrupt, lineNumber, err)
		}
		if previous, exists := j.results[r.Key]; exists {
			if !bytes.Equal(previous, r.Result) {
				return fmt.Errorf("%w: conflicting result for %q on line %d", ErrCorrupt, r.Key, lineNumber)
			}
		} else {
			if len(j.results) == maxResults {
				return fmt.Errorf("%w: more than %d results", ErrCorrupt, maxResults)
			}
			j.results[r.Key] = r.Result
		}
		j.size += int64(len(line))
	}
}

func validText(value string) bool {
	return len(value) > 0 && len(value) <= maxTextSize && utf8.ValidString(value)
}

func decode(data []byte, destination any) error {
	if !utf8.Valid(data) || !json.Valid(data) {
		return errors.New("invalid JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(destination)
}

func decodeResult[T any](data []byte) (T, error) {
	var result T
	err := json.Unmarshal(data, &result)
	return result, err
}

func syncParent(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

// Get returns a fresh decoding of a completed result.
func (j *Journal[T]) Get(key string) (T, bool) {
	j.mu.RLock()
	data, found := j.results[key]
	j.mu.RUnlock()
	if !found {
		var zero T
		return zero, false
	}
	// Custom JSON decoders must never receive the journal's shared snapshot.
	result, err := decodeResult[T](bytes.Clone(data))
	return result, err == nil
}

// Put persists a result before publishing it. Repeating an identical JSON value
// is idempotent. A conflicting value for the same key is an error.
func (j *Journal[T]) Put(key string, result T) error {
	if !validText(key) {
		return errors.New("checkpoint key must be nonempty UTF-8 of at most 4096 bytes")
	}
	data, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode checkpoint result: %w", err)
	}
	if len(data) >= maxLineSize {
		return fmt.Errorf("checkpoint record exceeds %d bytes", maxLineSize)
	}
	if _, err := decodeResult[T](bytes.Clone(data)); err != nil {
		return fmt.Errorf("decode checkpoint result: %w", err)
	}
	line, err := json.Marshal(record{Key: key, Result: data})
	if err != nil {
		return fmt.Errorf("encode checkpoint record: %w", err)
	}
	line = append(line, '\n')
	if len(line) > maxLineSize {
		return fmt.Errorf("checkpoint record exceeds %d bytes", maxLineSize)
	}

	j.mu.Lock()
	defer j.mu.Unlock()
	if j.file == nil {
		return os.ErrClosed
	}
	if j.failure != nil {
		return j.failure
	}
	if previous, exists := j.results[key]; exists {
		if bytes.Equal(previous, data) {
			return nil
		}
		return fmt.Errorf("checkpoint has a conflicting result for %q", key)
	}
	if len(j.results) == maxResults || j.size+int64(len(line)) > maxFileSize {
		return errors.New("checkpoint capacity exceeded")
	}
	if err := j.persist(line); err != nil {
		// Continuing after an ambiguous write could turn a torn tail into a bad record.
		j.failure = fmt.Errorf("persist checkpoint result: %w", err)
		return j.failure
	}
	j.results[key] = data
	return nil
}

func (j *Journal[T]) persist(line []byte) error {
	if _, err := j.file.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	if _, err := j.file.Write(line); err != nil {
		return err
	}
	if err := j.file.Sync(); err != nil {
		return err
	}
	j.size += int64(len(line))
	return nil
}

// Close releases the exclusive lock and discards the in-memory index.
func (j *Journal[T]) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.file == nil {
		return nil
	}
	err := j.file.Close()
	j.file = nil
	j.results = nil
	return err
}
