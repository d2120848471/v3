package architecture

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 快照来自架构迁移前的源码，包含公开签名、结构字段/JSON tag、错误常量。
// Client 的私有实现不在快照中；增加公开声明允许，已有声明不得悄悄改变。
func TestPublicSDKCompatibility(t *testing.T) {
	raw, err := os.ReadFile("testdata/slider_api.json")
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]string
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if len(want) == 0 {
		t.Fatal("empty API baseline")
	}
	got, err := collectPublicAPI(filepath.Join(moduleRoot(), "pkg", "slider"))
	if err != nil {
		t.Fatal(err)
	}
	for name, signature := range want {
		if got[name] != signature {
			t.Errorf("public API %s changed:\nwas: %s\nnow: %s", name, signature, got[name])
		}
	}
}

func collectPublicAPI(directory string) (map[string]string, error) {
	files, err := filepath.Glob(filepath.Join(directory, "*.go"))
	if err != nil {
		return nil, err
	}
	result := make(map[string]string)
	set := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(set, path, nil, 0)
		if err != nil {
			return nil, err
		}
		for _, declaration := range file.Decls {
			switch value := declaration.(type) {
			case *ast.FuncDecl:
				if !value.Name.IsExported() {
					continue
				}
				name := value.Name.Name
				if value.Recv != nil {
					name = printNode(value.Recv.List[0].Type) + "." + name
				}
				removeParameterNames(value.Type.Params)
				removeParameterNames(value.Type.Results)
				result["func "+name] = printNode(value.Type)
			case *ast.GenDecl:
				for _, specification := range value.Specs {
					switch item := specification.(type) {
					case *ast.TypeSpec:
						if !item.Name.IsExported() {
							continue
						}
						if structure, ok := item.Type.(*ast.StructType); ok {
							public := make([]*ast.Field, 0, len(structure.Fields.List))
							for _, field := range structure.Fields.List {
								if len(field.Names) == 0 || field.Names[0].IsExported() {
									field.Doc, field.Comment = nil, nil
									public = append(public, field)
								}
							}
							structure.Fields.List = public
						}
						result["type "+item.Name.Name] = printNode(item.Type)
					case *ast.ValueSpec:
						for index, name := range item.Names {
							if !name.IsExported() {
								continue
							}
							signature := ""
							if item.Type != nil {
								signature = printNode(item.Type)
							}
							if index < len(item.Values) {
								signature += " = " + printNode(item.Values[index])
							}
							result[value.Tok.String()+" "+name.Name] = signature
						}
					}
				}
			}
		}
	}
	return result, nil
}

func removeParameterNames(fields *ast.FieldList) {
	if fields == nil {
		return
	}
	var expanded []*ast.Field
	for _, field := range fields.List {
		for range max(1, len(field.Names)) {
			expanded = append(expanded, &ast.Field{Type: field.Type})
		}
	}
	fields.List = expanded
}

func printNode(node ast.Node) string {
	var output bytes.Buffer
	if err := format.Node(&output, token.NewFileSet(), node); err != nil {
		panic(fmt.Sprintf("format API declaration: %v", err))
	}
	return strings.Join(strings.Fields(output.String()), " ")
}
