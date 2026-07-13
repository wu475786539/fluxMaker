package audit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Logger struct {
	path     string
	maxBytes int64
	backups  int
	mu       sync.Mutex
	pending  [][]byte
}

type record struct {
	Timestamp time.Time `json:"timestamp"`
	Type      string    `json:"type"`
	Data      any       `json:"data"`
}

func New(path string) *Logger { return NewWithRotation(path, 0, 0) }

func NewWithRotation(path string, maxBytes int64, backups int) *Logger {
	return &Logger{path: path, maxBytes: maxBytes, backups: backups, pending: make([][]byte, 0, 32)}
}

func (l *Logger) Record(eventType string, data any) error {
	if l == nil || l.path == "" {
		return nil
	}
	b, err := json.Marshal(record{Timestamp: time.Now().UTC(), Type: eventType, Data: data})
	if err != nil {
		return err
	}
	l.mu.Lock()
	l.pending = append(l.pending, b)
	l.mu.Unlock()
	return nil
}

// Flush writes all queued events with one open/write/fsync cycle. The engine
// calls it at trading-cycle boundaries, preserving durable audit records
// without paying one fsync for every quote and OMS event.
func (l *Logger) Flush() error {
	if l == nil || l.path == "" {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.pending) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o750); err != nil {
		return err
	}
	pendingBytes := int64(0)
	for _, item := range l.pending {
		pendingBytes += int64(len(item) + 1)
	}
	if err := l.rotateIfNeeded(pendingBytes); err != nil {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	writer := bufio.NewWriterSize(f, 64*1024)
	for _, item := range l.pending {
		if _, err := fmt.Fprintln(writer, string(item)); err != nil {
			_ = f.Close()
			return err
		}
	}
	if err := writer.Flush(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	l.pending = l.pending[:0]
	return nil
}

func (l *Logger) PendingCount() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.pending)
}

func (l *Logger) rotateIfNeeded(incoming int64) error {
	if l.maxBytes <= 0 || l.backups <= 0 {
		return nil
	}
	info, err := os.Stat(l.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Size() == 0 || info.Size()+incoming <= l.maxBytes {
		return nil
	}
	_ = os.Remove(fmt.Sprintf("%s.%d", l.path, l.backups))
	for index := l.backups - 1; index >= 1; index-- {
		oldPath := fmt.Sprintf("%s.%d", l.path, index)
		newPath := fmt.Sprintf("%s.%d", l.path, index+1)
		if err := os.Rename(oldPath, newPath); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return os.Rename(l.path, l.path+".1")
}
