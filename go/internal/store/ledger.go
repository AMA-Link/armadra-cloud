package store

import (
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
)

// Migration 是一个迁移文件。SHA384 是文件字节的 SHA-384 小写 hex，记进账本。
type Migration struct {
	ID     int
	Name   string
	SHA384 string
	SQL    string
}

// Applied 是账本里已记的一行。
type Applied struct {
	ID     int
	Name   string
	SHA384 string
}

// LedgerProblem 是账本对不上的原因。
type LedgerProblem string

// 账本问题。
const (
	LedgerNewer    LedgerProblem = "newer"
	LedgerMismatch LedgerProblem = "mismatch"
	LedgerGap      LedgerProblem = "gap"
)

// LedgerError 是账本与程序带的迁移对不上。
type LedgerError struct {
	Problem LedgerProblem
	Msg     string
}

func (e *LedgerError) Error() string { return e.Msg }

var migrationName = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)

// ParseMigrationName：0001_init.sql → (1, "0001_init")；不是迁移文件答 ok=false。
func ParseMigrationName(file string) (id int, name string, ok bool) {
	m := migrationName.FindStringSubmatch(file)
	if m == nil {
		return 0, "", false
	}
	id, _ = strconv.Atoi(m[1])
	return id, file[:len(file)-len(".sql")], true
}

// LoadMigrations 读 dir 下的迁移文件，按编号排序；编号不从 1 起连续是打包错误。
func LoadMigrations(fsys fs.FS, dir string) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	var out []Migration
	for _, entry := range entries {
		id, name, ok := ParseMigrationName(entry.Name())
		if !ok || entry.IsDir() {
			continue
		}
		data, err := fs.ReadFile(fsys, dir+"/"+entry.Name())
		if err != nil {
			return nil, err
		}
		sum := sha512.Sum384(data)
		out = append(out, Migration{ID: id, Name: name, SHA384: hex.EncodeToString(sum[:]), SQL: string(data)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if err := CheckAvailable(out); err != nil {
		return nil, err
	}
	return out, nil
}

// CheckAvailable：程序带的迁移编号从 1 起连续，否则是打包错误。
func CheckAvailable(files []Migration) error {
	for i, file := range files {
		if file.ID != i+1 {
			return &LedgerError{LedgerGap, fmt.Sprintf("迁移编号不连续：第 %d 个是 %s", i+1, file.Name)}
		}
	}
	return nil
}

// PendingMigrations 对账：库里记的必须是程序带的前缀（名字与 SHA-384 都一致），
// 答还没跑的迁移；库里比程序多（库是新版本写的）拒绝。
func PendingMigrations(applied []Applied, available []Migration) ([]Migration, error) {
	if err := CheckAvailable(available); err != nil {
		return nil, err
	}
	if len(applied) > len(available) {
		return nil, &LedgerError{LedgerNewer, fmt.Sprintf("库是更新的版本写的：已记 %d 个迁移，程序只认识 %d 个", len(applied), len(available))}
	}
	for i, row := range applied {
		if row.ID != i+1 {
			return nil, &LedgerError{LedgerGap, fmt.Sprintf("迁移账本不连续：第 %d 行是 %d", i+1, row.ID)}
		}
		file := available[i]
		if row.Name != file.Name || row.SHA384 != file.SHA384 {
			return nil, &LedgerError{LedgerMismatch, fmt.Sprintf("迁移账本对不上：%s", row.Name)}
		}
	}
	return available[len(applied):], nil
}

// LedgerToStoreError 把账本问题换成库的问题（newer → KindNewer，其余 KindUnknown）。
func LedgerToStoreError(err *LedgerError, suffix string) *Error {
	kind := KindUnknown
	if err.Problem == LedgerNewer {
		kind = KindNewer
	}
	return Errorf(kind, "%s，拒绝启动%s", err.Msg, suffix)
}
