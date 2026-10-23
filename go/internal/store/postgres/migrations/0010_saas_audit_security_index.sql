-- 0010_saas_audit_security_index.sql：账号安全日志的保留期清理（audit.retention）按时间扫「没有团队的 security.* 行」，
-- 给它一个部分索引，免得每天全表扫。团队审计走 0008 的 audit_log_team。分区表上建索引会在每个分区上各建一个。
CREATE INDEX audit_log_security ON audit_log (tenant_id, at_ms) WHERE team_id IS NULL AND action LIKE 'security.%';
