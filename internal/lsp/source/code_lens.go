// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package source

import (
	"context"
	"go/ast"
	"go/token"
	"go/types"
	"log"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/tools/internal/lsp/command"
	"golang.org/x/tools/internal/lsp/protocol"
	"golang.org/x/tools/internal/span"
)

type LensFunc func(context.Context, Snapshot, FileHandle) ([]protocol.CodeLens, error)

// LensFuncs returns the supported lensFuncs for Go files.
func LensFuncs() map[command.Command]LensFunc {
	return map[command.Command]LensFunc{
		command.Generate:      goGenerateCodeLens,
		command.Test:          runTestCodeLens,
		command.RegenerateCgo: regenerateCgoLens,
		command.GCDetails:     toggleDetailsCodeLens,
	}
}

var (
	testRe      = regexp.MustCompile("^Test[^a-z]")
	benchmarkRe = regexp.MustCompile("^Benchmark[^a-z]")
)

func runTestCodeLens(ctx context.Context, snapshot Snapshot, fh FileHandle) ([]protocol.CodeLens, error) {
	codeLens := make([]protocol.CodeLens, 0)

	fns, err := TestsAndBenchmarks(ctx, snapshot, fh)
	if err != nil {
		return nil, err
	}
	puri := protocol.URIFromSpanURI(fh.URI())
	for _, fn := range fns.Tests {
		title := "run test"
		if fn.SubFn {
			title = "run sub-test"
		}
		cmd, err := command.NewTestCommand(title, puri, []string{fn.Name}, nil)
		if err != nil {
			return nil, err
		}
		rng := protocol.Range{Start: fn.Rng.Start, End: fn.Rng.Start}
		codeLens = append(codeLens, protocol.CodeLens{Range: rng, Command: cmd})
	}

	for _, fn := range fns.Benchmarks {
		title := "run benchmark"
		if fn.SubFn {
			title = "run sub-benchmark"
		}
		cmd, err := command.NewTestCommand(title, puri, nil, []string{fn.Name})
		if err != nil {
			return nil, err
		}
		rng := protocol.Range{Start: fn.Rng.Start, End: fn.Rng.Start}
		codeLens = append(codeLens, protocol.CodeLens{Range: rng, Command: cmd})
	}

	if len(fns.Benchmarks) > 0 {
		_, pgf, err := GetParsedFile(ctx, snapshot, fh, WidestPackage)
		if err != nil {
			return nil, err
		}
		// add a code lens to the top of the file which runs all benchmarks in the file
		rng, err := NewMappedRange(snapshot.FileSet(), pgf.Mapper, pgf.File.Package, pgf.File.Package).Range()
		if err != nil {
			return nil, err
		}
		var benches []string
		for _, fn := range fns.Benchmarks {
			benches = append(benches, fn.Name)
		}
		cmd, err := command.NewTestCommand("run file benchmarks", puri, nil, benches)
		if err != nil {
			return nil, err
		}
		codeLens = append(codeLens, protocol.CodeLens{Range: rng, Command: cmd})
	}
	return codeLens, nil
}

type testFn struct {
	Name  string
	Rng   protocol.Range
	SubFn bool
}

type testFns struct {
	Tests      []testFn
	Benchmarks []testFn
}

func TestsAndBenchmarks(ctx context.Context, snapshot Snapshot, fh FileHandle) (testFns, error) {
	var out testFns

	if !strings.HasSuffix(fh.URI().Filename(), "_test.go") {
		return out, nil
	}
	pkg, pgf, err := GetParsedFile(ctx, snapshot, fh, WidestPackage)
	if err != nil {
		return out, err
	}

	for _, d := range pgf.File.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}

		rng, err := NewMappedRange(snapshot.FileSet(), pgf.Mapper, d.Pos(), fn.End()).Range()
		if err != nil {
			return out, err
		}

		if matchTestFunc(fn, pkg, testRe, "T") {
			out.Tests = append(out.Tests, testFn{fn.Name.Name, rng, false})

			if funcs := findSubTestFuncs(fn.Name.Name, &funcLit{fn.Type, fn.Body}, snapshot, pgf.Mapper); len(funcs) != 0 {
				log.Println(funcs)
				out.Tests = append(out.Tests, funcs...)
			}
		}

		if matchTestFunc(fn, pkg, benchmarkRe, "B") {
			out.Benchmarks = append(out.Benchmarks, testFn{fn.Name.Name, rng, false})

			if funcs := findSubTestFuncs(fn.Name.Name, &funcLit{fn.Type, fn.Body}, snapshot, pgf.Mapper); len(funcs) != 0 {
				out.Benchmarks = append(out.Benchmarks, funcs...)
			}
		}
	}

	return out, nil
}

func matchTestFunc(fn *ast.FuncDecl, pkg Package, nameRe *regexp.Regexp, paramID string) bool {
	// Make sure that the function name matches a test function.
	if !nameRe.MatchString(fn.Name.Name) {
		return false
	}
	info := pkg.GetTypesInfo()
	if info == nil {
		return false
	}
	obj := info.ObjectOf(fn.Name)
	if obj == nil {
		return false
	}
	sig, ok := obj.Type().(*types.Signature)
	if !ok {
		return false
	}
	// Test functions should have only one parameter.
	if sig.Params().Len() != 1 {
		return false
	}

	// Check the type of the only parameter
	paramTyp, ok := sig.Params().At(0).Type().(*types.Pointer)
	if !ok {
		return false
	}
	named, ok := paramTyp.Elem().(*types.Named)
	if !ok {
		return false
	}
	namedObj := named.Obj()
	if namedObj.Pkg().Path() != "testing" {
		return false
	}
	return namedObj.Id() == paramID
}

type funcLit struct {
	Sign *ast.FuncType
	Body *ast.BlockStmt
}

func findSubTestFuncs(parentTestFuncName string, fn *funcLit, snapshot Snapshot, mapper *protocol.ColumnMapper) (sutTests []testFn) {
	addSubTest := func(subTestName *ast.BasicLit, subTestFn *funcLit, pos, end token.Pos) {
		testName, err := strconv.Unquote(subTestName.Value)
		if err != nil {
			// TODO
			return
		}
		testName = parentTestFuncName + "/" + testName

		rng, err := NewMappedRange(snapshot.FileSet(), mapper, pos, end).Range()
		if err != nil {
			// TODO
			return
		}
		sutTests = append(sutTests, testFn{testName, rng, true})

		sutTests = append(sutTests, findSubTestFuncs(testName, subTestFn, snapshot, mapper)...)
	}

	// TODO: remove duplicates, errors

	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		subTestFn := getSubTestFunc(fn.Sign, call)
		if subTestFn == nil {
			return false
		}

		switch subTestNameArg := call.Args[0].(type) {
		// TODO: const

		case *ast.BasicLit: // constant name
			addSubTest(subTestNameArg, subTestFn, call.Pos(), call.End())

		case *ast.SelectorExpr: // name from test case
			id, ok := subTestNameArg.X.(*ast.Ident)
			if !ok {
				break
			}
			testCases := getTestCases(id)
			if testCases == nil {
				break
			}

			subTestNameIndex, ok := bar(subTestNameArg.Sel, testCases)
			if !ok {
				break
			}

			for _, testCase := range testCases.Elts {
				testCaseParams, ok := testCase.(*ast.CompositeLit)
				if !ok {
					continue
				}

				var testAdded bool
				for i, param := range testCaseParams.Elts {
					if testAdded {
						break
					}

					switch param := param.(type) {
					case *ast.BasicLit:
						if i == subTestNameIndex {
							addSubTest(param, subTestFn, testCase.Pos(), testCase.End())
							testAdded = true
						}

					case *ast.KeyValueExpr:
						key, ok := param.Key.(*ast.Ident)
						if !ok {
							break
						}
						if subTestNameArg.Sel.Name != key.Name {
							break
						}

						value, ok := param.Value.(*ast.BasicLit)
						if !ok {
							break
						}
						addSubTest(value, subTestFn, testCase.Pos(), testCase.End())
						testAdded = true
					}
				}
			}
		}

		return true
	})

	return sutTests
}

func getSubTestFunc(parentFuncSign *ast.FuncType, call *ast.CallExpr) *funcLit {
	// TODO
	if len(call.Args) != 2 {
		return nil
	}

	methodCall, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || methodCall.Sel.Name != "Run" {
		return nil
	}

	receiver, ok := methodCall.X.(*ast.Ident)
	if !ok || receiver.Obj.Decl == nil {
		return nil
	}
	receiverDecl, ok := receiver.Obj.Decl.(*ast.Field)
	if !ok {
		return nil
	}

	// TODO
	if parentFuncSign.Params.List[0] != receiverDecl {
		return nil
	}

	switch subTestFunc := call.Args[1].(type) {
	case *ast.FuncLit:
		return &funcLit{subTestFunc.Type, subTestFunc.Body}

	case *ast.Ident:
		decl, ok := subTestFunc.Obj.Decl.(*ast.FuncDecl)
		if !ok {
			break
		}
		return &funcLit{decl.Type, decl.Body}
	}
	return nil
}

func getTestCases(id *ast.Ident) *ast.CompositeLit {
	if id.Obj.Decl == nil {
		return nil
	}
	decl, ok := id.Obj.Decl.(*ast.AssignStmt)
	if !ok || len(decl.Rhs) != 1 {
		return nil
	}

	switch expr := decl.Rhs[0].(type) {
	case *ast.CompositeLit: // TODO: tests := []struct{}{}
		return expr

	case *ast.UnaryExpr: // TODO: for _, tt := range []struct{}{} {}
		switch x := expr.X.(type) {
		case *ast.CompositeLit:
			return x
		case *ast.Ident:
			return getTestCases(x)
		}
	}

	return nil
}

// TODO: rename
func bar(subTestName *ast.Ident, testCases *ast.CompositeLit) (index int, ok bool) {
	testCasesType, ok := testCases.Type.(*ast.ArrayType)
	if !ok {
		return 0, false
	}
	testCaseType, ok := testCasesType.Elt.(*ast.StructType)
	if !ok {
		return 0, false
	}

	for i, f := range testCaseType.Fields.List {
		for j, name := range f.Names {
			if name.Name == subTestName.Name {
				return i + j, true
			}
		}
	}
	return 0, false
}

func goGenerateCodeLens(ctx context.Context, snapshot Snapshot, fh FileHandle) ([]protocol.CodeLens, error) {
	pgf, err := snapshot.ParseGo(ctx, fh, ParseFull)
	if err != nil {
		return nil, err
	}
	const ggDirective = "//go:generate"
	for _, c := range pgf.File.Comments {
		for _, l := range c.List {
			if !strings.HasPrefix(l.Text, ggDirective) {
				continue
			}
			rng, err := NewMappedRange(snapshot.FileSet(), pgf.Mapper, l.Pos(), l.Pos()+token.Pos(len(ggDirective))).Range()
			if err != nil {
				return nil, err
			}
			dir := protocol.URIFromSpanURI(span.URIFromPath(filepath.Dir(fh.URI().Filename())))
			nonRecursiveCmd, err := command.NewGenerateCommand("run go generate", command.GenerateArgs{Dir: dir, Recursive: false})
			if err != nil {
				return nil, err
			}
			recursiveCmd, err := command.NewGenerateCommand("run go generate ./...", command.GenerateArgs{Dir: dir, Recursive: true})
			if err != nil {
				return nil, err
			}
			return []protocol.CodeLens{
				{Range: rng, Command: recursiveCmd},
				{Range: rng, Command: nonRecursiveCmd},
			}, nil

		}
	}
	return nil, nil
}

func regenerateCgoLens(ctx context.Context, snapshot Snapshot, fh FileHandle) ([]protocol.CodeLens, error) {
	pgf, err := snapshot.ParseGo(ctx, fh, ParseFull)
	if err != nil {
		return nil, err
	}
	var c *ast.ImportSpec
	for _, imp := range pgf.File.Imports {
		if imp.Path.Value == `"C"` {
			c = imp
		}
	}
	if c == nil {
		return nil, nil
	}
	rng, err := NewMappedRange(snapshot.FileSet(), pgf.Mapper, c.Pos(), c.EndPos).Range()
	if err != nil {
		return nil, err
	}
	puri := protocol.URIFromSpanURI(fh.URI())
	cmd, err := command.NewRegenerateCgoCommand("regenerate cgo definitions", command.URIArg{URI: puri})
	if err != nil {
		return nil, err
	}
	return []protocol.CodeLens{{Range: rng, Command: cmd}}, nil
}

func toggleDetailsCodeLens(ctx context.Context, snapshot Snapshot, fh FileHandle) ([]protocol.CodeLens, error) {
	_, pgf, err := GetParsedFile(ctx, snapshot, fh, WidestPackage)
	if err != nil {
		return nil, err
	}
	rng, err := NewMappedRange(snapshot.FileSet(), pgf.Mapper, pgf.File.Package, pgf.File.Package).Range()
	if err != nil {
		return nil, err
	}
	puri := protocol.URIFromSpanURI(fh.URI())
	cmd, err := command.NewGCDetailsCommand("Toggle gc annotation details", puri)
	if err != nil {
		return nil, err
	}
	return []protocol.CodeLens{{Range: rng, Command: cmd}}, nil
}
