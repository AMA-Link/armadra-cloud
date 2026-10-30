package mail

import (
	"strconv"
	"strings"
)

// RenderMetrics 输出本实例的发信计数（Prometheus 文本格式 0.0.4）：只有次数，不带收件地址、模板参数与正文。
// 每个控制面实例各计各的，告警按实例求和。
func (s *Service) RenderMetrics() string {
	var b strings.Builder
	counter := func(name, help string, value int64) {
		b.WriteString("# HELP " + name + " " + help + "\n")
		b.WriteString("# TYPE " + name + " counter\n")
		b.WriteString(name + " " + strconv.FormatInt(value, 10) + "\n")
	}
	counter("armadra_cloud_mail_sent_total", "发出的事务邮件数。", s.sent.Load())
	counter("armadra_cloud_mail_send_failures_total", "发信失败的次数（每次尝试计一次，作业按退避重试）。", s.failed.Load())
	counter("armadra_cloud_mail_abandoned_total", "用尽重试仍没发出、发件箱记失败的邮件数。", s.abandoned.Load())
	counter("armadra_cloud_mail_backlog_dropped_total", "没配 SMTP 时因积压超限丢掉的邮件数。", s.dropped.Load())
	return b.String()
}
