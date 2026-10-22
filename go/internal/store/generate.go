package store

// 迁移 SQL 以 TypeScript 包里的为准，这里拷一份给 go:embed 用（逐字节一致由 repo-check 校验）。
//go:generate go run ../../tools/copymigrations -from ../../../packages/control-store-sql/migrations -to sqlite/migrations
//go:generate go run ../../tools/copymigrations -from ../../../packages/control-store-postgres/migrations -to postgres/migrations
