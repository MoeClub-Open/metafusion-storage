-- The catalog lifecycle projection uses the same single state field as catalog.
ALTER TABLE storage.catalog_lifecycle_state RENAME COLUMN status TO state;
ALTER TABLE storage.catalog_lifecycle_state RENAME CONSTRAINT catalog_lifecycle_state_status_check TO catalog_lifecycle_state_state_check;
