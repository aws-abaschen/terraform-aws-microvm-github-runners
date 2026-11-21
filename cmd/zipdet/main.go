// Command zipdet writes a reproducible zip on any OS: fixed 1980-01-01 mtime,
// sorted entries, and Unix mode 0755 for the files named with -x (0644 otherwise).
// Windows tools write no Unix mode bits, so a Lambda "bootstrap" zipped there
// would not be executable.
//
//	go run ./cmd/zipdet -o dist/lambda.zip -C dist/lambda -x bootstrap bootstrap
package main

import (
	"archive/zip"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func main() {
	out := flag.String("o", "", "output zip")
	dir := flag.String("C", ".", "directory the file names are relative to")
	exec := flag.String("x", "", "comma-separated names that get mode 0755")
	flag.Parse()
	if *out == "" || flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: zipdet -o out.zip [-C dir] [-x a,b] file...")
		os.Exit(2)
	}
	if err := write(*out, *dir, *exec, flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, "zipdet:", err)
		os.Exit(1)
	}
}

func write(out, dir, execList string, names []string) error {
	executable := map[string]bool{}
	for _, n := range strings.Split(execList, ",") {
		if n != "" {
			executable[n] = true
		}
	}
	sort.Strings(names)

	f, err := os.Create(out)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(f)
	mtime := time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, n := range names {
		mode := os.FileMode(0o644)
		if executable[n] {
			mode = 0o755
		}
		h := &zip.FileHeader{Name: filepath.ToSlash(n), Method: zip.Deflate, Modified: mtime}
		h.SetMode(mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		src, err := os.Open(filepath.Join(dir, n))
		if err != nil {
			return err
		}
		_, err = io.Copy(w, src)
		_ = src.Close() // read-only file: a close error cannot lose data
		if err != nil {
			return err
		}
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return f.Close()
}
