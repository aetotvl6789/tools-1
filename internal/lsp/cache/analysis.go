// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cache

import (
	"context"
	"fmt"
	"go/ast"
	"go/types"
	"reflect"
	"sync"

	"golang.org/x/sync/errgroup"
	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/internal/analysisinternal"
	"golang.org/x/tools/internal/event"
	"golang.org/x/tools/internal/lsp/debug/tag"
	"golang.org/x/tools/internal/lsp/source"
	"golang.org/x/tools/internal/memoize"
	"golang.org/x/tools/internal/span"
)

func (s *snapshot) Analyze(ctx context.Context, id string, analyzers []*source.Analyzer) ([]*source.Diagnostic, error) {
	var roots []*actionHandle
	for _, a := range analyzers {
		if !a.IsEnabled(s.view) {
			continue
		}
		ah, err := s.actionHandle(ctx, PackageID(id), a.Analyzer)
		if err != nil {
			return nil, err
		}
		roots = append(roots, ah)
	}

	// Check if the context has been canceled before running the analyses.
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	var results []*source.Diagnostic
	for _, ah := range roots {
		diagnostics, _, err := ah.analyze(ctx, s)
		if err != nil {
			// Keep going if a single analyzer failed.
			event.Error(ctx, fmt.Sprintf("analyzer %q failed", ah.analyzer.Name), err)
			continue
		}
		results = append(results, diagnostics...)
	}
	return results, nil
}

type actionHandleKey source.Hash

// An action represents one unit of analysis work: the application of
// one analysis to one package. Actions form a DAG, both within a
// package (as different analyzers are applied, either in sequence or
// parallel), and across packages (as dependencies are analyzed).
type actionHandle struct {
	handle *memoize.Handle

	analyzer *analysis.Analyzer
	pkg      *pkg
}

type actionData struct {
	diagnostics  []*source.Diagnostic
	result       interface{}
	objectFacts  map[objectFactKey]analysis.Fact
	packageFacts map[packageFactKey]analysis.Fact
	err          error
}

type objectFactKey struct {
	obj types.Object
	typ reflect.Type
}

type packageFactKey struct {
	pkg *types.Package
	typ reflect.Type
}

func (s *snapshot) actionHandle(ctx context.Context, id PackageID, a *analysis.Analyzer) (*actionHandle, error) {
	// TODO(adonovan): opt: this block of code sequentially loads a package
	// (and all its dependencies), then sequentially creates action handles
	// for the direct dependencies (whose packages have by then been loaded
	// as a consequence of ph.check) which does a sequential recursion
	// down the action graph. Only once all that work is complete do we
	// put a handle in the cache. As with buildPackageHandle, this does
	// not exploit the natural parallelism in the problem, and the naive
	// use of concurrency would lead to an exponential amount of duplicated
	// work. We should instead use an atomically updated future cache
	// and a parallel graph traversal.
	ph, err := s.buildPackageHandle(ctx, id, source.ParseFull)
	if err != nil {
		return nil, err
	}
	if act := s.getActionHandle(id, ph.mode, a); act != nil {
		return act, nil
	}
	if len(ph.key) == 0 {
		return nil, fmt.Errorf("actionHandle: no key for package %s", id)
	}
	pkg, err := ph.check(ctx, s)
	if err != nil {
		return nil, err
	}

	// Add a dependency on each required analyzer.
	var deps []*actionHandle
	for _, req := range a.Requires {
		reqActionHandle, err := s.actionHandle(ctx, id, req)
		if err != nil {
			return nil, err
		}
		deps = append(deps, reqActionHandle)
	}

	// TODO(golang/go#35089): Re-enable this when we doesn't use ParseExported
	// mode for dependencies. In the meantime, disable analysis for dependencies,
	// since we don't get anything useful out of it.
	if false {
		// An analysis that consumes/produces facts
		// must run on the package's dependencies too.
		if len(a.FactTypes) > 0 {
			for _, importID := range ph.m.Deps {
				depActionHandle, err := s.actionHandle(ctx, importID, a)
				if err != nil {
					return nil, err
				}
				deps = append(deps, depActionHandle)
			}
		}
	}

	handle, release := s.store.Handle(buildActionKey(a, ph), func(ctx context.Context, arg interface{}) interface{} {
		snapshot := arg.(*snapshot)
		// Analyze dependencies first.
		results, err := execAll(ctx, snapshot, deps)
		if err != nil {
			return &actionData{
				err: err,
			}
		}
		return runAnalysis(ctx, snapshot, a, pkg, results)
	})

	act := &actionHandle{
		analyzer: a,
		pkg:      pkg,
		handle:   handle,
	}
	act = s.addActionHandle(act, release)
	return act, nil
}

func (act *actionHandle) analyze(ctx context.Context, snapshot *snapshot) ([]*source.Diagnostic, interface{}, error) {
	d, err := snapshot.awaitHandle(ctx, act.handle)
	if err != nil {
		return nil, nil, err
	}
	data, ok := d.(*actionData)
	if !ok {
		return nil, nil, fmt.Errorf("unexpected type for %s:%s", act.pkg.ID(), act.analyzer.Name)
	}
	if data == nil {
		return nil, nil, fmt.Errorf("unexpected nil analysis for %s:%s", act.pkg.ID(), act.analyzer.Name)
	}
	return data.diagnostics, data.result, data.err
}

func buildActionKey(a *analysis.Analyzer, ph *packageHandle) actionHandleKey {
	return actionHandleKey(source.Hashf("%p%s", a, ph.key[:]))
}

func (act *actionHandle) String() string {
	return fmt.Sprintf("%s@%s", act.analyzer, act.pkg.PkgPath())
}

func execAll(ctx context.Context, snapshot *snapshot, actions []*actionHandle) (map[*actionHandle]*actionData, error) {
	var mu sync.Mutex
	results := make(map[*actionHandle]*actionData)

	g, ctx := errgroup.WithContext(ctx)
	for _, act := range actions {
		act := act
		g.Go(func() error {
			v, err := snapshot.awaitHandle(ctx, act.handle)
			if err != nil {
				return err
			}
			data, ok := v.(*actionData)
			if !ok {
				return fmt.Errorf("unexpected type for %s: %T", act, v)
			}

			mu.Lock()
			defer mu.Unlock()
			results[act] = data

			return nil
		})
	}
	return results, g.Wait()
}

func runAnalysis(ctx context.Context, snapshot *snapshot, analyzer *analysis.Analyzer, pkg *pkg, deps map[*actionHandle]*actionData) (data *actionData) {
	data = &actionData{
		objectFacts:  make(map[objectFactKey]analysis.Fact),
		packageFacts: make(map[packageFactKey]analysis.Fact),
	}
	defer func() {
		if r := recover(); r != nil {
			data.err = fmt.Errorf("analysis %s for package %s panicked: %v", analyzer.Name, pkg.PkgPath(), r)
		}
	}()

	// Plumb the output values of the dependencies
	// into the inputs of this action.  Also facts.
	inputs := make(map[*analysis.Analyzer]interface{})

	for depHandle, depData := range deps {
		if depHandle.pkg == pkg {
			// Same package, different analysis (horizontal edge):
			// in-memory outputs of prerequisite analyzers
			// become inputs to this analysis pass.
			inputs[depHandle.analyzer] = depData.result
		} else if depHandle.analyzer == analyzer { // (always true)
			// Same analysis, different package (vertical edge):
			// serialized facts produced by prerequisite analysis
			// become available to this analysis pass.
			for key, fact := range depData.objectFacts {
				// Filter out facts related to objects
				// that are irrelevant downstream
				// (equivalently: not in the compiler export data).
				if !exportedFrom(key.obj, depHandle.pkg.types) {
					continue
				}
				data.objectFacts[key] = fact
			}
			for key, fact := range depData.packageFacts {
				// TODO: filter out facts that belong to
				// packages not mentioned in the export data
				// to prevent side channels.

				data.packageFacts[key] = fact
			}
		}
	}

	var syntax []*ast.File
	for _, cgf := range pkg.compiledGoFiles {
		syntax = append(syntax, cgf.File)
	}

	var diagnostics []*analysis.Diagnostic

	// Run the analysis.
	pass := &analysis.Pass{
		Analyzer:   analyzer,
		Fset:       snapshot.FileSet(),
		Files:      syntax,
		Pkg:        pkg.GetTypes(),
		TypesInfo:  pkg.GetTypesInfo(),
		TypesSizes: pkg.GetTypesSizes(),
		ResultOf:   inputs,
		Report: func(d analysis.Diagnostic) {
			// Prefix the diagnostic category with the analyzer's name.
			if d.Category == "" {
				d.Category = analyzer.Name
			} else {
				d.Category = analyzer.Name + "." + d.Category
			}
			diagnostics = append(diagnostics, &d)
		},
		ImportObjectFact: func(obj types.Object, ptr analysis.Fact) bool {
			if obj == nil {
				panic("nil object")
			}
			key := objectFactKey{obj, factType(ptr)}

			if v, ok := data.objectFacts[key]; ok {
				reflect.ValueOf(ptr).Elem().Set(reflect.ValueOf(v).Elem())
				return true
			}
			return false
		},
		ExportObjectFact: func(obj types.Object, fact analysis.Fact) {
			if obj.Pkg() != pkg.types {
				panic(fmt.Sprintf("internal error: in analysis %s of package %s: Fact.Set(%s, %T): can't set facts on objects belonging another package",
					analyzer, pkg.ID(), obj, fact))
			}
			key := objectFactKey{obj, factType(fact)}
			data.objectFacts[key] = fact // clobber any existing entry
		},
		ImportPackageFact: func(pkg *types.Package, ptr analysis.Fact) bool {
			if pkg == nil {
				panic("nil package")
			}
			key := packageFactKey{pkg, factType(ptr)}
			if v, ok := data.packageFacts[key]; ok {
				reflect.ValueOf(ptr).Elem().Set(reflect.ValueOf(v).Elem())
				return true
			}
			return false
		},
		ExportPackageFact: func(fact analysis.Fact) {
			key := packageFactKey{pkg.types, factType(fact)}
			data.packageFacts[key] = fact // clobber any existing entry
		},
		AllObjectFacts: func() []analysis.ObjectFact {
			facts := make([]analysis.ObjectFact, 0, len(data.objectFacts))
			for k := range data.objectFacts {
				facts = append(facts, analysis.ObjectFact{Object: k.obj, Fact: data.objectFacts[k]})
			}
			return facts
		},
		AllPackageFacts: func() []analysis.PackageFact {
			facts := make([]analysis.PackageFact, 0, len(data.packageFacts))
			for k := range data.packageFacts {
				facts = append(facts, analysis.PackageFact{Package: k.pkg, Fact: data.packageFacts[k]})
			}
			return facts
		},
	}
	analysisinternal.SetTypeErrors(pass, pkg.typeErrors)

	if pkg.IsIllTyped() {
		data.err = fmt.Errorf("analysis skipped due to errors in package")
		return data
	}
	data.result, data.err = pass.Analyzer.Run(pass)
	if data.err != nil {
		return data
	}

	if got, want := reflect.TypeOf(data.result), pass.Analyzer.ResultType; got != want {
		data.err = fmt.Errorf(
			"internal error: on package %s, analyzer %s returned a result of type %v, but declared ResultType %v",
			pass.Pkg.Path(), pass.Analyzer, got, want)
		return data
	}

	// disallow calls after Run
	pass.ExportObjectFact = func(obj types.Object, fact analysis.Fact) {
		panic(fmt.Sprintf("%s:%s: Pass.ExportObjectFact(%s, %T) called after Run", analyzer.Name, pkg.PkgPath(), obj, fact))
	}
	pass.ExportPackageFact = func(fact analysis.Fact) {
		panic(fmt.Sprintf("%s:%s: Pass.ExportPackageFact(%T) called after Run", analyzer.Name, pkg.PkgPath(), fact))
	}

	for _, diag := range diagnostics {
		srcDiags, err := analysisDiagnosticDiagnostics(snapshot, pkg, analyzer, diag)
		if err != nil {
			event.Error(ctx, "unable to compute analysis error position", err, tag.Category.Of(diag.Category), tag.Package.Of(pkg.ID()))
			continue
		}
		if ctx.Err() != nil {
			data.err = ctx.Err()
			return data
		}
		data.diagnostics = append(data.diagnostics, srcDiags...)
	}
	return data
}

// exportedFrom reports whether obj may be visible to a package that imports pkg.
// This includes not just the exported members of pkg, but also unexported
// constants, types, fields, and methods, perhaps belonging to other packages,
// that find there way into the API.
// This is an overapproximation of the more accurate approach used by
// gc export data, which walks the type graph, but it's much simpler.
//
// TODO(adonovan): do more accurate filtering by walking the type graph.
func exportedFrom(obj types.Object, pkg *types.Package) bool {
	switch obj := obj.(type) {
	case *types.Func:
		return obj.Exported() && obj.Pkg() == pkg ||
			obj.Type().(*types.Signature).Recv() != nil
	case *types.Var:
		return obj.Exported() && obj.Pkg() == pkg ||
			obj.IsField()
	case *types.TypeName, *types.Const:
		return true
	}
	return false // Nil, Builtin, Label, or PkgName
}

func factType(fact analysis.Fact) reflect.Type {
	t := reflect.TypeOf(fact)
	if t.Kind() != reflect.Ptr {
		panic(fmt.Sprintf("invalid Fact type: got %T, want pointer", fact))
	}
	return t
}

func (s *snapshot) DiagnosePackage(ctx context.Context, spkg source.Package) (map[span.URI][]*source.Diagnostic, error) {
	pkg := spkg.(*pkg)
	// Apply type error analyzers. They augment type error diagnostics with their own fixes.
	var analyzers []*source.Analyzer
	for _, a := range s.View().Options().TypeErrorAnalyzers {
		analyzers = append(analyzers, a)
	}
	var errorAnalyzerDiag []*source.Diagnostic
	if pkg.HasTypeErrors() {
		var err error
		errorAnalyzerDiag, err = s.Analyze(ctx, pkg.ID(), analyzers)
		if err != nil {
			// Keep going: analysis failures should not block diagnostics.
			event.Error(ctx, "type error analysis failed", err, tag.Package.Of(pkg.ID()))
		}
	}
	diags := map[span.URI][]*source.Diagnostic{}
	for _, diag := range pkg.diagnostics {
		for _, eaDiag := range errorAnalyzerDiag {
			if eaDiag.URI == diag.URI && eaDiag.Range == diag.Range && eaDiag.Message == diag.Message {
				// Type error analyzers just add fixes and tags. Make a copy,
				// since we don't own either, and overwrite.
				// The analyzer itself can't do this merge because
				// analysis.Diagnostic doesn't have all the fields, and Analyze
				// can't because it doesn't have the type error, notably its code.
				clone := *diag
				clone.SuggestedFixes = eaDiag.SuggestedFixes
				clone.Tags = eaDiag.Tags
				clone.Analyzer = eaDiag.Analyzer
				diag = &clone
			}
		}
		diags[diag.URI] = append(diags[diag.URI], diag)
	}
	return diags, nil
}
