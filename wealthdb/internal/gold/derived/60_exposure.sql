-- Exposure: each holding looked through to its leaves, the labels of
-- the leaves, and the exposure reports.

-- exposure_norm: a label in the form the maps hold it.
CREATE MACRO exposure_norm(p_label) AS
    lower(trim(regexp_replace(p_label, '\s+', ' ', 'g')));

-- exposure_settings_row: the settings, the defaults where none is
-- stamped yet.
CREATE MACRO exposure_settings_row() AS TABLE (
    SELECT COALESCE(MAX(max_gap_days), 400) AS max_gap_days,
           COALESCE(MAX(aged_days), 35) AS aged_days,
           COALESCE(MAX(partial_pct), 3.0) AS partial_pct
      FROM exposure_settings
);

-- exposure_quality: an aggregate over a group of leaves, each flag the
-- group's leaves carry with the share of the group's gross value it
-- covers ("proxy=12%", "aged=<1%"), comma-separated in a fixed order.
CREATE MACRO exposure_flag_share(p_flag, p_part, p_total) AS
    CASE WHEN COALESCE(p_part, 0) > 0 AND p_total > 0 THEN
         p_flag || '=' || CASE WHEN 100 * p_part / p_total < 1 THEN '<1'
                               ELSE CAST(round(100 * p_part / p_total) AS INTEGER)::VARCHAR END || '%' END;

-- exposure_quality: an aggregate over a group of leaves, each flag the
-- group's leaves carry with the share of the group's gross value it
-- covers ("proxy=12%", "aged=<1%"), comma-separated in a fixed order.
CREATE MACRO exposure_quality(p_flags, p_value) AS
    array_to_string(list_filter([
        exposure_flag_share('proxy', SUM(abs(p_value)) FILTER (WHERE list_contains(p_flags, 'proxy')), SUM(abs(p_value))),
        exposure_flag_share('declared', SUM(abs(p_value)) FILTER (WHERE list_contains(p_flags, 'declared')), SUM(abs(p_value))),
        exposure_flag_share('aged', SUM(abs(p_value)) FILTER (WHERE list_contains(p_flags, 'aged')), SUM(abs(p_value))),
        exposure_flag_share('forward', SUM(abs(p_value)) FILTER (WHERE list_contains(p_flags, 'forward')), SUM(abs(p_value))),
        exposure_flag_share('partial', SUM(abs(p_value)) FILTER (WHERE list_contains(p_flags, 'partial')), SUM(abs(p_value))),
        exposure_flag_share('unmapped', SUM(abs(p_value)) FILTER (WHERE list_contains(p_flags, 'unmapped')), SUM(abs(p_value))),
        exposure_flag_share('by_name', SUM(abs(p_value)) FILTER (WHERE list_contains(p_flags, 'by_name')), SUM(abs(p_value))),
        exposure_flag_share('opaque', SUM(abs(p_value)) FILTER (WHERE list_contains(p_flags, 'opaque')), SUM(abs(p_value))),
        exposure_flag_share('undeclared', SUM(abs(p_value)) FILTER (WHERE list_contains(p_flags, 'undeclared')), SUM(abs(p_value))),
        exposure_flag_share('unmatched', SUM(abs(p_value)) FILTER (WHERE list_contains(p_flags, 'unmatched')), SUM(abs(p_value)))
    ], lambda x: x IS NOT NULL), ',');

-- exposure_public: a holding of a listed kind, which a source can
-- describe. Anything else is private, a private company's shares and
-- notes among it.
CREATE MACRO exposure_public(p_vehicle, p_class) AS
    (p_vehicle IN ('stock', 'bond') AND p_class NOT IN ('private_equity', 'private_debt'))
        OR (p_vehicle IN ('etf', 'fund')
            AND p_class IN ('public_equity', 'fixed_income', 'multi_asset', 'infrastructure', 'cash', 'other'));

-- exposure_situs_at: per holding, its situs at the instant: the latest
-- row dated on or before it, else the one before any dated record.
CREATE MACRO exposure_situs_at(p_asof) AS TABLE (
    SELECT s.*
      FROM exposure_situs s
     WHERE s.as_of IS NULL OR s.as_of <= p_asof
   QUALIFY ROW_NUMBER() OVER (PARTITION BY s.silver_source_id, s.account_external_id, s.position_key
                              ORDER BY s.as_of DESC NULLS LAST) = 1
);

-- exposure_situs_quality: an aggregate over a group of leaves, the
-- share of the group's gross value each situs basis covers
-- ("declared=12%,isin=80%"), in the rule's order.
CREATE MACRO exposure_situs_quality(p_basis, p_value) AS
    array_to_string(list_filter([
        exposure_flag_share('declared', SUM(abs(p_value)) FILTER (WHERE p_basis = 'declared'), SUM(abs(p_value))),
        exposure_flag_share('underlying', SUM(abs(p_value)) FILTER (WHERE p_basis = 'underlying'), SUM(abs(p_value))),
        exposure_flag_share('isin', SUM(abs(p_value)) FILTER (WHERE p_basis = 'isin'), SUM(abs(p_value))),
        exposure_flag_share('domicile', SUM(abs(p_value)) FILTER (WHERE p_basis = 'domicile'), SUM(abs(p_value))),
        exposure_flag_share('custody', SUM(abs(p_value)) FILTER (WHERE p_basis = 'custody'), SUM(abs(p_value))),
        exposure_flag_share('country', SUM(abs(p_value)) FILTER (WHERE p_basis = 'country'), SUM(abs(p_value))),
        exposure_flag_share('unknown', SUM(abs(p_value)) FILTER (WHERE p_basis = 'unknown'), SUM(abs(p_value)))
    ], lambda x: x IS NOT NULL), ',');

-- instrument_terms_at: each instrument's terms on a day: instrument_terms
-- with the ledger's terms and rating records in force on it laid over,
-- field by field, origin declared. The record in force is the latest
-- dated on or before the day, else the one without a date; a newer
-- record replaces an older one whole. A record names an instrument no
-- source states terms for too, a fund's factsheet figures among them
-- (duration, yield_pct). A bond with a coupon and no kind stated is fixed
-- by convention, or zero at a coupon of 0. A record that makes a bond
-- floating drops the frequency its market's convention gave it: a
-- floating note's frequency is its reset's, quarterly where nothing
-- states one. A rating record clears a source's default: the grade it
-- states is the bond's. fixed_until and reset_date are a terms record's
-- alone; terms_as_of is its day.
CREATE MACRO instrument_terms_at(p_asof) AS TABLE (
    WITH d AS (
        SELECT * FROM yield_declared_terms
         WHERE as_of IS NULL OR as_of <= p_asof
       QUALIFY row_number() OVER (PARTITION BY silver_source_id, instrument_external_id
                                  ORDER BY as_of DESC NULLS LAST, line_no DESC) = 1),
    r AS (
        SELECT * FROM yield_declared_ratings
         WHERE as_of IS NULL OR as_of <= p_asof
       QUALIFY row_number() OVER (PARTITION BY silver_source_id, instrument_external_id
                                  ORDER BY as_of DESC NULLS LAST, line_no DESC) = 1),
    k AS (
        SELECT silver_source_id, instrument_external_id FROM instrument_terms
        UNION SELECT silver_source_id, instrument_external_id FROM d
        UNION SELECT silver_source_id, instrument_external_id FROM r),
    m AS (
        SELECT k.silver_source_id, k.instrument_external_id, t.vehicle,
               COALESCE(d.coupon_pct, t.coupon_pct) AS coupon_pct,
               CASE WHEN d.coupon_pct IS NOT NULL THEN 'declared' ELSE t.coupon_origin END AS coupon_origin,
               COALESCE(d.maturity, t.maturity) AS maturity,
               CASE WHEN d.maturity IS NOT NULL THEN 'declared' ELSE t.maturity_origin END AS maturity_origin,
               CASE WHEN d.frequency IS NOT NULL THEN d.frequency
                    WHEN d.coupon_kind = 'floating' AND t.frequency_origin = 'convention' THEN NULL
                    ELSE t.frequency END AS frequency,
               CASE WHEN d.frequency IS NOT NULL THEN 'declared'
                    WHEN d.coupon_kind = 'floating' AND t.frequency_origin = 'convention' THEN NULL
                    ELSE t.frequency_origin END AS frequency_origin,
               d.coupon_kind AS declared_kind, t.coupon_kind AS source_kind, t.coupon_kind_origin AS source_kind_origin,
               COALESCE(d.day_count, t.day_count) AS day_count,
               CASE WHEN d.day_count IS NOT NULL THEN 'declared' ELSE t.day_count_origin END AS day_count_origin,
               COALESCE(d.call_date, t.call_date) AS call_date,
               CASE WHEN d.call_date IS NOT NULL THEN 'declared' ELSE t.call_origin END AS call_origin,
               COALESCE(t.make_whole, FALSE) AND d.call_date IS NULL AS make_whole,
               COALESCE(r.rating, t.rating) AS rating,
               CASE WHEN r.rating IS NOT NULL THEN 'declared' ELSE t.rating_origin END AS rating_origin,
               CASE WHEN r.rating IS NOT NULL THEN FALSE ELSE COALESCE(t.in_default, FALSE) END AS in_default,
               d.fixed_until, d.reset_date, d.duration, d.yield_pct, d.as_of AS terms_as_of
          FROM k
          LEFT JOIN instrument_terms t USING (silver_source_id, instrument_external_id)
          LEFT JOIN d USING (silver_source_id, instrument_external_id)
          LEFT JOIN r USING (silver_source_id, instrument_external_id))
    SELECT m.* EXCLUDE (declared_kind, source_kind, source_kind_origin),
           CASE WHEN declared_kind IS NOT NULL THEN declared_kind
                WHEN source_kind IS NOT NULL THEN source_kind
                WHEN vehicle = 'bond' AND coupon_pct = 0 THEN 'zero'
                WHEN vehicle = 'bond' AND coupon_pct > 0 THEN 'fixed' END AS coupon_kind,
           CASE WHEN declared_kind IS NOT NULL THEN 'declared'
                WHEN source_kind IS NOT NULL THEN source_kind_origin
                WHEN vehicle = 'bond' AND coupon_pct IS NOT NULL THEN 'convention' END AS coupon_kind_origin
      FROM m
);

-- exposure_label_words: a camel-cased key as words: its first letter in
-- upper case, then a space at each step from lower to upper case and
-- after a figure (aA as AA, notRated as Not Rated, 5to10Years as 5 to10
-- Years). A label in words stays as it is.
CREATE MACRO exposure_label_words(p_label) AS
    regexp_replace(regexp_replace(upper(left(p_label, 1)) || substr(p_label, 2), '([a-z])([A-Z])', '\1 \2', 'g'),
                   '([0-9])([A-Za-z])', '\1 \2', 'g');

-- exposure_rating_label: the bucket of a label a fund's credit-quality
-- breakdown or a description states (AAA, AA+, Baa1, (P)A2, AA Rated,
-- Below B, B and below, Not Rated, Cash and/or Derivatives, or the same
-- as one camel-cased key: aA, notRated, cCCandbelow): AAA to B (B and
-- below; a grade of default among them), unrated, cash, else NULL.
-- exposure_rating_grade reads a label once it is upper case and bare.
CREATE MACRO exposure_rating_grade(g) AS
    CASE WHEN g LIKE 'CASH%' OR g LIKE '%DERIVATIVE%' THEN 'cash'
         WHEN g IN ('NR', 'N/R', 'NOT RATED', 'NOTRATED', 'UNRATED', 'NON RATED', 'NON-RATED', 'NONRATED',
                    'NO RATING', 'NORATING', 'N/A', 'NA', 'WR', 'WD') THEN 'unrated'
         WHEN regexp_matches(g, '^AAA[+-]?( RATED)?$') THEN 'AAA'
         WHEN regexp_matches(g, '^(AA[+-]?|AA[1-3])( RATED)?$') THEN 'AA'
         WHEN regexp_matches(g, '^(A[+-]?|A[1-3])( RATED)?$') THEN 'A'
         WHEN regexp_matches(g, '^(BBB[+-]?|BAA[1-3]?)( RATED)?$') THEN 'BBB'
         WHEN regexp_matches(g, '^(BB[+-]?|BA[1-3]?)( RATED)?$') THEN 'BB'
         WHEN regexp_matches(g,
                  '^(B[+-]?|B[1-3]|CCC.*|CC|C|D|DD|DDD|RD|SD|CAA.*|CA|BELOW ?[BC].*|BB? ?AND ?BELOW)( RATED)?$')
              THEN 'B' END;

CREATE MACRO exposure_rating_label(p_label) AS
    exposure_rating_grade(regexp_replace(trim(regexp_replace(regexp_replace(upper(
        exposure_label_words(trim(COALESCE(p_label, '')))),
        '^\(P\)\s*', ''), '[*()]', '', 'g')), '\s+', ' ', 'g'));

-- exposure_maturity_band: the bucket of a time to maturity in years.
CREATE MACRO exposure_maturity_band(p_years) AS
    CASE WHEN p_years < 1 THEN 'under 1y' WHEN p_years < 3 THEN '1-3y' WHEN p_years < 5 THEN '3-5y'
         WHEN p_years < 10 THEN '5-10y' WHEN p_years < 20 THEN '10-20y' ELSE 'over 20y' END;

-- exposure_maturity_years: the first figure of a band a fund's maturity
-- breakdown states, in years (a band in months over twelve).
-- exposure_maturity_upper: its second, after a dash or TO; NULL for one
-- figure.
CREATE MACRO exposure_maturity_years(p_label) AS
    TRY_CAST(regexp_extract(p_label, '([0-9]+(\.[0-9]+)?)', 1) AS DOUBLE)
        / CASE WHEN upper(p_label) LIKE '%MONTH%' THEN 12 ELSE 1 END;

CREATE MACRO exposure_maturity_upper(p_label) AS
    TRY_CAST(regexp_extract(upper(p_label), '[0-9.]+\s*(-|–|TO)\s*([0-9]+(\.[0-9]+)?)', 2) AS DOUBLE)
        / CASE WHEN upper(p_label) LIKE '%MONTH%' THEN 12 ELSE 1 END;

-- exposure_maturity_label: the bucket of a band a fund's maturity
-- breakdown states (0 - 1 Years, 6-12 Months, 20+ Years, > 30 Years,
-- Under 1 Year, 10 Years and over, or one camel-cased key: 5to10Years).
-- A band that lies in one bucket reads that bucket. One that spans
-- buckets reads NULL: a closed band across a bucket's edge, an open band
-- from under 20 years up, one up to more than a year. Cash; else NULL.
-- exposure_maturity_words reads a band once it is upper case and spaced.
CREATE MACRO exposure_maturity_words(u) AS
    CASE WHEN u LIKE 'CASH%' OR u LIKE '%DERIVATIVE%' THEN 'cash'
         WHEN regexp_matches(u, '^(>|OVER|MORE ?THAN|GREATER ?THAN|ABOVE)\s*[0-9]')
              OR regexp_matches(u, '^[0-9.]+\s*(\+|PLUS\b)')
              OR regexp_matches(u, '^[0-9].*(\+| AND (OVER|ABOVE|LONGER)| OR (MORE|LONGER))$') THEN
              CASE WHEN exposure_maturity_years(u) >= 20 THEN 'over 20y' END
         WHEN regexp_matches(u, '^(<|UNDER|LESS ?THAN|UP ?TO|BELOW)\s*[0-9]') THEN
              CASE WHEN exposure_maturity_years(u) <= 1 THEN 'under 1y' END
         WHEN NOT regexp_matches(u, '^[0-9]') THEN NULL
         WHEN exposure_maturity_upper(u) IS NOT NULL AND exposure_maturity_band(exposure_maturity_years(u))
              <> exposure_maturity_band(exposure_maturity_upper(u) - 1e-6) THEN NULL
         ELSE exposure_maturity_band(exposure_maturity_years(u)) END;

CREATE MACRO exposure_maturity_label(p_label) AS
    exposure_maturity_words(upper(trim(exposure_label_words(trim(COALESCE(p_label, ''))))));

-- exposure_maturity_bucket: the bucket of a maturity on a day: under a
-- year to over 20 years, perpetual from a century on. A maturity exactly
-- N years on is in the longer bucket.
CREATE MACRO exposure_maturity_bucket(p_day, p_maturity) AS
    CASE WHEN p_maturity < p_day + INTERVAL 1 YEAR THEN 'under 1y'
         WHEN p_maturity < p_day + INTERVAL 3 YEAR THEN '1-3y'
         WHEN p_maturity < p_day + INTERVAL 5 YEAR THEN '3-5y'
         WHEN p_maturity < p_day + INTERVAL 10 YEAR THEN '5-10y'
         WHEN p_maturity < p_day + INTERVAL 20 YEAR THEN '10-20y'
         WHEN p_maturity < p_day + INTERVAL 100 YEAR THEN 'over 20y'
         ELSE 'perpetual' END;

-- exposure_issuer_text: a name upper case, ß, Ø and the accents read
-- as plain letters, its words up to the first figure after the first
-- word (a coupon, a date), N/B and A/S as one word, U.S., UNITED STATES
-- and TREAS as US and TREASURY, punctuation as spaces, a leading THE
-- dropped.
CREATE MACRO exposure_issuer_text(p_name) AS
    regexp_replace(regexp_replace(regexp_replace(regexp_replace(regexp_replace(regexp_replace(regexp_replace(
        upper(strip_accents(replace(replace(replace(replace(COALESCE(p_name, ''), 'ß', 'ss'), 'ẞ', 'SS'), 'Ø', 'O'),
                                    'ø', 'o'))),
        '^(\S+.*?)\s+\S*[0-9].*$', '\1'),
        '\b([A-Z])/([A-Z])\b', '\1\2', 'g'),
        '\bU\.\s?S\.?(\s|$)', 'US\1', 'g'),
        '^UNITED STATES\b', 'US'),
        '\bTREAS\b', 'TREASURY', 'g'),
        '[^A-Z0-9& ]', ' ', 'g'),
        '^\s*THE(\s+|$)', '');

-- exposure_issuer_name: a name as the issuers view groups it: its text
-- (exposure_issuer_text), its first word kept, from the others a class
-- letter (CLASS A, CL B) and the words of a legal form, a share class or
-- a bond's kind (INC, CORP, PLC, AG, ADR, NOTE, MTN, SR UNSECURED)
-- dropped.
CREATE MACRO exposure_issuer_name(p_name) AS
    nullif(trim(regexp_replace(
        regexp_extract(exposure_issuer_text(p_name), '^\s*(\S*)', 1) || ' ' ||
        regexp_replace(regexp_replace(exposure_issuer_text(p_name), '^\s*\S*', ''),
            '\b(CLASS|CL|SERIES|SER)\s+[A-Z]\b'
            || '|\b(THE|INC|INCORPORATED|CORP|CORPORATION|CO|COMPANY|LTD|LIMITED|PLC|AG|SA|NV|SE|AB|AS|ASA|SPA'
            || '|LLC|LP|HOLDINGS|HOLDING|GROUP|CLASS|CL|COM|COMMON|STOCK|ORD|SHS|SHARES|ADR|ADS|GDR|SPONSORED'
            || '|UNSPONSORED|REG|REGISTERED|NEW|NB|NOTE|NOTES|BOND|BONDS|BILL|BILLS|MTN|EMTN|GMTN|SR|SNR|SENIOR'
            || '|SUB|SUBORDINATED|UNSECURED|SECURED|REGS|144A|FRN)\b',
            ' ', 'g'),
        '\s+', ' ', 'g')), '');

-- exposure_credit_bucket: a leaf's credit bucket: cash; (none) for no
-- debt; its rating; else the bucket of its page fund's breakdown; else
-- unrated. exposure_credit_unknown: a debt leaf no rating or known label
-- describes. exposure_maturity_split: a leaf's maturity bucket, its page
-- fund's band where it has none.
CREATE MACRO exposure_credit_bucket(p_class, p_debt, p_rating, p_page) AS
    CASE WHEN p_class = 'cash' THEN 'cash' WHEN NOT p_debt THEN '(none)'
         ELSE COALESCE(p_rating, p_page, 'unrated') END;

CREATE MACRO exposure_credit_unknown(p_debt, p_rating, p_page_unknown) AS
    p_debt AND p_rating IS NULL AND COALESCE(p_page_unknown, TRUE);

CREATE MACRO exposure_maturity_split(p_bucket, p_page) AS
    CASE WHEN p_bucket = 'unknown' AND p_page IS NOT NULL THEN p_page ELSE p_bucket END;

-- exposure_credit_grade: the grade a credit bucket falls in: IG from AAA
-- to BBB, HY for BB and B, NULL for any other bucket.
CREATE MACRO exposure_credit_grade(p_bucket) AS
    CASE WHEN p_bucket IN ('AAA', 'AA', 'A', 'BBB') THEN 'IG' WHEN p_bucket IN ('BB', 'B') THEN 'HY' END;

-- exposure_bucket_order: the place of a bucket in its debt or equity
-- dimension's order, as the views print them: p_dimension is credit,
-- maturity, coupon or cycle, as report_exposure_debt names them. (none)
-- comes last.
CREATE MACRO exposure_bucket_order(p_dimension, p_bucket) AS
    list_position(CASE p_dimension
                       WHEN 'credit' THEN ['AAA', 'AA', 'A', 'BBB', 'BB', 'B', 'unrated', 'cash', '(none)']
                       WHEN 'maturity' THEN ['under 1y', '1-3y', '3-5y', '5-10y', '10-20y', 'over 20y', 'perpetual',
                                             'unknown', '(none)']
                       WHEN 'coupon' THEN ['fixed', 'floating', 'inflation_linked', 'zero', 'unknown', '(none)']
                       WHEN 'cycle' THEN ['cyclical', 'defensive', 'sensitive', 'unknown', '(none)'] END,
                  p_bucket);

-- exposure_class_share: a bucket's share, in percent, of the total of
-- the buckets its dimension describes, every bucket but (none). Where
-- that total is zero or less, a loan or an overdraft outweighing the
-- rest, there is none: a share of it would flip its sign or have none.
CREATE MACRO exposure_class_share(p_value, p_total) AS
    100 * p_value / CASE WHEN p_total > 0 THEN p_total END;

-- ============================================================
-- exposure_key: a security key as the alias table resolves it. It binds
-- its argument to the caller, wherever the caller passes it.
--
-- exposure_key(p_key) expands to a subquery on security_aliases. A
-- caller may pass an unqualified column named `security_key` or `alias`
-- in a WHERE clause or a JOIN condition. Read straight from
-- security_aliases, that name would bind to the table's own column
-- inside the subquery, and the alias would never resolve. So the
-- subquery reads security_aliases through a derived table whose name
-- and column names no caller uses, and the argument binds to the
-- caller in every clause.
-- ============================================================

CREATE MACRO exposure_key(p_key) AS
    COALESCE((SELECT exposure_key_alias.exposure_key_to
                FROM (SELECT alias AS exposure_key_from, security_key AS exposure_key_to
                        FROM security_aliases) exposure_key_alias
               WHERE exposure_key_alias.exposure_key_from = p_key), p_key);

-- exposure_proxy_refs: the config's proxies, each fund and proxy read as
-- its security key. Of two entries that name one fund by two keys, the
-- one under its own key wins, else the first.
CREATE MACRO exposure_proxy_refs() AS TABLE (
    SELECT exposure_key(p.fund_ref) AS fund_ref, exposure_key(p.proxy_ref) AS proxy_ref, p.always, p.leverage
      FROM exposure_proxies p
   QUALIFY ROW_NUMBER() OVER (PARTITION BY exposure_key(p.fund_ref)
                              ORDER BY exposure_key(p.fund_ref) <> p.fund_ref, p.fund_ref) = 1
);

-- exposure_attributes_at: per fund, its attributes as of the instant
-- (the latest on or before, else the earliest after).
CREATE MACRO exposure_attributes_at(p_asof) AS TABLE (
    SELECT * FROM (
        SELECT exposure_key(a.fund_ref) AS fund_ref, a.as_of, a.silver_source_id, a.domicile, a.replication,
               a.hedged_currency, a.lending
          FROM fund_attributes a)
   QUALIFY ROW_NUMBER() OVER (PARTITION BY fund_ref ORDER BY as_of > p_asof, abs(as_of - p_asof), silver_source_id) = 1
);

-- exposure_page_breakdown: a fund's newest breakdown of one field on its
-- page within the exposure's max_gap_days, its fund's cash left out: each
-- bucket's share of the rest, and whether the bucket comes from a label
-- the map does not know (unknown). The values are summed as the decimals
-- they are stored as, so a share never follows the order of the rows.
CREATE MACRO exposure_page_breakdown(p_asof, p_field) AS TABLE (
    SELECT fund, bucket, unknown,
           CAST(SUM(v) AS DOUBLE) / CAST(SUM(SUM(v)) OVER (PARTITION BY fund) AS DOUBLE) AS share
      FROM (SELECT exposure_key(f.fund_ref) AS fund,
                   CASE p_field WHEN 'credit_quality' THEN exposure_rating_label(f.label)
                                ELSE exposure_maturity_label(f.label) END AS mapped,
                   COALESCE(mapped, CASE p_field WHEN 'credit_quality' THEN 'unrated' ELSE 'unknown' END) AS bucket,
                   mapped IS NULL AS unknown, f.value AS v
              FROM fund_characteristics f, exposure_settings_row() s
             WHERE f.field = p_field AND f.as_of <= p_asof
               AND (p_asof // 86400) - (f.as_of // 86400) <= s.max_gap_days
           QUALIFY dense_rank() OVER (PARTITION BY exposure_key(f.fund_ref) ORDER BY f.as_of DESC, f.provider) = 1)
     WHERE bucket <> 'cash' AND v > 0
     GROUP BY fund, bucket, unknown
);

-- exposure_holdings: the holdings the look-through starts from,
-- positions and cash at each account's active snapshot.
CREATE MACRO exposure_holdings(p_asof, p_ccy) AS TABLE (
    WITH lp AS (SELECT src, acct, snap FROM pos_active_at([p_asof])),
    lines AS (
        SELECT p.silver_source_id AS src, p.account_external_id AS acct, p.position_key,
               p.instrument_external_id AS iid, p.asset_class, p.vehicle, p.currency AS ccy,
               p.market_value AS amt, p.snapshot_at AS snap, FALSE AS is_cash
          FROM positions p JOIN lp ON p.silver_source_id = lp.src AND p.account_external_id = lp.acct
           AND p.snapshot_at = lp.snap
        UNION ALL
        SELECT silver_source_id, account_external_id, 'cash:' || currency, NULL, 'cash', 'demand_deposit',
               currency, amount, snapshot_at, TRUE
          FROM cash_chosen(p_asof)),
    valued AS (
        SELECT l.*,
               CAST(COALESCE(CASE WHEN l.ccy = p_ccy THEN l.amt::DOUBLE END,
                   l.amt::DOUBLE * o0.rate, l.amt::DOUBLE * o1.rate * o2.rate,
                   l.amt::DOUBLE * o3.rate * o4.rate) AS DECIMAL(28,4)) AS value
          FROM lines l
          JOIN accounts a ON a.silver_source_id = l.src AND a.account_external_id = l.acct
          ASOF LEFT JOIN fx_daily o0 ON o0.from_ccy = l.ccy AND o0.to_ccy = p_ccy AND o0.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o1 ON o1.from_ccy = l.ccy AND o1.to_ccy = 'CHF' AND o1.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o2 ON o2.from_ccy = 'CHF' AND o2.to_ccy = p_ccy AND o2.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o3 ON o3.from_ccy = l.ccy AND o3.to_ccy = 'USD' AND o3.day <= (l.snap // 86400)
          ASOF LEFT JOIN fx_daily o4 ON o4.from_ccy = 'USD' AND o4.to_ccy = p_ccy AND o4.day <= (l.snap // 86400)),
    dropped AS (
        SELECT src, acct FROM valued GROUP BY 1, 2
        HAVING (COUNT(*) FILTER (WHERE NOT is_cash) > 0 AND COUNT(value) FILTER (WHERE NOT is_cash) = 0)
            OR (COUNT(*) FILTER (WHERE is_cash) > 0 AND COUNT(value) FILTER (WHERE is_cash) = 0)),
    keyed AS (
        SELECT v.*, COALESCE(ha.to_key, hd.to_key, ia.k) AS akey
          FROM valued v
          ANTI JOIN dropped d ON d.src = v.src AND d.acct = v.acct
          LEFT JOIN holding_aliases ha ON ha.silver_source_id = v.src AND ha.account_external_id = v.acct
               AND ha.from_key = holding_key(v.iid, v.position_key)
          LEFT JOIN holding_aliases hd ON hd.silver_source_id = v.src AND hd.account_external_id = ''
               AND hd.from_key = holding_key(v.iid, v.position_key)
          LEFT JOIN instrument_aliases() ia ON ia.src = v.src AND ia.alias = v.iid)
    SELECT k.src, k.acct, k.position_key, k.iid, k.asset_class, k.vehicle, k.ccy, k.snap, k.is_cash,
           k.value::DOUBLE AS value, il.symbol, il.name,
           exposure_key(COALESCE(ik1.security_key, ik2.security_key)) AS key
      FROM keyed k
      LEFT JOIN instrument_keys ik1 ON ik1.silver_source_id = k.src AND ik1.instrument_external_id = k.akey
      LEFT JOIN instrument_keys ik2 ON ik2.silver_source_id = k.src AND ik2.instrument_external_id = k.iid
      LEFT JOIN instrument_labels() il ON il.silver_source_id = k.src AND il.instrument_external_id = k.iid
);

-- ============================================================
-- A line of the composition in use takes the coupon kind, the default
-- flag and the arrears flag its fund's filing states, where it states
-- none (docs/EXPOSURE.md §7, docs/YIELD.md §3).
--
-- An issuer's daily file is often a fund's newest composition, so the
-- look-through reads it, but it states no coupon kind; the fund's
-- N-PORT filing (source kind `filing`) states one for each debt line.
-- fund_filings_at gives each fund's filing at an instant, and
-- fund_filing_kinds what it states of each of its lines' securities.
-- exposure_leaves reads a line's coupon kind from it where the line
-- states none, as the yield report reads the kind and the two flags
-- (gold.readFundLines). exposure_yield_leaves reads a stated floating
-- kind, or a floating name where no kind is stated, before a real
-- yield: a provider states a real yield for a floating note too. A kind
-- reads as the coupon-kind label map says (exposure_labels).
-- ============================================================

-- fund_filings_at: per fund, read as its security key, its filing (a
-- composition of source kind `filing`) at the instant, as
-- exposure_funds_at picks a composition: the latest on or before it,
-- else the earliest after, within max_gap_days; one source where two
-- file the same day.
CREATE MACRO fund_filings_at(p_asof) AS TABLE (
    SELECT * FROM (
        SELECT c.silver_source_id, c.fund_ref AS stored_ref, exposure_key(c.fund_ref) AS fund_ref, c.as_of, c.basis,
               c.fund_name
          FROM fund_compositions c, exposure_settings_row() s
         WHERE c.source_kind = 'filing' AND c.basis = 'holdings'
           AND abs((p_asof // 86400) - (c.as_of // 86400)) <= s.max_gap_days)
   QUALIFY row_number() OVER (PARTITION BY fund_ref
                              ORDER BY as_of > p_asof, abs(as_of - p_asof), silver_source_id, stored_ref) = 1
);

-- fund_filing_kinds: per fund and line security, both read as security
-- keys, what the fund's filing at the instant (fund_filings_at) states
-- of the line: its coupon kind (as N-PORT writes it), its default flag
-- and its arrears flag. Of two lines of one security, each comes from
-- the first line that states it.
CREATE MACRO fund_filing_kinds(p_asof) AS TABLE (
    WITH l AS (
        SELECT f.fund_ref, exposure_key(l.security_key) AS key, l.line_no, l.coupon_kind, l.is_default, l.in_arrears
          FROM fund_filings_at(p_asof) f
          JOIN fund_lines l ON l.silver_source_id = f.silver_source_id AND l.fund_ref = f.stored_ref
               AND l.as_of = f.as_of AND l.basis = f.basis
         WHERE l.security_key IS NOT NULL
           AND (l.coupon_kind IS NOT NULL OR l.is_default IS NOT NULL OR l.in_arrears IS NOT NULL))
    SELECT fund_ref, key,
           arg_min(coupon_kind, line_no) FILTER (WHERE coupon_kind IS NOT NULL) AS coupon_kind,
           arg_min(is_default, line_no) FILTER (WHERE is_default IS NOT NULL) AS is_default,
           arg_min(in_arrears, line_no) FILTER (WHERE in_arrears IS NOT NULL) AS in_arrears
      FROM l
     GROUP BY fund_ref, key
);

-- exposure_declarations_at: per holding, the record that describes its
-- exposure at the instant: the latest dated on or before it that states
-- an economic dimension, the ledger's on an equal date, else one without
-- a date. A record that states only a situs is no record here. A
-- declaration has no age limit: a target does not go stale the way a
-- fund's roster does.
CREATE MACRO exposure_declarations_at(p_asof) AS TABLE (
    SELECT d.*
      FROM exposure_declarations d
     WHERE d.dimensions <> '' AND (d.as_of IS NULL OR d.as_of <= p_asof)
   QUALIFY ROW_NUMBER() OVER (PARTITION BY d.silver_source_id, d.instrument_external_id
                              ORDER BY d.as_of DESC NULLS LAST, d.origin = 'ledger' DESC) = 1
);

-- exposure_comps: the fund compositions the look-through reads, one
-- per fund and day, with each composition's origin and bound.
CREATE MACRO exposure_comps() AS TABLE (
    SELECT * FROM (
        SELECT c.silver_source_id, c.fund_ref AS stored_ref, exposure_key(c.fund_ref) AS fund_ref, c.as_of,
               c.basis, c.provider, c.currency, c.net_assets::DOUBLE AS net_assets,
               COALESCE(c.origin, 'provider') AS origin, c.valid_until
          FROM fund_compositions c
         WHERE c.basis <> 'collateral')
   QUALIFY ROW_NUMBER() OVER (PARTITION BY fund_ref, as_of
                              ORDER BY CASE basis WHEN 'holdings' THEN 1 ELSE 2 END, silver_source_id, stored_ref) = 1
);

-- exposure_fund_keys: every key known as a fund, one with a
-- composition or attributes.
CREATE MACRO exposure_fund_keys() AS TABLE (
    SELECT fund_ref FROM exposure_comps()
    UNION SELECT exposure_key(fund_ref) FROM fund_attributes
);

-- exposure_composition_wins: whether a holding's own composition, of
-- day p_own_as_of and origin p_own_origin, describes it where the
-- declaration p_decl (its line, origin and day) describes it too. With
-- no declaration, any composition does, a proxy's included. With one,
-- only a composition of the fund's own (which an always-proxy then
-- still replaces); and a parsed one only while no ledger record of the
-- same or a later day stands.
CREATE MACRO exposure_composition_wins(p_decl, p_decl_origin, p_decl_as_of, p_own_as_of, p_own_origin) AS
    p_decl IS NULL
    OR (p_own_as_of IS NOT NULL
        AND NOT COALESCE(p_own_origin = 'parsed' AND p_decl_origin = 'ledger' AND p_decl_as_of >= p_own_as_of, FALSE));

-- exposure_funds_at: per fund, the composition that describes it at the
-- instant (docs/EXPOSURE.md §4). The fund's own, the latest on or before
-- the instant, else the earliest after, within max_gap_days; a parsed
-- one only on or after its day and before its valid_until (its bound).
-- A proxy's where the config says always (the fund's own while the proxy
-- has none), or where the fund has none. Leverage applies to a proxy
-- alone.
-- own_as_of and own_origin are the day and origin of the fund's own
-- composition within the bound, even where a proxy replaces it: a
-- declaration yields to it alone (exposure_composition_wins). gap_days
-- is how many days older than the instant the composition is, negative
-- for one dated after it. wsum is the sum of the lines' weights in
-- percent, lines the count of them; partial says they stray from 100 by
-- more than partial_pct either way. A fund's attributes are its own,
-- even where a proxy describes its lines: a hedged class stays hedged.
CREATE MACRO exposure_funds_at(p_asof) AS TABLE (
    WITH s AS (SELECT * FROM exposure_settings_row()),
    c AS MATERIALIZED (SELECT * FROM exposure_comps()),
    px AS (SELECT * FROM exposure_proxy_refs()),
    near AS (
        SELECT c.fund_ref,
               MAX(c.as_of) FILTER (WHERE c.as_of <= p_asof AND (c.valid_until IS NULL OR p_asof < c.valid_until))
                   AS back,
               MIN(c.as_of) FILTER (WHERE c.as_of > p_asof AND c.origin <> 'parsed') AS fwd
          FROM c GROUP BY 1),
    pick AS (
        SELECT p.fund_ref, p.as_of, c.origin
          FROM (SELECT n.fund_ref,
                       CASE WHEN n.back IS NOT NULL AND (p_asof // 86400) - (n.back // 86400) <= s.max_gap_days
                            THEN n.back
                            WHEN n.fwd IS NOT NULL AND (n.fwd // 86400) - (p_asof // 86400) <= s.max_gap_days
                            THEN n.fwd
                       END AS as_of
                  FROM near n, s) p
          LEFT JOIN c ON c.fund_ref = p.fund_ref AND c.as_of = p.as_of),
    refs AS (SELECT fund_ref FROM pick WHERE as_of IS NOT NULL UNION SELECT fund_ref FROM px),
    res AS (
        SELECT r.fund_ref, px.proxy_ref, COALESCE(px.leverage, 1.0) AS proxy_leverage,
               CASE WHEN px.always AND pp.as_of IS NOT NULL THEN 'proxy'
                    WHEN own.as_of IS NOT NULL THEN 'direct'
                    WHEN pp.as_of IS NOT NULL THEN 'proxy' END AS via,
               own.as_of AS own_as_of, own.origin AS own_origin, pp.as_of AS proxy_as_of
          FROM refs r
          LEFT JOIN pick own ON own.fund_ref = r.fund_ref AND own.as_of IS NOT NULL
          LEFT JOIN px ON px.fund_ref = r.fund_ref
          LEFT JOIN pick pp ON pp.fund_ref = px.proxy_ref AND pp.as_of IS NOT NULL),
    chosen AS MATERIALIZED (
        SELECT res.fund_ref, res.via, res.proxy_ref, res.own_as_of, res.own_origin,
               CASE res.via WHEN 'proxy' THEN res.proxy_leverage ELSE 1.0 END AS leverage,
               c.silver_source_id, c.stored_ref, c.as_of, c.basis, c.provider, c.currency, c.net_assets
          FROM res
          JOIN c ON c.fund_ref = CASE res.via WHEN 'proxy' THEN res.proxy_ref ELSE res.fund_ref END
                AND c.as_of = CASE res.via WHEN 'proxy' THEN res.proxy_as_of ELSE res.own_as_of END
         WHERE res.via IS NOT NULL),
    w AS (
        SELECT l.silver_source_id, l.fund_ref AS stored_ref, l.as_of, l.basis,
               COALESCE(SUM(l.weight), 0)::DOUBLE AS wsum, COUNT(*) AS lines
          FROM fund_lines l
          SEMI JOIN (SELECT DISTINCT silver_source_id, stored_ref, as_of, basis FROM chosen) ch
               ON ch.silver_source_id = l.silver_source_id AND ch.stored_ref = l.fund_ref
              AND ch.as_of = l.as_of AND ch.basis = l.basis
         GROUP BY 1, 2, 3, 4)
    SELECT ch.fund_ref, ch.via, ch.as_of, ch.proxy_ref, ch.leverage, ch.own_as_of,
           (p_asof // 86400) - (ch.as_of // 86400) AS gap_days,
           ch.as_of > p_asof AS forward,
           (p_asof // 86400) - (ch.as_of // 86400) > s.aged_days AS aged,
           abs(100 - COALESCE(w.wsum, 0)) > s.partial_pct AS partial,
           ch.silver_source_id, ch.stored_ref, ch.basis, ch.provider, ch.currency, ch.net_assets,
           COALESCE(w.wsum, 0) AS wsum, COALESCE(w.lines, 0) AS lines,
           a.hedged_currency, a.domicile, a.replication, a.lending, ch.own_origin
      FROM chosen ch
      LEFT JOIN w USING (silver_source_id, stored_ref, as_of, basis)
      CROSS JOIN s
      LEFT JOIN exposure_attributes_at(p_asof) a ON a.fund_ref = ch.fund_ref
);

-- ============================================================
-- A proxy's divergence compares only what the proxy's file states.
--
-- exposure_divergence reads a line's sector and country as the ones
-- the leaves take: its security's, else the line's own label. A proxy
-- whose file labels no country still has some lines placed by their
-- securities' own facts, so its country mix would hold those alone and
-- read as a divergence that is not there. A dimension the proxy's file
-- labels on none of its lines is left out; one a line of the file
-- labels itself is compared. A report states no sector of its own and a
-- country only in some layouts; its securities place the rest, so the
-- report side keeps the fallback. A pair left with no dimension leaves
-- the divergence section.
--
--   * own_sector, own_country: the line labels the dimension itself.
--   * stated: each fund's dimensions its proxy's file labels.
-- ============================================================

-- exposure_divergence: per fund whose look-through reads a report and
-- whose config names a proxy with a composition of its own on the day,
-- how far the two compositions' equity lines lie apart: by sector and
-- by country, half the sum of the differences in each one's share, in
-- points. A line's sector and country are the ones the leaves take: its
-- security's, else the line's own label. Each mix holds only the lines
-- that state its dimension, and a dimension counts only where a line
-- of the proxy's file labels it itself. Only lines of positive
-- weight count, so a short line does not offset a long one.
CREATE MACRO exposure_divergence(p_asof) AS TABLE (
    WITH fa AS MATERIALIZED (SELECT * FROM exposure_funds_at(p_asof)),
    sides AS (
        SELECT f.fund_ref, p.fund_ref AS proxy_ref, x.side,
               CASE x.side WHEN 'fund' THEN f.silver_source_id ELSE p.silver_source_id END AS src,
               CASE x.side WHEN 'fund' THEN f.stored_ref ELSE p.stored_ref END AS ref,
               CASE x.side WHEN 'fund' THEN f.as_of ELSE p.as_of END AS as_of,
               CASE x.side WHEN 'fund' THEN f.basis ELSE p.basis END AS basis,
               CASE x.side WHEN 'fund' THEN f.provider ELSE p.provider END AS provider
          FROM fa f
          JOIN fund_compositions c ON c.silver_source_id = f.silver_source_id AND c.fund_ref = f.stored_ref
                                  AND c.as_of = f.as_of AND c.basis = f.basis
          JOIN fa p ON p.fund_ref = f.proxy_ref AND p.via = 'direct',
               (VALUES ('fund'), ('proxy')) x(side)
         WHERE f.via = 'direct' AND c.source_kind = 'report'),
    lines AS MATERIALIZED (
        SELECT s.fund_ref, s.proxy_ref, s.side, l.weight::DOUBLE AS w,
               COALESCE(sec.sector, ls.value) AS sector,
               COALESCE(sec.risk_country, lc.value, sec.domicile) AS country,
               trim(COALESCE(l.sector, '')) <> '' AS own_sector,
               trim(COALESCE(l.risk_country, l.country, '')) <> '' AS own_country
          FROM sides s
          JOIN (SELECT *, exposure_key(security_key) AS key FROM fund_lines) l
            ON l.silver_source_id = s.src AND l.fund_ref = s.ref AND l.as_of = s.as_of AND l.basis = s.basis
          LEFT JOIN fund_line_labels la ON la.provider = s.provider AND la.field = 'asset_type' AND la.raw = l.asset_type
          LEFT JOIN fund_line_labels ls ON ls.provider = s.provider AND ls.field = 'sector' AND ls.raw = l.sector
          LEFT JOIN fund_line_labels lc ON lc.provider = s.provider AND lc.field = 'country'
                                       AND lc.raw = COALESCE(l.risk_country, l.country)
          LEFT JOIN securities sec ON sec.security_key = l.key
         WHERE COALESCE(la.value, 'public_equity') = 'public_equity' AND l.weight > 0),
    mix AS (
        SELECT fund_ref, proxy_ref, side, dim, bucket,
               SUM(w) / SUM(SUM(w)) OVER (PARTITION BY fund_ref, side, dim) AS share
          FROM (SELECT fund_ref, proxy_ref, side, w, 'sector' AS dim, sector AS bucket FROM lines
                 WHERE sector IS NOT NULL
                UNION ALL
                SELECT fund_ref, proxy_ref, side, w, 'country', country FROM lines
                 WHERE country IS NOT NULL)
         GROUP BY fund_ref, proxy_ref, side, dim, bucket),
    stated AS (
        SELECT fund_ref, 'sector' AS dim FROM lines WHERE side = 'proxy' AND own_sector
        UNION
        SELECT fund_ref, 'country' FROM lines WHERE side = 'proxy' AND own_country),
    paired AS (SELECT fund_ref, dim FROM mix GROUP BY 1, 2 HAVING COUNT(DISTINCT side) = 2
               INTERSECT SELECT fund_ref, dim FROM stated),
    apart AS (
        SELECT COALESCE(f.fund_ref, p.fund_ref) AS fund_ref, COALESCE(f.proxy_ref, p.proxy_ref) AS proxy_ref,
               COALESCE(f.dim, p.dim) AS dim, 50 * SUM(abs(COALESCE(f.share, 0) - COALESCE(p.share, 0))) AS points
          FROM (SELECT * FROM mix WHERE side = 'fund') f
          FULL JOIN (SELECT * FROM mix WHERE side = 'proxy') p
            ON p.fund_ref = f.fund_ref AND p.dim = f.dim AND p.bucket = f.bucket
          SEMI JOIN paired b ON b.fund_ref = COALESCE(f.fund_ref, p.fund_ref) AND b.dim = COALESCE(f.dim, p.dim)
         GROUP BY 1, 2, 3)
    SELECT fund_ref, proxy_ref,
           MAX(points) FILTER (WHERE dim = 'sector') AS sector_points,
           MAX(points) FILTER (WHERE dim = 'country') AS country_points
      FROM apart GROUP BY fund_ref, proxy_ref
);

-- ============================================================
-- A forward held directly is held directly (docs/EXPOSURE.md §5).
--
-- A forward held directly is described by a composition of its own: one
-- parsed line, its legs. The legs are leaves at depth 1, so a reader
-- that split direct from via funds by depth would count them as looked
-- through, and the funds list would show each forward as a fund. A
-- forward is no fund: its legs are what the holding is.
--
--   * exposure_leaves carries direct: a leaf at depth 0, or a line of a
--     forward's own parsed composition (the held test). A fund's
--     forward lines stay looked through. The depth, how and the flags
--     of the legs are those of any depth-1 leaf, so coverage and
--     quality describe them as they describe any line.
--   * exposure_labeled_leaves and exposure_yield_leaves pass every
--     column through; exposure_debt_buckets passes direct.
--   * The views that split direct from via funds read direct:
--     report_exposure_classes, _sectors, _sizes, _countries,
--     _currencies, _coupons, _credit, _cycles, _maturities and
--     _securities. The materializer's via_funds reads it too.
--   * report_exposure_funds counts a forward held directly among the
--     other holdings.
-- ============================================================

-- exposure_leaves: every holding looked through to its leaves, with
-- whether a leaf is held directly, a held forward's legs among them.
CREATE MACRO exposure_leaves(p_asof, p_ccy, p_country := 'risk') AS TABLE (
    WITH RECURSIVE
    fa_all AS MATERIALIZED (SELECT * FROM exposure_funds_at(p_asof)),
    -- whole: per declaration, the class, sector and currency it states
    -- whole, each where every part names the same one.
    whole AS MATERIALIZED (
        SELECT line_no,
               CASE WHEN COUNT(DISTINCT asset_class) = 1 AND COUNT(asset_class) = COUNT(*)
                    THEN NULLIF(MIN(asset_class), 'unknown') END AS w_class,
               CASE WHEN COUNT(DISTINCT sector) = 1 AND COUNT(sector) = COUNT(*)
                    THEN NULLIF(MIN(sector), 'unknown') END AS w_sector,
               CASE WHEN COUNT(DISTINCT currency) = 1 AND COUNT(currency) = COUNT(*)
                    THEN NULLIF(MIN(currency), 'unknown') END AS w_ccy
          FROM exposure_declaration_leaves GROUP BY 1),
    -- decl: the record that describes the holding. expands: a composition
    -- does (exposure_composition_wins). Under the fund's own parsed
    -- composition (the key of the join to whole), the class, sector and
    -- currency the record states whole fill in what a line does not
    -- state, as they fill a parsed record's dimensions.
    h AS MATERIALIZED (
        SELECT h.*, d.line_no AS decl,
               f.fund_ref IS NOT NULL
                   AND exposure_composition_wins(d.line_no, d.origin, d.as_of, f.own_as_of, f.own_origin) AS expands,
               wh.w_class, wh.w_sector, wh.w_ccy
          FROM exposure_holdings(p_asof, p_ccy) h
          LEFT JOIN exposure_declarations_at(p_asof) d
                 ON d.silver_source_id = h.src AND d.instrument_external_id = h.iid
          LEFT JOIN fa_all f ON f.fund_ref = h.key
          LEFT JOIN whole wh
                 ON wh.line_no = CASE WHEN f.own_origin = 'parsed' AND f.via = 'direct' THEN d.line_no END
         WHERE h.value IS NOT NULL),
    -- r1 to r3: the funds a holding reaches at each level, a line's key
    -- read through the aliases.
    r1 AS (SELECT DISTINCT key AS fund_ref FROM h WHERE expands),
    r2 AS (
        SELECT DISTINCT f2.fund_ref
          FROM (SELECT exposure_key(l.security_key) AS key
                  FROM fa_all f1 SEMI JOIN r1 ON r1.fund_ref = f1.fund_ref
                  JOIN fund_lines l ON l.silver_source_id = f1.silver_source_id AND l.fund_ref = f1.stored_ref
                       AND l.as_of = f1.as_of AND l.basis = f1.basis) x
          JOIN fa_all f2 ON f2.fund_ref = x.key),
    r3 AS (
        SELECT DISTINCT f2.fund_ref
          FROM (SELECT exposure_key(l.security_key) AS key
                  FROM fa_all f1 SEMI JOIN r2 ON r2.fund_ref = f1.fund_ref
                  JOIN fund_lines l ON l.silver_source_id = f1.silver_source_id AND l.fund_ref = f1.stored_ref
                       AND l.as_of = f1.as_of AND l.basis = f1.basis) x
          JOIN fa_all f2 ON f2.fund_ref = x.key),
    -- fa: the funds the holdings reach, each with own_ref, the fund
    -- whose lines the look-through reads (a proxy's own, where one
    -- stands in).
    fa AS MATERIALIZED (
        SELECT *, CASE via WHEN 'proxy' THEN proxy_ref ELSE fund_ref END AS own_ref FROM fa_all
         WHERE fund_ref IN (SELECT fund_ref FROM r1 UNION SELECT fund_ref FROM r2 UNION SELECT fund_ref FROM r3)),
    -- fk: what the filing of each fund whose lines the look-through
    -- reads (own_ref) states of each line.
    fk AS MATERIALIZED (
        SELECT * FROM fund_filing_kinds(p_asof) WHERE fund_ref IN (SELECT own_ref FROM fa)),
    -- A line that states no coupon kind takes its filing's.
    kids AS MATERIALIZED (
        SELECT k.* EXCLUDE (own_ref) REPLACE (COALESCE(k.line_coupon_kind, fk.coupon_kind) AS line_coupon_kind)
          FROM (SELECT fa.fund_ref, 'line' AS kind, fa.leverage * l.weight::DOUBLE / 100 AS factor,
                       exposure_key(l.security_key) AS key, l.name, l.ticker, l.currency AS line_ccy,
                       l.asset_type AS asset_type_raw, lt.value AS class_map,
                       l.sector AS sector_raw, ls.value AS sector_map,
                       COALESCE(l.risk_country, l.country) AS country_raw, lc.value AS country_map,
                       l.long_currency, l.long_amount::DOUBLE AS long_amount, l.short_currency,
                       COALESCE(l.short_currency IS NOT NULL OR lt.value = 'foreign_exchange', FALSE) AS fx_line,
                       fa.as_of, fa.via, fa.gap_days, fa.forward, fa.aged, FALSE AS partial, fa.hedged_currency,
                       fa.currency AS fund_ccy, fa.net_assets, fa.leverage, fa.provider,
                       l.maturity AS line_maturity, l.coupon::DOUBLE AS line_coupon, l.coupon_kind AS line_coupon_kind,
                       l.real_yield IS NOT NULL AS line_real, l.lei AS line_lei, l.region AS line_region,
                       fa.own_ref
                  FROM fa
                  JOIN fund_lines l ON l.silver_source_id = fa.silver_source_id AND l.fund_ref = fa.stored_ref
                       AND l.as_of = fa.as_of AND l.basis = fa.basis
                  LEFT JOIN fund_line_labels lt ON lt.provider = fa.provider AND lt.field = 'asset_type'
                       AND lt.raw = l.asset_type
                  LEFT JOIN fund_line_labels ls ON ls.provider = fa.provider AND ls.field = 'sector' AND ls.raw = l.sector
                  LEFT JOIN fund_line_labels lc ON lc.provider = fa.provider AND lc.field = 'country'
                       AND lc.raw = COALESCE(l.risk_country, l.country)
                 WHERE l.weight IS NOT NULL) k
          LEFT JOIN fk ON fk.fund_ref = k.own_ref AND fk.key = k.key
        UNION ALL
        -- Weights above 100 leave a negative remainder: the fund's
        -- negative cash, as a leveraged proxy's leg is.
        SELECT fa.fund_ref, 'remainder', fa.leverage * (100 - fa.wsum) / 100, NULL, NULL, NULL, NULL,
               NULL, CASE WHEN fa.wsum > 100 THEN 'cash' END, NULL, CASE WHEN fa.wsum > 100 THEN 'cash' END,
               NULL, NULL, NULL, NULL, NULL, FALSE,
               fa.as_of, fa.via, fa.gap_days, fa.forward, fa.aged, fa.partial, fa.hedged_currency,
               fa.currency, fa.net_assets, fa.leverage, fa.provider, NULL, NULL, NULL, FALSE, NULL, NULL
          FROM fa WHERE fa.wsum <> 100
        UNION ALL
        SELECT fa.fund_ref, 'leverage', 1 - fa.leverage, NULL, NULL, NULL, NULL,
               NULL, 'cash', NULL, NULL, NULL, NULL, NULL, NULL, NULL, FALSE,
               fa.as_of, fa.via, fa.gap_days, fa.forward, fa.aged, FALSE, fa.hedged_currency,
               fa.currency, fa.net_assets, fa.leverage, fa.provider, NULL, NULL, NULL, FALSE, NULL, NULL
          FROM fa WHERE fa.leverage <> 1),
    tree AS (
        SELECT 0 AS depth, 'holding' AS kind, h.src, h.acct, h.position_key, h.iid,
               h.asset_class AS h_class, h.vehicle AS h_vehicle, h.ccy AS h_ccy, h.is_cash,
               h.name AS h_name, h.symbol AS h_symbol, h.key AS top_key, h.key AS key,
               CAST(NULL AS VARCHAR) AS fund_ref, h.value, CAST(NULL AS DOUBLE) AS fund_value,
               CASE WHEN h.expands THEN COALESCE(h.w_class, h.asset_class) ELSE h.asset_class END AS class,
               h.name AS name, h.symbol AS ticker, h.ccy AS line_ccy,
               CAST(NULL AS VARCHAR) AS asset_type_raw, CAST(NULL AS VARCHAR) AS sector_raw,
               CAST(NULL AS VARCHAR) AS sector_map, CAST(NULL AS VARCHAR) AS country_raw,
               CAST(NULL AS VARCHAR) AS country_map, CAST(NULL AS VARCHAR) AS long_currency,
               CAST(NULL AS DOUBLE) AS long_amount, CAST(NULL AS VARCHAR) AS short_currency,
               COALESCE(h.vehicle = 'forward', FALSE) AS fx_line, FALSE AS class_unmapped,
               CAST(NULL AS BIGINT) AS comp_as_of, CAST(NULL AS VARCHAR) AS via,
               CAST(NULL AS BIGINT) AS gap_days, FALSE AS forward, FALSE AS aged, FALSE AS partial,
               CAST(NULL AS VARCHAR) AS hedged, CAST(NULL AS VARCHAR) AS fund_ccy,
               CAST(NULL AS DOUBLE) AS net_assets, CAST(NULL AS DOUBLE) AS leverage,
               CAST(NULL AS VARCHAR) AS provider, h.decl, h.expands,
               CAST(NULL AS DATE) AS line_maturity, CAST(NULL AS DOUBLE) AS line_coupon,
               CAST(NULL AS VARCHAR) AS line_coupon_kind, FALSE AS line_real, CAST(NULL AS VARCHAR) AS line_lei,
               CAST(NULL AS VARCHAR) AS line_region, h.w_sector, h.w_ccy
          FROM h
        UNION ALL
        SELECT t.depth + 1, k.kind, t.src, t.acct, t.position_key, t.iid, t.h_class, t.h_vehicle, t.h_ccy,
               t.is_cash, t.h_name, t.h_symbol, t.top_key, k.key, k.fund_ref, t.value * k.factor, t.value,
               COALESCE(k.class_map, t.class), k.name, k.ticker, k.line_ccy, k.asset_type_raw, k.sector_raw,
               k.sector_map, k.country_raw, k.country_map, k.long_currency, k.long_amount, k.short_currency,
               k.fx_line,
               k.asset_type_raw IS NOT NULL AND k.class_map IS NULL,
               COALESCE(t.comp_as_of, k.as_of),
               CASE WHEN t.via = 'proxy' OR k.via = 'proxy' THEN 'proxy' ELSE 'direct' END,
               GREATEST(COALESCE(t.gap_days, 0), abs(k.gap_days)),
               t.forward OR k.forward, t.aged OR k.aged, k.partial,
               COALESCE(t.hedged, k.hedged_currency), k.fund_ccy, k.net_assets, k.leverage, k.provider,
               CAST(NULL AS INTEGER), CAST(NULL AS BOOLEAN),
               k.line_maturity, k.line_coupon, k.line_coupon_kind, k.line_real, k.line_lei, k.line_region,
               t.w_sector, t.w_ccy
          FROM tree t JOIN kids k ON k.fund_ref = t.key
         WHERE t.depth < 3 AND t.kind IN ('holding', 'line') AND (t.depth > 0 OR t.expands)),
    leaves AS (
        SELECT t.*,
               COALESCE(t.depth = 3 AND t.kind = 'line' AND t.key IN (SELECT fund_ref FROM fa_all), FALSE) AS truncated
          FROM tree t
         WHERE CASE WHEN t.depth = 0 THEN NOT t.expands
                    ELSE NOT (t.depth < 3 AND t.kind = 'line' AND COALESCE(t.key IN (SELECT fund_ref FROM fa), FALSE))
               END),
    -- A declared holding splits into the declaration's parts. A part's
    -- class is the one it states; the part a declaration leaves
    -- unknown keeps the holding's.
    parts AS (
        SELECT l.* REPLACE (l.value * COALESCE(x.share, 1) AS value,
                            COALESCE(NULLIF(x.asset_class, 'unknown'), l.class) AS class),
               x.region AS d_region, x.country AS d_country, x.currency AS d_currency, x.sector AS d_sector
          FROM leaves l
          -- The depth is in the key, so the join hashes.
          LEFT JOIN exposure_declaration_leaves x ON x.line_no = CASE WHEN l.depth = 0 THEN l.decl END),
    -- A currency forward line that states its long leg splits in two: the
    -- long leg at notional in its currency, the rest (the short leg, its
    -- value less the long leg) in the short currency, else the line's
    -- own, so the pair sums to the line. Without a long leg it counts in
    -- its own currency, flagged partial.
    -- In a fund, the long leg is the line's share of the fund: the
    -- notional at the fund's rate, scaled by the holding's value over the
    -- fund's net assets. A forward held directly (held: the one line of
    -- its own parsed composition) holds the whole contract, so its long
    -- leg is the notional at the reporting currency's rate whatever the
    -- holding's value, a mark of zero included.
    -- The rates are looked up once per long currency, fund currency and
    -- day the forward lines state, then joined back by those keys. A
    -- line without a long currency has no rate either way.
    held_refs AS (SELECT fund_ref FROM fa WHERE via = 'direct' AND own_origin = 'parsed'),
    fwd AS (
        SELECT f.long_currency, f.fund_ccy, f.day,
               COALESCE(CASE WHEN f.long_currency = f.fund_ccy THEN 1.0 END, r1.rate, u1.rate * u2.rate) AS long_rate,
               -- The reporting currency's rate, by the chain
               -- exposure_holdings values a holding by.
               COALESCE(CASE WHEN f.long_currency = p_ccy THEN 1.0 END, o0.rate, o1.rate * o2.rate, o3.rate * o4.rate)
                   AS held_rate
          FROM (SELECT DISTINCT long_currency, fund_ccy, comp_as_of // 86400 AS day
                  FROM parts WHERE long_currency IS NOT NULL) f
          ASOF LEFT JOIN fx_daily r1 ON r1.from_ccy = f.long_currency AND r1.to_ccy = f.fund_ccy
               AND r1.day <= f.day
          ASOF LEFT JOIN fx_daily u1 ON u1.from_ccy = f.long_currency AND u1.to_ccy = 'USD'
               AND u1.day <= f.day
          ASOF LEFT JOIN fx_daily u2 ON u2.from_ccy = 'USD' AND u2.to_ccy = f.fund_ccy
               AND u2.day <= f.day
          ASOF LEFT JOIN fx_daily o0 ON o0.from_ccy = f.long_currency AND o0.to_ccy = p_ccy AND o0.day <= f.day
          ASOF LEFT JOIN fx_daily o1 ON o1.from_ccy = f.long_currency AND o1.to_ccy = 'CHF' AND o1.day <= f.day
          ASOF LEFT JOIN fx_daily o2 ON o2.from_ccy = 'CHF' AND o2.to_ccy = p_ccy AND o2.day <= f.day
          ASOF LEFT JOIN fx_daily o3 ON o3.from_ccy = f.long_currency AND o3.to_ccy = 'USD' AND o3.day <= f.day
          ASOF LEFT JOIN fx_daily o4 ON o4.from_ccy = 'USD' AND o4.to_ccy = p_ccy AND o4.day <= f.day),
    legs AS (
        SELECT l.*, fwd.long_rate, fwd.held_rate,
               COALESCE(l.depth = 1 AND l.kind = 'line' AND l.h_vehicle = 'forward'
                        AND l.fund_ref IN (SELECT fund_ref FROM held_refs), FALSE) AS held
          FROM parts l
          LEFT JOIN fwd ON fwd.long_currency = l.long_currency
               AND fwd.fund_ccy IS NOT DISTINCT FROM l.fund_ccy
               AND fwd.day IS NOT DISTINCT FROM l.comp_as_of // 86400),
    -- can: whether the line splits, held directly or in a fund.
    can AS (
        SELECT legs.*,
               legs.fx_line AND legs.long_amount IS NOT NULL
                   AND CASE WHEN legs.held THEN legs.held_rate IS NOT NULL
                            ELSE legs.long_rate IS NOT NULL AND legs.net_assets IS NOT NULL AND legs.net_assets <> 0
                       END AS can_split
          FROM legs),
    split AS (
        SELECT can.* EXCLUDE (can_split), FALSE AS long_leg, can.fx_line AND NOT can.can_split AS fx_unsplit
          FROM can
        UNION ALL
        SELECT can.* EXCLUDE (can_split), TRUE, FALSE
          FROM can
         WHERE can.can_split),
    known AS (SELECT fund_ref FROM exposure_fund_keys()),
    valued AS (
        SELECT s.*,
               CASE WHEN s.fx_line AND NOT s.fx_unsplit AND s.held THEN s.long_amount * s.held_rate
                    WHEN s.fx_line AND NOT s.fx_unsplit THEN
                        s.fund_value * s.leverage * s.long_amount * s.long_rate / s.net_assets END AS long_value,
               -- A fund that stays a leaf: held with no composition, or
               -- a line no level expands. Where it is domiciled says
               -- nothing of where its value is.
               COALESCE((s.depth = 0 AND s.h_vehicle IN ('etf', 'fund'))
                        OR (s.depth > 0 AND s.key IN (SELECT fund_ref FROM known)), FALSE) AS fund_leaf
          FROM split s),
    cls AS (
        SELECT v.depth, v.kind, v.src, v.acct, v.position_key, v.iid, v.h_class, v.h_vehicle, v.h_name,
               v.h_symbol, v.top_key, v.fund_ref, v.key, v.comp_as_of, v.via, v.gap_days, v.forward, v.aged,
               v.partial, v.truncated, v.class_unmapped, v.fx_unsplit, v.provider, v.is_cash,
               v.asset_type_raw, v.sector_raw, v.country_raw, v.sector_map, v.country_map, v.fund_leaf,
               v.decl IS NOT NULL AS declared,
               -- A leaf is held directly at depth 0, or as a leg of a
               -- forward held directly: the forward is no fund.
               v.depth = 0 OR v.held AS direct,
               v.line_maturity, v.line_coupon, v.line_coupon_kind, v.line_real, v.line_lei, v.line_region,
               CASE WHEN NOT (v.fx_line AND NOT v.fx_unsplit) THEN v.value
                    WHEN v.long_leg THEN v.long_value ELSE v.value - v.long_value END AS value,
               CASE WHEN v.long_leg THEN 'foreign_exchange' ELSE v.class END AS asset_class,
               COALESCE(sec.name, v.name, v.h_name) AS name, COALESCE(sec.name, v.name) AS own_name, v.ticker,
               sec.market_cap,
               CASE WHEN v.class = 'public_equity' AND NOT v.long_leg AND NOT v.fund_leaf THEN sec.size END AS size,
               CASE WHEN v.kind = 'remainder' THEN v.sector_map
                    WHEN v.kind = 'leverage' OR v.class = 'cash' THEN 'cash'
                    WHEN v.class = 'metal' THEN 'metal'
                    WHEN v.class = 'crypto' THEN 'crypto'
                    WHEN v.depth = 0 AND v.d_sector IS NOT NULL THEN NULLIF(v.d_sector, 'unknown')
                    WHEN v.fund_leaf THEN CASE WHEN v.class = 'real_estate' THEN 'real_estate'
                                               WHEN v.depth = 0 AND v.class = 'private_equity'
                                               THEN 'diversified' END
                    ELSE COALESCE(sec.sector, v.sector_map, CASE WHEN v.sector_raw IS NULL THEN v.w_sector END,
                                  CASE WHEN v.class = 'real_estate' THEN 'real_estate' END)
               END AS sector,
               CASE WHEN v.kind IN ('remainder', 'leverage') OR v.class IN ('metal', 'crypto') THEN NULL
                    -- A declared region states no country.
                    WHEN v.depth = 0 AND (v.d_country IS NOT NULL OR v.d_region IS NOT NULL)
                         THEN NULLIF(v.d_country, 'unknown')
                    WHEN v.depth = 0 AND v.is_cash THEN cu.country
                    WHEN v.fund_leaf THEN NULL
                    WHEN p_country = 'domicile' THEN COALESCE(sec.domicile, sec.risk_country, v.country_map)
                    ELSE COALESCE(sec.risk_country, v.country_map, sec.domicile)
               END AS country,
               CASE WHEN v.class NOT IN ('metal', 'crypto') THEN NULLIF(v.d_region, 'unknown') END AS declared_region,
               CASE WHEN v.long_leg THEN v.long_currency
                    WHEN v.fx_line AND NOT v.fx_unsplit THEN COALESCE(v.short_currency, v.line_ccy)
                    WHEN v.fx_line THEN COALESCE(v.line_ccy, v.fund_ccy, v.h_ccy)
                    WHEN v.depth > 0 AND v.hedged IS NOT NULL THEN v.hedged
                    WHEN v.class = 'metal' THEN
                         CASE WHEN regexp_matches(lower(COALESCE(v.name, v.h_name, '')), 'silver') THEN 'XAG'
                              WHEN regexp_matches(lower(COALESCE(v.name, v.h_name, '')), 'platinum') THEN 'XPT'
                              WHEN regexp_matches(lower(COALESCE(v.name, v.h_name, '')), 'palladium') THEN 'XPD'
                              ELSE 'XAU' END
                    -- A coin held in a wallet is its symbol; a coin in a
                    -- product or a fund is the one its name states.
                    WHEN v.class = 'crypto' THEN
                         CASE WHEN v.depth = 0 AND v.h_vehicle = 'physical' THEN upper(v.h_symbol)
                              WHEN regexp_matches(lower(COALESCE(v.name, v.h_name, '')), '\bbitcoin\b') THEN 'BTC'
                              WHEN regexp_matches(lower(COALESCE(v.name, v.h_name, '')), '\bether(eum)?\b') THEN 'ETH'
                              WHEN v.depth = 0 THEN NULLIF(v.d_currency, 'unknown')
                         END
                    WHEN v.depth = 0 AND v.d_currency IS NOT NULL THEN NULLIF(v.d_currency, 'unknown')
                    WHEN v.depth = 0 THEN v.line_ccy
                    WHEN v.kind IN ('remainder', 'leverage') THEN COALESCE(v.fund_ccy, v.h_ccy)
                    ELSE COALESCE(v.line_ccy, sec.currency, v.w_ccy, v.fund_ccy, v.h_ccy)
               END AS currency,
               CASE WHEN v.fx_line OR (v.depth > 0 AND v.hedged IS NOT NULL) THEN 'fiat'
                    WHEN v.class = 'metal' THEN 'metal' WHEN v.class = 'crypto' THEN 'crypto'
                    ELSE 'fiat' END AS currency_kind,
               -- A receipt's sector, country or domicile taken from its
               -- share, matched by name (exposure_receipts.go).
               COALESCE(starts_with(sec.sector_origin, 'underlying:')
                        OR starts_with(sec.risk_country_origin, 'underlying:')
                        OR starts_with(sec.domicile_origin, 'underlying:'), FALSE) AS by_name,
               exposure_public(v.h_vehicle, v.h_class) AS public,
               -- What a source states, not what an ISIN's prefix implies;
               -- nothing for a fund no composition describes.
               NOT v.fund_leaf
                   AND (sec.sector IS NOT NULL OR sec.risk_country IS NOT NULL
                        OR (sec.domicile IS NOT NULL AND sec.domicile_origin <> 'isin_prefix')) AS described
          FROM valued v
          LEFT JOIN securities sec ON sec.security_key = v.key
          LEFT JOIN exposure_custody cu ON cu.silver_source_id = v.src)
    SELECT c.*,
           co.name AS country_name, COALESCE(co.region, c.declared_region, c.line_region) AS region,
           CASE WHEN c.depth = 0 AND c.is_cash THEN 'cash'
                WHEN c.depth = 0 AND c.asset_class IN ('metal', 'crypto') THEN 'asset_class'
                WHEN c.depth = 0 AND c.declared THEN 'declared'
                WHEN c.depth = 0 AND c.public AND c.described THEN 'security'
                WHEN c.depth = 0 THEN 'unknown'
                WHEN c.kind = 'remainder' THEN 'remainder'
                WHEN c.truncated THEN 'truncated'
                WHEN c.via = 'proxy' THEN 'proxied'
                ELSE 'looked_through' END AS how,
           list_filter([
               CASE WHEN c.via = 'proxy' THEN 'proxy' END,
               CASE WHEN c.declared THEN 'declared' END,
               CASE WHEN c.aged THEN 'aged' END,
               CASE WHEN c.forward THEN 'forward' END,
               CASE WHEN (c.kind = 'remainder' AND c.partial) OR c.fx_unsplit OR c.truncated THEN 'partial' END,
               CASE WHEN c.depth > 0 AND (c.class_unmapped
                         OR (c.sector_raw IS NOT NULL AND c.sector_map IS NULL AND c.sector IS NULL)
                         OR (c.country_raw IS NOT NULL AND c.country_map IS NULL AND c.country IS NULL))
                    THEN 'unmapped' END,
               CASE WHEN c.by_name AND NOT c.declared AND NOT c.fund_leaf THEN 'by_name' END,
               CASE WHEN c.depth = 0 AND c.fund_leaf AND c.asset_class = 'private_equity' THEN 'opaque' END,
               CASE WHEN c.depth = 0 AND NOT c.is_cash AND c.asset_class NOT IN ('metal', 'crypto', 'cash')
                         AND NOT c.public AND NOT c.declared THEN 'undeclared' END,
               CASE WHEN c.depth = 0 AND c.public AND NOT c.declared
                         AND (c.sector IS NULL OR c.country IS NULL) THEN 'unmatched' END
           ], lambda x: x IS NOT NULL) AS flags,
           st.situs, COALESCE(st.basis, 'unknown') AS situs_basis
      FROM cls c
      LEFT JOIN exposure_countries co ON co.code = c.country
      LEFT JOIN exposure_situs_at(p_asof) st
             ON st.silver_source_id = c.src AND st.account_external_id = c.acct AND st.position_key = c.position_key
     WHERE c.value IS NOT NULL
);

-- exposure_labeled_leaves: exposure_leaves with each leaf's buckets as
-- the views name them. A sector or a currency no source states is
-- unknown. A country or a region is (none) for a metal or a coin, else
-- unknown where none is known. A size is (none) outside public equity,
-- else unknown where none is known. security is the row a leaf counts
-- under in the securities view: a fund's remainder or leverage by its
-- fund; cash no declaration or security names by its currency; a
-- security by its key; a line no key names by its fund and its name; a
-- holding no key names by its source and instrument.
CREATE MACRO exposure_labeled_leaves(p_asof, p_ccy, p_country := 'risk') AS TABLE (
    SELECT l.*,
           COALESCE(l.sector, 'unknown') AS sector_bucket,
           CASE WHEN l.country IS NOT NULL THEN l.country
                WHEN l.asset_class IN ('metal', 'crypto') THEN '(none)' ELSE 'unknown' END AS country_bucket,
           COALESCE(l.region, CASE WHEN l.asset_class IN ('metal', 'crypto') THEN '(none)' ELSE 'unknown' END)
               AS region_bucket,
           COALESCE(l.currency, 'unknown') AS currency_bucket,
           CASE WHEN l.asset_class <> 'public_equity' THEN '(none)' ELSE COALESCE(l.size, 'unknown') END AS size_bucket,
           CASE WHEN l.kind IN ('remainder', 'leverage') THEN '(' || l.kind || ') ' || l.fund_ref
                WHEN l.asset_class = 'cash' AND NOT l.declared AND (l.key IS NULL OR l.key LIKE 'name:%')
                     THEN 'cash:' || l.currency
                WHEN l.key IS NOT NULL THEN l.key
                WHEN l.depth > 0 THEN 'line:' || l.fund_ref || ':' || COALESCE(l.name, l.asset_type_raw, '')
                ELSE 'holding:' || l.src || ':' || COALESCE(l.iid, l.position_key) END AS security
      FROM exposure_leaves(p_asof, p_ccy, p_country := p_country) l
);

-- report_exposure_situs: one row per legal situs, a holding's whole
-- value at its own: no fund is looked through. funds_pct is the share of
-- the row's gross value held in pooled vehicles (vehicle etf or fund),
-- quality the situs bases it rests on.
CREATE MACRO report_exposure_situs(p_asof, p_ccy) AS TABLE (
    WITH l AS (SELECT * FROM exposure_leaves(p_asof, p_ccy)),
    t AS (SELECT SUM(value) AS total FROM l)
    SELECT COALESCE(l.situs, 'unknown') AS situs, MAX(co.name) AS country_name, MAX(co.region) AS region,
           SUM(l.value) AS value,
           100 * SUM(l.value) / NULLIF(t.total, 0) AS share_pct,
           100 * COALESCE(SUM(abs(l.value)) FILTER (WHERE l.h_vehicle IN ('etf', 'fund')), 0)
               / NULLIF(SUM(abs(l.value)), 0) AS funds_pct,
           exposure_situs_quality(l.situs_basis, l.value) AS quality
      FROM l
      LEFT JOIN exposure_countries co ON co.code = l.situs, t
     GROUP BY 1, t.total
     ORDER BY value DESC, situs
);

-- report_exposure_coverage: what the figures rest on. Section `how`
-- splits net worth by how each leaf is classified and sums to it;
-- `age` splits what compositions describe by their distance from the
-- day; `unmapped` lists the provider labels no map knows; `unmatched`
-- the listed securities held directly that no source describes;
-- `undeclared` the private holdings no record of the declarations
-- ledger describes, each with the silver_source_id and instrument a
-- record names; `divergence` each fund read from a report beside its
-- proxy: the value held through it, and how far the two mixes lie
-- apart. `situs` splits net worth by the basis of each holding's situs
-- and sums to it, then lists each holding whose situs is unknown.
-- In the sections that list labels or holdings, what has no gross value
-- shares one line, `(no value)`. A situs line counts holdings.
CREATE MACRO report_exposure_coverage(p_asof, p_ccy) AS TABLE (
    WITH l AS (SELECT * FROM exposure_leaves(p_asof, p_ccy)),
    t AS (SELECT SUM(value) AS total FROM l),
    how AS (
        SELECT 1 AS section_rank, 'how' AS section, how AS reason, COUNT(*) AS items, SUM(value) AS value,
               CAST(NULL AS VARCHAR) AS detail, CAST(NULL AS VARCHAR) AS silver_source_id,
               CAST(NULL AS VARCHAR) AS instrument,
               CASE how WHEN 'looked_through' THEN 1 WHEN 'proxied' THEN 2 WHEN 'security' THEN 3
                        WHEN 'declared' THEN 4 WHEN 'cash' THEN 5 WHEN 'asset_class' THEN 6
                        WHEN 'remainder' THEN 7 WHEN 'truncated' THEN 8 ELSE 9 END AS reason_rank
          FROM l GROUP BY how),
    age AS (
        SELECT 2, 'age', bucket, COUNT(DISTINCT top_key), SUM(value), CAST(NULL AS VARCHAR), CAST(NULL AS VARCHAR),
               CAST(NULL AS VARCHAR), bucket_rank
          FROM (SELECT *,
                       CASE WHEN forward THEN 'forward'
                            WHEN abs(gap_days) <= 35 THEN '0-35 days'
                            WHEN abs(gap_days) <= 95 THEN '36-95 days'
                            WHEN abs(gap_days) <= 185 THEN '96-185 days'
                            ELSE '186+ days' END AS bucket,
                       CASE WHEN forward THEN 5 WHEN abs(gap_days) <= 35 THEN 1 WHEN abs(gap_days) <= 95 THEN 2
                            WHEN abs(gap_days) <= 185 THEN 3 ELSE 4 END AS bucket_rank
                  FROM l WHERE depth > 0)
         GROUP BY bucket, bucket_rank),
    unmapped_items AS (
        SELECT 3 AS section_rank, 'unmapped' AS section, f.field || ': ' || f.raw AS reason, COUNT(*) AS items,
               SUM(f.value) AS value, SUM(abs(f.value)) AS gross, f.provider AS detail,
               CAST(NULL AS VARCHAR) AS silver_source_id, CAST(NULL AS VARCHAR) AS instrument
          FROM (SELECT 'sector' AS field, sector_raw AS raw, provider, value FROM l
                 WHERE depth > 0 AND sector_raw IS NOT NULL AND sector_map IS NULL
                UNION ALL
                SELECT 'country', country_raw, provider, value FROM l
                 WHERE depth > 0 AND country_raw IS NOT NULL AND country_map IS NULL
                UNION ALL
                SELECT 'asset_type', asset_type_raw, provider, value FROM l
                 WHERE depth > 0 AND class_unmapped) f
         GROUP BY f.field, f.raw, f.provider),
    unmatched_items AS (
        SELECT CASE WHEN undeclared THEN 5 ELSE 4 END AS section_rank,
               CASE WHEN undeclared THEN 'undeclared' ELSE 'unmatched' END AS section,
               holding AS reason, COUNT(*) AS items, SUM(value) AS value, SUM(abs(value)) AS gross,
               MAX(name) AS detail, src AS silver_source_id, iid AS instrument
          FROM (SELECT *, COALESCE(key, 'holding:' || src || ':' || COALESCE(iid, position_key)) AS holding,
                       list_contains(flags, 'undeclared') AS undeclared FROM l
                 WHERE list_contains(flags, 'unmatched') OR list_contains(flags, 'undeclared'))
         GROUP BY undeclared, holding, src, iid),
    -- situs: a holding's leaves all carry its situs, so its name is the
    -- holding's and its key the one it is held under.
    situs_bases AS (
        SELECT 7 AS section_rank, 'situs' AS section, situs_basis AS reason,
               COUNT(DISTINCT (src, acct, position_key)) AS items, SUM(value) AS value,
               CAST(NULL AS VARCHAR) AS detail, CAST(NULL AS VARCHAR) AS silver_source_id,
               CAST(NULL AS VARCHAR) AS instrument,
               CASE situs_basis WHEN 'declared' THEN 1 WHEN 'underlying' THEN 2 WHEN 'isin' THEN 3
                                WHEN 'domicile' THEN 4 WHEN 'custody' THEN 5 WHEN 'country' THEN 6 ELSE 7 END
                   AS reason_rank
          FROM l GROUP BY situs_basis),
    situs_items AS (
        SELECT 7 AS section_rank, 'situs' AS section, holding AS reason,
               COUNT(DISTINCT (acct, position_key)) AS items, SUM(value) AS value, SUM(abs(value)) AS gross,
               COALESCE(MAX(name) FILTER (WHERE depth = 0), MAX(h_name)) AS detail,
               src AS silver_source_id, iid AS instrument
          FROM (SELECT *, COALESCE(top_key, 'holding:' || src || ':' || COALESCE(iid, position_key)) AS holding
                  FROM l WHERE situs_basis = 'unknown')
         GROUP BY holding, src, iid),
    -- A label or holding whose leaves have no gross value is listed
    -- only as a count.
    listable AS (
        SELECT * FROM unmapped_items UNION ALL BY NAME SELECT * FROM unmatched_items
        UNION ALL BY NAME SELECT * FROM situs_items),
    listed AS (
        SELECT section_rank, section, reason, items, value, detail, silver_source_id, instrument, 10 AS reason_rank
          FROM listable
         WHERE round(gross, 2) <> 0
        UNION ALL
        SELECT section_rank, section, '(no value)', CAST(SUM(items) AS BIGINT), SUM(value), CAST(NULL AS VARCHAR),
               CAST(NULL AS VARCHAR), CAST(NULL AS VARCHAR), 11
          FROM listable
         WHERE round(gross, 2) = 0
         GROUP BY section_rank, section),
    divergence AS (
        SELECT 6, 'divergence', d.fund_ref, v.items, v.value,
               concat_ws(', ', printf('sector %.1f', d.sector_points), printf('country %.1f', d.country_points))
                   || ' points from ' || d.proxy_ref,
               CAST(NULL AS VARCHAR), CAST(NULL AS VARCHAR), 0
          FROM exposure_divergence(p_asof) d
          JOIN (SELECT fund_ref, COUNT(DISTINCT key) AS items, SUM(value) AS value FROM l WHERE depth > 0 GROUP BY 1) v
            USING (fund_ref))
    SELECT section, reason, items, value, 100 * value / NULLIF(t.total, 0) AS share_pct, detail,
           silver_source_id, instrument
      FROM (SELECT * FROM how UNION ALL SELECT * FROM age UNION ALL SELECT * FROM listed
            UNION ALL SELECT * FROM divergence UNION ALL SELECT * FROM situs_bases) x, t
     ORDER BY section_rank, reason_rank, abs(value) DESC, reason, silver_source_id, instrument
);

-- exposure_yield_leaves: exposure_leaves with each leaf's debt and issuer
-- dimensions at the instant:
--
--   * debt: a leaf of fixed income or private debt; deposit: a holding of
--     cash at its custodian (a balance, a demand or a time deposit);
--   * maturity: a holding's from its terms, a deposit's else the end of a
--     term its account's rates state; a line's as its fund states it.
--     maturity_bucket reads it on the day (exposure_maturity_bucket); cash
--     without a term counts under a year, a debt leaf without one that
--     says PERP is perpetual, any other unknown;
--   * coupon_kind: fixed, floating, inflation_linked or zero; a holding's
--     from its terms; a line's floating where the coupon-kind label map
--     (exposure_labels) reads its fund's type as floating (N-PORT's
--     Floating, Variable), else inflation-linked where its name says so
--     (TIPS, INFLATION), else floating where its name does (FRN,
--     FLOATING) and its fund types it no kind, else inflation-linked
--     where it states a real yield (a provider states one for a floating
--     note too), else fixed or zero as the map reads its fund's type
--     (Fixed, None), else read from its coupon (zero is zero, any other
--     fixed);
--   * page_fund: the fund whose page may describe a leaf: a fund leaf's
--     own, a line's fund;
--   * rating: a holding's bucket from its terms, else the worse end of
--     the range yield_rating_funds gives its page fund;
--   * cycle: a public equity leaf's, by its sector (yield_cycles);
--   * issuer_key and issuer_name: a deposit's custodian, else its source;
--     a security's issuer by the LEI its line or GLEIF states, else by
--     the name exposure_issuer_name gives, which a GLEIF legal name of the
--     same text ties to its LEI; else by its key, else `unknown`; NULL
--     where no issuer applies (a fund nothing looks through, the parts a
--     declaration splits a private holding into, real estate, a metal, a
--     fund's cash). name_rank orders the names an issuer may print by:
--     GLEIF's, a stock's, any other (its normalized name).
CREATE MACRO exposure_yield_leaves(p_asof, p_ccy) AS TABLE (
    WITH l AS MATERIALIZED (SELECT * FROM exposure_leaves(p_asof, p_ccy)),
    t AS (SELECT * FROM instrument_terms_at(p_asof)),
    rf AS (SELECT exposure_key(fund_ref) AS fund_ref,
                  arg_max(bucket_low, list_position(['AAA', 'AA', 'A', 'BBB', 'BB', 'B'], bucket_low)) AS bucket_low
             FROM yield_rating_funds GROUP BY 1),
    gk AS (SELECT exposure_key(security_key) AS key, first(lei ORDER BY as_of DESC, lei) AS lei
             FROM security_identities WHERE provider = 'gleif' AND lei IS NOT NULL GROUP BY 1),
    gl AS (SELECT lei, first(name ORDER BY as_of DESC, name) AS name FROM security_identities
            WHERE provider = 'gleif' AND lei IS NOT NULL AND name IS NOT NULL GROUP BY 1),
    gn AS (SELECT exposure_issuer_name(name) AS norm, MIN(lei) AS lei FROM gl
            GROUP BY 1 HAVING norm IS NOT NULL AND count(DISTINCT lei) = 1),
    ck AS (SELECT label, value FROM exposure_labels WHERE provider = '' AND field = 'coupon_kind'),
    te AS (SELECT silver_source_id, account_external_id, COALESCE(currency, '') AS ccy, term_end
             FROM account_rates
            WHERE side = 'credit' AND term_end > CAST(to_timestamp(p_asof) AT TIME ZONE 'UTC' AS DATE)),
    -- What each distinct name says, read once: its issuer text and
    -- whether it names an inflation-linked, a floating or a perpetual
    -- bond.
    nm AS MATERIALIZED (
        SELECT name, exposure_issuer_name(name) AS norm,
               regexp_matches(upper(name), '\b(TIPS|INFLATION|INFL|LINKERS?|I/L|INDEX[- ]LINKED|CPI)\b') AS infl,
               regexp_matches(upper(name), '\b(FRN|FLOAT|FLOATING|FLTG)\b') AS frn,
               regexp_matches(upper(name), '\bPERP') AS perp
          FROM (SELECT own_name AS name FROM l WHERE own_name IS NOT NULL
                UNION SELECT custodian FROM exposure_custody WHERE custodian IS NOT NULL)),
    d AS (
        SELECT l.*,
               l.asset_class IN ('fixed_income', 'private_debt') AS debt,
               l.depth = 0 AND (l.is_cash OR (l.asset_class = 'cash'
                                              AND l.h_vehicle IN ('demand_deposit', 'time_deposit'))) AS deposit,
               CASE WHEN l.fund_leaf THEN l.key WHEN l.depth > 0 THEN l.fund_ref END AS page_fund,
               CASE WHEN l.depth > 0 THEN l.line_maturity
                    WHEN l.is_cash OR (l.asset_class = 'cash' AND l.h_vehicle IN ('demand_deposit', 'time_deposit'))
                         THEN COALESCE(t.maturity, (SELECT MIN(te.term_end) FROM te
                                                     WHERE te.silver_source_id = l.src
                                                       AND te.account_external_id = l.acct
                                                       AND te.ccy IN (l.currency, '')))
                    ELSE t.maturity END AS maturity,
               CASE WHEN l.depth = 0 THEN t.coupon_kind
                    WHEN ck.value = 'floating' THEN 'floating'
                    WHEN o.infl THEN 'inflation_linked'
                    WHEN o.frn AND l.line_coupon_kind IS NULL THEN 'floating'
                    WHEN l.line_real THEN 'inflation_linked'
                    WHEN ck.value IN ('fixed', 'zero') THEN ck.value
                    WHEN l.line_coupon = 0 THEN 'zero'
                    WHEN l.line_coupon > 0 THEN 'fixed' END AS coupon_kind,
               CASE WHEN l.depth = 0 THEN t.rating END AS own_rating,
               cu.custodian, COALESCE(o.perp, FALSE) AS named_perpetual, o.norm AS own_norm, c.norm AS custodian_norm,
               COALESCE(l.line_lei, gk.lei) AS stated_lei
          FROM l
          -- Every use of t below is a holding's own (depth 0).
          LEFT JOIN t ON t.silver_source_id = l.src AND t.instrument_external_id = l.iid
          LEFT JOIN gk ON gk.key = l.key
          LEFT JOIN exposure_custody cu ON cu.silver_source_id = l.src
          LEFT JOIN nm o ON o.name = l.own_name
          LEFT JOIN nm c ON c.name = cu.custodian
          LEFT JOIN ck ON ck.label = exposure_norm(l.line_coupon_kind)),
    n AS (
        SELECT d.* EXCLUDE (own_norm, custodian_norm),
               CASE WHEN d.deposit THEN d.custodian_norm ELSE d.own_norm END AS norm,
               d.deposit OR (d.kind IN ('holding', 'line') AND NOT d.fund_leaf AND NOT (d.depth = 0 AND d.declared)
                             AND d.asset_class IN ('public_equity', 'fixed_income', 'private_debt')) AS has_issuer
          FROM d)
    SELECT n.*,
           CASE WHEN n.asset_class = 'cash' AND n.maturity IS NULL THEN 'under 1y'
                WHEN NOT n.debt AND n.asset_class <> 'cash' THEN '(none)'
                WHEN n.maturity IS NULL AND n.named_perpetual THEN 'perpetual'
                WHEN n.maturity IS NULL THEN 'unknown'
                ELSE exposure_maturity_bucket(CAST(to_timestamp(p_asof) AT TIME ZONE 'UTC' AS DATE), n.maturity)
           END AS maturity_bucket,
           CASE WHEN NOT n.debt THEN '(none)' ELSE COALESCE(n.coupon_kind, 'unknown') END AS coupon_bucket,
           COALESCE(n.own_rating, rf.bucket_low) AS rating,
           CASE WHEN n.asset_class <> 'public_equity' THEN '(none)' ELSE COALESCE(cy.cycle, 'unknown') END
               AS cycle,
           CASE WHEN NOT n.has_issuer THEN NULL
                WHEN COALESCE(CASE WHEN NOT n.deposit THEN n.stated_lei END, gn.lei) IS NOT NULL
                     THEN 'lei:' || COALESCE(CASE WHEN NOT n.deposit THEN n.stated_lei END, gn.lei)
                WHEN n.norm IS NOT NULL THEN 'name:' || n.norm
                WHEN n.deposit THEN 'source:' || n.src
                WHEN n.key IS NOT NULL THEN 'key:' || n.key
                ELSE 'unknown' END AS issuer_key,
           CASE WHEN NOT n.has_issuer THEN NULL
                ELSE COALESCE(gl.name, CASE WHEN n.deposit THEN COALESCE(n.custodian, n.src) END,
                              CASE WHEN n.asset_class = 'public_equity' THEN n.own_name END, n.norm, n.own_name, n.key,
                              n.stated_lei, 'unknown') END AS issuer_name,
           CASE WHEN gl.name IS NOT NULL THEN 2 WHEN n.asset_class = 'public_equity' THEN 1 ELSE 0 END AS name_rank,
           CASE WHEN n.has_issuer THEN COALESCE(CASE WHEN NOT n.deposit THEN n.stated_lei END, gn.lei) END AS lei
      FROM n
      LEFT JOIN rf ON rf.fund_ref = n.page_fund
      LEFT JOIN yield_cycles cy ON cy.sector = n.sector
      LEFT JOIN gn ON gn.norm = n.norm
      LEFT JOIN gl ON gl.lei = COALESCE(CASE WHEN NOT n.deposit THEN n.stated_lei END, gn.lei)
);

-- report_exposure_issuers: one row per issuer, its stock, bonds and
-- deposits together, the largest first, with the running share of net
-- worth (cumulative_share_pct) and what part of the issuer's value is
-- equity, debt and cash. Its name is GLEIF's, else its stock's, else its
-- largest leaf's. The leaves no issuer applies to are one (none) row at
-- the end.
CREATE MACRO report_exposure_issuers(p_asof, p_ccy) AS TABLE (
    WITH l AS (SELECT *, COALESCE(issuer_key, '(none)') AS ik FROM exposure_yield_leaves(p_asof, p_ccy)),
    t AS (SELECT SUM(value) AS total FROM l),
    g AS (
        SELECT ik AS issuer_key,
               CASE WHEN ik = '(none)' THEN '(none)'
                    ELSE first(issuer_name ORDER BY name_rank DESC, abs(value) DESC, issuer_name) END AS issuer,
               MIN(lei) AS lei, SUM(value) AS value,
               SUM(value) FILTER (WHERE asset_class = 'public_equity') AS equity,
               SUM(value) FILTER (WHERE debt) AS debt,
               SUM(value) FILTER (WHERE deposit) AS cash,
               COUNT(DISTINCT key) AS securities,
               exposure_quality(flags, value) AS quality
          FROM l GROUP BY ik)
    SELECT g.issuer, g.lei, g.value, 100 * g.value / NULLIF(t.total, 0) AS share_pct,
           CASE WHEN g.issuer_key <> '(none)' THEN
                100 * SUM(g.value) OVER (ORDER BY g.issuer_key = '(none)', g.value DESC, g.issuer_key
                                         ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) / NULLIF(t.total, 0) END
               AS cumulative_share_pct,
           100 * COALESCE(g.equity, 0) / NULLIF(g.value, 0) AS equity_pct,
           100 * COALESCE(g.debt, 0) / NULLIF(g.value, 0) AS debt_pct,
           100 * COALESCE(g.cash, 0) / NULLIF(g.value, 0) AS cash_pct,
           g.securities, g.quality
      FROM g, t
     ORDER BY g.issuer_key = '(none)', g.value DESC, g.issuer_key
);

-- exposure_debt_buckets: each leaf of exposure_yield_leaves in each of
-- the four debt and equity dimensions (docs/EXPOSURE.md §7), one row per
-- leaf and dimension, passing direct and the flags through:
--
--   * credit: its credit bucket; a debt leaf no rating describes split by
--     its page fund's credit breakdown. unknown: no rating or known label
--     describes it;
--   * maturity: its maturity bucket; an unknown one split by its page
--     fund's maturity breakdown;
--   * coupon: its coupon kind;
--   * cycle: its cycle.
--
-- Each dimension sums to the leaves' value.
CREATE MACRO exposure_debt_buckets(p_asof, p_ccy) AS TABLE (
    WITH l AS MATERIALIZED (SELECT * FROM exposure_yield_leaves(p_asof, p_ccy))
    SELECT l.src, l.acct, l.position_key, l.depth, l.direct, l.flags, 'credit' AS dimension,
           exposure_credit_bucket(l.asset_class, l.debt, l.rating, b.bucket) AS bucket,
           exposure_credit_unknown(l.debt, l.rating, b.unknown) AS unknown,
           l.value * COALESCE(b.share, 1) AS value
      FROM l
      -- The condition is in the key, so the join hashes.
      LEFT JOIN exposure_page_breakdown(p_asof, 'credit_quality') b
             ON b.fund = CASE WHEN l.debt AND l.rating IS NULL THEN l.page_fund END
    UNION ALL
    SELECT l.src, l.acct, l.position_key, l.depth, l.direct, l.flags, 'maturity',
           exposure_maturity_split(l.maturity_bucket, b.bucket), FALSE, l.value * COALESCE(b.share, 1)
      FROM l
      -- The condition is in the key, so the join hashes.
      LEFT JOIN exposure_page_breakdown(p_asof, 'maturity_breakdown') b
             ON b.fund = CASE WHEN l.maturity_bucket = 'unknown' THEN l.page_fund END
    UNION ALL
    SELECT l.src, l.acct, l.position_key, l.depth, l.direct, l.flags, 'coupon', l.coupon_bucket, FALSE, l.value FROM l
    UNION ALL
    SELECT l.src, l.acct, l.position_key, l.depth, l.direct, l.flags, 'cycle', l.cycle, FALSE, l.value FROM l
);

-- report_exposure_classes: the leaves by asset class, split by direct.
CREATE MACRO report_exposure_classes(p_asof, p_ccy) AS TABLE (
    WITH l AS (SELECT * FROM exposure_leaves(p_asof, p_ccy)),
    t AS (SELECT SUM(value) AS total FROM l)
    SELECT l.asset_class, SUM(l.value) AS value,
           100 * SUM(l.value) / NULLIF(t.total, 0) AS share_pct,
           100 * COALESCE(SUM(l.value) FILTER (WHERE l.direct), 0) / NULLIF(t.total, 0) AS direct_pct,
           100 * COALESCE(SUM(l.value) FILTER (WHERE NOT l.direct), 0) / NULLIF(t.total, 0) AS via_funds_pct,
           exposure_quality(l.flags, l.value) AS quality
      FROM l, t GROUP BY l.asset_class, t.total
     ORDER BY value DESC, asset_class
);

-- report_exposure_sectors: the leaves by sector, split by direct.
CREATE MACRO report_exposure_sectors(p_asof, p_ccy) AS TABLE (
    WITH l AS (SELECT * FROM exposure_labeled_leaves(p_asof, p_ccy)),
    t AS (SELECT SUM(value) AS total FROM l),
    c AS (SELECT asset_class, SUM(value) AS class_total FROM l GROUP BY 1)
    SELECT l.asset_class, l.sector_bucket AS sector, SUM(l.value) AS value,
           100 * SUM(l.value) / NULLIF(t.total, 0) AS share_pct,
           100 * SUM(l.value) / NULLIF(c.class_total, 0) AS class_share_pct,
           100 * COALESCE(SUM(l.value) FILTER (WHERE l.direct), 0) / NULLIF(t.total, 0) AS direct_pct,
           100 * COALESCE(SUM(l.value) FILTER (WHERE NOT l.direct), 0) / NULLIF(t.total, 0) AS via_funds_pct,
           exposure_quality(l.flags, l.value) AS quality
      FROM l JOIN c USING (asset_class), t
     GROUP BY l.asset_class, l.sector_bucket, t.total, c.class_total
     ORDER BY c.class_total DESC, l.asset_class, value DESC, sector
);

-- report_exposure_sizes: the equity leaves by company size, split by
-- direct.
CREATE MACRO report_exposure_sizes(p_asof, p_ccy) AS TABLE (
    WITH l AS (SELECT * FROM exposure_labeled_leaves(p_asof, p_ccy)),
    t AS (SELECT SUM(value) AS total FROM l),
    c AS (SELECT asset_class, SUM(value) AS class_total FROM l GROUP BY 1)
    SELECT l.asset_class, l.size_bucket AS size, SUM(l.value) AS value,
           100 * SUM(l.value) / NULLIF(t.total, 0) AS share_pct,
           100 * SUM(l.value) / NULLIF(c.class_total, 0) AS class_share_pct,
           100 * COALESCE(SUM(l.value) FILTER (WHERE l.direct), 0) / NULLIF(t.total, 0) AS direct_pct,
           100 * COALESCE(SUM(l.value) FILTER (WHERE NOT l.direct), 0) / NULLIF(t.total, 0) AS via_funds_pct,
           exposure_quality(l.flags, l.value) AS quality
      FROM l JOIN c USING (asset_class), t
     GROUP BY l.asset_class, l.size_bucket, t.total, c.class_total
     ORDER BY c.class_total DESC, l.asset_class,
              CASE l.size_bucket WHEN 'large' THEN 1 WHEN 'mid' THEN 2 WHEN 'small' THEN 3 WHEN 'unknown' THEN 4 ELSE 5 END
);

-- report_exposure_countries: the leaves by country, split by direct.
CREATE MACRO report_exposure_countries(p_asof, p_ccy, p_country := 'risk') AS TABLE (
    WITH l AS (SELECT * FROM exposure_labeled_leaves(p_asof, p_ccy, p_country := p_country)),
    t AS (SELECT SUM(value) AS total FROM l)
    SELECT l.country_bucket AS country, MAX(l.country_name) AS country_name, l.region, SUM(l.value) AS value,
           100 * SUM(l.value) / NULLIF(t.total, 0) AS share_pct,
           100 * COALESCE(SUM(l.value) FILTER (WHERE l.direct), 0) / NULLIF(t.total, 0) AS direct_pct,
           100 * COALESCE(SUM(l.value) FILTER (WHERE NOT l.direct), 0) / NULLIF(t.total, 0) AS via_funds_pct,
           exposure_quality(l.flags, l.value) AS quality
      FROM l, t GROUP BY l.country_bucket, l.region, t.total
     ORDER BY value DESC, country, region
);

-- report_exposure_currencies: the leaves by currency, split by direct.
CREATE MACRO report_exposure_currencies(p_asof, p_ccy) AS TABLE (
    WITH l AS (SELECT * FROM exposure_labeled_leaves(p_asof, p_ccy)),
    t AS (SELECT SUM(value) AS total FROM l)
    SELECT l.currency_bucket AS currency, l.currency_kind, SUM(l.value) AS value,
           100 * SUM(l.value) / NULLIF(t.total, 0) AS share_pct,
           100 * COALESCE(SUM(l.value) FILTER (WHERE l.direct), 0) / NULLIF(t.total, 0) AS direct_pct,
           100 * COALESCE(SUM(l.value) FILTER (WHERE NOT l.direct), 0) / NULLIF(t.total, 0) AS via_funds_pct,
           exposure_quality(l.flags, l.value) AS quality
      FROM l, t GROUP BY 1, 2, t.total
     ORDER BY value DESC, currency
);

-- ============================================================
-- The four debt and equity views read exposure_debt_buckets, one
-- dimension each, and share one shape: the rows of a dimension in
-- exposure_bucket_order, each with its share of net worth, its share of
-- what the dimension describes (class_share_pct, exposure_class_share)
-- and its split by direct. What the dimension describes is every row
-- but (none): the debt and the cash in credit and maturities, the debt
-- in coupons, the public equity in cycles.
-- ============================================================

-- report_exposure_coupons: the leaves by coupon kind, split by direct.
CREATE MACRO report_exposure_coupons(p_asof, p_ccy) AS TABLE (
    WITH l AS (SELECT * FROM exposure_debt_buckets(p_asof, p_ccy) WHERE dimension = 'coupon'),
    t AS (SELECT SUM(value) AS total, SUM(value) FILTER (WHERE bucket <> '(none)') AS described FROM l)
    SELECT bucket AS coupon_kind, SUM(value) AS value,
           100 * SUM(value) / NULLIF(t.total, 0) AS share_pct,
           CASE WHEN bucket <> '(none)' THEN exposure_class_share(SUM(value), t.described) END AS class_share_pct,
           100 * COALESCE(SUM(value) FILTER (WHERE direct), 0) / NULLIF(t.total, 0) AS direct_pct,
           100 * COALESCE(SUM(value) FILTER (WHERE NOT direct), 0) / NULLIF(t.total, 0) AS via_funds_pct,
           exposure_quality(flags, value) AS quality
      FROM l, t
     GROUP BY bucket, t.total, t.described
     ORDER BY exposure_bucket_order('coupon', bucket)
);

-- report_exposure_credit: the leaves by credit bucket, with its grade,
-- split by direct.
CREATE MACRO report_exposure_credit(p_asof, p_ccy) AS TABLE (
    WITH l AS (SELECT * FROM exposure_debt_buckets(p_asof, p_ccy) WHERE dimension = 'credit'),
    t AS (SELECT SUM(value) AS total, SUM(value) FILTER (WHERE bucket <> '(none)') AS described FROM l)
    SELECT bucket, exposure_credit_grade(bucket) AS grade, SUM(value) AS value,
           100 * SUM(value) / NULLIF(t.total, 0) AS share_pct,
           CASE WHEN bucket <> '(none)' THEN exposure_class_share(SUM(value), t.described) END AS class_share_pct,
           100 * COALESCE(SUM(value) FILTER (WHERE direct), 0) / NULLIF(t.total, 0) AS direct_pct,
           100 * COALESCE(SUM(value) FILTER (WHERE NOT direct), 0) / NULLIF(t.total, 0) AS via_funds_pct,
           concat_ws(',', nullif(exposure_quality(flags, value), ''),
                     exposure_flag_share('unknown', SUM(abs(value)) FILTER (WHERE unknown), SUM(abs(value))))
               AS quality
      FROM l, t
     GROUP BY bucket, t.total, t.described
     ORDER BY exposure_bucket_order('credit', bucket)
);

-- report_exposure_cycles: the leaves by economic cycle, split by direct.
CREATE MACRO report_exposure_cycles(p_asof, p_ccy) AS TABLE (
    WITH l AS (SELECT * FROM exposure_debt_buckets(p_asof, p_ccy) WHERE dimension = 'cycle'),
    t AS (SELECT SUM(value) AS total, SUM(value) FILTER (WHERE bucket <> '(none)') AS described FROM l)
    SELECT bucket AS cycle, SUM(value) AS value,
           100 * SUM(value) / NULLIF(t.total, 0) AS share_pct,
           CASE WHEN bucket <> '(none)' THEN exposure_class_share(SUM(value), t.described) END AS class_share_pct,
           100 * COALESCE(SUM(value) FILTER (WHERE direct), 0) / NULLIF(t.total, 0) AS direct_pct,
           100 * COALESCE(SUM(value) FILTER (WHERE NOT direct), 0) / NULLIF(t.total, 0) AS via_funds_pct,
           exposure_quality(flags, value) AS quality
      FROM l, t
     GROUP BY bucket, t.total, t.described
     ORDER BY exposure_bucket_order('cycle', bucket)
);

-- report_exposure_maturities: the leaves by maturity bucket, split by
-- direct.
CREATE MACRO report_exposure_maturities(p_asof, p_ccy) AS TABLE (
    WITH l AS (SELECT * FROM exposure_debt_buckets(p_asof, p_ccy) WHERE dimension = 'maturity'),
    t AS (SELECT SUM(value) AS total, SUM(value) FILTER (WHERE bucket <> '(none)') AS described FROM l)
    SELECT bucket, SUM(value) AS value,
           100 * SUM(value) / NULLIF(t.total, 0) AS share_pct,
           CASE WHEN bucket <> '(none)' THEN exposure_class_share(SUM(value), t.described) END AS class_share_pct,
           100 * COALESCE(SUM(value) FILTER (WHERE direct), 0) / NULLIF(t.total, 0) AS direct_pct,
           100 * COALESCE(SUM(value) FILTER (WHERE NOT direct), 0) / NULLIF(t.total, 0) AS via_funds_pct,
           exposure_quality(flags, value) AS quality
      FROM l, t
     GROUP BY bucket, t.total, t.described
     ORDER BY exposure_bucket_order('maturity', bucket)
);

-- report_exposure_securities: the leaves by security, split by direct.
-- A line's security keeps its name by depth.
CREATE MACRO report_exposure_securities(p_asof, p_ccy, p_country := 'risk') AS TABLE (
    WITH g AS (SELECT * FROM exposure_labeled_leaves(p_asof, p_ccy, p_country := p_country)),
    t AS (SELECT SUM(value) AS total FROM g)
    SELECT g.security, MAX(g.name) AS name, MAX(g.ticker) AS ticker,
           arg_max(g.asset_class, (abs(g.value), g.asset_class)) AS asset_class,
           arg_max(g.sector, (abs(g.value), g.sector)) AS sector,
           arg_max(g.country, (abs(g.value), g.country)) AS country,
           arg_max(g.currency, (abs(g.value), g.currency)) AS currency, MAX(g.market_cap) AS market_cap, MAX(g.size) AS size,
           SUM(g.value) AS value,
           COALESCE(SUM(g.value) FILTER (WHERE g.direct), 0) AS direct_value,
           COALESCE(SUM(g.value) FILTER (WHERE NOT g.direct), 0) AS via_funds_value,
           COUNT(DISTINCT g.top_key) FILTER (WHERE NOT g.direct) AS funds,
           100 * SUM(g.value) / NULLIF(t.total, 0) AS share_pct,
           exposure_quality(g.flags, g.value) AS quality
      FROM g, t
     GROUP BY g.security, t.total
     ORDER BY abs(SUM(g.value)) DESC, security
);

-- report_exposure_funds: each holding and the composition that
-- describes it, a forward held directly among the other holdings.
CREATE MACRO report_exposure_funds(p_asof, p_ccy) AS TABLE (
    WITH h AS (
        SELECT h.*, d.line_no AS decl, d.origin AS decl_origin, d.as_of AS decl_as_of, d.dimensions AS decl_dimensions,
               {'situs': st.situs, 'basis': COALESCE(st.basis, 'unknown')} AS holding_situs
          FROM exposure_holdings(p_asof, p_ccy) h
          LEFT JOIN exposure_declarations_at(p_asof) d
                 ON d.silver_source_id = h.src AND d.instrument_external_id = h.iid
          LEFT JOIN exposure_situs_at(p_asof) st
                 ON st.silver_source_id = h.src AND st.account_external_id = h.acct AND st.position_key = h.position_key
         WHERE h.value IS NOT NULL),
    t AS (SELECT SUM(value) AS total FROM h),
    fa AS (SELECT * FROM exposure_funds_at(p_asof)),
    -- comp_ref: the composition that describes the holding, as
    -- exposure_leaves reads it (exposure_composition_wins). A declared
    -- holding that is no fund counts among the other holdings, which no
    -- declaration describes. The join is on the key alone, so it hashes;
    -- fa holds one row per fund, so testing after the join gives the
    -- same rows. A forward held directly, described by a composition of
    -- its own, is no fund: it counts among the other holdings.
    k AS (
        SELECT h.*, CASE WHEN exposure_composition_wins(h.decl, h.decl_origin, h.decl_as_of, fa.own_as_of, fa.own_origin)
                              AND NOT COALESCE(h.vehicle = 'forward' AND fa.via = 'direct' AND fa.own_origin = 'parsed',
                                               FALSE)
                         THEN fa.fund_ref END AS comp_ref,
               comp_ref IS NULL AND h.decl IS NOT NULL AND h.vehicle IN ('etf', 'fund') AS declared
          FROM h LEFT JOIN fa ON fa.fund_ref = h.key),
    f AS (
        SELECT CASE WHEN k.comp_ref IS NOT NULL OR k.vehicle IN ('etf', 'fund') THEN COALESCE(k.key, 'holding:' || k.src || ':' || COALESCE(k.iid, k.position_key))
                    ELSE '(other holdings)' END AS fund,
               k.* FROM k),
    g AS (
        SELECT f.fund, MAX(f.name) FILTER (WHERE f.fund <> '(other holdings)') AS name,
               SUM(f.value) AS value, 100 * SUM(f.value) / NULLIF(t.total, 0) AS share_pct,
               COALESCE(MAX(fa.via), CASE WHEN BOOL_OR(f.declared) THEN 'declared' END) AS via, MAX(fa.proxy_ref) FILTER (WHERE fa.via = 'proxy') AS proxy,
               MAX(fa.leverage) FILTER (WHERE fa.via = 'proxy') AS leverage,
               MAX(fa.provider) AS provider, MAX(fa.basis) AS basis, MAX(fa.as_of) AS composition_as_of,
               MAX(fa.gap_days) AS gap_days, MAX(fa.lines) AS lines, MAX(fa.wsum) AS weight_sum,
               COALESCE(MAX(fa.domicile), MAX(fat.domicile)) AS domicile,
               arg_max(f.holding_situs, abs(f.value)) FILTER (WHERE f.fund <> '(other holdings)') AS top_situs,
               COALESCE(MAX(fa.replication), MAX(fat.replication)) AS replication,
               COALESCE(BOOL_OR(fa.lending), BOOL_OR(fat.lending)) AS lending,
               COALESCE(MAX(fa.hedged_currency), MAX(fat.hedged_currency)) AS hedged_currency,
               MAX(f.decl_as_of) FILTER (WHERE f.declared) AS declaration_as_of,
               MAX(f.decl_dimensions) FILTER (WHERE f.declared) AS declared_dimensions,
               array_to_string(list_sort(list_distinct(list_filter([
                   CASE WHEN BOOL_OR(fa.via = 'proxy') THEN 'proxy' END,
                   CASE WHEN BOOL_OR(fa.aged) THEN 'aged' END,
                   CASE WHEN BOOL_OR(fa.forward) THEN 'forward' END,
                   CASE WHEN BOOL_OR(fa.partial) THEN 'partial' END,
                   CASE WHEN BOOL_OR(f.declared) THEN 'declared' END,
                   CASE WHEN f.fund <> '(other holdings)' AND MAX(fa.fund_ref) IS NULL AND NOT BOOL_OR(f.declared)
                        THEN 'no_composition' END],
                   lambda x: x IS NOT NULL))), ',') AS quality
          FROM f
          LEFT JOIN fa ON fa.fund_ref = f.comp_ref
          -- A declared fund's attributes are its own, as they are under a
          -- composition (exposure_funds_at). The condition is in the key,
          -- so the join hashes.
          LEFT JOIN exposure_attributes_at(p_asof) fat ON fat.fund_ref = CASE WHEN f.declared THEN f.key END, t
         GROUP BY f.fund, t.total)
    SELECT * EXCLUDE (top_situs), top_situs.situs AS situs, top_situs.basis AS situs_basis
      FROM g
     ORDER BY fund = '(other holdings)', abs(value) DESC, fund
);
