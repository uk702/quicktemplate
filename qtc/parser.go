package main

import (
	"bytes"
	"fmt"
	"go/ast"
	goparser "go/parser"
	gotoken "go/token"
	"io"
	"path/filepath"
	"strconv"
	"strings"
)

type parser struct {
	s                 *scanner
	w                 io.Writer
	packageName       string
	prefix            string
	forDepth          int
	switchDepth       int
	skipOutputDepth   int
	importsUseEmitted bool
}

func parse(w io.Writer, r io.Reader, filePath, packageName string) error {
	p := &parser{
		s:           newScanner(r, filePath),
		w:           w,
		packageName: packageName,
	}
	return p.parseTemplate()
}

func (p *parser) parseTemplate() error {
	s := p.s
	fmt.Fprintf(p.w, `// This file is automatically generated by qtc from %q.
// See https://github.com/valyala/quicktemplate for details.

`,
		filepath.Base(s.filePath))
	p.Printf("package %s\n", p.packageName)
	p.Printf(`import (
	qtio%s "io"

	qt%s "github.com/valyala/quicktemplate"
)
`, mangleSuffix, mangleSuffix)
	for s.Next() {
		t := s.Token()
		switch t.ID {
		case text:
			p.emitComment(t.Value)
		case tagName:
			if string(t.Value) == "import" {
				if p.importsUseEmitted {
					return fmt.Errorf("imports must be at the top of the template. Found at %s", s.Context())
				}
				if err := p.parseImport(); err != nil {
					return err
				}
			} else {
				p.emitImportsUse()
				switch string(t.Value) {
				case "interface", "iface":
					if err := p.parseInterface(); err != nil {
						return err
					}
				case "code":
					if err := p.parseTemplateCode(); err != nil {
						return err
					}
				case "func":
					if err := p.parseFunc(); err != nil {
						return err
					}
				default:
					return fmt.Errorf("unexpected tag found outside func: %q at %s", t.Value, s.Context())
				}
			}
		default:
			return fmt.Errorf("unexpected token found %s outside func at %s", t, s.Context())
		}
	}
	p.emitImportsUse()
	if err := s.LastError(); err != nil {
		return fmt.Errorf("cannot parse template: %s", err)
	}
	return nil
}

func (p *parser) emitComment(comment []byte) {
	isFirstNonemptyLine := false
	for len(comment) > 0 {
		n := bytes.IndexByte(comment, '\n')
		if n < 0 {
			n = len(comment)
		}
		line := stripTrailingSpace(comment[:n])
		if bytes.HasPrefix(line, []byte("//")) {
			line = line[2:]
			if len(line) > 0 && isSpace(line[0]) {
				line = line[1:]
			}
		}
		if len(line) == 0 {
			if isFirstNonemptyLine {
				fmt.Fprintf(p.w, "//\n")
			}
		} else {
			fmt.Fprintf(p.w, "// %s\n", line)
			isFirstNonemptyLine = true
		}

		if n < len(comment) {
			comment = comment[n+1:]
		} else {
			comment = comment[n:]
		}
	}
	fmt.Fprintf(p.w, "\n")
}

func (p *parser) emitImportsUse() {
	if p.importsUseEmitted {
		return
	}
	p.Printf(`var (
	_ = qtio%s.Copy
	_ = qt%s.AcquireByteBuffer
)
`, mangleSuffix, mangleSuffix)
	p.importsUseEmitted = true
}

func (p *parser) parseFunc() error {
	s := p.s
	t, err := expectTagContents(s)
	if err != nil {
		return err
	}
	funcStr := "func " + string(t.Value)
	f, err := parseFuncDef(t.Value)
	if err != nil {
		return fmt.Errorf("error in %q at %s: %s", funcStr, s.Context(), err)
	}
	p.emitFuncStart(f)
	for s.Next() {
		t := s.Token()
		switch t.ID {
		case text:
			p.emitText(t.Value)
		case tagName:
			ok, err := p.tryParseCommonTags(t.Value)
			if err != nil {
				return fmt.Errorf("error in %q: %s", funcStr, err)
			}
			if ok {
				continue
			}
			switch string(t.Value) {
			case "endfunc":
				if err = skipTagContents(s); err != nil {
					return err
				}
				p.emitFuncEnd(f)
				return nil
			default:
				return fmt.Errorf("unexpected tag found in %q: %q at %s", funcStr, t.Value, s.Context())
			}
		default:
			return fmt.Errorf("unexpected token found when parsing %q: %s at %s", funcStr, t, s.Context())
		}
	}
	if err := s.LastError(); err != nil {
		return fmt.Errorf("cannot parse %q: %s", funcStr, err)
	}
	return fmt.Errorf("cannot find endfunc tag for %q at %s", funcStr, s.Context())
}

func (p *parser) parseFor() error {
	s := p.s
	t, err := expectTagContents(s)
	if err != nil {
		return err
	}
	forStr := "for " + string(t.Value)
	if err = validateForStmt(t.Value); err != nil {
		return fmt.Errorf("invalid statement %q at %s: %s", forStr, s.Context(), err)
	}
	p.Printf("for %s {", t.Value)
	p.prefix += "\t"
	p.forDepth++
	for s.Next() {
		t := s.Token()
		switch t.ID {
		case text:
			p.emitText(t.Value)
		case tagName:
			ok, err := p.tryParseCommonTags(t.Value)
			if err != nil {
				return fmt.Errorf("error in %q: %s", forStr, err)
			}
			if ok {
				continue
			}
			switch string(t.Value) {
			case "endfor":
				if err = skipTagContents(s); err != nil {
					return err
				}
				p.forDepth--
				p.prefix = p.prefix[1:]
				p.Printf("}")
				return nil
			default:
				return fmt.Errorf("unexpected tag found in %q: %q at %s", forStr, t.Value, s.Context())
			}
		default:
			return fmt.Errorf("unexpected token found when parsing %q: %s at %s", forStr, t, s.Context())
		}
	}
	if err := s.LastError(); err != nil {
		return fmt.Errorf("cannot parse %q: %s", forStr, err)
	}
	return fmt.Errorf("cannot find endfor tag for %q at %s", forStr, s.Context())
}

func (p *parser) parseDefault() error {
	s := p.s
	if err := skipTagContents(s); err != nil {
		return err
	}
	stmtStr := "default"
	p.Printf("default:")
	p.prefix += "\t"
	for s.Next() {
		t := s.Token()
		switch t.ID {
		case text:
			p.emitText(t.Value)
		case tagName:
			ok, err := p.tryParseCommonTags(t.Value)
			if err != nil {
				return fmt.Errorf("error in %q: %s", stmtStr, err)
			}
			if !ok {
				s.Rewind()
				p.prefix = p.prefix[1:]
				return nil
			}
		default:
			return fmt.Errorf("unexpected token found when parsing %q: %s at %s", stmtStr, t, s.Context())
		}
	}
	if err := s.LastError(); err != nil {
		return fmt.Errorf("cannot parse %q: %s", stmtStr, err)
	}
	return fmt.Errorf("cannot find end of %q at %s", stmtStr, s.Context())
}

func (p *parser) parseCase() error {
	s := p.s
	t, err := expectTagContents(s)
	if err != nil {
		return err
	}
	caseStr := "case " + string(t.Value)
	if err = validateCaseStmt(t.Value); err != nil {
		return fmt.Errorf("invalid statement %q at %s: %s", caseStr, s.Context(), err)
	}
	p.Printf("case %s:", t.Value)
	p.prefix += "\t"
	for s.Next() {
		t := s.Token()
		switch t.ID {
		case text:
			p.emitText(t.Value)
		case tagName:
			ok, err := p.tryParseCommonTags(t.Value)
			if err != nil {
				return fmt.Errorf("error in %q: %s", caseStr, err)
			}
			if !ok {
				s.Rewind()
				p.prefix = p.prefix[1:]
				return nil
			}
		default:
			return fmt.Errorf("unexpected token found when parsing %q: %s at %s", caseStr, t, s.Context())
		}
	}
	if err := s.LastError(); err != nil {
		return fmt.Errorf("cannot parse %q: %s", caseStr, err)
	}
	return fmt.Errorf("cannot find end of %q at %s", caseStr, s.Context())
}

func (p *parser) parseSwitch() error {
	s := p.s
	t, err := expectTagContents(s)
	if err != nil {
		return err
	}
	switchStr := "switch " + string(t.Value)
	if err = validateSwitchStmt(t.Value); err != nil {
		return fmt.Errorf("invalid statement %q at %s: %s", switchStr, s.Context(), err)
	}
	p.Printf("switch %s {", t.Value)
	caseNum := 0
	defaultFound := false
	p.switchDepth++
	for s.Next() {
		t := s.Token()
		switch t.ID {
		case text:
			if caseNum == 0 {
				comment := stripLeadingSpace(t.Value)
				if len(comment) > 0 {
					p.emitComment(comment)
				}
			} else {
				p.emitText(t.Value)
			}
		case tagName:
			switch string(t.Value) {
			case "endswitch":
				if caseNum == 0 {
					return fmt.Errorf("empty statement %q found at %s", switchStr, s.Context())
				}
				if err = skipTagContents(s); err != nil {
					return err
				}
				p.switchDepth--
				p.Printf("}")
				return nil
			case "case":
				caseNum++
				if err = p.parseCase(); err != nil {
					return err
				}
			case "default":
				if defaultFound {
					return fmt.Errorf("duplicate default tag found in %q at %s", switchStr, s.Context())
				}
				defaultFound = true
				caseNum++
				if err = p.parseDefault(); err != nil {
					return err
				}
			default:
				return fmt.Errorf("unexpected tag found in %q: %q at %s", switchStr, t.Value, s.Context())
			}
		default:
			return fmt.Errorf("unexpected token found when parsing %q: %s at %s", switchStr, t, s.Context())
		}
	}
	if err := s.LastError(); err != nil {
		return fmt.Errorf("cannot parse %q: %s", switchStr, err)
	}
	return fmt.Errorf("cannot find endswitch tag for %q at %s", switchStr, s.Context())
}

func (p *parser) parseIf() error {
	s := p.s
	t, err := expectTagContents(s)
	if err != nil {
		return err
	}
	if len(t.Value) == 0 {
		return fmt.Errorf("empty if condition at %s", s.Context())
	}
	ifStr := "if " + string(t.Value)
	if err = validateIfStmt(t.Value); err != nil {
		return fmt.Errorf("invalid statement %q at %s: %s", ifStr, s.Context(), err)
	}
	p.Printf("if %s {", t.Value)
	p.prefix += "\t"
	elseUsed := false
	for s.Next() {
		t := s.Token()
		switch t.ID {
		case text:
			p.emitText(t.Value)
		case tagName:
			ok, err := p.tryParseCommonTags(t.Value)
			if err != nil {
				return fmt.Errorf("error in %q: %s", ifStr, err)
			}
			if ok {
				continue
			}
			switch string(t.Value) {
			case "endif":
				if err = skipTagContents(s); err != nil {
					return err
				}
				p.prefix = p.prefix[1:]
				p.Printf("}")
				return nil
			case "else":
				if elseUsed {
					return fmt.Errorf("duplicate else branch found for %q at %s", ifStr, s.Context())
				}
				if err = skipTagContents(s); err != nil {
					return err
				}
				p.prefix = p.prefix[1:]
				p.Printf("} else {")
				p.prefix += "\t"
				elseUsed = true
			case "elseif":
				if elseUsed {
					return fmt.Errorf("unexpected elseif branch found after else branch for %q at %s",
						ifStr, s.Context())
				}
				t, err = expectTagContents(s)
				if err != nil {
					return err
				}
				p.prefix = p.prefix[1:]
				p.Printf("} else if %s {", t.Value)
				p.prefix += "\t"
			default:
				return fmt.Errorf("unexpected tag found in %q: %q at %s", ifStr, t.Value, s.Context())
			}
		default:
			return fmt.Errorf("unexpected token found when parsing %q: %s at %s", ifStr, t, s.Context())
		}
	}
	if err := s.LastError(); err != nil {
		return fmt.Errorf("cannot parse %q: %s", ifStr, err)
	}
	return fmt.Errorf("cannot find endif tag for %q at %s", ifStr, s.Context())
}

func (p *parser) tryParseCommonTags(tagBytes []byte) (bool, error) {
	s := p.s
	tagNameStr, prec := splitTagNamePrec(string(tagBytes))
	switch tagNameStr {
	case "s", "v", "d", "f", "q", "z", "j", "u",
		"s=", "v=", "d=", "f=", "q=", "z=", "j=", "u=",
		"sz", "qz", "jz", "uz",
		"sz=", "qz=", "jz=", "uz=":
		t, err := expectTagContents(s)
		if err != nil {
			return false, err
		}
		if err = validateOutputTagValue(t.Value); err != nil {
			return false, fmt.Errorf("invalid output tag value at %s: %s", s.Context(), err)
		}
		filter := "N()."
		switch tagNameStr {
		case "s", "v", "q", "z", "j", "sz", "qz", "jz":
			filter = "E()."
		}
		if strings.HasSuffix(tagNameStr, "=") {
			tagNameStr = tagNameStr[:len(tagNameStr)-1]
		}
		if tagNameStr == "f" && prec >= 0 {
			p.Printf("qw%s.N().FPrec(%s, %d)", mangleSuffix, t.Value, prec)
		} else {
			tagNameStr = strings.ToUpper(tagNameStr)
			p.Printf("qw%s.%s%s(%s)", mangleSuffix, filter, tagNameStr, t.Value)
		}
	case "=":
		t, err := expectTagContents(s)
		if err != nil {
			return false, err
		}
		f, err := parseFuncCall(t.Value)
		if err != nil {
			return false, fmt.Errorf("error at %s: %s", s.Context(), err)
		}
		p.Printf("%s", f.CallStream("qw"+mangleSuffix))
	case "return":
		if err := p.skipAfterTag(tagNameStr); err != nil {
			return false, err
		}
	case "break":
		if p.forDepth <= 0 && p.switchDepth <= 0 {
			return false, fmt.Errorf("found break tag outside for loop and switch block")
		}
		if err := p.skipAfterTag(tagNameStr); err != nil {
			return false, err
		}
	case "continue":
		if p.forDepth <= 0 {
			return false, fmt.Errorf("found continue tag outside for loop")
		}
		if err := p.skipAfterTag(tagNameStr); err != nil {
			return false, err
		}
	case "code":
		if err := p.parseFuncCode(); err != nil {
			return false, err
		}
	case "for":
		if err := p.parseFor(); err != nil {
			return false, err
		}
	case "if":
		if err := p.parseIf(); err != nil {
			return false, err
		}
	case "switch":
		if err := p.parseSwitch(); err != nil {
			return false, err
		}
	default:
		return false, nil
	}
	return true, nil
}

func splitTagNamePrec(tagName string) (string, int) {
	parts := strings.Split(tagName, ".")
	if len(parts) == 2 && parts[0] == "f" {
		p := parts[1]
		if strings.HasSuffix(p, "=") {
			p = p[:len(p)-1]
		}
		if len(p) == 0 {
			return "f", 0
		}
		prec, err := strconv.Atoi(p)
		if err == nil && prec >= 0 {
			return "f", prec
		}
	}
	return tagName, -1
}

func (p *parser) skipAfterTag(tagStr string) error {
	s := p.s
	if err := skipTagContents(s); err != nil {
		return err
	}
	p.Printf("%s", tagStr)
	p.skipOutputDepth++
	defer func() {
		p.skipOutputDepth--
	}()
	for s.Next() {
		t := s.Token()
		switch t.ID {
		case text:
			// skip text
		case tagName:
			ok, err := p.tryParseCommonTags(t.Value)
			if err != nil {
				return fmt.Errorf("error when parsing contents after %q: %s", tagStr, err)
			}
			if ok {
				continue
			}
			switch string(t.Value) {
			case "endfunc", "endfor", "endif", "else", "elseif", "case", "default", "endswitch":
				s.Rewind()
				return nil
			default:
				return fmt.Errorf("unexpected tag found after %q: %q at %s", tagStr, t.Value, s.Context())
			}
		default:
			return fmt.Errorf("unexpected token found when parsing contents after %q: %s at %s", tagStr, t, s.Context())
		}
	}
	if err := s.LastError(); err != nil {
		return fmt.Errorf("cannot parse contents after %q: %s", tagStr, err)
	}
	return fmt.Errorf("cannot find closing tag after %q at %s", tagStr, s.Context())
}

func (p *parser) parseInterface() error {
	s := p.s
	t, err := expectTagContents(s)
	if err != nil {
		return err
	}

	n := bytes.IndexByte(t.Value, '{')
	if n < 0 {
		return fmt.Errorf("missing '{' in interface at %s", s.Context())
	}
	ifname := string(stripTrailingSpace(t.Value[:n]))
	if len(ifname) == 0 {
		return fmt.Errorf("missing interface name at %s", s.Context())
	}
	p.Printf("type %s interface {", ifname)
	p.prefix = "\t"

	tail := t.Value[n:]
	exprStr := fmt.Sprintf("interface %s", tail)
	expr, err := goparser.ParseExpr(exprStr)
	if err != nil {
		return fmt.Errorf("error when parsing interface at %s: %s", s.Context(), err)
	}
	it, ok := expr.(*ast.InterfaceType)
	if !ok {
		return fmt.Errorf("unexpected interface type at %s: %T", s.Context(), expr)
	}
	methods := it.Methods.List
	if len(methods) == 0 {
		return fmt.Errorf("interface must contain at least one method at %s", s.Context())
	}

	for _, m := range it.Methods.List {
		methodStr := exprStr[m.Pos()-1 : m.End()-1]
		f, err := parseFuncDef([]byte(methodStr))
		if err != nil {
			return fmt.Errorf("when when parsing %q at %s: %s", methodStr, s.Context(), err)
		}
		p.Printf("%s string", methodStr)
		p.Printf("%s", f.DefStream("qw"+mangleSuffix))
		p.Printf("%s", f.DefWrite("qq"+mangleSuffix))
	}
	p.prefix = ""
	p.Printf("}")
	return nil
}

func (p *parser) parseImport() error {
	t, err := expectTagContents(p.s)
	if err != nil {
		return err
	}
	if len(t.Value) == 0 {
		return fmt.Errorf("empty import found at %s", p.s.Context())
	}
	if err = validateImport(t.Value); err != nil {
		return fmt.Errorf("invalid import found at %s: %s", p.s.Context(), err)
	}
	p.Printf("import %s\n", t.Value)
	return nil
}

func (p *parser) parseTemplateCode() error {
	t, err := expectTagContents(p.s)
	if err != nil {
		return err
	}
	if err = validateTemplateCode(t.Value); err != nil {
		return fmt.Errorf("invalid code at %s: %s", p.s.Context(), err)
	}
	p.Printf("%s\n", t.Value)
	return nil
}

func (p *parser) parseFuncCode() error {
	t, err := expectTagContents(p.s)
	if err != nil {
		return err
	}
	if err = validateFuncCode(t.Value); err != nil {
		return fmt.Errorf("invalid code at %s: %s", p.s.Context(), err)
	}
	p.Printf("%s\n", t.Value)
	return nil
}

func (p *parser) emitText(text []byte) {
	for len(text) > 0 {
		n := bytes.IndexByte(text, '`')
		if n < 0 {
			p.Printf("qw%s.N().S(`%s`)", mangleSuffix, text)
			return
		}
		p.Printf("qw%s.N().S(`%s`)", mangleSuffix, text[:n])
		p.Printf("qw%s.N().S(\"`\")", mangleSuffix)
		text = text[n+1:]
	}
}

func (p *parser) emitFuncStart(f *funcType) {
	p.Printf("func %s {", f.DefStream("qw"+mangleSuffix))
	p.prefix = "\t"
}

func (p *parser) emitFuncEnd(f *funcType) {
	p.prefix = ""
	p.Printf("}\n")

	p.Printf("func %s {", f.DefWrite("qq"+mangleSuffix))
	p.prefix = "\t"
	p.Printf("qw%s := qt%s.AcquireWriter(qq%s)", mangleSuffix, mangleSuffix, mangleSuffix)
	p.Printf("%s", f.CallStream("qw"+mangleSuffix))
	p.Printf("qt%s.ReleaseWriter(qw%s)", mangleSuffix, mangleSuffix)
	p.prefix = ""
	p.Printf("}\n")

	p.Printf("func %s {", f.DefString())
	p.prefix = "\t"
	p.Printf("qb%s := qt%s.AcquireByteBuffer()", mangleSuffix, mangleSuffix)
	p.Printf("%s", f.CallWrite("qb"+mangleSuffix))
	p.Printf("qs%s := string(qb%s.B)", mangleSuffix, mangleSuffix)
	p.Printf("qt%s.ReleaseByteBuffer(qb%s)", mangleSuffix, mangleSuffix)
	p.Printf("return qs%s", mangleSuffix)
	p.prefix = ""
	p.Printf("}\n")
}

func (p *parser) Printf(format string, args ...interface{}) {
	if p.skipOutputDepth > 0 {
		return
	}
	w := p.w
	fmt.Fprintf(w, "%s", p.prefix)
	p.s.WriteLineComment(w)
	fmt.Fprintf(w, "%s", p.prefix)
	fmt.Fprintf(w, format, args...)
	fmt.Fprintf(w, "\n")
}

func skipTagContents(s *scanner) error {
	_, err := expectTagContents(s)
	return err
}

func expectTagContents(s *scanner) (*token, error) {
	return expectToken(s, tagContents)
}

func expectToken(s *scanner, id int) (*token, error) {
	if !s.Next() {
		return nil, fmt.Errorf("cannot find token %s: %v", tokenIDToStr(id), s.LastError())
	}
	t := s.Token()
	if t.ID != id {
		return nil, fmt.Errorf("unexpected token found %s. Expecting %s at %s", t, tokenIDToStr(id), s.Context())
	}
	return t, nil
}

func validateOutputTagValue(stmt []byte) error {
	exprStr := string(stmt)
	_, err := goparser.ParseExpr(exprStr)
	return err
}

func validateForStmt(stmt []byte) error {
	exprStr := fmt.Sprintf("func () { for %s {} }", stmt)
	_, err := goparser.ParseExpr(exprStr)
	return err
}

func validateIfStmt(stmt []byte) error {
	exprStr := fmt.Sprintf("func () { if %s {} }", stmt)
	_, err := goparser.ParseExpr(exprStr)
	return err
}

func validateSwitchStmt(stmt []byte) error {
	exprStr := fmt.Sprintf("func () { switch %s {} }", stmt)
	_, err := goparser.ParseExpr(exprStr)
	return err
}

func validateCaseStmt(stmt []byte) error {
	exprStr := fmt.Sprintf("func () { switch {case %s:} }", stmt)
	_, err := goparser.ParseExpr(exprStr)
	return err
}

func validateFuncCode(code []byte) error {
	exprStr := fmt.Sprintf("func () { for { %s\n } }", code)
	_, err := goparser.ParseExpr(exprStr)
	return err
}

func validateTemplateCode(code []byte) error {
	codeStr := fmt.Sprintf("package foo\nvar _ = a\n%s", code)
	fset := gotoken.NewFileSet()
	_, err := goparser.ParseFile(fset, "", codeStr, 0)
	return err
}

func validateImport(code []byte) error {
	codeStr := fmt.Sprintf("package foo\nimport %s", code)
	fset := gotoken.NewFileSet()
	f, err := goparser.ParseFile(fset, "", codeStr, 0)
	if err != nil {
		return err
	}
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok {
			return fmt.Errorf("unexpected code found: %T. Expecting ast.GenDecl", d)
		}
		for _, s := range gd.Specs {
			if _, ok := s.(*ast.ImportSpec); !ok {
				return fmt.Errorf("unexpected code found: %T. Expecting ast.ImportSpec", s)
			}
		}
	}
	return nil
}
