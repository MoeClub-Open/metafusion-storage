-- Lifecycle receipts are storage-owned projections, not foreign keys into the
-- catalog schema. Monotonic versions prevent a delayed pre-merge snapshot from
-- moving references back to a retired identity.
CREATE TABLE IF NOT EXISTS storage.catalog_lifecycle_state (
  entity_id uuid PRIMARY KEY,
  version bigint NOT NULL CHECK (version > 0),
  status text NOT NULL CHECK (status IN ('draft','pending_review','published','deleted','merged')),
  canonical_id uuid NOT NULL,
  entity_updated_at timestamptz NOT NULL,
  applied_at timestamptz NOT NULL DEFAULT now()
);
