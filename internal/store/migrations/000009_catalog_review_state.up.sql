-- Match the catalog's publication-free lifecycle projection without changing
-- identity, version watermarks, bindings or retention timestamps.
ALTER TABLE storage.catalog_lifecycle_state DROP CONSTRAINT catalog_lifecycle_state_status_check;
UPDATE storage.catalog_lifecycle_state SET status='active' WHERE status IN ('draft','pending_review','published');
ALTER TABLE storage.catalog_lifecycle_state ADD CONSTRAINT catalog_lifecycle_state_status_check CHECK(status IN ('active','deleted','merged'));
