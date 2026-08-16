package watch

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/term"
)

const (
	enterAlt   = "\033[?1049h"
	leaveAlt   = "\033[?1049l"
	hideCursor = "\033[?25l"
	showCursor = "\033[?25h"
	homeCursor = "\033[H"
	clearEOL   = "\033[K"
	clearEOS   = "\033[J"

	// collectBound keeps one hung apiserver call from freezing the frame
	// until the user gives up. The session itself has no default timeout.
	collectBound = 15 * time.Second
)

// Options configures a live session or a single print.
type Options struct {
	// Collect returns the next observation. Called on an interval and on 'r'.
	Collect func(ctx context.Context) (Snapshot, error)
	// Interval is the refresh period. Values below 500ms are raised so we
	// do not hammer metrics-server.
	Interval time.Duration
	Color    bool
	// Select is the app name to highlight first.
	Select string
	Stdin  io.Reader
	Stdout io.Writer
	// StdinFD / StdoutFD are used for raw mode and size. Defaults are os.Stdin/Stdout.
	StdinFD  int
	StdoutFD int
}

// Live enters the alt screen and refreshes until ctx is cancelled or the
// user presses q / Ctrl+C. One Ctrl+C exits; a two-press confirm is for
// deploy, not for a dashboard you opened to look.
func Live(ctx context.Context, opts Options) error {
	if opts.Collect == nil {
		return fmt.Errorf("watch: no collector")
	}
	if opts.Interval < 500*time.Millisecond {
		opts.Interval = 2 * time.Second
	}
	if opts.Stdin == nil {
		opts.Stdin = os.Stdin
	}
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	if opts.StdinFD == 0 {
		opts.StdinFD = int(os.Stdin.Fd())
	}
	if opts.StdoutFD == 0 {
		opts.StdoutFD = int(os.Stdout.Fd())
	}

	old, err := term.MakeRaw(opts.StdinFD)
	if err != nil {
		return fmt.Errorf("putting the terminal in raw mode: %w", err)
	}
	defer func() { _ = term.Restore(opts.StdinFD, old) }()

	if _, err := io.WriteString(opts.Stdout, enterAlt+hideCursor); err != nil {
		return err
	}
	defer func() { _, _ = io.WriteString(opts.Stdout, showCursor+leaveAlt) }()

	keys := make(chan byte, 16)
	go readKeys(ctx, opts.Stdin, keys)

	resize, stopResize := notifyResize()
	if stopResize != nil {
		defer stopResize()
	}

	ticker := time.NewTicker(opts.Interval)
	defer ticker.Stop()

	type result struct {
		snap Snapshot
		err  error
	}
	results := make(chan result, 1)
	collecting := false
	kick := func() {
		if collecting {
			return
		}
		collecting = true
		go func() {
			cctx, cancel := context.WithTimeout(ctx, collectBound)
			snap, err := opts.Collect(cctx)
			cancel()
			select {
			case results <- result{snap, err}:
			case <-ctx.Done():
			}
		}()
	}

	var (
		snap     Snapshot
		hist     History
		lastErr  string
		selected int
		have     bool
		name     = opts.Select
	)

	draw := func() {
		w, h := termSize(opts.StdoutFD)
		v := View{
			Snapshot:    snap,
			History:     hist,
			Selected:    selected,
			Interval:    opts.Interval,
			Now:         time.Now(),
			Width:       w,
			Height:      h,
			Color:       opts.Color,
			Interactive: true,
			Err:         lastErr,
		}
		writeFrame(opts.Stdout, Render(v))
	}

	kick()
	draw()

	for {
		select {
		case <-ctx.Done():
			return nil
		case res := <-results:
			collecting = false
			if res.err != nil {
				lastErr = res.err.Error()
			} else {
				lastErr = ""
				snap = res.snap
				hist.Record(snap)
				if !have {
					selected = snap.SelectIndex(name)
					have = true
				} else if name = snap.Selected(selected).Name; name != "" {
					selected = snap.SelectIndex(name)
				}
				selected = ClampSelected(selected, len(snap.Apps))
			}
			draw()
		case <-ticker.C:
			kick()
		case <-resize:
			draw()
		case k, ok := <-keys:
			if !ok {
				return nil
			}
			switch k {
			case 'q', 'Q', 0x03: // Ctrl+C
				return nil
			case 'r', 'R', 0x0c: // Ctrl+L
				kick()
			case 'j', 0x0e: // Ctrl+N
				selected = ClampSelected(selected+1, len(snap.Apps))
				if have {
					name = snap.Selected(selected).Name
				}
				draw()
			case 'k', 0x10: // Ctrl+P
				selected = ClampSelected(selected-1, len(snap.Apps))
				if have {
					name = snap.Selected(selected).Name
				}
				draw()
			}
		}
	}
}

// Report renders one snapshot for --once and non-TTY stdout.
func Report(snap Snapshot, color bool, width int) string {
	return Render(View{
		Snapshot:    snap,
		Now:         time.Now(),
		Width:       width,
		Color:       color,
		Interactive: false,
	}) + "\n"
}

func readKeys(ctx context.Context, r io.Reader, out chan<- byte) {
	defer close(out)
	buf := make([]byte, 8)
	for {
		if ctx.Err() != nil {
			return
		}
		n, err := r.Read(buf)
		if err != nil {
			return
		}
		if n == 0 {
			continue
		}
		// Arrow keys arrive as CSI sequences; map them onto j/k so the
		// rest of the loop stays a single-byte switch.
		key := buf[0]
		if n >= 3 && buf[0] == 0x1b && buf[1] == '[' {
			switch buf[2] {
			case 'A':
				key = 'k'
			case 'B':
				key = 'j'
			}
		}
		select {
		case out <- key:
		case <-ctx.Done():
			return
		}
	}
}

// writeFrame paints one dashboard in raw mode.
//
// term.MakeRaw clears OPOST, so ONLCR is off and \n is a line feed only —
// the cursor stays in the same column. Each line therefore ends with
// erase-to-EOL and a CR before the next LF. The last line is not followed
// by LF so a full-height frame does not scroll. clear-to-EOS then drops
// leftover rows from a taller previous frame.
func writeFrame(w io.Writer, frame string) {
	var b strings.Builder
	b.WriteString(homeCursor)
	lines := strings.Split(frame, "\n")
	for i, line := range lines {
		b.WriteString(line)
		b.WriteString(clearEOL)
		if i < len(lines)-1 {
			b.WriteString("\r\n")
		}
	}
	b.WriteString(clearEOS)
	_, _ = io.WriteString(w, b.String())
}

func termSize(fd int) (int, int) {
	w, h, err := term.GetSize(fd)
	if err != nil || w <= 0 {
		return 100, 40
	}
	if h <= 0 {
		h = 40
	}
	return w, h
}
