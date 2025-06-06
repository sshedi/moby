//go:build linux || freebsd

package initlayer // import "github.com/docker/docker/daemon/initlayer"

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/moby/sys/user"
	"golang.org/x/sys/unix"
)

// Setup populates a directory with mountpoints suitable
// for bind-mounting things into the container.
//
// This extra layer is used by all containers as the top-most ro layer. It protects
// the container from unwanted side-effects on the rw layer.
func Setup(initLayerFs string, uid int, gid int) error {
	// Since all paths are local to the container, we can just extract initLayerFs.Path()
	initLayer := initLayerFs

	for pth, typ := range map[string]string{
		"/dev/pts":         "dir",
		"/dev/shm":         "dir",
		"/proc":            "dir",
		"/sys":             "dir",
		"/.dockerenv":      "file",
		"/etc/resolv.conf": "file",
		"/etc/hosts":       "file",
		"/etc/hostname":    "file",
		"/dev/console":     "file",
		"/etc/mtab":        "/proc/mounts",
	} {
		parts := strings.Split(pth, "/")
		prev := "/"
		for _, p := range parts[1:] {
			prev = filepath.Join(prev, p)
			unix.Unlink(filepath.Join(initLayer, prev))
		}

		if _, err := os.Stat(filepath.Join(initLayer, pth)); err != nil {
			if os.IsNotExist(err) {
				if err := user.MkdirAllAndChown(filepath.Join(initLayer, filepath.Dir(pth)), 0o755, uid, gid, user.WithOnlyNew); err != nil {
					return err
				}
				switch typ {
				case "dir":
					if err := user.MkdirAllAndChown(filepath.Join(initLayer, pth), 0o755, uid, gid, user.WithOnlyNew); err != nil {
						return err
					}
				case "file":
					f, err := os.OpenFile(filepath.Join(initLayer, pth), os.O_CREATE, 0o755)
					if err != nil {
						return err
					}
					f.Chown(uid, gid)
					f.Close()
				default:
					if err := os.Symlink(typ, filepath.Join(initLayer, pth)); err != nil {
						return err
					}
				}
			} else {
				return err
			}
		}
	}

	// Layer is ready to use, if it wasn't before.
	return nil
}
