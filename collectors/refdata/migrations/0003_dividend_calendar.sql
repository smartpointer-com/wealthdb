-- refdata silver schema v3: the dividend calendar.
--
-- dividend_events holds the distributions of the funds and securities
-- gold holds, as the providers state them: an issuer's fund page or
-- workbook for a fund, an exchange's list or a company's filing for a
-- share. Each row is one event: its kind, its ex, record, pay and
-- declared days, and its gross amount per share or unit in its
-- currency. A provider states every event its source lists, the oldest
-- too. A source that lists none, as for an accumulating share class,
-- states no row. Gold reads a payment's record day from it.
--
-- Conventions as in 0001. as_of is the day the source stated the
-- events, not a day of an event; an event's own days are YYYY-MM-DD
-- text. A source that gives an event no id of its own gets one from its
-- kind and its days (providers/base.py, event_id).

-- One distribution of a fund or a security as a provider states it.
-- A later run's row replaces an earlier one of the same event.
CREATE TABLE dividend_events (
    ref           TEXT    NOT NULL,              -- a fund_ref or a security_key as the wanted list names it;
                                                 -- jpx:<code> for a JPX list row
    provider      TEXT    NOT NULL,
    event_id      TEXT    NOT NULL,              -- the source's id, else kind|ex|record|pay joined
    as_of         INTEGER NOT NULL,              -- the day the source stated it, UTC midnight epoch
    kind          TEXT    NOT NULL,              -- dividend | capital_gain | return_of_capital | special | other
    ex_date       TEXT,                          -- YYYY-MM-DD, NULL where the source states none
    record_date   TEXT,
    pay_date      TEXT,
    declared_date TEXT,
    amount        TEXT,                          -- gross per share or unit, a decimal string
    currency      TEXT,
    status        TEXT,                          -- final | estimated; NULL where the source says neither
    run_at        INTEGER NOT NULL,
    payload       TEXT    NOT NULL DEFAULT '{}',
    PRIMARY KEY (ref, provider, event_id)
);
CREATE INDEX idx_dividend_events_run ON dividend_events (run_at);

INSERT INTO schema_meta (silver_schema_version, applied_at)
VALUES (3, CAST(strftime('%s', 'now') AS INTEGER));
