// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:generate go run resolver_gen.go

// The parsego package defines the [File] type, a wrapper around a go/ast.File
// that is useful for answering LSP queries. Notably, it bundles the
// *token.File and *protocol.Mapper necessary for token.Pos locations to and
// from UTF-16 LSP positions.
//
// Run `go generate` to update resolver.go from GOROOT.
package parsego

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"reflect"
	"slices"

	"golang.org/x/tools/go/ast/inspector"
	"golang.org/x/tools/gopls/internal/label"
	"golang.org/x/tools/gopls/internal/protocol"
	"golang.org/x/tools/gopls/internal/util/astutil"
	"golang.org/x/tools/gopls/internal/util/safetoken"
	internalastutil "golang.org/x/tools/internal/astutil"
	"golang.org/x/tools/internal/diff"
	"golang.org/x/tools/internal/event"
)

// Common parse modes; these should be reused wherever possible to increase
// cache hits.
const (
	// Header specifies that the main package declaration and imports are needed.
	// This is the mode used when attempting to examine the package graph structure.
	Header = parser.AllErrors | parser.ParseComments | parser.ImportsOnly | parser.SkipObjectResolution

	// Full specifies the full AST is needed.
	// This is used for files of direct interest where the entire contents must
	// be considered.
	Full = parser.AllErrors | parser.ParseComments | parser.SkipObjectResolution
)

// Parse parses a buffer of Go source, repairing the tree if necessary.
//
// The provided ctx is used only for logging.
func Parse(ctx context.Context, fset *token.FileSet, uri protocol.DocumentURI, src []byte, mode parser.Mode, purgeFuncBodies bool) (res *File, fixes []FixType) {
	if purgeFuncBodies {
		src = astutil.PurgeFuncBodies(src)
	}
	ctx, done := event.Start(ctx, "cache.ParseGoSrc", label.File.Of(uri.Path()))
	defer done()

	file, err := parser.ParseFile(fset, uri.Path(), src, mode)
	var parseErr scanner.ErrorList
	if err != nil {
		// We passed a byte slice, so the only possible error is a parse error.
		parseErr = err.(scanner.ErrorList)
	}
	// Inv: file != nil.

	tokenFile := func(file *ast.File) *token.File {
		return fset.File(file.FileStart)
	}

	tok := tokenFile(file)

	fixedSrc := false
	fixedAST := false
	// If there were parse errors, attempt to fix them up.
	if parseErr != nil {
		// Fix any badly parsed parts of the AST.
		astFixes := fixAST(file, tok, src)
		fixedAST = len(astFixes) > 0
		if fixedAST {
			fixes = append(fixes, astFixes...)
		}

		for i := range 10 {
			// Fix certain syntax errors that render the file unparseable.
			newSrc, srcFix := fixSrc(file, tok, src)
			if newSrc == nil {
				break
			}

			// If we thought there was something to fix 10 times in a row,
			// it is likely we got stuck in a loop somehow. Log out a diff
			// of the last changes we made to aid in debugging.
			if i == 9 {
				unified := diff.Unified("before", "after", string(src), string(newSrc))
				event.Log(ctx, fmt.Sprintf("fixSrc loop - last diff:\n%v", unified), label.File.Of(tok.Name()))
			}

			newFile, newErr := parser.ParseFile(fset, uri.Path(), newSrc, mode)
			assert(newFile != nil, "ParseFile returned nil") // I/O error can't happen

			// Maintain the original parseError so we don't try formatting the
			// doctored file.
			file = newFile
			src = newSrc
			tok = tokenFile(file)

			// Only now that we accept the fix do we record the src fix from above.
			fixes = append(fixes, srcFix)
			fixedSrc = true

			if newErr == nil {
				break // nothing to fix
			}

			// Note that fixedAST is reset after we fix src.
			astFixes = fixAST(file, tok, src)
			fixedAST = len(astFixes) > 0
			if fixedAST {
				fixes = append(fixes, astFixes...)
			}
		}
	}
	assert(file != nil, "nil *ast.File")

	// Provide a cursor for fast and convenient navigation.
	inspect := inspector.New([]*ast.File{file})
	curFile, _ := inspect.Root().FirstChild()
	_ = curFile.Node().(*ast.File)

	return &File{
		URI:      uri,
		Mode:     mode,
		Src:      src,
		fixedSrc: fixedSrc,
		fixedAST: fixedAST,
		File:     file,
		Tok:      tok,
		Cursor:   curFile,
		Mapper:   protocol.NewMapper(uri, src),
		ParseErr: parseErr,
	}, fixes
}

// fixAST inspects the AST and potentially modifies any *ast.BadStmts so that it can be
// type-checked more effectively.
//
// If fixAST returns true, the resulting AST is considered "fixed", meaning
// positions have been mangled, and type checker errors may not make sense.
func fixAST(n ast.Node, tok *token.File, src []byte) (fixes []FixType) {
	var err error
	internalastutil.PreorderStack(n, nil, func(n ast.Node, stack []ast.Node) bool {
		var parent ast.Node
		if len(stack) > 0 {
			parent = stack[len(stack)-1]
		}

		switch n := n.(type) {
		case *ast.BadStmt:
			if fixDeferOrGoStmt(n, parent, tok, src) {
				fixes = append(fixes, FixedDeferOrGo)
				// Recursively fix in our fixed node.
				moreFixes := fixAST(parent, tok, src)
				fixes = append(fixes, moreFixes...)
			} else {
				err = fmt.Errorf("unable to parse defer or go from *ast.BadStmt: %v", err)
			}
			return false
		case *ast.BadExpr:
			if fixArrayType(n, parent, tok, src) {
				fixes = append(fixes, FixedArrayType)
				// Recursively fix in our fixed node.
				moreFixes := fixAST(parent, tok, src)
				fixes = append(fixes, moreFixes...)
				return false
			}

			// Fix cases where parser interprets if/for/switch "init"
			// statement as "cond" expression, e.g.:
			//
			//   // "i := foo" is init statement, not condition.
			//   for i := foo
			//
			if fixInitStmt(n, parent, tok, src) {
				fixes = append(fixes, FixedInit)
			}
			return false
		case *ast.SelectorExpr:
			// Fix cases where a keyword prefix results in a phantom "_" selector, e.g.:
			//
			//   foo.var<> // want to complete to "foo.variance"
			//
			if fixPhantomSelector(n, tok, src) {
				fixes = append(fixes, FixedPhantomSelector)
			}
			return true

		case *ast.BlockStmt:
			switch parent.(type) {
			case *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt:
				// Adjust closing curly brace of empty switch/select
				// statements so we can complete inside them.
				if fixEmptySwitch(n, tok, src) {
					fixes = append(fixes, FixedEmptySwitch)
				}
			}

			return true
		default:
			return true
		}
	})
	return fixes
}

// TODO(rfindley): revert this intrumentation once we're certain the crash in
// #59097 is fixed.
type FixType int

const (
	noFix FixType = iota
	FixedCurlies
	FixedDanglingSelector
	FixedDeferOrGo
	FixedArrayType
	FixedInit
	FixedPhantomSelector
	FixedEmptySwitch
)

// fixSrc attempts to modify the file's source code to fix certain
// syntax errors that leave the rest of the file unparsed.
//
// fixSrc returns a non-nil result if and only if a fix was applied.
func fixSrc(f *ast.File, tf *token.File, src []byte) (newSrc []byte, fix FixType) {
	internalastutil.PreorderStack(f, nil, func(n ast.Node, stack []ast.Node) bool {
		if newSrc != nil {
			return false
		}

		switch n := n.(type) {
		case *ast.BlockStmt:
			parent := stack[len(stack)-1]
			newSrc = fixMissingCurlies(f, n, parent, tf, src)
			if newSrc != nil {
				fix = FixedCurlies
			}
		case *ast.SelectorExpr:
			newSrc = fixDanglingSelector(n, tf, src)
			if newSrc != nil {
				fix = FixedDanglingSelector
			}
		}

		return newSrc == nil
	})

	return newSrc, fix
}

// fixMissingCurlies adds in curly braces for block statements that
// are missing curly braces. For example:
//
//	if foo
//
// becomes
//
//	if foo {}
func fixMissingCurlies(f *ast.File, b *ast.BlockStmt, parent ast.Node, tok *token.File, src []byte) []byte {
	// If the "{" is already in the source code, there isn't anything to
	// fix since we aren't missing curlies.
	if b.Lbrace.IsValid() {
		braceOffset, err := safetoken.Offset(tok, b.Lbrace)
		if err != nil {
			return nil
		}
		if braceOffset < len(src) && src[braceOffset] == '{' {
			return nil
		}
	}

	parentLine := safetoken.Line(tok, parent.Pos())

	if parentLine >= tok.LineCount() {
		// If we are the last line in the file, no need to fix anything.
		return nil
	}

	// Insert curlies at the end of parent's starting line. The parent
	// is the statement that contains the block, e.g. *ast.IfStmt. The
	// block's Pos()/End() can't be relied upon because they are based
	// on the (missing) curly braces. We assume the statement is a
	// single line for now and try sticking the curly braces at the end.
	insertPos := tok.LineStart(parentLine+1) - 1

	// Scootch position backwards until it's not in a comment. For example:
	//
	// if foo<> // some amazing comment |
	// someOtherCode()
	//
	// insertPos will be located at "|", so we back it out of the comment.
	didSomething := true
	for didSomething {
		didSomething = false
		for _, c := range f.Comments {
			if c.Pos() < insertPos && insertPos <= c.End() {
				insertPos = c.Pos()
				didSomething = true
			}
		}
	}

	// Bail out if line doesn't end in an ident or ".". This is to avoid
	// cases like below where we end up making things worse by adding
	// curlies:
	//
	//   if foo &&
	//     bar<>
	switch precedingToken(insertPos, tok, src) {
	case token.IDENT, token.PERIOD:
		// ok
	default:
		return nil
	}

	var buf bytes.Buffer
	buf.Grow(len(src) + 3)
	offset, err := safetoken.Offset(tok, insertPos)
	if err != nil {
		return nil
	}
	buf.Write(src[:offset])

	// Detect if we need to insert a semicolon to fix "for" loop situations like:
	//
	//   for i := foo(); foo<>
	//
	// Just adding curlies is not sufficient to make things parse well.
	if fs, ok := parent.(*ast.ForStmt); ok {
		if _, ok := fs.Cond.(*ast.BadExpr); !ok {
			if xs, ok := fs.Post.(*ast.ExprStmt); ok {
				if _, ok := xs.X.(*ast.BadExpr); ok {
					buf.WriteByte(';')
				}
			}
		}
	}

	// Insert "{}" at insertPos.
	buf.WriteByte('{')
	buf.WriteByte('}')
	buf.Write(src[offset:])
	return buf.Bytes()
}

// fixEmptySwitch moves empty switch/select statements' closing curly
// brace down one line. This allows us to properly detect incomplete
// "case" and "default" keywords as inside the switch statement. For
// example:
//
//	switch {
//	def<>
//	}
//
// gets parsed like:
//
//	switch {
//	}
//
// Later we manually pull out the "def" token, but we need to detect
// that our "<>" position is inside the switch block. To do that we
// move the curly brace so it looks like:
//
//	switch {
//
//	}
//
// The resulting bool reports whether any fixing occurred.
func fixEmptySwitch(body *ast.BlockStmt, tok *token.File, src []byte) bool {
	// We only care about empty switch statements.
	if len(body.List) > 0 || !body.Rbrace.IsValid() {
		return false
	}

	// If the right brace is actually in the source code at the
	// specified position, don't mess with it.
	braceOffset, err := safetoken.Offset(tok, body.Rbrace)
	if err != nil {
		return false
	}
	if braceOffset < len(src) && src[braceOffset] == '}' {
		return false
	}

	braceLine := safetoken.Line(tok, body.Rbrace)
	if braceLine >= tok.LineCount() {
		// If we are the last line in the file, no need to fix anything.
		return false
	}

	// Move the right brace down one line.
	body.Rbrace = tok.LineStart(braceLine + 1)
	return true
}

// fixDanglingSelector inserts a real "_" selector expression in place
// of a phantom parser-inserted "_" selector so that the parser will
// not consume the following non-identifier token.
// For example:
//
//	func _() {
//		x.<>
//	}
//
// var x struct { i int }
//
// To fix completion at "<>", we insert a real "_" after the "." so the
// following declaration of "x" can be parsed and type checked
// normally.
func fixDanglingSelector(s *ast.SelectorExpr, tf *token.File, src []byte) []byte {
	if !isPhantomUnderscore(s.Sel, tf, src) {
		return nil
	}

	if !s.X.End().IsValid() {
		return nil
	}

	insertOffset, err := safetoken.Offset(tf, s.X.End())
	if err != nil {
		return nil
	}
	// Insert directly after the selector's ".".
	insertOffset++
	if src[insertOffset-1] != '.' {
		return nil
	}

	return slices.Concat(src[:insertOffset], []byte("_"), src[insertOffset:])
}

// fixPhantomSelector tries to fix selector expressions whose Sel is a
// phantom (parser-invented) "_". If the text after the '.' is a
// keyword, it updates Sel to a fake ast.Ident of that name. For
// example:
//
// foo.var
//
// yields a "_" selector instead of "var" since "var" is a keyword.
//
// TODO(rfindley): should this constitute an ast 'fix'?
//
// The resulting bool reports whether any fixing occurred.
func fixPhantomSelector(sel *ast.SelectorExpr, tf *token.File, src []byte) bool {
	if !isPhantomUnderscore(sel.Sel, tf, src) {
		return false
	}

	// Only consider selectors directly abutting the selector ".". This
	// avoids false positives in cases like:
	//
	//   foo. // don't think "var" is our selector
	//   var bar = 123
	//
	if sel.Sel.Pos() != sel.X.End()+1 {
		return false
	}

	maybeKeyword := readKeyword(sel.Sel.Pos(), tf, src)
	if maybeKeyword == "" {
		return false
	}

	return replaceNode(sel, sel.Sel, &ast.Ident{
		Name:    maybeKeyword,
		NamePos: sel.Sel.Pos(),
	})
}

// isPhantomUnderscore reports whether the given ident from a
// SelectorExpr.Sel was invented by the parser and is not present in
// source text. The parser creates a blank "_" identifier when the
// syntax (e.g. a selector) demands one but none is present. The fixer
// also inserts them.
func isPhantomUnderscore(id *ast.Ident, tok *token.File, src []byte) bool {
	switch id.Name {
	case "_": // go1.24 parser
		offset, err := safetoken.Offset(tok, id.Pos())
		return err == nil && offset < len(src) && src[offset] != '_'
	}
	return false // real
}

// fixInitStmt fixes cases where the parser misinterprets an
// if/for/switch "init" statement as the "cond" conditional. In cases
// like "if i := 0" the user hasn't typed the semicolon yet so the
// parser is looking for the conditional expression. However, "i := 0"
// are not valid expressions, so we get a BadExpr.
//
// The resulting bool reports whether any fixing occurred.
func fixInitStmt(bad *ast.BadExpr, parent ast.Node, tok *token.File, src []byte) bool {
	if !bad.Pos().IsValid() || !bad.End().IsValid() {
		return false
	}

	// Try to extract a statement from the BadExpr.
	start, end, err := safetoken.Offsets(tok, bad.Pos(), bad.End())
	if err != nil {
		return false
	}
	assert(end <= len(src), "offset overflow") // golang/go#72026
	stmtBytes := src[start:end]
	stmt, err := parseStmt(tok, bad.Pos(), stmtBytes)
	if err != nil {
		return false
	}

	// If the parent statement doesn't already have an "init" statement,
	// move the extracted statement into the "init" field and insert a
	// dummy expression into the required "cond" field.
	switch p := parent.(type) {
	case *ast.IfStmt:
		if p.Init != nil {
			return false
		}
		p.Init = stmt
		p.Cond = &ast.Ident{
			Name:    "_",
			NamePos: stmt.End(),
		}
		return true
	case *ast.ForStmt:
		if p.Init != nil {
			return false
		}
		p.Init = stmt
		p.Cond = &ast.Ident{
			Name:    "_",
			NamePos: stmt.End(),
		}
		return true
	case *ast.SwitchStmt:
		if p.Init != nil {
			return false
		}
		p.Init = stmt
		p.Tag = nil
		return true
	}
	return false
}

// readKeyword reads the keyword starting at pos, if any.
func readKeyword(pos token.Pos, tok *token.File, src []byte) string {
	var kwBytes []byte
	offset, err := safetoken.Offset(tok, pos)
	if err != nil {
		return ""
	}
	for i := offset; i < len(src); i++ {
		// Use a simplified identifier check since keywords are always lowercase ASCII.
		if src[i] < 'a' || src[i] > 'z' {
			break
		}
		kwBytes = append(kwBytes, src[i])

		// Stop search at arbitrarily chosen too-long-for-a-keyword length.
		if len(kwBytes) > 15 {
			return ""
		}
	}

	if kw := string(kwBytes); token.Lookup(kw).IsKeyword() {
		return kw
	}

	return ""
}

// fixArrayType tries to parse an *ast.BadExpr into an *ast.ArrayType.
// go/parser often turns lone array types like "[]int" into BadExprs
// if it isn't expecting a type.
func fixArrayType(bad *ast.BadExpr, parent ast.Node, tok *token.File, src []byte) bool {
	// Our expected input is a bad expression that looks like "[]someExpr".

	from, to := bad.Pos(), bad.End()
	fromOffset, toOffset, err := safetoken.Offsets(tok, from, to)
	if err != nil {
		return false
	}

	exprBytes := bytes.TrimSpace(slices.Clone(src[fromOffset:toOffset]))

	// If our expression ends in "]" (e.g. "[]"), add a phantom selector
	// so we can complete directly after the "[]".
	if bytes.HasSuffix(exprBytes, []byte("]")) {
		exprBytes = append(exprBytes, '_')
	}

	// Add "{}" to turn our ArrayType into a CompositeLit. This is to
	// handle the case of "[...]int" where we must make it a composite
	// literal to be parseable.
	exprBytes = append(exprBytes, '{', '}')

	expr, err := parseExpr(tok, from, exprBytes)
	if err != nil {
		return false
	}

	cl, _ := expr.(*ast.CompositeLit)
	if cl == nil {
		return false
	}

	at, _ := cl.Type.(*ast.ArrayType)
	if at == nil {
		return false
	}

	return replaceNode(parent, bad, at)
}

// precedingToken scans src to find the token preceding pos.
func precedingToken(pos token.Pos, tok *token.File, src []byte) token.Token {
	s := &scanner.Scanner{}
	s.Init(tok, src, nil, 0)

	var lastTok token.Token
	for {
		p, t, _ := s.Scan()
		if t == token.EOF || p >= pos {
			break
		}

		lastTok = t
	}
	return lastTok
}

// fixDeferOrGoStmt tries to parse an *ast.BadStmt into a defer or a go statement.
//
// go/parser packages a statement of the form "defer x." as an *ast.BadStmt because
// it does not include a call expression. This means that go/types skips type-checking
// this statement entirely, and we can't use the type information when completing.
// Here, we try to generate a fake *ast.DeferStmt or *ast.GoStmt to put into the AST,
// instead of the *ast.BadStmt.
func fixDeferOrGoStmt(bad *ast.BadStmt, parent ast.Node, tok *token.File, src []byte) bool {
	// Check if we have a bad statement containing either a "go" or "defer".
	s := &scanner.Scanner{}
	s.Init(tok, src, nil, 0)

	var (
		pos token.Pos
		tkn token.Token
	)
	for {
		if tkn == token.EOF {
			return false
		}
		if pos >= bad.From {
			break
		}
		pos, tkn, _ = s.Scan()
	}

	var stmt ast.Stmt
	switch tkn {
	case token.DEFER:
		stmt = &ast.DeferStmt{
			Defer: pos,
		}
	case token.GO:
		stmt = &ast.GoStmt{
			Go: pos,
		}
	default:
		return false
	}

	var (
		from, to, last   token.Pos
		lastToken        token.Token
		braceDepth       int
		phantomSelectors []token.Pos
	)
FindTo:
	for {
		to, tkn, _ = s.Scan()

		if from == token.NoPos {
			from = to
		}

		switch tkn {
		case token.EOF:
			break FindTo
		case token.SEMICOLON:
			// If we aren't in nested braces, end of statement means
			// end of expression.
			if braceDepth == 0 {
				break FindTo
			}
		case token.LBRACE:
			braceDepth++
		}

		// This handles the common dangling selector case. For example in
		//
		// defer fmt.
		// y := 1
		//
		// we notice the dangling period and end our expression.
		//
		// If the previous token was a "." and we are looking at a "}",
		// the period is likely a dangling selector and needs a phantom
		// "_". Likewise if the current token is on a different line than
		// the period, the period is likely a dangling selector.
		if lastToken == token.PERIOD && (tkn == token.RBRACE || safetoken.Line(tok, to) > safetoken.Line(tok, last)) {
			// Insert phantom "_" selector after the dangling ".".
			phantomSelectors = append(phantomSelectors, last+1)
			// If we aren't in a block then end the expression after the ".".
			if braceDepth == 0 {
				to = last + 1
				break
			}
		}

		lastToken = tkn
		last = to

		switch tkn {
		case token.RBRACE:
			braceDepth--
			if braceDepth <= 0 {
				if braceDepth == 0 {
					// +1 to include the "}" itself.
					to += 1
				}
				break FindTo
			}
		}
	}

	fromOffset, toOffset, err := safetoken.Offsets(tok, from, to)
	if err != nil {
		return false
	}
	if !from.IsValid() || fromOffset >= len(src) {
		return false
	}
	if !to.IsValid() || toOffset >= len(src) {
		return false
	}

	// Insert any phantom selectors needed to prevent dangling "." from messing
	// up the AST.
	exprBytes := make([]byte, 0, int(to-from)+len(phantomSelectors))
	for i, b := range src[fromOffset:toOffset] {
		if len(phantomSelectors) > 0 && from+token.Pos(i) == phantomSelectors[0] {
			exprBytes = append(exprBytes, '_')
			phantomSelectors = phantomSelectors[1:]
		}
		exprBytes = append(exprBytes, b)
	}

	if len(phantomSelectors) > 0 {
		exprBytes = append(exprBytes, '_')
	}

	expr, err := parseExpr(tok, from, exprBytes)
	if err != nil {
		return false
	}

	// Package the expression into a fake *ast.CallExpr and re-insert
	// into the function.
	call := &ast.CallExpr{
		Fun:    expr,
		Lparen: to,
		Rparen: to,
	}

	switch stmt := stmt.(type) {
	case *ast.DeferStmt:
		stmt.Call = call
	case *ast.GoStmt:
		stmt.Call = call
	}

	return replaceNode(parent, bad, stmt)
}

// parseStmt parses the statement in src and updates its position to
// start at pos.
//
// tok is the original file containing pos. Used to ensure that all adjusted
// positions are valid.
func parseStmt(tok *token.File, pos token.Pos, src []byte) (ast.Stmt, error) {
	// Wrap our expression to make it a valid Go file we can pass to ParseFile.
	fileSrc := slices.Concat([]byte("package fake;func _(){"), src, []byte("}"))

	// Use ParseFile instead of ParseExpr because ParseFile has
	// best-effort behavior, whereas ParseExpr fails hard on any error.
	fakeFile, err := parser.ParseFile(token.NewFileSet(), "", fileSrc, parser.SkipObjectResolution)
	if fakeFile == nil {
		return nil, fmt.Errorf("error reading fake file source: %v", err)
	}

	// Extract our expression node from inside the fake file.
	if len(fakeFile.Decls) == 0 {
		return nil, fmt.Errorf("error parsing fake file: %v", err)
	}

	fakeDecl, _ := fakeFile.Decls[0].(*ast.FuncDecl)
	if fakeDecl == nil || len(fakeDecl.Body.List) == 0 {
		return nil, fmt.Errorf("no statement in %s: %v", src, err)
	}

	stmt := fakeDecl.Body.List[0]

	// parser.ParseFile returns undefined positions.
	// Adjust them for the current file.
	offsetPositions(tok, stmt, pos-1-(stmt.Pos()-1))

	return stmt, nil
}

// parseExpr parses the expression in src and updates its position to
// start at pos.
func parseExpr(tok *token.File, pos token.Pos, src []byte) (ast.Expr, error) {
	stmt, err := parseStmt(tok, pos, src)
	if err != nil {
		return nil, err
	}

	exprStmt, ok := stmt.(*ast.ExprStmt)
	if !ok {
		return nil, fmt.Errorf("no expr in %s: %v", src, err)
	}

	return exprStmt.X, nil
}

var tokenPosType = reflect.TypeOf(token.NoPos)

// offsetPositions applies an offset to the positions in an ast.Node.
func offsetPositions(tok *token.File, n ast.Node, offset token.Pos) {
	fileBase := token.Pos(tok.Base())
	fileEnd := fileBase + token.Pos(tok.Size())
	ast.Inspect(n, func(n ast.Node) bool {
		if n == nil {
			return false
		}

		v := reflect.ValueOf(n).Elem()

		switch v.Kind() {
		case reflect.Struct:
			for i := 0; i < v.NumField(); i++ {
				f := v.Field(i)
				if f.Type() != tokenPosType {
					continue
				}

				if !f.CanSet() {
					continue
				}

				pos := token.Pos(f.Int())

				// Don't offset invalid positions: they should stay invalid.
				if !pos.IsValid() {
					continue
				}

				// Clamp value to valid range; see #64335.
				//
				// TODO(golang/go#64335): this is a hack, because our fixes should not
				// produce positions that overflow (but they do; see golang/go#64488,
				// #73438, #66790, #66683, #67704).
				pos = min(max(pos+offset, fileBase), fileEnd)

				f.SetInt(int64(pos))
			}
		}

		return true
	})
}

// replaceNode updates parent's child oldChild to be newChild. It
// returns whether it replaced successfully.
func replaceNode(parent, oldChild, newChild ast.Node) bool {
	if parent == nil || oldChild == nil || newChild == nil {
		return false
	}

	parentVal := reflect.ValueOf(parent).Elem()
	if parentVal.Kind() != reflect.Struct {
		return false
	}

	newChildVal := reflect.ValueOf(newChild)

	tryReplace := func(v reflect.Value) bool {
		if !v.CanSet() || !v.CanInterface() {
			return false
		}

		// If the existing value is oldChild, we found our child. Make
		// sure our newChild is assignable and then make the swap.
		if v.Interface() == oldChild && newChildVal.Type().AssignableTo(v.Type()) {
			v.Set(newChildVal)
			return true
		}

		return false
	}

	// Loop over parent's struct fields.
	for i := 0; i < parentVal.NumField(); i++ {
		f := parentVal.Field(i)

		switch f.Kind() {
		// Check interface and pointer fields.
		case reflect.Interface, reflect.Pointer:
			if tryReplace(f) {
				return true
			}

		// Search through any slice fields.
		case reflect.Slice:
			for i := 0; i < f.Len(); i++ {
				if tryReplace(f.Index(i)) {
					return true
				}
			}
		}
	}

	return false
}
