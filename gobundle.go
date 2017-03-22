// Copyright 2015 Google Inc. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

// Program gobundle bundles an application's dependencies into a "vendor" directory.
//
// The target program must be in "package main" and this program run from that directory.
package main

import (
	"flag"
	"fmt"
	"go/build"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var (
	skipFiles = map[string]bool{
		".git": true,
		".hg":  true,
	}
)

func usage() {
	fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
	fmt.Fprintf(os.Stderr, "\t%s [command]\n", os.Args[0])
	fmt.Fprintf(os.Stderr, "For example:\n")
	fmt.Fprintf(os.Stderr, "\t%s tar zcvf $PWD/app.tar.gz .\n", os.Args[0])
}

var verbose bool

// vlogf logs to stderr if the "-v" flag is provided.
func vlogf(f string, v ...interface{}) {
	if !verbose {
		return
	}
	log.Printf("[gobundle] "+f, v...)
}

func main() {
	flag.BoolVar(&verbose, "v", false, "Verbose logging.")
	flag.Usage = usage
	flag.Parse()
	if flag.NArg() < 1 {
		usage()
		os.Exit(1)
	}

	if err := bundle(); err != nil {
		fmt.Fprintf(os.Stderr, os.Args[0]+": Error: %v\n", err)
		os.Exit(1)
	}
}

func bundle() error {
	app, err := analyze()
	if err != nil {
		return err
	}

	tmpDir, err := app.bundle()
	if tmpDir != "" {
		defer os.RemoveAll(tmpDir)
	}
	if err != nil {
		return err
	}

	if err := os.Chdir(tmpDir); err != nil {
		return fmt.Errorf("unable to chdir to %v: %v", tmpDir, err)
	}
	return deploy()
}

// deploy calls the provided command to deploy the app from the temporary directory.
func deploy() error {
	vlogf("Running command %v", flag.Args())
	cmd := exec.Command(flag.Arg(0), flag.Args()[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("unable to run %q: %v", strings.Join(flag.Args(), " "), err)
	}
	return nil
}

type app struct {
	imports map[string]string
}

// analyze checks the app for building and returns a map of imports to directory names.
func analyze() (*app, error) {
	ctxt := buildContext()
	vlogf("Using build context %#v", ctxt)
	im, err := imports(ctxt, ".")
	return &app{
		imports: im,
	}, err
}

// buildContext returns the context for greedily finding dependencies.
func buildContext() *build.Context {
	return &build.Context{
		GOARCH:      "amd64",
		GOOS:        "linux",
		GOROOT:      build.Default.GOROOT,
		GOPATH:      build.Default.GOPATH,
		Compiler:    build.Default.Compiler,
		UseAllFiles: true,
	}
}

// bundle bundles the app into a temporary directory.
func (s *app) bundle() (tmpdir string, err error) {
	workDir, err := ioutil.TempDir("", "gobundle")
	if err != nil {
		return "", fmt.Errorf("unable to create tmpdir: %v", err)
	}

	for srcDir, importName := range s.imports {
		dstDir := "vendor/" + importName
		if err := copyTree(workDir, dstDir, srcDir); err != nil {
			return workDir, fmt.Errorf("unable to copy directory %v to %v: %v", srcDir, dstDir, err)
		}
	}
	if err := copyTree(workDir, ".", "."); err != nil {
		return workDir, fmt.Errorf("unable to copy root directory to /app: %v", err)
	}
	return workDir, nil
}

// imports returns a map of all import directories used by the app.
// The return value maps full directory names to original import names.
func imports(ctxt *build.Context, srcDir string) (map[string]string, error) {
	result := make(map[string]string)

	type importFrom struct {
		path, fromDir string
	}
	var imports []importFrom
	visited := make(map[importFrom]bool)

	pkg, err := ctxt.ImportDir(srcDir, 0)
	if err != nil {
		// Ignore error (hopefully due to bad build tags).
	}
	for _, v := range pkg.Imports {
		imports = append(imports, importFrom{
			path:    v,
			fromDir: srcDir,
		})
	}

	// Resolve all non-standard-library imports
	for len(imports) != 0 {
		i := imports[0]
		imports = imports[1:] // shift
		if i.path == "C" {
			// ignore cgo
			continue
		}
		if _, ok := visited[i]; ok {
			// already scanned
			continue
		}
		visited[i] = true

		abs, err := filepath.Abs(i.fromDir)
		if err != nil {
			return nil, fmt.Errorf("unable to get absolute directory of %q: %v", i.fromDir, err)
		}
		pkg, err := ctxt.Import(i.path, abs, 0)
		if err != nil {
			// Ignore error (hopefully due to bad build tags).
		}

		// TODO(cbro): handle packages that are vendored by multiple imports correctly.

		if pkg.Goroot {
			// ignore standard library imports
			continue
		}

		if pkg.Dir == "" {
			// couldn't find package (hopefully due to bad build tags).
			continue
		}

		vlogf("Located %q (imported from %q) -> %q", i.path, i.fromDir, pkg.Dir)
		result[pkg.Dir] = i.path

		for _, v := range pkg.Imports {
			imports = append(imports, importFrom{
				path:    v,
				fromDir: pkg.Dir,
			})
		}
	}

	return result, nil
}

// copyTree copies srcDir to dstDir relative to dstRoot, ignoring skipFiles.
func copyTree(dstRoot, dstDir, srcDir string) error {
	vlogf("Copying %q to %q", srcDir, dstDir)
	d := filepath.Join(dstRoot, dstDir)
	if err := os.MkdirAll(d, 0755); err != nil {
		return fmt.Errorf("unable to create directory %q: %v", d, err)
	}

	entries, err := ioutil.ReadDir(srcDir)
	if err != nil {
		return fmt.Errorf("unable to read dir %q: %v", srcDir, err)
	}
	for _, entry := range entries {
		n := entry.Name()
		if skipFiles[n] {
			continue
		}
		s := filepath.Join(srcDir, n)
		if entry.Mode()&os.ModeSymlink == os.ModeSymlink {
			if entry, err = os.Stat(s); err != nil {
				return fmt.Errorf("unable to stat %v: %v", s, err)
			}
		}
		d := filepath.Join(dstDir, n)
		if entry.IsDir() {
			if err := copyTree(dstRoot, d, s); err != nil {
				return fmt.Errorf("unable to copy dir %q to %q: %v", s, d, err)
			}
			continue
		}
		if err := copyFile(dstRoot, d, s); err != nil {
			return fmt.Errorf("unable to copy dir %q to %q: %v", s, d, err)
		}
	}
	return nil
}

// copyFile copies src to dst relative to dstRoot.
func copyFile(dstRoot, dst, src string) error {
	s, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("unable to open %q: %v", src, err)
	}
	defer s.Close()

	dst = filepath.Join(dstRoot, dst)
	d, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("unable to create %q: %v", dst, err)
	}
	_, err = io.Copy(d, s)
	if err != nil {
		d.Close() // ignore error, copy already failed.
		return fmt.Errorf("unable to copy %q to %q: %v", src, dst, err)
	}
	if err := d.Close(); err != nil {
		return fmt.Errorf("unable to close %q: %v", dst, err)
	}
	return nil
}
