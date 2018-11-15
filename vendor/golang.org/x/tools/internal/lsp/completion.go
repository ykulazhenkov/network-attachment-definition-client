package lsp

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/token"
	"go/types"
	"strings"

	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/internal/lsp/protocol"
	"golang.org/x/tools/internal/lsp/source"
)

func completion(v *source.View, uri protocol.DocumentURI, pos protocol.Position) (items []protocol.CompletionItem, err error) {
	f := v.GetFile(source.URI(uri))
	if err != nil {
		return nil, err
	}
	tok, err := f.GetToken()
	if err != nil {
		return nil, err
	}
	p := fromProtocolPosition(tok, pos)
	file, err := f.GetAST() // Use p to prune the AST?
	if err != nil {
		return nil, err
	}
	pkg, err := f.GetPackage()
	if err != nil {
		return nil, err
	}
	items, _, err = completions(v.Config.Fset, file, p, pkg.Types, pkg.TypesInfo)
	return items, err
}

// Completions returns the map of possible candidates for completion,
// given a position, a file AST, and type information. The prefix is
// computed based on the preceding identifier and can be used by the
// client to score the quality of the completion. For instance, some
// clients may tolerate imperfect matches as valid completion results,
// since users may make typos.
func completions(fset *token.FileSet, file *ast.File, pos token.Pos, pkg *types.Package, info *types.Info) (completions []protocol.CompletionItem, prefix string, err error) {
	path, _ := astutil.PathEnclosingInterval(file, pos, pos)
	if path == nil {
		return nil, "", fmt.Errorf("cannot find node enclosing position")
	}
	// If the position is not an identifier but immediately follows
	// an identifier or selector period (as is common when
	// requesting a completion), use the path to the preceding node.
	if _, ok := path[0].(*ast.Ident); !ok {
		if p, _ := astutil.PathEnclosingInterval(file, pos-1, pos-1); p != nil {
			switch p[0].(type) {
			case *ast.Ident, *ast.SelectorExpr:
				path = p // use preceding ident/selector
			}
		}
	}

	expectedTyp := expectedType(path, pos, info)
	enclosing := enclosingFunc(path, pos, info)
	pkgStringer := qualifier(file, pkg, info)

	seen := make(map[types.Object]bool)
	const stdWeight = 1 // default rank for a completion result

	// found adds a candidate completion.
	// Only the first candidate of a given name is considered.
	found := func(obj types.Object, weight float32) {
		if obj.Pkg() != nil && obj.Pkg() != pkg && !obj.Exported() {
			return // inaccessible
		}
		if !seen[obj] {
			seen[obj] = true
			if expectedTyp != nil && matchingTypes(expectedTyp, obj.Type()) {
				weight *= 10
			}
			item := formatCompletion(obj, pkgStringer, weight, func(v *types.Var) bool {
				return isParam(enclosing, v)
			})
			completions = append(completions, item)
		}
	}

	// selector finds completions for
	// the specified selector expression.
	// TODO(rstambler): Set the prefix filter correctly for selectors.
	selector := func(sel *ast.SelectorExpr) error {
		// Is sel a qualified identifier?
		if id, ok := sel.X.(*ast.Ident); ok {
			if pkgname, ok := info.Uses[id].(*types.PkgName); ok {
				// Enumerate package members.
				// TODO(adonovan): can Imported() be nil?
				scope := pkgname.Imported().Scope()
				// TODO testcase: bad import
				for _, name := range scope.Names() {
					found(scope.Lookup(name), stdWeight)
				}
				return nil
			}
		}

		// Inv: sel is a true selector.
		tv, ok := info.Types[sel.X]
		if !ok {
			var buf bytes.Buffer
			format.Node(&buf, fset, sel.X) // TODO check for error
			return fmt.Errorf("cannot resolve %s", &buf)
		}

		// methods of T
		mset := types.NewMethodSet(tv.Type)
		for i := 0; i < mset.Len(); i++ {
			found(mset.At(i).Obj(), stdWeight)
		}

		// methods of *T
		if tv.Addressable() && !types.IsInterface(tv.Type) && !isPointer(tv.Type) {
			mset := types.NewMethodSet(types.NewPointer(tv.Type))
			for i := 0; i < mset.Len(); i++ {
				found(mset.At(i).Obj(), stdWeight)
			}
		}

		// fields of T
		for _, f := range fieldSelections(tv.Type) {
			found(f, stdWeight)
		}

		return nil
	}

	// lexical finds completions in the lexical environment.
	lexical := func(path []ast.Node) {
		var scopes []*types.Scope // scopes[i], where i<len(path), is the possibly nil Scope of path[i].
		for _, n := range path {
			switch node := n.(type) {
			case *ast.FuncDecl:
				n = node.Type
			case *ast.FuncLit:
				n = node.Type
			}
			scopes = append(scopes, info.Scopes[n])
		}
		scopes = append(scopes, pkg.Scope(), types.Universe)

		// Process scopes innermost first.
		for i, scope := range scopes {
			if scope == nil {
				continue
			}
			for _, name := range scope.Names() {
				declScope, obj := scope.LookupParent(name, pos)
				if declScope != scope {
					continue // Name was declared in some enclosing scope, or not at all.
				}
				// If obj's type is invalid, find the AST node that defines the lexical block
				// containing the declaration of obj. Don't resolve types for packages.
				if _, ok := obj.(*types.PkgName); !ok && obj.Type() == types.Typ[types.Invalid] {
					// Match the scope to its ast.Node. If the scope is the package scope,
					// use the *ast.File as the starting node.
					var node ast.Node
					if i < len(path) {
						node = path[i]
					} else if i == len(path) { // use the *ast.File for package scope
						node = path[i-1]
					}
					if node != nil {
						if resolved := resolveInvalid(obj, node, info); resolved != nil {
							obj = resolved
						}
					}
				}

				score := float32(stdWeight)
				// Rank builtins significantly lower than other results.
				if scope == types.Universe {
					score *= 0.1
				}
				found(obj, score)
			}
		}
	}

	// complit finds completions for field names inside a composite literal.
	// It reports whether the node was handled as part of a composite literal.
	complit := func(node ast.Node) bool {
		var lit *ast.CompositeLit

		switch n := node.(type) {
		case *ast.CompositeLit:
			// The enclosing node will be a composite literal if the user has just
			// opened the curly brace (e.g. &x{<>) or the completion request is triggered
			// from an already completed composite literal expression (e.g. &x{foo: 1, <>})
			//
			// If the cursor position is within a key-value expression inside the composite
			// literal, we try to determine if it is before or after the colon. If it is before
			// the colon, we return field completions. If the cursor does not belong to any
			// expression within the composite literal, we show composite literal completions.
			var expr ast.Expr
			for _, e := range n.Elts {
				if e.Pos() <= pos && pos < e.End() {
					expr = e
					break
				}
			}
			lit = n
			// If the position belongs to a key-value expression and is after the colon,
			// don't show composite literal completions.
			if kv, ok := expr.(*ast.KeyValueExpr); ok && pos > kv.Colon {
				lit = nil
			}
		case *ast.KeyValueExpr:
			// If the enclosing node is a key-value expression (e.g. &x{foo: <>}),
			// we show composite literal completions if the cursor position is before the colon.
			if len(path) > 1 && pos < n.Colon {
				if l, ok := path[1].(*ast.CompositeLit); ok {
					lit = l
				}
			}
		case *ast.Ident:
			// If the enclosing node is an identifier, it can either be an identifier that is
			// part of a composite literal (e.g. &x{fo<>}), or it can be an identifier that is
			// part of a key-value expression, which is part of a composite literal (e.g. &x{foo: ba<>).
			// We handle both of these cases, showing composite literal completions only if
			// the cursor position for the key-value expression is before the colon.
			if len(path) > 1 {
				if l, ok := path[1].(*ast.CompositeLit); ok {
					lit = l
				} else if len(path) > 2 {
					if l, ok := path[2].(*ast.CompositeLit); ok {
						// Confirm that cursor position is inside curly braces.
						if l.Lbrace <= pos && pos <= l.Rbrace {
							lit = l
							if kv, ok := path[1].(*ast.KeyValueExpr); ok {
								if pos > kv.Colon {
									lit = nil
								}
							}
						}
					}
				}
			}
		}
		if lit == nil {
			return false
		}
		// Mark fields that have already been set, apart from the current field.
		hasKeys := false // true if the composite literal already has key-value pairs
		addedFields := make(map[*types.Var]bool)
		for _, el := range lit.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				hasKeys = true
				if kv.Pos() <= pos && pos <= kv.End() {
					continue
				}
				if key, ok := kv.Key.(*ast.Ident); ok {
					if used, ok := info.Uses[key]; ok {
						if usedVar, ok := used.(*types.Var); ok {
							addedFields[usedVar] = true
						}
					}
				}
			}
		}
		// If the underlying type of the composite literal is a struct,
		// we show completions for the fields of this struct.
		if tv, ok := info.Types[lit]; ok {
			var structPkg *types.Package // package containing the struct type declaration
			if s, ok := tv.Type.Underlying().(*types.Struct); ok {
				for i := 0; i < s.NumFields(); i++ {
					field := s.Field(i)
					if i == 0 {
						structPkg = field.Pkg()
					}
					if !addedFields[field] {
						found(field, stdWeight*10)
					}
				}
				// Add lexical completions if the user hasn't typed a key value expression
				// and if the struct fields are defined in the same package as the user is in.
				if !hasKeys && structPkg == pkg {
					lexical(path)
				}
				return true
			}
		}
		return false
	}

	if complit(path[0]) {
		return completions, prefix, nil
	}

	switch n := path[0].(type) {
	case *ast.Ident:
		// Set the filter prefix.
		prefix = n.Name[:pos-n.Pos()]

		// Is this the Sel part of a selector?
		if sel, ok := path[1].(*ast.SelectorExpr); ok && sel.Sel == n {
			if err := selector(sel); err != nil {
				return nil, prefix, err
			}
		} else {
			// reject defining identifiers
			if obj, ok := info.Defs[n]; ok {
				if v, ok := obj.(*types.Var); ok && v.IsField() {
					// An anonymous field is also a reference to a type.
				} else {
					of := ""
					if obj != nil {
						qual := types.RelativeTo(pkg)
						of += ", of " + types.ObjectString(obj, qual)
					}
					return nil, "", fmt.Errorf("this is a definition%s", of)
				}
			}

			lexical(path)
		}

	// Support completions when no letters of the function name have been
	// typed yet, but the parens are there:
	//   recv.‸(arg)
	case *ast.TypeAssertExpr:
		// Create a fake selector expression.
		if err := selector(&ast.SelectorExpr{X: n.X}); err != nil {
			return nil, prefix, err
		}

	case *ast.SelectorExpr:
		if err := selector(n); err != nil {
			return nil, prefix, err
		}

	default:
		// TODO(adonovan): a lexical query may not be what the
		// user expects when completing after the period of a
		// type assertion.

		lexical(path)
	}

	return completions, prefix, nil
}

// qualifier returns a function that appropriately formats a types.PkgName appearing in q.file.
func qualifier(f *ast.File, pkg *types.Package, info *types.Info) types.Qualifier {
	// Construct mapping of import paths to their defined or implicit names.
	imports := make(map[*types.Package]string)
	for _, imp := range f.Imports {
		var obj types.Object
		if imp.Name != nil {
			obj = info.Defs[imp.Name]
		} else {
			obj = info.Implicits[imp]
		}
		if pkgname, ok := obj.(*types.PkgName); ok {
			imports[pkgname.Imported()] = pkgname.Name()
		}
	}
	// Define qualifier to replace full package paths with names of the imports.
	return func(pkg *types.Package) string {
		if pkg == pkg {
			return ""
		}
		if name, ok := imports[pkg]; ok {
			return name
		}
		return pkg.Name()
	}
}

// enclosingFunc returns the signature of the function enclosing the position.
func enclosingFunc(path []ast.Node, pos token.Pos, info *types.Info) *types.Signature {
	for _, node := range path {
		switch t := node.(type) {
		case *ast.FuncDecl:
			if obj, ok := info.Defs[t.Name]; ok {
				return obj.Type().(*types.Signature)
			}
		case *ast.FuncLit:
			if typ, ok := info.Types[t]; ok {
				return typ.Type.(*types.Signature)
			}
		}
	}
	return nil
}

// formatCompletion returns the label, details, and kind for a types.Object,
// fitting the format of a LSP completion item.
func formatCompletion(obj types.Object, qualifier types.Qualifier, score float32, isParam func(*types.Var) bool) protocol.CompletionItem {
	label := obj.Name()
	detail := types.TypeString(obj.Type(), qualifier)

	var kind protocol.CompletionItemKind

	switch o := obj.(type) {
	case *types.TypeName:
		detail, kind = formatType(o.Type(), qualifier)
		if obj.Parent() == types.Universe {
			detail = ""
		}
	case *types.Const:
		if obj.Parent() == types.Universe {
			detail = ""
		} else {
			val := o.Val().ExactString()
			if !strings.Contains(val, "\\n") { // skip any multiline constants
				label += " = " + o.Val().ExactString()
			}
		}
		kind = protocol.ConstantCompletion
	case *types.Var:
		if _, ok := o.Type().(*types.Struct); ok {
			detail = "struct{...}" // for anonymous structs
		}
		if o.IsField() {
			kind = protocol.FieldCompletion
		} else if isParam(o) {
			kind = protocol.TypeParameterCompletion
		} else {
			kind = protocol.VariableCompletion
		}
	case *types.Func:
		if sig, ok := o.Type().(*types.Signature); ok {
			label += formatParams(sig.Params(), sig.Variadic(), qualifier)
			detail = strings.Trim(types.TypeString(sig.Results(), qualifier), "()")
			kind = protocol.FunctionCompletion
			if sig.Recv() != nil {
				kind = protocol.MethodCompletion
			}
		}
	case *types.Builtin:
		item, ok := builtinDetails[obj.Name()]
		if !ok {
			break
		}
		label, detail = item.label, item.detail
		kind = protocol.FunctionCompletion
	case *types.PkgName:
		kind = protocol.ModuleCompletion // package??
		detail = fmt.Sprintf("\"%s\"", o.Imported().Path())
	case *types.Nil:
		kind = protocol.VariableCompletion
		detail = ""
	}

	detail = strings.TrimPrefix(detail, "untyped ")

	return protocol.CompletionItem{
		Label:  label,
		Detail: detail,
		Kind:   float64(kind),
	}
}

// formatType returns the detail and kind for an object of type *types.TypeName.
func formatType(typ types.Type, qualifier types.Qualifier) (detail string, kind protocol.CompletionItemKind) {
	if types.IsInterface(typ) {
		detail = "interface{...}"
		kind = protocol.InterfaceCompletion
	} else if _, ok := typ.(*types.Struct); ok {
		detail = "struct{...}"
		kind = protocol.StructCompletion
	} else if typ != typ.Underlying() {
		detail, kind = formatType(typ.Underlying(), qualifier)
	} else {
		detail = types.TypeString(typ, qualifier)
		kind = protocol.TypeParameterCompletion // ???
	}
	return detail, kind
}

func formatParams(t *types.Tuple, variadic bool, qualifier types.Qualifier) string {
	var b strings.Builder
	b.WriteByte('(')
	for i := 0; i < t.Len(); i++ {
		if i > 0 {
			b.WriteString(", ")
		}
		el := t.At(i)
		typ := types.TypeString(el.Type(), qualifier)
		// Handle a variadic parameter (can only be the final parameter).
		if variadic && i == t.Len()-1 {
			typ = strings.Replace(typ, "[]", "...", 1)
		}
		fmt.Fprintf(&b, "%v %v", el.Name(), typ)
	}
	b.WriteByte(')')
	return b.String()
}

func isParam(sig *types.Signature, v *types.Var) bool {
	if sig == nil {
		return false
	}
	for i := 0; i < sig.Params().Len(); i++ {
		if sig.Params().At(i) == v {
			return true
		}
	}
	return false
}

// expectedType returns the expected type for an expression at the query position.
func expectedType(path []ast.Node, pos token.Pos, info *types.Info) types.Type {
	for i, node := range path {
		if i == 2 {
			break
		}
		switch expr := node.(type) {
		case *ast.BinaryExpr:
			// Determine if query position comes from left or right of op.
			e := expr.X
			if pos < expr.OpPos {
				e = expr.Y
			}
			if tv, ok := info.Types[e]; ok {
				return tv.Type
			}
		case *ast.AssignStmt:
			// Only rank completions if you are on the right side of the token.
			if pos <= expr.TokPos {
				break
			}
			i := exprAtPos(pos, expr.Rhs)
			if i >= len(expr.Lhs) {
				i = len(expr.Lhs) - 1
			}
			if tv, ok := info.Types[expr.Lhs[i]]; ok {
				return tv.Type
			}
		case *ast.CallExpr:
			if tv, ok := info.Types[expr.Fun]; ok {
				if sig, ok := tv.Type.(*types.Signature); ok {
					if sig.Params().Len() == 0 {
						return nil
					}
					i := exprAtPos(pos, expr.Args)
					// Make sure not to run past the end of expected parameters.
					if i >= sig.Params().Len() {
						i = sig.Params().Len() - 1
					}
					return sig.Params().At(i).Type()
				}
			}
		}
	}
	return nil
}

// matchingTypes reports whether actual is a good candidate type
// for a completion in a context of the expected type.
func matchingTypes(expected, actual types.Type) bool {
	// Use a function's return type as its type.
	if sig, ok := actual.(*types.Signature); ok {
		if sig.Results().Len() == 1 {
			actual = sig.Results().At(0).Type()
		}
	}
	return types.Identical(types.Default(expected), types.Default(actual))
}

// exprAtPos returns the index of the expression containing pos.
func exprAtPos(pos token.Pos, args []ast.Expr) int {
	for i, expr := range args {
		if expr.Pos() <= pos && pos <= expr.End() {
			return i
		}
	}
	return len(args)
}

// fieldSelections returns the set of fields that can
// be selected from a value of type T.
func fieldSelections(T types.Type) (fields []*types.Var) {
	// TODO(adonovan): this algorithm doesn't exclude ambiguous
	// selections that match more than one field/method.
	// types.NewSelectionSet should do that for us.

	seen := make(map[types.Type]bool) // for termination on recursive types
	var visit func(T types.Type)
	visit = func(T types.Type) {
		if !seen[T] {
			seen[T] = true
			if T, ok := deref(T).Underlying().(*types.Struct); ok {
				for i := 0; i < T.NumFields(); i++ {
					f := T.Field(i)
					fields = append(fields, f)
					if f.Anonymous() {
						visit(f.Type())
					}
				}
			}
		}
	}
	visit(T)

	return fields
}

func isPointer(T types.Type) bool {
	_, ok := T.(*types.Pointer)
	return ok
}

// deref returns a pointer's element type; otherwise it returns typ.
func deref(typ types.Type) types.Type {
	if p, ok := typ.Underlying().(*types.Pointer); ok {
		return p.Elem()
	}
	return typ
}

// resolveInvalid traverses the node of the AST that defines the scope
// containing the declaration of obj, and attempts to find a user-friendly
// name for its invalid type. The resulting Object and its Type are fake.
func resolveInvalid(obj types.Object, node ast.Node, info *types.Info) types.Object {
	// Construct a fake type for the object and return a fake object with this type.
	formatResult := func(expr ast.Expr) types.Object {
		var typename string
		switch t := expr.(type) {
		case *ast.SelectorExpr:
			typename = fmt.Sprintf("%s.%s", t.X, t.Sel)
		case *ast.Ident:
			typename = t.String()
		default:
			return nil
		}
		typ := types.NewNamed(types.NewTypeName(token.NoPos, obj.Pkg(), typename, nil), nil, nil)
		return types.NewVar(obj.Pos(), obj.Pkg(), obj.Name(), typ)
	}
	var resultExpr ast.Expr
	ast.Inspect(node, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.ValueSpec:
			for _, name := range n.Names {
				if info.Defs[name] == obj {
					resultExpr = n.Type
				}
			}
			return false
		case *ast.Field: // This case handles parameters and results of a FuncDecl or FuncLit.
			for _, name := range n.Names {
				if info.Defs[name] == obj {
					resultExpr = n.Type
				}
			}
			return false
		// TODO(rstambler): Handle range statements.
		default:
			return true
		}
	})
	return formatResult(resultExpr)
}

type itemDetails struct {
	label, detail string
}

var builtinDetails = map[string]itemDetails{
	"append": { // append(slice []T, elems ...T)
		label:  "append(slice []T, elems ...T)",
		detail: "[]T",
	},
	"cap": { // cap(v []T) int
		label:  "cap(v []T)",
		detail: "int",
	},
	"close": { // close(c chan<- T)
		label: "close(c chan<- T)",
	},
	"complex": { // complex(r, i float64) complex128
		label:  "complex(real, imag float64)",
		detail: "complex128",
	},
	"copy": { // copy(dst, src []T) int
		label:  "copy(dst, src []T)",
		detail: "int",
	},
	"delete": { // delete(m map[T]T1, key T)
		label: "delete(m map[K]V, key K)",
	},
	"imag": { // imag(c complex128) float64
		label:  "imag(complex128)",
		detail: "float64",
	},
	"len": { // len(v T) int
		label:  "len(T)",
		detail: "int",
	},
	"make": { // make(t T, size ...int) T
		label:  "make(t T, size ...int)",
		detail: "T",
	},
	"new": { // new(T) *T
		label:  "new(T)",
		detail: "*T",
	},
	"panic": { // panic(v interface{})
		label: "panic(interface{})",
	},
	"print": { // print(args ...T)
		label: "print(args ...T)",
	},
	"println": { // println(args ...T)
		label: "println(args ...T)",
	},
	"real": { // real(c complex128) float64
		label:  "real(complex128)",
		detail: "float64",
	},
	"recover": { // recover() interface{}
		label:  "recover()",
		detail: "interface{}",
	},
}
