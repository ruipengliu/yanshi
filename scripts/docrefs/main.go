// docrefs 检查文档中对代码的引用是否仍然存在（make lint）：
//   - 反引号中的 Go 标识符：`pkg.Name`、`pkg.Type.Member`、`Type.Member`（只检查仓库内的包与类型，标准库与外部包跳过）；
//   - 反引号中的仓库路径：`internal/...`、`docs/...` 等；
//   - Markdown 的相对链接。
//
// 文档是智能体判断的依据：重命名或删除标识符后留在文档里的旧名会误导下一次改动，所以在 lint 中拦下。
// docs/review 是评审当时的记录，不检查。
package main

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// skipDirs 不含仓库自己的 Go 代码或文档。
var skipDirs = map[string]bool{".git": true, ".claude": true, "node_modules": true, "build": true, "bin": true}

type pkgDecls struct {
	names   map[string]bool            // 顶层声明
	members map[string]map[string]bool // 类型 → 方法、字段、内嵌类型
	embeds  map[string][]string        // 类型 → 内嵌类型名（成员可经内嵌提升）
}

func newPkg() *pkgDecls {
	return &pkgDecls{names: map[string]bool{}, members: map[string]map[string]bool{}, embeds: map[string][]string{}}
}

func (p *pkgDecls) addMember(typ, name string) {
	if p.members[typ] == nil {
		p.members[typ] = map[string]bool{}
	}
	p.members[typ][name] = true
}

func (p *pkgDecls) anyMember(name string) bool {
	for _, ms := range p.members {
		if ms[name] {
			return true
		}
	}
	return false
}

// index 是仓库内全部 Go 包的声明，按包名合并（同名的包合在一起，宁可漏报，不误报）。
type index struct {
	pkgs map[string]*pkgDecls
}

func typeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return typeName(t.X)
	case *ast.SelectorExpr:
		return t.Sel.Name
	case *ast.IndexExpr:
		return typeName(t.X)
	case *ast.IndexListExpr:
		return typeName(t.X)
	}
	return ""
}

func buildIndex(root string) (*index, error) {
	ix := &index{pkgs: map[string]*pkgDecls{}}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		name := strings.TrimSuffix(f.Name.Name, "_test")
		p := ix.pkgs[name]
		if p == nil {
			p = newPkg()
			ix.pkgs[name] = p
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv != nil && len(d.Recv.List) > 0 {
					p.addMember(typeName(d.Recv.List[0].Type), d.Name.Name)
				} else {
					p.names[d.Name.Name] = true
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						p.names[s.Name.Name] = true
						addTypeMembers(p, s.Name.Name, s.Type)
					case *ast.ValueSpec:
						for _, n := range s.Names {
							p.names[n.Name] = true
						}
					}
				}
			}
		}
		return nil
	})
	return ix, err
}

func addTypeMembers(p *pkgDecls, typ string, e ast.Expr) {
	var fields *ast.FieldList
	switch t := e.(type) {
	case *ast.StructType:
		fields = t.Fields
	case *ast.InterfaceType:
		fields = t.Methods
	default:
		return
	}
	for _, f := range fields.List {
		if len(f.Names) == 0 {
			if n := typeName(f.Type); n != "" {
				p.addMember(typ, n)
				p.embeds[typ] = append(p.embeds[typ], n)
			}
			continue
		}
		for _, n := range f.Names {
			p.addMember(typ, n.Name)
		}
	}
}

// hasMember 报告类型 typ（在 pkg 中，pkg 为空时在任意包中）是否有成员 m，含经内嵌提升的成员。
func (ix *index) hasMember(pkg, typ, m string, depth int) bool {
	if depth > 4 {
		return false
	}
	for name, p := range ix.pkgs {
		if pkg != "" && name != pkg {
			continue
		}
		// proto 字段（含 oneof）在生成代码中是 GetX 方法：文档按契约写 ContentBlock.Media。
		if p.members[typ][m] || p.members[typ]["Get"+m] {
			return true
		}
		for _, emb := range p.embeds[typ] {
			if ix.hasMember("", emb, m, depth+1) {
				return true
			}
		}
	}
	return false
}

func (ix *index) hasType(typ string) bool {
	for _, p := range ix.pkgs {
		if p.names[typ] {
			return true
		}
	}
	return false
}

var (
	codeSpan = regexp.MustCompile("`([^`\n]+)`")
	// pkg.Name、pkg.Type.Member、path/to/pkg.Name，可带 "()"。
	pkgRef = regexp.MustCompile(`^(?:[\w-]+/)*([a-z][a-z0-9]*)\.([A-Z]\w*)(?:\.([A-Z]\w*))?(?:\(\))?$`)
	// Type.Member，可带 "()"。
	typeRef = regexp.MustCompile(`^([A-Z]\w*)\.([A-Z]\w*)(?:\(\))?$`)
	// 仓库路径，可带 ":行号" 或 ":起-止"。
	pathRef  = regexp.MustCompile(`^((?:internal|cmd|sdk|proto|docs|agents|evals|deploy|scripts|gen)/[\w./-]*?)/?(?::\d+(?:-\d+)?)?$`)
	mdLink   = regexp.MustCompile(`\]\(([^)\s]+)\)`)
	fenceTag = "```"
)

func (ix *index) checkSpan(s string) string {
	if m := pkgRef.FindStringSubmatch(s); m != nil {
		p := ix.pkgs[m[1]]
		if p == nil {
			return "" // 标准库或外部包
		}
		// 文档常把方法写成 pkg.Method（如 service.SubmitWithID），包内任一类型的成员也算存在。
		if !p.names[m[2]] && !p.anyMember(m[2]) {
			return fmt.Sprintf("%s.%s 不存在", m[1], m[2])
		}
		if m[3] != "" && p.members[m[2]] != nil && !ix.hasMember(m[1], m[2], m[3], 0) {
			return fmt.Sprintf("%s.%s 没有成员 %s", m[1], m[2], m[3])
		}
		return ""
	}
	if m := typeRef.FindStringSubmatch(s); m != nil {
		if ix.hasType(m[1]) && !ix.hasMember("", m[1], m[2], 0) {
			return fmt.Sprintf("类型 %s 没有成员 %s", m[1], m[2])
		}
		return ""
	}
	if m := pathRef.FindStringSubmatch(s); m != nil && !strings.ContainsAny(s, "*<>{}") {
		if _, err := os.Stat(m[1]); err != nil {
			return fmt.Sprintf("路径 %s 不存在", m[1])
		}
	}
	return ""
}

func checkLink(mdFile, target string) string {
	if strings.Contains(target, "://") || strings.HasPrefix(target, "#") || strings.HasPrefix(target, "mailto:") {
		return ""
	}
	target, _, _ = strings.Cut(target, "#")
	if target == "" {
		return ""
	}
	path := filepath.Join(filepath.Dir(mdFile), target)
	if i := strings.LastIndex(path, ":"); i > 0 && strings.Trim(path[i+1:], "0123456789-") == "" {
		path = path[:i] // file.go:127
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Sprintf("链接 %s 不存在", target)
	}
	return ""
}

func docFiles() ([]string, error) {
	files := []string{"AGENTS.md", "CONTEXT.md", "README.md"}
	err := filepath.WalkDir("docs", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path == filepath.Join("docs", "review") {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(path, ".md") {
			files = append(files, path)
		}
		return nil
	})
	sort.Strings(files)
	return files, err
}

func main() {
	ix, err := buildIndex(".")
	if err != nil {
		fmt.Fprintln(os.Stderr, "docrefs:", err)
		os.Exit(2)
	}
	files, err := docFiles()
	if err != nil {
		fmt.Fprintln(os.Stderr, "docrefs:", err)
		os.Exit(2)
	}
	bad := 0
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "docrefs:", err)
			os.Exit(2)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		inFence := false
		for n := 1; sc.Scan(); n++ {
			line := sc.Text()
			// 代码块中是示例与命令，不是对现有代码的引用。
			if strings.HasPrefix(strings.TrimSpace(line), fenceTag) {
				inFence = !inFence
				continue
			}
			if inFence {
				continue
			}
			for _, m := range codeSpan.FindAllStringSubmatch(line, -1) {
				if msg := ix.checkSpan(strings.TrimSpace(m[1])); msg != "" {
					fmt.Printf("%s:%d: %s\n", path, n, msg)
					bad++
				}
			}
			for _, m := range mdLink.FindAllStringSubmatch(line, -1) {
				if msg := checkLink(path, m[1]); msg != "" {
					fmt.Printf("%s:%d: %s\n", path, n, msg)
					bad++
				}
			}
		}
		f.Close()
	}
	if bad > 0 {
		fmt.Printf("docrefs: %d 处文档引用与代码不符：改正文档，或在代码改名时一并更新\n", bad)
		os.Exit(1)
	}
}
