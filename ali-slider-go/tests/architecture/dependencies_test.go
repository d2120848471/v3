package architecture

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/d2120848471/v3/ali-slider-go/"

func moduleRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// TestProductionDependencies 检查真实生产 import，而不是对文档中的层级做断言。
// 测试实现可以跨层组装替身与集成场景，因此不把 *_test.go 限制成生产依赖。
func TestProductionDependencies(t *testing.T) {
	allowed := map[string][]string{
		"domain":         {"domain", "foundation"},
		"foundation":     {"foundation"},
		"platform":       {"platform", "foundation"},
		"application":    {"application", "domain", "foundation"},
		"infrastructure": {"infrastructure", "application", "domain", "foundation", "platform"},
		"interfaces":     {"application", "domain", "foundation"},
		"bootstrap":      {"bootstrap", "interfaces", "application", "infrastructure", "domain", "foundation", "platform"},
		"sdk":            {"bootstrap", "application", "domain", "foundation"},
		"cmd":            {"bootstrap"},
	}
	root := moduleRoot()
	count := 0
	for _, directory := range []string{"internal", "pkg", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, directory), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			relative = filepath.ToSlash(relative)
			layer := dependencyLayer(relative)
			targets, exists := allowed[layer]
			if !exists {
				t.Errorf("%s: unclassified production package", relative)
				return nil
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			count++
			for _, imported := range file.Imports {
				name, err := strconv.Unquote(imported.Path.Value)
				if err != nil {
					return err
				}
				if layer == "domain" || layer == "application" {
					if name == "net/http" || name == "os/exec" {
						t.Errorf("%s: %s belongs behind an infrastructure port", relative, name)
					}
				}
				if !strings.HasPrefix(name, modulePath) {
					continue
				}
				target := dependencyLayer(strings.TrimPrefix(name, modulePath))
				permitted := false
				for _, candidate := range targets {
					permitted = permitted || target == candidate
				}
				if !permitted {
					t.Errorf("%s: forbidden %s -> %s import %q", relative, layer, target, name)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if count == 0 {
		t.Fatal("no production Go files inspected")
	}
	t.Logf("checked imports in %d production Go files", count)
}

func dependencyLayer(path string) string {
	parts := strings.Split(filepath.ToSlash(path), "/")
	if len(parts) < 2 {
		return "unknown"
	}
	switch parts[0] {
	case "internal":
		return parts[1]
	case "pkg":
		if parts[1] == "slider" || parts[1] == "baxia" {
			return "sdk"
		}
	case "cmd":
		return "cmd"
	}
	return "unknown"
}

// SDK 可以依赖生产资源组装，但不应因 HTTP 页面或 handler 的变更而无法编译。
// 直接 import 规则无法识别 SDK -> bootstrap -> HTTP 的间接耦合，所以另遍历包图。
func TestSDKDoesNotTransitivelyImportHTTPInterface(t *testing.T) {
	root := moduleRoot()
	graph := make(map[string][]string)
	for _, directory := range []string{"internal", "pkg"} {
		err := filepath.WalkDir(filepath.Join(root, directory), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			relative, err := filepath.Rel(root, filepath.Dir(path))
			if err != nil {
				return err
			}
			packageName := filepath.ToSlash(relative)
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			for _, imported := range file.Imports {
				name, err := strconv.Unquote(imported.Path.Value)
				if err != nil {
					return err
				}
				if strings.HasPrefix(name, modulePath) {
					graph[packageName] = append(graph[packageName], strings.TrimPrefix(name, modulePath))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(graph["pkg/slider"]) == 0 {
		t.Fatal("SDK dependency graph is empty")
	}
	visited := make(map[string]bool)
	var visit func(string, []string)
	visit = func(name string, chain []string) {
		if visited[name] {
			return
		}
		visited[name] = true
		chain = append(chain, name)
		if strings.HasPrefix(name, "internal/interfaces/") {
			t.Errorf("SDK depends on HTTP interface: %s", strings.Join(chain, " -> "))
			return
		}
		for _, imported := range graph[name] {
			visit(imported, chain)
		}
	}
	visit("pkg/slider", nil)
}
