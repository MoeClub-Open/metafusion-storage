-- Private workspace/history references retain bytes without publishing them.
-- Catalog owns workspace identity; storage owns asset authorization and GC.
CREATE TABLE storage.catalog_asset_references (
  workspace_id uuid NOT NULL,
  entity_id uuid NOT NULL,
  asset_id uuid NOT NULL REFERENCES storage.assets(id) ON DELETE CASCADE,
  actor_id uuid NOT NULL,
  prepared boolean NOT NULL DEFAULT false,
  expires_at timestamptz DEFAULT (now() + interval '48 hours'),
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY(workspace_id,entity_id,asset_id)
);
CREATE INDEX catalog_asset_references_asset ON storage.catalog_asset_references(asset_id);
CREATE INDEX catalog_asset_references_prepared ON storage.catalog_asset_references(entity_id,asset_id) WHERE prepared;
