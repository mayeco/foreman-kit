package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/joho/godotenv"
)

// dotenvFile is the dotenv file read from the directory foreman-kit runs in.
const dotenvFile = ".env"

// loadDotenv loads dotenvFile from dir into the process environment.
// Variables already present in the environment are never overridden, so the
// file only supplies defaults. A missing file is not an error; a file that
// cannot be read or parsed produces a warning on stderr and is skipped.
func loadDotenv(dir string, stderr io.Writer) {
	path := filepath.Join(dir, dotenvFile)
	if _, err := os.Stat(path); err != nil {
		if !errors.Is(err, fs.ErrNotExist) && stderr != nil {
			fmt.Fprintf(stderr, "foreman-kit: warning: cannot read %s: %v\n", path, err)
		}
		return
	}
	if err := godotenv.Load(path); err != nil && stderr != nil {
		fmt.Fprintf(stderr, "foreman-kit: warning: cannot load %s: %v\n", path, err)
	}
}
