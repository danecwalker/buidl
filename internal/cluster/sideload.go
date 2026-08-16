package cluster

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/danecwalker/buidl/internal/inventory"
)

// remoteArchiveDir is world-writable on typical Linux images and is not a
// tmpfs the way /tmp often is, so a multi-hundred-MB image will not OOM
// the node just by landing there.
const remoteArchiveDir = "/var/tmp"

// SideloadImage copies a local docker archive onto every inventory server,
// imports it into that node's containerd, and deletes the remote file.
// The caller deletes the local archive.
func (m *Manager) SideloadImage(ctx context.Context, archive string) (int, error) {
	if _, err := os.Stat(archive); err != nil {
		return 0, fmt.Errorf("local image archive: %w", err)
	}

	inv, err := m.provider.Resolve(ctx)
	if err != nil {
		return 0, fmt.Errorf("resolving servers to copy the image onto: %w", err)
	}
	if len(inv.Servers) == 0 {
		return 0, fmt.Errorf("no servers listed under infra.servers\n\nhint: buidl add server <ip>, or set image to a registry repository")
	}

	remotePath := remoteArchivePath(archive)
	for _, server := range inv.Servers {
		if err := m.sideloadOne(ctx, server, archive, remotePath); err != nil {
			return 0, err
		}
	}
	return len(inv.Servers), nil
}

func remoteArchivePath(local string) string {
	return remoteArchiveDir + "/" + filepath.Base(local)
}

func (m *Manager) sideloadOne(ctx context.Context, server inventory.Server, localPath, remotePath string) error {
	client, err := m.connect(ctx, server)
	if err != nil {
		return err
	}

	// Always try to remove the remote archive, including after a failed
	// import: a leftover tar in /var/tmp is worse than a noisy rm.
	defer func() {
		if err := client.Remove(ctx, remotePath); err != nil {
			m.log.Detail("%s: removing %s: %v", server.Host, remotePath, err)
		}
	}()

	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("opening image archive: %w", err)
	}
	defer f.Close()

	m.log.Detail("%s: uploading %s", server.Host, filepath.Base(localPath))
	if err := client.WriteStream(ctx, remotePath, f, "0600"); err != nil {
		return fmt.Errorf("copying image to %s: %w", server.Host, err)
	}

	cmd := m.distro.ImportImageCommand(remotePath)
	m.log.Detail("%s: importing into containerd", server.Host)
	if _, err := client.Sudo(ctx, cmd); err != nil {
		return fmt.Errorf("importing image on %s: %w", server.Host, err)
	}
	return nil
}
