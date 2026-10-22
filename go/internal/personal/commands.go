package personal

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/AMA-Link/armadra-cloud/go/internal/core"
	"github.com/AMA-Link/armadra-cloud/go/internal/store"
	"github.com/AMA-Link/armadra-cloud/go/internal/tlsx"
)

// personal init | status | passwd | rotate-key | links | traffic：只读写存储的子命令。
//
// 与正在运行的 serve 并存：SQLite 的写入走 BEGIN IMMEDIATE 事务、PostgreSQL 的写入先锁租户行，
// serve 每秒察觉一次外部改动，改口令、轮换密钥、撤销链接立即生效（撤销的访客 WS 会被断开）。
// 输出只有账号名、id、时间、指纹这类元数据，不打印口令、令牌或私钥。

const dash = "—"

func formatTime(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05Z")
}

func formatTimePtr(ms *int64) string {
	if ms == nil {
		return dash
	}
	return formatTime(*ms)
}

// coreContext 是 CLI（不经 serve）用的最小装配：只有存储端口与口令策略。
func coreContext(st Store) *core.Context {
	return &core.Context{
		TenantID:       store.DefaultTenant,
		Ports:          st.Ports(),
		PasswordPolicy: core.DefaultPasswordPolicy,
	}
}

// RunInit 是 personal init。
func RunInit(ctx context.Context, o InitOptions, out Print) error {
	issuer := o.PublicOrigin
	if issuer == "" {
		issuer = DefaultIssuer(o.Host, o.Port)
	}
	password, err := ObtainPassword(o.PasswordFile, true)
	if err != nil {
		return err
	}
	st, err := CreateState(ctx, CreateStateInput{
		DataDir: o.DataDir, Account: o.Account, Password: password, Issuer: issuer,
		Store: o.Store, Database: o.Database,
	})
	if err != nil {
		return err
	}
	kind := st.Kind()
	if err := st.Close(); err != nil {
		return err
	}
	out(fmt.Sprintf("已初始化：%s", o.DataDir))
	out(fmt.Sprintf("存储：%s", StoreLabel(kind)))
	out(fmt.Sprintf("账号：%s", o.Account))
	out(fmt.Sprintf("对外地址：%s", issuer))
	if o.TLS == "self-signed" {
		material, err := tlsx.SelfSignedMaterial(o.DataDir, tlsx.Names(o.Host))
		if err != nil {
			return err
		}
		out(fmt.Sprintf("CA 指纹：%s", material.Fingerprint))
	}
	return nil
}

// RunStatus 是 personal status。
func RunStatus(ctx context.Context, dataDir string, db *DatabaseOptions, out Print, now time.Time) error {
	var (
		snap store.TenantSnapshot
		kind StoreKind
	)
	err := WithStore(ctx, dataDir, db, func(st Store) error {
		var err error
		snap, err = st.Snapshot(ctx)
		kind = st.Kind()
		return err
	})
	if err != nil {
		return err
	}
	nowMs := now.UnixMilli()
	account := snap.Accounts[0]
	fingerprint, ok := tlsx.ExistingCAFingerprint(dataDir)
	if !ok {
		fingerprint = dash
	}
	out(fmt.Sprintf("存储：%s", StoreLabel(kind)))
	out(fmt.Sprintf("账号：%s", account.Name))
	out(fmt.Sprintf("对外地址：%s", snap.Tenant.Issuer))
	out(fmt.Sprintf("CA 指纹：%s", fingerprint))
	if account.LockedUntilMs > nowMs {
		out(fmt.Sprintf("锁定到：%s", formatTime(account.LockedUntilMs)))
	}
	keys := make([]string, 0, len(snap.SigningKeys))
	for _, key := range snap.SigningKeys {
		if key.RetiredAtMs == nil {
			keys = append(keys, key.Kid)
		} else {
			keys = append(keys, fmt.Sprintf("%s（退役于 %s）", key.Kid, formatTime(*key.RetiredAtMs)))
		}
	}
	out(fmt.Sprintf("签名钥：%s", strings.Join(keys, "、")))
	var sources []store.SourceRecord
	for _, row := range snap.Sources {
		if row.RevokedAtMs == nil {
			sources = append(sources, row)
		}
	}
	out(fmt.Sprintf("源：%d", len(sources)))
	for _, source := range sources {
		out(fmt.Sprintf("  %s  %s  %s  %s  最近在线 %s",
			source.SourceID, source.Name, source.Kind, source.CoreVersion, formatTimePtr(source.LastSeenAtMs)))
	}
	links := 0
	for _, row := range snap.Links {
		if row.RevokedAtMs == nil && row.ExpiresAtMs > nowMs {
			links++
		}
	}
	out(fmt.Sprintf("链接：%d", links))
	devices := 0
	for _, row := range snap.Devices {
		if row.RevokedAtMs == nil && row.ExpiresAtMs > nowMs {
			devices++
		}
	}
	out(fmt.Sprintf("设备：%d", devices))
	return nil
}

// RunPasswd 是 personal passwd。
func RunPasswd(ctx context.Context, dataDir, passwordFile string, db *DatabaseOptions, out Print) error {
	// 先确认存储能打开，再问口令。
	if err := WithStore(ctx, dataDir, db, func(Store) error { return nil }); err != nil {
		return err
	}
	password, err := ObtainPassword(passwordFile, true)
	if err != nil {
		return err
	}
	err = WithStore(ctx, dataDir, db, func(st Store) error {
		snap, err := st.Snapshot(ctx)
		if err != nil {
			return err
		}
		err = core.ChangePasswordByName(ctx, coreContext(st), snap.Accounts[0].Name, password, time.Now().UnixMilli())
		return PasswordUsageError(err)
	})
	if err != nil {
		return err
	}
	out("口令已更新，账号的全部设备需要重新登录")
	return nil
}

// RunRotateKey 是 personal rotate-key。
func RunRotateKey(ctx context.Context, dataDir string, db *DatabaseOptions, out Print) error {
	var kid string
	err := WithStore(ctx, dataDir, db, func(st Store) error {
		var err error
		kid, err = st.RotateKey(ctx, time.Now().UnixMilli())
		return err
	})
	if err != nil {
		return err
	}
	out(fmt.Sprintf("已启用新签名钥：%s", kid))
	return nil
}

// RunLinksList 是 personal links list。
func RunLinksList(ctx context.Context, dataDir string, db *DatabaseOptions, out Print, now time.Time) error {
	var links []core.LinkSummary
	err := WithStore(ctx, dataDir, db, func(st Store) error {
		var err error
		links, err = core.ListLinks(ctx, coreContext(st), "")
		return err
	})
	if err != nil {
		return err
	}
	if len(links) == 0 {
		out("没有链接")
		return nil
	}
	nowMs := now.UnixMilli()
	for _, link := range links {
		status := "有效"
		switch {
		case link.RevokedAtMs != nil:
			status = "已撤销"
		case link.ExpiresAtMs <= nowMs:
			status = "已过期"
		case link.MaxUses != nil && link.Uses >= *link.MaxUses:
			status = "已用完"
		}
		uses := fmt.Sprintf("%d", link.Uses)
		if link.MaxUses != nil {
			uses = fmt.Sprintf("%d/%d", link.Uses, *link.MaxUses)
		}
		label := link.Label
		if label == "" {
			label = dash
		}
		out(fmt.Sprintf("  %s  %s  %s  %s  %s  到期 %s  源 %s",
			link.LinkID, status, label, link.Role, uses, formatTime(link.ExpiresAtMs), link.SourceID))
	}
	return nil
}

// RunLinksRevoke 是 personal links revoke。
func RunLinksRevoke(ctx context.Context, dataDir, linkID string, db *DatabaseOptions, out Print) error {
	var found bool
	err := WithStore(ctx, dataDir, db, func(st Store) error {
		var err error
		_, found, err = core.RevokeLink(ctx, coreContext(st), linkID, time.Now().UnixMilli())
		return err
	})
	if err != nil {
		return err
	}
	if !found {
		return Usagef("没有这条有效链接：%s", linkID)
	}
	out(fmt.Sprintf("已撤销链接：%s", linkID))
	return nil
}

// FormatBytes 是字节数的可读写法（1024 进位）。
func FormatBytes(bytes int64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	value := float64(bytes)
	unit := 0
	for value >= 1024 && unit < len(units)-1 {
		value /= 1024
		unit++
	}
	if unit == 0 {
		return fmt.Sprintf("%d B", bytes)
	}
	return fmt.Sprintf("%.1f %s", value, units[unit])
}

// displayWidth 按显示宽度算（码点 > 0xff 的算两格：中文）。
func displayWidth(text string) int {
	width := 0
	for _, r := range text {
		if r > 0xff {
			width += 2
		} else {
			width++
		}
	}
	return width
}

// formatTable 是一张对齐的表：最后一列不补空格，行尾空白去掉。
func formatTable(rows [][]string) []string {
	var widths []int
	if len(rows) > 0 {
		widths = make([]int, len(rows[0]))
		for _, row := range rows {
			for column := range widths {
				if column < len(row) {
					widths[column] = max(widths[column], displayWidth(row[column]))
				}
			}
		}
	}
	lines := make([]string, 0, len(rows))
	for _, row := range rows {
		cells := make([]string, len(row))
		for column, cell := range row {
			if column == len(row)-1 {
				cells[column] = cell
			} else {
				cells[column] = cell + strings.Repeat(" ", widths[column]-displayWidth(cell))
			}
		}
		lines = append(lines, strings.TrimRight(strings.Join(cells, "  "), " \t\r\n"))
	}
	return lines
}

// RunTraffic 是 personal traffic [--since 7d] [--source <id>]：读存储里的流量统计，按
// 源 × 账号 × 设备 × 类别合计成一张表，按总字节从多到少。
func RunTraffic(ctx context.Context, o TrafficOptions, out Print, now time.Time) error {
	nowMs := now.UnixMilli()
	var rows []store.TrafficRow
	err := WithStore(ctx, o.DataDir, o.Database, func(st Store) error {
		var err error
		rows, err = st.Ports().Traffic.Query(ctx, store.DefaultTenant, store.TrafficQuery{
			FromMs:      nowMs - o.SinceMs - nowMs%store.HourMs,
			ToMs:        nowMs + 1,
			Granularity: store.GranularityDay,
			Limit:       1_000_000,
			SourceID:    o.SourceID,
		})
		return err
	})
	if err != nil {
		return err
	}
	merged := []*store.TrafficRow{}
	index := map[string]int{}
	for _, row := range rows {
		key := strings.Join([]string{row.SourceID, row.Subject, row.DeviceID, row.Kind}, "\x00")
		if at, ok := index[key]; ok {
			merged[at].Connections += row.Connections
			merged[at].BytesToSource += row.BytesToSource
			merged[at].BytesToClient += row.BytesToClient
			continue
		}
		copied := row
		index[key] = len(merged)
		merged = append(merged, &copied)
	}
	if len(merged) == 0 {
		out("没有流量记录")
		return nil
	}
	sort.SliceStable(merged, func(i, j int) bool {
		return merged[i].BytesToSource+merged[i].BytesToClient > merged[j].BytesToSource+merged[j].BytesToClient
	})
	var connections, up, down int64
	table := [][]string{{"源", "账号", "设备", "类别", "连接", "上行", "下行"}}
	for _, row := range merged {
		connections += row.Connections
		up += row.BytesToSource
		down += row.BytesToClient
		device := row.DeviceID
		if device == "" {
			device = dash
		}
		table = append(table, []string{
			row.SourceID, row.Subject, device, row.Kind,
			fmt.Sprintf("%d", row.Connections), FormatBytes(row.BytesToSource), FormatBytes(row.BytesToClient),
		})
	}
	table = append(table, []string{"合计", "", "", "", fmt.Sprintf("%d", connections), FormatBytes(up), FormatBytes(down)})
	for _, line := range formatTable(table) {
		out(line)
	}
	return nil
}
