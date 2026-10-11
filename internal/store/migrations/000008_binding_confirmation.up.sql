-- Upload/rebind precedes the catalog picture Save. A repeated binding keeps its
-- original creator/ID/time, but confirms that this reference is being used now.
-- An older catalog snapshot may only withdraw references confirmed before it.
ALTER TABLE storage.bindings ADD COLUMN IF NOT EXISTS confirmed_at timestamptz NOT NULL DEFAULT now();
CREATE INDEX IF NOT EXISTS catalog_lifecycle_canonical ON storage.catalog_lifecycle_state(canonical_id,entity_id);
-- Existing references start conservatively at upgrade time. No old snapshot can
-- erase them until the corresponding identity has a newer catalog write.
