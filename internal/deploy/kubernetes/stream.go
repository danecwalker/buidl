package kubernetes

import (
	"bufio"
	"io"
)

// copyWithPrefix copies src to dst, prefixing every line.
//
// Streaming line-by-line rather than buffering matters for `logs --follow`:
// output must appear as the app emits it, not when a buffer fills.
func copyWithPrefix(dst io.Writer, src io.Reader, prefix string) error {
	scanner := bufio.NewScanner(src)
	// Container log lines can be long (stack traces, JSON logs); the default
	// 64KB limit truncates them.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		if _, err := io.WriteString(dst, prefix); err != nil {
			return err
		}
		if _, err := dst.Write(scanner.Bytes()); err != nil {
			return err
		}
		if _, err := io.WriteString(dst, "\n"); err != nil {
			return err
		}
	}
	return scanner.Err()
}
