// 命令 copymigrations 把 TypeScript 包里的迁移 SQL 逐字节拷进 Go 的 embed 目录。
//
//	go run ./tools/copymigrations -from <源目录> -to <目标目录>
//
// SQL 文件以 TypeScript 包里的为准（packages/control-store-sql/migrations 与
// packages/control-store-postgres/migrations），新迁移只在那边加；目标目录里多出来的 .sql
// 会被删掉，保证两边文件集合一致。逐字节一致由 tools/repo-check.mjs 按 migrations.lock 校验。
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	from := flag.String("from", "", "迁移的源目录")
	to := flag.String("to", "", "迁移的目标目录")
	flag.Parse()
	if *from == "" || *to == "" {
		fmt.Fprintln(os.Stderr, "用法: copymigrations -from <源目录> -to <目标目录>")
		os.Exit(2)
	}
	if err := run(*from, *to); err != nil {
		fmt.Fprintln(os.Stderr, "拷贝迁移失败：", err)
		os.Exit(1)
	}
}

func run(from, to string) error {
	entries, err := os.ReadDir(from)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(to, 0o755); err != nil {
		return err
	}
	want := map[string]bool{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(from, entry.Name()))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(to, entry.Name()), data, 0o644); err != nil {
			return err
		}
		want[entry.Name()] = true
	}
	existing, err := os.ReadDir(to)
	if err != nil {
		return err
	}
	for _, entry := range existing {
		if strings.HasSuffix(entry.Name(), ".sql") && !want[entry.Name()] {
			if err := os.Remove(filepath.Join(to, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}
