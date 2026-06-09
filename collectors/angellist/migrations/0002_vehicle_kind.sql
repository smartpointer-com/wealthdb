-- Add vehicles.kind: 'spv' (single-company special-purpose vehicle) or
-- 'fund' (multi-company venture/PE fund). load.py derives it from the
-- AngelList investableGuid suffix: '-f' => fund, anything else => spv.
-- The '-f' set matches portfolio_summary.totalFundsCount exactly (the
-- venture/rolling funds); SPVs and RUVs are single-company. The gold
-- adapter maps spv => asset_class 'spv', fund => 'private_fund'.

ALTER TABLE vehicles ADD COLUMN kind TEXT;

INSERT INTO schema_meta (silver_schema_version) VALUES (2);
