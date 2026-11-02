package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// EnvUploads overrides uploads.dir, for a machine that keeps the files elsewhere.
const EnvUploads = "DRIFTGATE_UPLOADS"

// Uploads is the folder of the real files catalog requests upload. It lives
// outside the repository: the files may hold real data, and only their
// paths and keyed hashes are committed.
type Uploads struct {
	Dir string `yaml:"dir,omitempty"` // ~/ is the home folder; relative to the project root otherwise
}

// UploadsDir returns the uploads folder: DRIFTGATE_UPLOADS, then
// uploads.dir (relative to the folder of driftgate.yaml). A folder inside
// the git repository of the project is refused.
func (p *Project) UploadsDir() (string, error) {
	dir := os.Getenv(EnvUploads)
	if dir == "" {
		dir = p.Uploads.Dir
	}
	if dir == "" {
		return "", fmt.Errorf("catalog requests upload files, but no uploads folder is configured (uploads: { dir: ~/… } in %s, or %s)", FileName, EnvUploads)
	}
	if _, home := HomeRelative(dir); !filepath.IsAbs(dir) && !home {
		dir = filepath.Join(p.Root, dir)
	}
	abs, err := expand(dir)
	if err != nil {
		return "", err
	}
	// Anywhere in the git repository (also beside a configuration in a
	// subfolder), as written or through a symbolic link.
	if within(repository(p.Root), abs) {
		return "", invalid{fmt.Errorf("the uploads folder %s is inside the repository; real files must stay out of it (keep them e.g. under ~/Data)", abs)}
	}
	return abs, nil
}
