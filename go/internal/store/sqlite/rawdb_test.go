package sqlite_test

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

// openRaw 绕过实现直接开库，造被篡改的库用。
func openRaw(t *testing.T, path string) *sql.DB {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
