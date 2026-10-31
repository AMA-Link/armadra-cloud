// 命令 schemagen 读协议包的 schema 导出，写 Go 侧的生成物。
//
//	go run ./tools/schemagen -schema <packages/platform-protocol/schema> -out <目录>
//
// 只用标准库。输出的顺序由输入决定，同样的输入两次生成逐字节相同。
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func main() {
	schemaDir := flag.String("schema", "", "协议包的 schema/ 目录")
	outDir := flag.String("out", "", "生成物目录")
	flag.Parse()
	if *schemaDir == "" || *outDir == "" {
		fmt.Fprintln(os.Stderr, "用法: schemagen -schema <目录> -out <目录>")
		os.Exit(2)
	}
	files, err := generate(*schemaDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "生成失败：", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "生成失败：", err)
		os.Exit(1)
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(*outDir, name), data, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "生成失败：", err)
			os.Exit(1)
		}
	}
}

func readOrdered(dir, name string) (*value, error) {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return nil, err
	}
	v, err := parseOrdered(data)
	if err != nil {
		return nil, fmt.Errorf("%s：%w", name, err)
	}
	if err := normalize(v); err != nil {
		return nil, fmt.Errorf("%s：%w", name, err)
	}
	return v, nil
}

// schemaFiles 列出目录里的 *.json，按文件名排序。
func schemaFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// generate 返回文件名到内容的映射。
func generate(schemaDir string) (map[string][]byte, error) {
	out := map[string][]byte{}

	constants, err := readOrdered(schemaDir, "constants.json")
	if err != nil {
		return nil, err
	}
	emailPattern = constants.get("validation").get("emailPattern").str
	if out["constants.go"], err = genConstants(constants); err != nil {
		return nil, fmt.Errorf("constants.json：%w", err)
	}

	errorsDoc, err := readOrdered(schemaDir, "errors.json")
	if err != nil {
		return nil, err
	}
	if out["errors.go"], err = genErrors(errorsDoc); err != nil {
		return nil, fmt.Errorf("errors.json：%w", err)
	}

	openapi, err := readOrdered(schemaDir, "cloud-openapi.json")
	if err != nil {
		return nil, err
	}
	if out["routes.go"], err = genRoutes(openapi); err != nil {
		return nil, fmt.Errorf("cloud-openapi.json：%w", err)
	}

	if out["inputs.go"], err = genInputs(openapi); err != nil {
		return nil, fmt.Errorf("cloud-openapi.json（入参）：%w", err)
	}

	if out["claims.go"], err = genStructs(schemaDir, []string{"assertion"}, nil); err != nil {
		return nil, err
	}
	if out["tunnel.go"], err = genStructs(schemaDir, []string{"tunnel"}, []string{"e2e/accept.json"}); err != nil {
		return nil, err
	}
	return out, nil
}

// genStructs 为目录下的每个 schema 生成结构体与规则；extra 是目录外的单个文件。
func genStructs(schemaDir string, dirs, extra []string) ([]byte, error) {
	g := newStructGen()
	type input struct{ id string }
	var inputs []input
	for _, dir := range dirs {
		names, err := schemaFiles(filepath.Join(schemaDir, dir))
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			inputs = append(inputs, input{id: dir + "/" + name})
		}
	}
	for _, id := range extra {
		inputs = append(inputs, input{id: id})
	}
	for _, in := range inputs {
		root, err := readOrdered(schemaDir, in.id)
		if err != nil {
			return nil, err
		}
		base := strings.TrimSuffix(filepath.Base(in.id), ".json")
		name := pascal(base)
		if dir := filepath.Dir(in.id); dir == "e2e" {
			name = pascal(dir + "-" + base)
		}
		if err := g.add(name, in.id, root); err != nil {
			return nil, err
		}
	}
	var f file
	g.render(&f)
	return f.source("gen", "", []string{"github.com/AMA-Link/armadra-cloud/go/internal/protocol/schema"})
}
