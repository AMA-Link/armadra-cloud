// Package sqlite 是控制核存储端口的 SQLite 实现（modernc.org/sqlite，纯 Go），
// 与 packages/control-store-sqlite 读写同一个库文件。
//
//   - 新建（Create）：先以 0600 独占建文件，再设 WAL、application_id、跑迁移；文件已在就拒绝。
//   - 打开已有的：不是 SQLite、application_id 不对、quick_check 不过、迁移账本对不上、库比程序新
//     ——一律拒绝启动（store.Error），绝不自动清库或重建。账本是前缀时补跑新迁移。
//   - 同一文件可以被多个进程同时打开：写都在 BEGIN IMMEDIATE 事务里，busy_timeout 5 秒内等
//     对方提交。进程内只有一条连接，读写串行；跨进程的变更用 data_version 察觉。
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"os"
	"sync"

	_ "modernc.org/sqlite" // 注册 "sqlite" 驱动

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

// ApplicationID 是 PRAGMA application_id：「ARMD」。别的程序的 SQLite 库一眼认出来，拒绝打开。
const ApplicationID = 0x41524d44

const busyTimeoutMs = 5_000

//go:embed migrations/*.sql
var migrationFS embed.FS

// Migrations 是随包的迁移（拷自 packages/control-store-sql/migrations）。
func Migrations() ([]store.Migration, error) {
	return store.LoadMigrations(migrationFS, "migrations")
}

// DB 是一个打开的库：一条连接，读写串行。
type DB struct {
	path string

	mu     sync.Mutex
	db     *sql.DB
	conn   *sql.Conn
	closed bool
}

// Options 是打开库的选项。
type Options struct {
	// Create 为真时新建（文件必须不存在）；缺省打开已有的。
	Create bool
	// Migrations 缺省用随包的。
	Migrations []store.Migration
	// Now 缺省 time.Now；账本的 applied_at_ms 用它。
	Now func() int64
}

var _ store.Backend = (*DB)(nil)

// Path 是库文件的位置。
func (d *DB) Path() string { return d.path }

func removeFiles(path string) {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(path + suffix)
	}
}

// Open 打开（或新建）库并迁移到最新。
func Open(ctx context.Context, path string, options Options) (*DB, error) {
	migrations := options.Migrations
	if migrations == nil {
		var err error
		if migrations, err = Migrations(); err != nil {
			return nil, store.Errorf(store.KindUnknown, "迁移文件打包有误：%v，拒绝执行", err)
		}
	}
	now := options.Now
	if now == nil {
		now = nowMs
	}
	info, statErr := os.Stat(path)
	exists := statErr == nil
	if options.Create && exists {
		return nil, store.Errorf(store.KindExists, "数据库已存在：%s", path)
	}
	if !options.Create && !exists {
		return nil, store.Errorf(store.KindNotFound, "数据库不存在：%s", path)
	}
	if options.Create {
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, store.Errorf(store.KindCorrupt, "数据库建不出来：%s", path)
		}
		_ = file.Close()
	} else if info.Size() == 0 {
		return nil, store.Errorf(store.KindUnknown, "数据库是空文件，拒绝启动：%s", path)
	}

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		if options.Create {
			removeFiles(path)
		}
		return nil, wrapOpenError(path, err)
	}
	// 一条连接：进程内读写串行，data_version 只在别的进程提交后才变。
	raw.SetMaxOpenConns(1)
	raw.SetMaxIdleConns(1)
	raw.SetConnMaxLifetime(0)
	conn, err := raw.Conn(ctx)
	if err != nil {
		_ = raw.Close()
		if options.Create {
			removeFiles(path)
		}
		return nil, wrapOpenError(path, err)
	}
	db := &DB{path: path, db: raw, conn: conn}
	if err := db.prepare(ctx, options.Create, migrations, now); err != nil {
		_ = db.Close()
		// 自己刚建的文件没写成：删掉，下次还能重建；已有的库一个字节也不动。
		if options.Create {
			removeFiles(path)
		}
		return nil, wrapOpenError(path, err)
	}
	return db, nil
}

func wrapOpenError(path string, err error) error {
	if _, ok := err.(*store.Error); ok {
		return err
	}
	return store.Errorf(store.KindCorrupt, "数据库打不开或已损坏，拒绝启动：%s", path)
}

func (d *DB) prepare(ctx context.Context, create bool, migrations []store.Migration, now func() int64) error {
	if _, err := d.conn.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout = %d", busyTimeoutMs)); err != nil {
		return err
	}
	var applicationID int64
	if err := d.conn.QueryRowContext(ctx, "PRAGMA application_id").Scan(&applicationID); err != nil {
		return err
	}
	if create {
		if err := d.pragmaWAL(ctx); err != nil {
			return err
		}
		if _, err := d.conn.ExecContext(ctx, fmt.Sprintf("PRAGMA application_id = %d", ApplicationID)); err != nil {
			return err
		}
	} else {
		if applicationID != ApplicationID {
			return store.Errorf(store.KindUnknown, "不是本程序的数据库，拒绝启动：%s", d.path)
		}
		var check string
		if err := d.conn.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check); err != nil {
			return err
		}
		if check != "ok" {
			return store.Errorf(store.KindCorrupt, "数据库校验不通过，拒绝启动：%s", d.path)
		}
		if err := d.pragmaWAL(ctx); err != nil {
			return err
		}
	}
	if _, err := d.conn.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		return err
	}
	_, err := d.migrate(ctx, migrations, now)
	return err
}

func (d *DB) pragmaWAL(ctx context.Context) error {
	var mode string
	return d.conn.QueryRowContext(ctx, "PRAGMA journal_mode = WAL").Scan(&mode)
}

// Close 关库（幂等）。
func (d *DB) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	_ = d.conn.Close()
	return d.db.Close()
}

// read 在持锁的连接上读（不开事务）。
func (d *DB) read(ctx context.Context, fn func(q *sql.Conn) error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return sql.ErrConnDone
	}
	return fn(d.conn)
}

// write 开 BEGIN IMMEDIATE 写事务：一开始就拿写锁，读-改-写之间别人插不进来。
// fn 返回错误就回滚并原样返回。
func (d *DB) write(ctx context.Context, fn func(q *sql.Conn) error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return sql.ErrConnDone
	}
	if _, err := d.conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	if err := fn(d.conn); err != nil {
		_, _ = d.conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		return err
	}
	if _, err := d.conn.ExecContext(ctx, "COMMIT"); err != nil {
		_, _ = d.conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		return err
	}
	return nil
}

// snapshot 在一个读事务里取一致的多表读。
func (d *DB) snapshot(ctx context.Context, fn func(q *sql.Conn) error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return sql.ErrConnDone
	}
	if _, err := d.conn.ExecContext(ctx, "BEGIN"); err != nil {
		return err
	}
	err := fn(d.conn)
	_, _ = d.conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
	return err
}

// DataVersion 别的连接（别的进程）提交过就变，自己的提交不变：serve 用它察觉 CLI 的改动。
func (d *DB) DataVersion(ctx context.Context) (int64, error) {
	var version int64
	err := d.read(ctx, func(q *sql.Conn) error {
		return q.QueryRowContext(ctx, "PRAGMA data_version").Scan(&version)
	})
	return version, err
}

// IntegrityCheck 是 PRAGMA integrity_check 的结果行；只有一行 ok 是好的。
func (d *DB) IntegrityCheck(ctx context.Context) ([]string, error) {
	var out []string
	err := d.read(ctx, func(q *sql.Conn) error {
		rows, err := q.QueryContext(ctx, "PRAGMA integrity_check")
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				return err
			}
			out = append(out, line)
		}
		return rows.Err()
	})
	return out, err
}

// Backup 在线、一致地备份（VACUUM INTO）；目标已存在就拒绝。备份文件 0600。
func (d *DB) Backup(ctx context.Context, target string) error {
	if _, err := os.Stat(target); err == nil {
		return store.Errorf(store.KindExists, "备份目标已存在：%s", target)
	}
	// VACUUM INTO 要目标不存在或为空：先建一个 0600 的空文件，权限随它。
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return store.Errorf(store.KindExists, "备份目标建不出来：%s", target)
	}
	_ = file.Close()
	err = d.read(ctx, func(q *sql.Conn) error {
		_, err := q.ExecContext(ctx, "VACUUM INTO ?", target)
		return err
	})
	if err != nil {
		_ = os.Remove(target)
	}
	return err
}
