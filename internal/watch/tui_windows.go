//go:build windows

package watch

import "os"

func notifyResize() (<-chan os.Signal, func()) {
	return make(chan os.Signal), func() {}
}
