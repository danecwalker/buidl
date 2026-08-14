package secrets

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Set writes name=value into the shared or per-environment secrets file.
// Existing keys are replaced in place so comments and other values survive.
func Set(root, environment, name, value string) (string, error) {
	rel := EnvironmentFile(environment)
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := upsertLine(path, name, value); err != nil {
		return "", err
	}
	return rel, nil
}

// Has reports whether name is already set in the target secrets file.
func Has(root, environment, name string) bool {
	lines, err := readLines(filepath.Join(root, EnvironmentFile(environment)))
	if err != nil {
		return false
	}
	for _, line := range lines {
		key, ok := lineKey(line)
		if ok && key == name {
			return true
		}
	}
	return false
}

// Unset removes name from the shared or per-environment secrets file.
// A missing file or key is not an error.
func Unset(root, environment, name string) error {
	rel := EnvironmentFile(environment)
	path := filepath.Join(root, rel)
	return removeLine(path, name)
}

func upsertLine(path, name, value string) error {
	lines, err := readLines(path)
	if err != nil {
		return err
	}
	encoded := encodeSecretValue(value)
	replaced := false
	for i, line := range lines {
		key, ok := lineKey(line)
		if !ok || key != name {
			continue
		}
		lines[i] = name + "=" + encoded
		replaced = true
		break
	}
	if !replaced {
		lines = append(lines, name+"="+encoded)
	}
	return writeLines(path, lines)
}

func removeLine(path, name string) error {
	lines, err := readLines(path)
	if err != nil {
		return err
	}
	if lines == nil {
		return nil
	}
	out := lines[:0]
	for _, line := range lines {
		key, ok := lineKey(line)
		if ok && key == name {
			continue
		}
		out = append(out, line)
	}
	return writeLines(path, out)
}

func readLines(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var lines []string
	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines, scanner.Err()
}

func writeLines(path string, lines []string) error {
	var b strings.Builder
	for _, line := range lines {
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

func lineKey(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", false
	}
	trimmed = strings.TrimPrefix(trimmed, "export ")
	key, _, ok := strings.Cut(trimmed, "=")
	if !ok {
		return "", false
	}
	return strings.TrimSpace(key), true
}

func encodeSecretValue(v string) string {
	if strings.ContainsAny(v, " \t#\"'") || strings.Contains(v, "\n") {
		return strconv.Quote(v)
	}
	return v
}
