package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/danecwalker/buidl/internal/ui"
	"github.com/danecwalker/buidl/internal/update"
)

// newUpdateCmd replaces this binary with the latest GitHub release.
func newUpdateCmd(a *App) *cobra.Command {
	var checkOnly bool

	cmd := &cobra.Command{
		Use:   "update",
		Short: "Install the latest buidl release",
		Long: `Replace this binary with the latest GitHub release.

The download is checked against checksums.txt from the same release. After a
command notices a newer version, this is how you install it.

If this binary lives in a directory you cannot write (typically
/usr/local/bin), the new file is written to ~/.local/bin instead. Point
the old path at it once so later updates do not need sudo. Run this
command without sudo; wrapping it in sudo keeps the binary in a
root-owned directory.

  buidl update
  buidl update --check`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// The notice after a command exists to send people here. Printing
			// it at the end of `update` itself would be noise.
			a.skipUpdateNotice = true
			return a.runUpdate(checkOnly)
		},
	}
	cmd.Flags().BoolVar(&checkOnly, "check", false, "report whether an update is available without installing")
	return cmd
}

func (a *App) runUpdate(checkOnly bool) error {
	if a.opts.timeout <= 0 {
		a.opts.timeout = 30 * time.Minute
	}
	ctx, cancel := a.context()
	defer cancel()

	client := a.updateClient()

	a.log.Step("checking for update")
	// Always hit GitHub here. The 24h cache is for the background notice;
	// `buidl update` is the user asking, so a release from an hour ago
	// must still be found.
	latest, err := client.Latest(ctx)
	if err != nil {
		return fmt.Errorf("checking latest release: %w\n\nhint: %s/releases/latest", err, client.BaseURL)
	}
	current := client.Current
	if current == "" {
		current = Version
	}
	newer := update.Newer(current, latest)
	a.log.Info("current %s", displayVersion(current))
	a.log.Info("latest  %s", latest)

	if checkOnly {
		if newer {
			a.log.Success("%s is available; run `buidl update` to install it", latest)
			return nil
		}
		a.log.Success("already up to date")
		return nil
	}

	dest := a.updateDest
	if dest == "" {
		dest, err = update.Executable()
		if err != nil {
			return err
		}
	}
	previous := dest

	// A binary in /usr/local/bin needs sudo to replace. Relocate even
	// when the version is current, otherwise a first `sudo buidl update`
	// would leave the new binary in a root-owned path forever.
	if !update.CanReplace(dest) {
		alt, altErr := update.UserInstallPath(dest)
		if altErr != nil {
			if !newer && update.Parseable(current) {
				a.log.Success("already up to date (%s)", latest)
				a.log.Info("%s is not writable; later updates will need sudo", dest)
				return nil
			}
			return fmt.Errorf("cannot replace %s: %w\n\nhint: install to a directory you own:\n  curl -fsSL https://raw.githubusercontent.com/danecwalker/buidl/main/install.sh | bash", dest, altErr)
		}
		a.log.Info("%s is not writable; installing to %s", dest, alt)
		dest = alt
	}

	if !newer && update.Parseable(current) && previous == dest {
		a.log.Success("already up to date (%s)", latest)
		return nil
	}

	a.log.Step("downloading " + client.PlatformAsset())
	client.Progress = func(done, total int64) {
		a.log.Progress(done, total)
	}
	if err := client.Install(ctx, latest, dest); err != nil {
		return err
	}
	a.log.Success("installed %s to %s", latest, dest)
	if previous != dest {
		a.hintRelocatedUpdate(previous, dest)
	} else if os.Geteuid() == 0 {
		a.log.Info("running as root; later updates will still need sudo")
		a.log.Info("run `buidl update` without sudo to move the binary to ~/.local/bin")
	}
	return nil
}

// hintRelocatedUpdate tells the user how to retire a leftover system
// binary so PATH picks up the user-owned copy. We do not exec sudo:
// that would hang tests and surprise anyone who did not type it.
func (a *App) hintRelocatedUpdate(previous, dest string) {
	if err := replaceWithLink(dest, previous); err == nil {
		a.log.Info("pointed %s at %s; later updates will not need sudo", previous, dest)
		return
	}
	a.log.Info("%s is still first on PATH. Point it at the new binary once:", previous)
	a.log.Info("  sudo rm -f %s", previous)
	a.log.Info("  sudo ln -s %s %s", dest, previous)
	a.log.Info("or add %s to PATH ahead of %s", filepath.Dir(dest), filepath.Dir(previous))
}

func replaceWithLink(target, link string) error {
	if target == "" || link == "" || target == link {
		return fmt.Errorf("no link to create")
	}
	if !update.CanReplace(link) {
		return fmt.Errorf("cannot write %s", link)
	}
	if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Symlink(target, link)
}

func displayVersion(v string) string {
	if v == "" {
		return "dev"
	}
	return v
}

func (a *App) updateClient() *update.Client {
	if a.updater != nil {
		return a.updater
	}
	return update.New(Version)
}

// updateCheckDisabled is the background lookup, not `buidl update` itself.
// CI and BUIDL_NO_UPDATE_CHECK skip it so pipelines do not wait on GitHub
// and so a failed lookup cannot become a CI annotation.
func updateCheckDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("BUIDL_NO_UPDATE_CHECK"))) {
	case "1", "true", "yes":
		return true
	}
	return ui.DetectCI().Detected
}

func (a *App) startUpdateCheck() {
	if updateCheckDisabled() || !update.Parseable(Version) {
		return
	}
	a.updateResult = make(chan update.Result, 1)
	client := a.updateClient()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		res, err := client.Check(ctx)
		if err != nil {
			return
		}
		a.updateResult <- res
	}()
}

func (a *App) maybeNotifyUpdate() {
	if a.skipUpdateNotice || a.updateResult == nil || a.log == nil {
		return
	}
	if a.log.Mode() == ui.ModeJSON {
		return
	}
	var res update.Result
	select {
	case res = <-a.updateResult:
	case <-time.After(400 * time.Millisecond):
		return
	}
	if !res.Newer {
		return
	}
	a.log.Info("")
	a.log.Info("buidl %s is available (you have %s); run `buidl update`", res.Latest, displayVersion(res.Current))
}
