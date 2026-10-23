package server_test

import (
	"context"
	"testing"
)

// 设备码批准 / 拒绝写账号安全日志（S3 留的钩子，设计 §4.3）：设备名是发起设备报的名字的快照。
func TestDeviceDecisionWritesSecurityLog(t *testing.T) {
	r := newRig(t, rigOptions{})
	browser := r.registerCookie(r.email())

	_, approved := r.startDevice(nil)
	r.expect(r.approve(browser, approved, true), 200, "")
	_, denied := r.startDevice(nil)
	r.expect(r.approve(browser, denied, false), 200, "")

	rows, err := r.run.DB.Pool().Query(context.Background(),
		"SELECT action, device_name FROM audit_log WHERE account_id = $1 AND action LIKE 'security.device%' ORDER BY action", browser.accountID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var action, name string
		if err := rows.Scan(&action, &name); err != nil {
			t.Fatal(err)
		}
		got[action] = name
	}
	if len(got) != 2 || got["security.deviceApprove"] != "我的 Mac" || got["security.deviceDeny"] != "我的 Mac" {
		t.Fatalf("安全日志：%v", got)
	}
}
