-- PostgreSQL 多节点录像升级；先备份，在 gowvp 目标库通过 psql -v ON_ERROR_STOP=1 -f 执行。
-- CREATE INDEX CONCURRENTLY 不可放入事务，也不能使用 psql --single-transaction。
ALTER TABLE media_servers ADD COLUMN IF NOT EXISTS record_mount_path text NOT NULL DEFAULT '';
ALTER TABLE recordings ADD COLUMN IF NOT EXISTS media_server_id text NOT NULL DEFAULT '';
ALTER TABLE recordings ADD COLUMN IF NOT EXISTS vhost text NOT NULL DEFAULT '__defaultVhost__';
ALTER TABLE recordings ADD COLUMN IF NOT EXISTS storage_root text NOT NULL DEFAULT '';
ALTER TABLE recordings ADD COLUMN IF NOT EXISTS file_path text NOT NULL DEFAULT '';
ALTER TABLE recordings ADD COLUMN IF NOT EXISTS file_key varchar(64);

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_recordings_node_started ON recordings (media_server_id, started_at DESC);
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_recordings_file_key ON recordings (file_key);

-- 返回的两个索引都必须 indisvalid=true、indisready=true。
SELECT c.relname, i.indisvalid, i.indisready
FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid
WHERE i.indrelid='recordings'::regclass
  AND c.relname IN ('idx_recordings_node_started', 'idx_recordings_file_key');

-- 若中断后留下无效索引，先核对再单独执行对应 DROP INDEX CONCURRENTLY，随后重跑脚本。
-- DROP INDEX CONCURRENTLY IF EXISTS idx_recordings_node_started;
-- DROP INDEX CONCURRENTLY IF EXISTS idx_recordings_file_key;
-- 不对旧录像直接设置 media_server_id，不根据通道当前绑定推断历史来源。
