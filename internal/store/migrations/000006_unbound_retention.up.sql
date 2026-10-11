-- 已完成资产从最后解绑起保留；存量无绑定资产从迁移时刻保守起算。
ALTER TABLE storage.assets ADD COLUMN IF NOT EXISTS unbound_since timestamptz;
-- 删除字节前持久登记不可逆阶段：进程中断/数据库提交失败后禁止再次激活，worker 可重试。
ALTER TABLE storage.assets ADD COLUMN IF NOT EXISTS reclaim_deleting boolean NOT NULL DEFAULT false;

UPDATE storage.assets a SET unbound_since=now()
WHERE a.status='complete' AND a.unbound_since IS NULL
  AND NOT EXISTS (SELECT 1 FROM storage.bindings b WHERE b.asset_id=a.id);
CREATE INDEX IF NOT EXISTS assets_unbound_retention ON storage.assets(unbound_since)
  WHERE status='complete' AND NOT blocked;

CREATE OR REPLACE FUNCTION storage.asset_unbound_status() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.status <> 'complete' THEN
    NEW.unbound_since := NULL;
  ELSIF TG_OP = 'INSERT' OR OLD.status <> 'complete' THEN
    IF EXISTS (SELECT 1 FROM storage.bindings WHERE asset_id=NEW.id) THEN
      NEW.unbound_since := NULL;
    ELSE
      NEW.unbound_since := now();
    END IF;
  END IF;
  RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS assets_unbound_status ON storage.assets;
CREATE TRIGGER assets_unbound_status BEFORE INSERT OR UPDATE OF status ON storage.assets
  FOR EACH ROW EXECUTE FUNCTION storage.asset_unbound_status();

-- 与回收统一先锁父资产；直接 SQL 和批量生命周期操作也必须遵守删除门禁。
CREATE OR REPLACE FUNCTION storage.binding_reclaim_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE deleting boolean;
BEGIN
  IF TG_OP = 'DELETE' THEN
    PERFORM id FROM storage.assets WHERE id=OLD.asset_id FOR UPDATE;
    RETURN OLD;
  END IF;
  IF TG_OP = 'UPDATE' THEN
    PERFORM id FROM storage.assets WHERE id IN (OLD.asset_id, NEW.asset_id) ORDER BY id FOR UPDATE;
  END IF;
  SELECT reclaim_deleting INTO deleting FROM storage.assets WHERE id=NEW.asset_id FOR UPDATE;
  IF deleting THEN
    RAISE EXCEPTION 'asset_reclaiming' USING ERRCODE='55000';
  END IF;
  RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS bindings_reclaim_guard ON storage.bindings;
CREATE TRIGGER bindings_reclaim_guard BEFORE INSERT OR DELETE OR UPDATE OF asset_id ON storage.bindings
  FOR EACH ROW EXECUTE FUNCTION storage.binding_reclaim_guard();

CREATE OR REPLACE FUNCTION storage.binding_unbound_changed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP <> 'DELETE' THEN
    UPDATE storage.assets SET unbound_since=NULL, reclaim_token='', reclaim_claimed_at=NULL
      WHERE id=NEW.asset_id;
  END IF;
  IF TG_OP = 'DELETE' OR (TG_OP = 'UPDATE' AND OLD.asset_id <> NEW.asset_id) THEN
    UPDATE storage.assets a SET unbound_since=COALESCE(a.unbound_since, now())
      WHERE a.id=OLD.asset_id AND a.status='complete'
        AND NOT EXISTS (SELECT 1 FROM storage.bindings b WHERE b.asset_id=a.id);
  END IF;
  RETURN NULL;
END $$;
DROP TRIGGER IF EXISTS bindings_unbound_changed ON storage.bindings;
CREATE TRIGGER bindings_unbound_changed AFTER INSERT OR DELETE OR UPDATE OF asset_id ON storage.bindings
  FOR EACH ROW EXECUTE FUNCTION storage.binding_unbound_changed();
