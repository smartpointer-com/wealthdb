-- `web_cashflow`: the serving view the Cash Flow dashboards read.
--
-- `web_income`'s shape over the cashflow lines, and for the same
-- reasons. One row per (line, reporting currency) so a currency picker
-- is a column choice rather than a re-query; timestamps as epoch_ms,
-- which is what Metabase's date machinery understands; the account's
-- display name and label resolved through the shared helpers.
--
-- LINE GRAIN, deliberately, and not a pre-netted table. A node's SIDE
-- of the diagram is the sign of its net over the FILTERED window, so
-- the nets and the sides have to be computed where the dashboard's
-- pickers apply. A pre-netted table would be netted over the wrong
-- window the moment a reader moved a picker.
--
-- THE NODE NAMES are carried ready to draw, and that is the one thing
-- this view adds over the report macro it wraps. A Sankey visualisation
-- keys a node by the STRING it is called, so two nodes sharing a name
-- merge into one: a gift given and a gift received would become a
-- single node with a self-edge. Three values of the shared vocabulary
-- reach both operating sections — a gift, an other, and an unplaced row
-- — so a node whose key is one of them takes its direction as a suffix.
-- The test is read off the dimension's own `family` column rather than
-- from a list here, so a delta added as family 'both' is disambiguated
-- the day it lands.
--
-- The cards fold the investing section themselves, from the dashboard's
-- Investing picker: `--investing whole` is a display grain rather than
-- a population, and baking it in would need two views.
CREATE OR REPLACE VIEW web_cashflow AS
    WITH lines AS (
        SELECT t.*,
               CASE WHEN t.section = 'operating_in'  AND (t.grp = '(uncategorized)' OR sg.family = 'both')
                        THEN t.group_label || ' in'
                    WHEN t.section = 'operating_out' AND (t.grp = '(uncategorized)' OR sg.family = 'both')
                        THEN t.group_label || ' out'
                    ELSE t.group_label END AS group_node,
               CASE WHEN t.section = 'operating_in'  AND t.class = '(uncategorized)'
                        THEN t.class_label || ' in'
                    WHEN t.section = 'operating_out' AND t.class = '(uncategorized)'
                        THEN t.class_label || ' out'
                    ELSE t.class_label END AS class_node
          FROM report_cashflow_transactions_multi(0, 9223372036854775807) t
          LEFT JOIN spend_categories sg ON sg.spend_detailed = t.grp
    )
    SELECT epoch_ms(occurred_at * 1000) AS occurred_at,
           silver_source_id, account_external_id,
           account_display_name(display_name, account_external_id) AS display_name,
           account_label(display_name, account_external_id,
                         silver_source_id, account_kind) AS account_label,
           account_kind, kind,
           section, class, class_label, class_node,
           grp, group_label, group_node,
           name,
           value_usd, value_chf, value_eur
      FROM lines;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (84, CAST(epoch(now()) AS BIGINT));
