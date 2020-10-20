package interp

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"go/build"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
)

// importSrc calls gta on the source code for the package identified by
// importPath. rPath is the relative path to the directory containing the source
// code for the package. It can also be "main" as a special value.
func (interp *Interpreter) importSrc(rPath, importPath string, skipTest bool) (string, error) {
	var dir string
	var err error

	if interp.srcPkg[importPath] != nil {
		name, ok := interp.pkgNames[importPath]
		if !ok {
			return "", fmt.Errorf("inconsistent knowledge about %s", importPath)
		}
		return name, nil
	}

	// For relative import paths in the form "./xxx" or "../xxx", the initial
	// base path is the directory of the interpreter input file, or "." if no file
	// was provided.
	// In all other cases, absolute import paths are resolved from the GOPATH
	// and the nested "vendor" directories.
	if isPathRelative(importPath) {
		if rPath == "main" {
			rPath = "."
		}
		dir = filepath.Join(filepath.Dir(interp.name), rPath, importPath)
	} else {
		root, err := interp.rootFromSourceLocation(rPath)
		if err != nil {
			return "", err
		}
		if dir, rPath, err = pkgDir(&interp.context, root, importPath); err != nil {
			return "", err
		}
	}

	if interp.rdir[importPath] {
		return "", fmt.Errorf("import cycle not allowed\n\timports %s", importPath)
	}
	interp.rdir[importPath] = true

	files, err := ioutil.ReadDir(dir)
	if err != nil {
		return "", err
	}

	var initNodes []*node
	var rootNodes []*node
	revisit := make(map[string][]*node)

	var root *node
	var pkgName string

	// Parse source files.
	for _, file := range files {
		name := file.Name()
		if skipFile(&interp.context, name, skipTest) {
			continue
		}

		name = filepath.Join(dir, name)
		var buf []byte
		if buf, err = ioutil.ReadFile(name); err != nil {
			return "", err
		}

		var pname string
		if pname, root, err = interp.ast(string(buf), name, false); err != nil {
			return "", err
		}
		if root == nil {
			continue
		}

		if interp.astDot {
			dotCmd := interp.dotCmd
			if dotCmd == "" {
				dotCmd = defaultDotCmd(name, "yaegi-ast-")
			}
			root.astDot(dotWriter(dotCmd), name)
		}
		if pkgName == "" {
			pkgName = pname
		} else if pkgName != pname && skipTest {
			return "", fmt.Errorf("found packages %s and %s in %s", pkgName, pname, dir)
		}
		rootNodes = append(rootNodes, root)

		subRPath := effectivePkg(rPath, importPath)
		var list []*node
		list, err = interp.gta(root, subRPath, importPath)
		if err != nil {
			return "", err
		}
		revisit[subRPath] = append(revisit[subRPath], list...)
	}

	// Revisit incomplete nodes where GTA could not complete.
	for _, nodes := range revisit {
		if err = interp.gtaRetry(nodes, importPath); err != nil {
			return "", err
		}
	}

	// Generate control flow graphs.
	for _, root := range rootNodes {
		var nodes []*node
		if nodes, err = interp.cfg(root, importPath); err != nil {
			return "", err
		}
		initNodes = append(initNodes, nodes...)
	}

	// Register source package in the interpreter. The package contains only
	// the global symbols in the package scope.
	interp.mutex.Lock()
	gs := interp.scopes[importPath]
	interp.srcPkg[importPath] = gs.sym
	interp.pkgNames[importPath] = pkgName

	interp.frame.mutex.Lock()
	interp.resizeFrame()
	interp.frame.mutex.Unlock()
	interp.mutex.Unlock()

	// Once all package sources have been parsed, execute entry points then init functions.
	for _, n := range rootNodes {
		if err = genRun(n); err != nil {
			return "", err
		}
		interp.run(n, nil)
	}

	// Wire and execute global vars in global scope gs.
	n, err := genGlobalVars(rootNodes, gs)
	if err != nil {
		return "", err
	}
	interp.run(n, nil)

	// Add main to list of functions to run, after all inits.
	if m := gs.sym[mainID]; pkgName == mainID && m != nil && skipTest {
		initNodes = append(initNodes, m.node)
	}

	for _, n := range initNodes {
		interp.run(n, interp.frame)
	}

	return pkgName, nil
}

func (interp *Interpreter) importSrcArchive(reader io.Reader, skipTest bool) (string, error) {
	var dir string
	var err error
	rPath := "."
	importPath := "/"

	// For relative import paths in the form "./xxx" or "../xxx", the initial
	// base path is the directory of the interpreter input file, or "." if no file
	// was provided.
	// In all other cases, absolute import paths are resolved from the GOPATH
	// and the nested "vendor" directories.
	if isPathRelative(importPath) {
		if rPath == "main" {
			rPath = "."
		}
		dir = filepath.Join(filepath.Dir(interp.name), rPath, importPath)
	} else {
		root, err := interp.rootFromSourceLocation(rPath)
		if err != nil {
			return "", err
		}
		if dir, rPath, err = pkgDir(&interp.context, root, importPath); err != nil {
			return "", err
		}
	}
	//dir := filepath.Join(rPath, importPath)
	interp.rdir[importPath] = true

	var initNodes []*node
	var rootNodes []*node
	revisit := make(map[string][]*node)

	var root *node
	var pkgName string

	uncompressedStream, err := gzip.NewReader(reader)
	if err != nil {
		return "", err
	}
	tarReader := tar.NewReader(uncompressedStream)

	// Parse source files.
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			fmt.Errorf("Not a tar file, %v", err)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			//TODO: Need to see if this is necessary to implement
		case tar.TypeReg:
			name := header.Name
			if skipFile(&interp.context, name, skipTest) {
				continue
			}

			name = filepath.Join(dir, name)
			var buf bytes.Buffer
			if _, err = buf.ReadFrom(tarReader); err != nil {
				return "", err
			}

			var pname string
			if pname, root, err = interp.ast(string(buf.Bytes()), name, false); err != nil {
				return "", err
			}
			if root == nil {
				continue
			}

			if interp.astDot {
				dotCmd := interp.dotCmd
				if dotCmd == "" {
					dotCmd = defaultDotCmd(name, "yaegi-ast-")
				}
				root.astDot(dotWriter(dotCmd), name)
			}
			if pkgName == "" {
				pkgName = pname
			} else if pkgName != pname && skipTest {
				return "", fmt.Errorf("found packages %s and %s in %s", pkgName, pname, dir)
			}
			rootNodes = append(rootNodes, root)

			subRPath := effectivePkg(rPath, importPath)
			var list []*node
			list, err = interp.gta(root, subRPath, importPath)
			if err != nil {
				return "", err
			}
			revisit[subRPath] = append(revisit[subRPath], list...)
		}

	}

	// Revisit incomplete nodes where GTA could not complete.
	for _, nodes := range revisit {
		if err = interp.gtaRetry(nodes, importPath); err != nil {
			return "", err
		}
	}

	// Generate control flow graphs.
	for _, root := range rootNodes {
		var nodes []*node
		if nodes, err = interp.cfg(root, importPath); err != nil {
			return "", err
		}
		initNodes = append(initNodes, nodes...)
	}

	// Register source package in the interpreter. The package contains only
	// the global symbols in the package scope.
	interp.mutex.Lock()
	gs := interp.scopes[importPath]
	interp.srcPkg[importPath] = gs.sym
	interp.pkgNames[importPath] = pkgName

	interp.frame.mutex.Lock()
	interp.resizeFrame()
	interp.frame.mutex.Unlock()
	interp.mutex.Unlock()

	// Once all package sources have been parsed, execute entry points then init functions.
	for _, n := range rootNodes {
		if err = genRun(n); err != nil {
			return "", err
		}
		interp.run(n, nil)
	}

	// Wire and execute global vars in global scope gs.
	n, err := genGlobalVars(rootNodes, gs)
	if err != nil {
		return "", err
	}
	interp.run(n, nil)

	// Add main to list of functions to run, after all inits.
	if m := gs.sym[mainID]; pkgName == mainID && m != nil && skipTest {
		initNodes = append(initNodes, m.node)
	}

	for _, n := range initNodes {
		interp.run(n, interp.frame)
	}

	return pkgName, nil
}

// rootFromSourceLocation returns the path to the directory containing the input
// Go file given to the interpreter, relative to $GOPATH/src.
// It is meant to be called in the case when the initial input is a main package.
func (interp *Interpreter) rootFromSourceLocation(rPath string) (string, error) {
	sourceFile := interp.name
	if rPath != "main" || !strings.HasSuffix(sourceFile, ".go") {
		return rPath, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	pkgDir := filepath.Join(wd, filepath.Dir(sourceFile))
	root := strings.TrimPrefix(pkgDir, filepath.Join(interp.context.GOPATH, "src")+"/")
	if root == wd {
		return "", fmt.Errorf("package location %s not in GOPATH", pkgDir)
	}
	return root, nil
}

// pkgDir returns the absolute path in filesystem for a package given its import path
// and the root of the subtree dependencies.
// pkgDir returns the absolute path in filesystem for a package given its name and
// the root of the subtree dependencies.
func pkgDir(ctx *build.Context, root, path string) (pdir string, proot string, err error) {
	rPath := filepath.Join(root, "vendor")
	dir := filepath.Join(ctx.GOPATH, "src", rPath, path)
	if _, err := os.Stat(dir); err == nil {
		return dir, rPath, nil // found!
	}

	dir = filepath.Join(ctx.GOPATH, "src", effectivePkg(root, path))
	if _, err := os.Stat(dir); err == nil {
		return dir, root, nil // found!
	}

	if len(root) == 0 {
		// for backwards compatibility behavior only use the 'normal' go
		// package location when current implementation fails to discover
		// the source.
		if pkg, err := ctx.Import(path, ".", build.FindOnly); err == nil {
			return pkg.Dir, pkg.Root, nil
		}

		return "", "", fmt.Errorf("unable to find source related to: %q", path)
	}

	return pkgDir(ctx, previousRoot(root), path)
}

const vendor = "vendor"

// Find the previous source root (vendor > vendor > ... > GOPATH).
func previousRoot(root string) string {
	splitRoot := strings.Split(root, string(filepath.Separator))

	var index int
	for i := len(splitRoot) - 1; i >= 0; i-- {
		if splitRoot[i] == "vendor" {
			index = i
			break
		}
	}

	if index == 0 {
		return ""
	}

	return filepath.Join(splitRoot[:index]...)
}

func effectivePkg(root, path string) string {
	splitRoot := strings.Split(root, string(filepath.Separator))
	splitPath := strings.Split(path, string(filepath.Separator))

	var result []string

	rootIndex := 0
	prevRootIndex := 0
	for i := 0; i < len(splitPath); i++ {
		part := splitPath[len(splitPath)-1-i]

		index := len(splitRoot) - 1 - rootIndex
		if index > 0 && part == splitRoot[index] && i != 0 {
			prevRootIndex = rootIndex
			rootIndex++
		} else if prevRootIndex == rootIndex {
			result = append(result, part)
		}
	}

	var frag string
	for i := len(result) - 1; i >= 0; i-- {
		frag = filepath.Join(frag, result[i])
	}

	return filepath.Join(root, frag)
}

// isPathRelative returns true if path starts with "./" or "../".
func isPathRelative(s string) bool {
	p := "." + string(filepath.Separator)
	return strings.HasPrefix(s, p) || strings.HasPrefix(s, "."+p)
}
