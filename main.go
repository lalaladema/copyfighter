package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	maxStructWidth = flag.Int64("max", 16, "maximum size in bytes a struct can be before by-value uses are flagged")
	wordSize       = flag.Int64("wordSize", 8, "word size to assume when calculation struct size")
	maxAlign       = flag.Int64("maxAlign", 8, "maximum word alignment to assume when calculating struct size")
)

func main() {
	log.SetPrefix("")
	log.SetFlags(0)
	flag.Parse()

	if flag.NArg() != 1 {
		log.Fatalf("usage: %s GO_PKG_DIR", os.Args[0])
	}
	p := flag.Arg(0)
	sites, fset, err := check(p, *maxStructWidth, *wordSize, *maxAlign)
	if err != nil {
		log.Fatal(err)
	}
	printSites(sites, fset, os.Stdout)
	if len(sites) > 0 {
		os.Exit(2)
	}

}

func check(p string, maxStructWidth, wordSize, maxAlign int64) ([]copySite, *token.FileSet, error) {
	fset := token.NewFileSet()

	_, err := os.Stat(p)
	switch {
	case os.IsNotExist(err):
		// File doesn't exist, probably a Go import path
		pkgs, err := parseGoPkg(p, fset)
		if err != nil {
			return nil, nil, err
		}
		sites := []copySite{}
		for _, pkg := range pkgs {
			s, err := checkPkg(pkg, fset, maxStructWidth, wordSize, maxAlign)
			if err != nil {
				return nil, nil, err
			}
			sites = append(sites, s...)
		}
		return sites, fset, nil
	case err == nil:
		// File exists, parses as such
		pkg, err := parsePkgDir(p, fset)
		if err != nil {
			return nil, nil, err
		}
		sites, err := checkPkg(pkg, fset, maxStructWidth, wordSize, maxAlign)
		if err != nil {
			return nil, nil, err
		}
		return sites, fset, nil
	default:
		return nil, nil, err
	}
}

func parsePkgDir(p string, fset *token.FileSet) (*ast.Package, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return nil, fmt.Errorf("unable to stat file %#v: %s", p, err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%#v is not a directory", p)
	}

	mp, err := parser.ParseDir(fset, p, nil, 0)
	if err != nil {
		return nil, fmt.Errorf("unable to parse package at %#v: %s", p, err)
	}
	if len(mp) != 1 {
		var ps []string
		for _, pkg := range mp {
			ps = append(ps, pkg.Name)
		}
		return nil, fmt.Errorf("more than one package found in %#v: %s", p, strings.Join(ps, ","))
	}
	var pkg *ast.Package
	for _, v := range mp {
		pkg = v
	}
	return pkg, nil
}

func pathToRegexp(p string) *regexp.Regexp {
	re := regexp.QuoteMeta(p)
	re = strings.Replace(re, `\.\.\.`, `.*`, -1)
	// Special case: foo/... matches foo too.
	if strings.HasSuffix(re, `/.*`) {
		re = re[:len(re)-len(`/.*`)] + `(/.*)?`
	}
	return regexp.MustCompile(`^` + re + `$`)
}

func parseGoPkg(p string, fset *token.FileSet) ([]*ast.Package, error) {
	p = filepath.Clean(p)
	dirs := []string{}
	re := pathToRegexp(p)
	buildContext := build.Default
	for _, src := range buildContext.SrcDirs() {
		src = filepath.Clean(src) + string(filepath.Separator)
		root := src
		filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
			if err != nil || !fi.IsDir() || path == src {
				return nil
			}

			// Avoid .foo, _foo, and testdata directory trees.
			_, elem := filepath.Split(path)
			if strings.HasPrefix(elem, ".") || strings.HasPrefix(elem, "_") || elem == "testdata" {
				return filepath.SkipDir
			}
			name := filepath.ToSlash(path[len(src):])
			if re.MatchString(name) {
				dirs = append(dirs, path)
			}
			return nil
		})
	}

	pkgs := []*ast.Package{}
	for _, d := range dirs {
		_, err := buildContext.ImportDir(d, 0)
		if err != nil {
			if _, noGo := err.(*build.NoGoError); noGo {
				continue
			}
			return nil, fmt.Errorf("unable to build code in %#v: %s", d, err)
		}
		pkg, err := parsePkgDir(d, fset)
		if err != nil {
			return nil, err
		}
		pkgs = append(pkgs, pkg)
	}
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("unable to find packages matching %#v", p)
	}

	return pkgs, nil
}

func checkPkg(pkg *ast.Package, fset *token.FileSet, maxWidth, wordSize, maxAlign int64) ([]copySite, error) {
	sizes := &types.StdSizes{WordSize: wordSize, MaxAlign: maxAlign}
	info := &types.Info{
		// Types is required to prevent duplicates, it seems, in Defs.
		Types: make(map[ast.Expr]types.TypeAndValue),
		Defs:  make(map[*ast.Ident]types.Object),
	}
	conf := &types.Config{
		Importer:                 importer.Default(),
		DisableUnusedImportCheck: true,
		Sizes: sizes,
	}
	files := []*ast.File{}
	for _, f := range pkg.Files {
		files = append(files, f)
	}

	_, err := conf.Check("", fset, files, info)
	if err != nil {
		return nil, fmt.Errorf("unable to type check package %#v: %s", pkg.Name, err)
	}

	wideStructs := make(map[string]bool)

	funcs := []*types.Func{}
	for _, obj := range info.Defs {
		if tn, ok := obj.(*types.TypeName); ok {
			if sizes.Sizeof(tn.Type()) > maxWidth {
				wideStructs[tn.Id()] = true
			}
		}
		if f, ok := obj.(*types.Func); ok {
			funcs = append(funcs, f)
		}
	}

	sites := findCopySites(funcs, wideStructs)

	return sites, nil
}

// findCopySites returns a slice of copySites that represent Go function calls
// that use a large struct without a pointer to it. The wideStructs argument is
// a map of the struct's TypeName id to its TypeName object.
func findCopySites(funcs []*types.Func, wideStructs map[string]bool) []copySite {
	sites := []copySite{}
	for _, f := range funcs {
		s := f.Type().(*types.Signature)
		shouldBe := []string{}

		// If the func is a method, check the receiver
		if s.Recv() != nil {
			rt := s.Recv().Type()
			if isWideStructTyped(rt, wideStructs) {
				shouldBe = append(shouldBe, "receiver")
			}
		}

		params := s.Params()
		for i := 0; i < params.Len(); i++ {
			v := params.At(i)
			if isWideStructTyped(v.Type(), wideStructs) {
				name := v.Name()
				parameter := "parameter"
				if name != "" {
					parameter = fmt.Sprintf("parameter '%s'", name)
				}
				shouldBe = append(shouldBe,
					fmt.Sprintf("%s at index %d", parameter, i))
			}
		}

		results := s.Results()
		for i := 0; i < results.Len(); i++ {
			v := results.At(i)
			if isWideStructTyped(v.Type(), wideStructs) {
				shouldBe = append(shouldBe,
					fmt.Sprintf("return value '%s' at index %d", v.Type(), i))
			}
		}
		if len(shouldBe) > 0 {
			sites = append(sites, copySite{f, shouldBe})
		}
	}
	return sites
}

func printSites(sites []copySite, fset *token.FileSet, w io.Writer) {
	sort.Sort(sortedCopySites{sites: sites, fset: fset})
	for _, site := range sites {
		f := site.fun
		shouldBe := site.shouldBe
		sb := sentence(shouldBe)
		msg := "should be made into"
		if len(shouldBe) > 1 {
			msg += " pointers"
		} else {
			msg += " a pointer"
		}
		pos := site.fun.Pos()
		file := fset.File(pos)
		position := file.Position(pos)
		fmt.Fprintf(w, "%s:%d:%d: %s %s (%s)\n", file.Name(), position.Line, position.Column, sb, msg, f)
	}
}

type copySite struct {
	fun      *types.Func
	shouldBe []string
}

// sortedCopySites sorts copySites as ordered by the filename, line, and column
// the func was found at.
type sortedCopySites struct {
	sites []copySite
	fset  *token.FileSet
}

func (s sortedCopySites) Len() int {
	return len(s.sites)
}
func (s sortedCopySites) Swap(i, j int) {
	s.sites[i], s.sites[j] = s.sites[j], s.sites[i]
}

func (s sortedCopySites) Less(i, j int) bool {
	left := s.fset.Position(s.sites[i].fun.Pos())
	right := s.fset.Position(s.sites[j].fun.Pos())

	if left.Filename != right.Filename {
		return left.Filename < right.Filename
	}
	if left.Line != right.Line {
		return left.Line < right.Line
	}
	return left.Column < right.Column
}

// isWideStructTyped returns true if the given type is a struct (not a pointer to
// a struct) that is in wideStructs.
func isWideStructTyped(t types.Type, wideStructs map[string]bool) bool {
	if named, ok := t.(*types.Named); ok {
		return wideStructs[named.Obj().Id()]
	}
	return false
}

func sentence(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	if len(parts) == 1 {
		return parts[0]
	}
	last := len(parts) - 1
	return strings.Join(parts[:last], ", ") + ", and " + parts[last]
}
