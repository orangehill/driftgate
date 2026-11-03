package capture

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/orangehill/driftgate/internal/appconf"
	"github.com/orangehill/driftgate/internal/config"
)

// sqliteServerPrefix starts the server of an SQLite database (serverOf):
// file: and the real path of its working file.
const sqliteServerPrefix = "file:"

// sqliteStorePristine is the default pristine copy of an SQLite database:
// a file of the local store next to the record of it (pristinePath), so
// every checkout and working file has its own.
func sqliteStorePristine(record string) string {
	return strings.TrimSuffix(record, ".json") + ".sqlite"
}

// sqliteFiles are the absolute paths of the working file and the pristine
// copy of the SQLite component sc, for state.Options: working as serverOf
// resolved it, pristine as set (inside the project, past symbolic links;
// it need not exist yet) or by default in the store (sqliteStorePristine).
// The pristine copy is never the working file, whatever links lead there.
func sqliteFiles(p *config.Project, sc config.Component, server, record string) (working, pristine string, err error) {
	working = strings.TrimPrefix(server, sqliteServerPrefix)
	if sc.Pristine == "" {
		return working, sqliteStorePristine(record), nil
	}
	file := path.Clean(strings.ReplaceAll(sc.Pristine, `\`, "/"))
	dir, err := appconf.LocalPath(p.Root, path.Dir(file))
	if err != nil {
		return "", "", fmt.Errorf("pristine: %w", err)
	}
	pristine = filepath.Join(dir, path.Base(file))
	if _, err := os.Lstat(pristine); err == nil {
		if pristine, err = appconf.LocalPath(p.Root, file); err != nil {
			return "", "", fmt.Errorf("pristine: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", "", fmt.Errorf("pristine: %s: %w", sc.Pristine, err)
	}
	// Stat only: a file SQLite may have open is never opened here.
	if a, err := os.Stat(pristine); err == nil {
		if b, err := os.Stat(working); err == nil && os.SameFile(a, b) {
			return "", "", fmt.Errorf("pristine %s is the working file %s", sc.Pristine, sc.Working)
		}
	}
	return working, pristine, nil
}
