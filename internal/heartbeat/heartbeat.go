package heartbeat

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func Touch(path string) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.FormatInt(time.Now().UnixMilli(), 10)+"\n"), 0o640)
}

func Age(path string) (time.Duration, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	ms, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, err
	}
	return time.Since(time.UnixMilli(ms)), nil
}
