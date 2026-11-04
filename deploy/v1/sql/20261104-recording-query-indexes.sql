-- 在新版本启动前，以表所有者连接目标 PostgreSQL 库执行。
-- CONCURRENTLY 不能放入事务；psql 请使用 -v ON_ERROR_STOP=1，不要使用 --single-transaction。
SET lock_timeout = '5s';
SET statement_timeout = '15min';

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_recordings_cid_started_at
    ON public.recordings (cid, started_at DESC);
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_channels_did_created_at
    ON public.channels (did, created_at DESC);

-- 同名无效索引或不同定义不能视为升级成功。
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_index
        WHERE indexrelid = 'public.idx_recordings_cid_started_at'::regclass
          AND indrelid = 'public.recordings'::regclass AND indisvalid AND indisready
          AND indnkeyatts = 2 AND indpred IS NULL
          AND pg_get_indexdef(indexrelid, 1, true) = 'cid'
          AND pg_get_indexdef(indexrelid, 2, true) = 'started_at'
          AND indoption[0] = 0 AND indoption[1] = 3
    ) OR NOT EXISTS (
        SELECT 1 FROM pg_index
        WHERE indexrelid = 'public.idx_channels_did_created_at'::regclass
          AND indrelid = 'public.channels'::regclass AND indisvalid AND indisready
          AND indnkeyatts = 2 AND indpred IS NULL
          AND pg_get_indexdef(indexrelid, 1, true) = 'did'
          AND pg_get_indexdef(indexrelid, 2, true) = 'created_at'
          AND indoption[0] = 0 AND indoption[1] = 3
    ) THEN
        RAISE EXCEPTION '索引无效或定义不符，请核实并修复后再发布应用';
    END IF;
END $$;

-- 两条索引均须 valid=true、ready=true；失败遗留的无效索引不能被 IF NOT EXISTS 修复。
SELECT c.relname AS index_name, i.indisvalid AS valid, i.indisready AS ready,
       pg_get_indexdef(i.indexrelid) AS definition
FROM pg_index i
JOIN pg_class c ON c.oid = i.indexrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = 'public'
  AND c.relname IN ('idx_recordings_cid_started_at', 'idx_channels_did_created_at');

RESET lock_timeout;
RESET statement_timeout;
