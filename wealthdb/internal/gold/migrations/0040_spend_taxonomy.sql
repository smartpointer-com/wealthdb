-- The spend-category dimension: the two-level vocabulary the spending
-- feature groups by. `spend_detailed` is what the enrichment overlay
-- (migration 0041) assigns per transaction or per merchant;
-- `spend_primary` is the coarse bucket a report rolls up to.
--
-- Seeded from internal/canonical/spendtaxonomy.go, which carries the
-- provenance in full: Plaid's Personal Finance Category taxonomy
-- (retrieved 2026-09-04, 16 primaries / 104 detailed pairs), reduced
-- to its spend side — 12 primaries / 80 detailed values, dropping the
-- flow primaries INCOME, TRANSFER_IN, TRANSFER_OUT and LOAN_PAYMENTS —
-- plus the product's own delta values, which are primary-level, so
-- primary == detailed. The deltas and their rationale are listed in
-- internal/canonical/spendtaxonomy.go and docs/SPENDING.md ("The six
-- deltas"); this migration seeds the values it establishes, and a
-- further delta arrives as its own migration (0045-0047).
-- TestSpendCategoriesMatchGoTable compares the migrated dimension —
-- this seed and every later one — against the Go table as a set, so the
-- two cannot drift; a taxonomy revision edits the Go table and ships a
-- new migration to re-seed.
--
-- The vendored values keep Plaid's uppercase spelling and the deltas
-- the repo's lowercase enum idiom: the casing marks provenance, so a
-- value's origin is readable straight off a query result.
--
-- No FK from the overlay tables onto this dimension: gold's fact
-- tables carry none either (DESIGN.md §7.2), and a category the
-- enrichment pass cannot place is left NULL rather than pointed at a
-- placeholder row.
--
-- CREATE TABLE IF NOT EXISTS + INSERT OR REPLACE keep this replayable
-- for the DDL-rerun test (see gold.Migrate's REPLAY note) — a bare
-- CREATE would raise "table already exists" and a bare INSERT would
-- collide on the primary key.
CREATE TABLE IF NOT EXISTS spend_categories (
    spend_primary  TEXT NOT NULL,
    spend_detailed TEXT NOT NULL PRIMARY KEY,
    description    TEXT NOT NULL
);

INSERT OR REPLACE INTO spend_categories (spend_primary, spend_detailed, description) VALUES
    ('BANK_FEES', 'BANK_FEES_ATM_FEES', 'Fees incurred for out-of-network ATMs'),
    ('BANK_FEES', 'BANK_FEES_FOREIGN_TRANSACTION_FEES', 'Fees incurred on non-domestic transactions'),
    ('BANK_FEES', 'BANK_FEES_INSUFFICIENT_FUNDS', 'Fees relating to insufficient funds'),
    ('BANK_FEES', 'BANK_FEES_INTEREST_CHARGE', 'Fees incurred for interest on purchases, including not-paid-in-full or interest on cash advances'),
    ('BANK_FEES', 'BANK_FEES_OVERDRAFT_FEES', 'Fees incurred when an account is in overdraft'),
    ('BANK_FEES', 'BANK_FEES_OTHER_BANK_FEES', 'Other miscellaneous bank fees'),

    ('ENTERTAINMENT', 'ENTERTAINMENT_CASINOS_AND_GAMBLING', 'Gambling, casinos, and sports betting'),
    ('ENTERTAINMENT', 'ENTERTAINMENT_MUSIC_AND_AUDIO', 'Digital and in-person music purchases, including music streaming services'),
    ('ENTERTAINMENT', 'ENTERTAINMENT_SPORTING_EVENTS_AMUSEMENT_PARKS_AND_MUSEUMS', 'Purchases made at sporting events, music venues, concerts, museums, and amusement parks'),
    ('ENTERTAINMENT', 'ENTERTAINMENT_TV_AND_MOVIES', 'In home movie streaming services and movie theaters'),
    ('ENTERTAINMENT', 'ENTERTAINMENT_VIDEO_GAMES', 'Digital and in-person video game purchases'),
    ('ENTERTAINMENT', 'ENTERTAINMENT_OTHER_ENTERTAINMENT', 'Other miscellaneous entertainment purchases, including night life and adult entertainment'),

    ('FOOD_AND_DRINK', 'FOOD_AND_DRINK_BEER_WINE_AND_LIQUOR', 'Beer, Wine & Liquor Stores'),
    ('FOOD_AND_DRINK', 'FOOD_AND_DRINK_COFFEE', 'Purchases at coffee shops or cafes'),
    ('FOOD_AND_DRINK', 'FOOD_AND_DRINK_FAST_FOOD', 'Dining expenses for fast food chains'),
    ('FOOD_AND_DRINK', 'FOOD_AND_DRINK_GROCERIES', 'Purchases for fresh produce and groceries, including farmers'' markets'),
    ('FOOD_AND_DRINK', 'FOOD_AND_DRINK_RESTAURANT', 'Dining expenses for restaurants, bars, gastropubs, and diners'),
    ('FOOD_AND_DRINK', 'FOOD_AND_DRINK_VENDING_MACHINES', 'Purchases made at vending machine operators'),
    ('FOOD_AND_DRINK', 'FOOD_AND_DRINK_OTHER_FOOD_AND_DRINK', 'Other miscellaneous food and drink, including desserts, juice bars, and delis'),

    ('GENERAL_MERCHANDISE', 'GENERAL_MERCHANDISE_BOOKSTORES_AND_NEWSSTANDS', 'Books, magazines, and news'),
    ('GENERAL_MERCHANDISE', 'GENERAL_MERCHANDISE_CLOTHING_AND_ACCESSORIES', 'Apparel, shoes, and jewelry'),
    ('GENERAL_MERCHANDISE', 'GENERAL_MERCHANDISE_CONVENIENCE_STORES', 'Purchases at convenience stores'),
    ('GENERAL_MERCHANDISE', 'GENERAL_MERCHANDISE_DEPARTMENT_STORES', 'Retail stores with wide ranges of consumer goods, typically specializing in clothing and home goods'),
    ('GENERAL_MERCHANDISE', 'GENERAL_MERCHANDISE_DISCOUNT_STORES', 'Stores selling goods at a discounted price'),
    ('GENERAL_MERCHANDISE', 'GENERAL_MERCHANDISE_ELECTRONICS', 'Electronics stores and websites'),
    ('GENERAL_MERCHANDISE', 'GENERAL_MERCHANDISE_GIFTS_AND_NOVELTIES', 'Photo, gifts, cards, and floral stores'),
    ('GENERAL_MERCHANDISE', 'GENERAL_MERCHANDISE_OFFICE_SUPPLIES', 'Stores that specialize in office goods'),
    ('GENERAL_MERCHANDISE', 'GENERAL_MERCHANDISE_ONLINE_MARKETPLACES', 'Multi-purpose e-commerce platforms such as Etsy, Ebay and Amazon'),
    ('GENERAL_MERCHANDISE', 'GENERAL_MERCHANDISE_PET_SUPPLIES', 'Pet supplies and pet food'),
    ('GENERAL_MERCHANDISE', 'GENERAL_MERCHANDISE_SPORTING_GOODS', 'Sporting goods, camping gear, and outdoor equipment'),
    ('GENERAL_MERCHANDISE', 'GENERAL_MERCHANDISE_SUPERSTORES', 'Superstores such as Target and Walmart, selling both groceries and general merchandise'),
    ('GENERAL_MERCHANDISE', 'GENERAL_MERCHANDISE_TOBACCO_AND_VAPE', 'Purchases for tobacco and vaping products'),
    ('GENERAL_MERCHANDISE', 'GENERAL_MERCHANDISE_OTHER_GENERAL_MERCHANDISE', 'Other miscellaneous merchandise, including toys, hobbies, and arts and crafts'),

    ('HOME_IMPROVEMENT', 'HOME_IMPROVEMENT_FURNITURE', 'Furniture, bedding, and home accessories'),
    ('HOME_IMPROVEMENT', 'HOME_IMPROVEMENT_HARDWARE', 'Building materials, hardware stores, paint, and wallpaper'),
    ('HOME_IMPROVEMENT', 'HOME_IMPROVEMENT_REPAIR_AND_MAINTENANCE', 'Plumbing, lighting, gardening, and roofing'),
    ('HOME_IMPROVEMENT', 'HOME_IMPROVEMENT_SECURITY', 'Home security system purchases'),
    ('HOME_IMPROVEMENT', 'HOME_IMPROVEMENT_OTHER_HOME_IMPROVEMENT', 'Other miscellaneous home purchases, including pool installation and pest control'),

    ('MEDICAL', 'MEDICAL_DENTAL_CARE', 'Dentists and general dental care'),
    ('MEDICAL', 'MEDICAL_EYE_CARE', 'Optometrists, contacts, and glasses stores'),
    ('MEDICAL', 'MEDICAL_NURSING_CARE', 'Nursing care and facilities'),
    ('MEDICAL', 'MEDICAL_PHARMACIES_AND_SUPPLEMENTS', 'Pharmacies and nutrition shops'),
    ('MEDICAL', 'MEDICAL_PRIMARY_CARE', 'Doctors and physicians'),
    ('MEDICAL', 'MEDICAL_VETERINARY_SERVICES', 'Prevention and care procedures for animals'),
    ('MEDICAL', 'MEDICAL_OTHER_MEDICAL', 'Other miscellaneous medical, including blood work, hospitals, and ambulances'),

    ('PERSONAL_CARE', 'PERSONAL_CARE_GYMS_AND_FITNESS_CENTERS', 'Gyms, fitness centers, and workout classes'),
    ('PERSONAL_CARE', 'PERSONAL_CARE_HAIR_AND_BEAUTY', 'Manicures, haircuts, waxing, spa/massages, and bath and beauty products'),
    ('PERSONAL_CARE', 'PERSONAL_CARE_LAUNDRY_AND_DRY_CLEANING', 'Wash and fold, and dry cleaning expenses'),
    ('PERSONAL_CARE', 'PERSONAL_CARE_OTHER_PERSONAL_CARE', 'Other miscellaneous personal care, including mental health apps and services'),

    ('GENERAL_SERVICES', 'GENERAL_SERVICES_ACCOUNTING_AND_FINANCIAL_PLANNING', 'Financial planning, and tax and accounting services'),
    ('GENERAL_SERVICES', 'GENERAL_SERVICES_AUTOMOTIVE', 'Oil changes, car washes, repairs, and towing'),
    ('GENERAL_SERVICES', 'GENERAL_SERVICES_CHILDCARE', 'Babysitters and daycare'),
    ('GENERAL_SERVICES', 'GENERAL_SERVICES_CONSULTING_AND_LEGAL', 'Consulting and legal services'),
    ('GENERAL_SERVICES', 'GENERAL_SERVICES_EDUCATION', 'Elementary, high school, professional schools, and college tuition'),
    ('GENERAL_SERVICES', 'GENERAL_SERVICES_INSURANCE', 'Insurance for auto, home, and healthcare'),
    ('GENERAL_SERVICES', 'GENERAL_SERVICES_POSTAGE_AND_SHIPPING', 'Mail, packaging, and shipping services'),
    ('GENERAL_SERVICES', 'GENERAL_SERVICES_STORAGE', 'Storage services and facilities'),
    ('GENERAL_SERVICES', 'GENERAL_SERVICES_OTHER_GENERAL_SERVICES', 'Other miscellaneous services, including advertising and cloud storage'),

    ('GOVERNMENT_AND_NON_PROFIT', 'GOVERNMENT_AND_NON_PROFIT_DONATIONS', 'Charitable, political, and religious donations'),
    ('GOVERNMENT_AND_NON_PROFIT', 'GOVERNMENT_AND_NON_PROFIT_GOVERNMENT_DEPARTMENTS_AND_AGENCIES', 'Government departments and agencies, such as driving licences, and passport renewal'),
    ('GOVERNMENT_AND_NON_PROFIT', 'GOVERNMENT_AND_NON_PROFIT_TAX_PAYMENT', 'Tax payments, including income and property taxes'),
    ('GOVERNMENT_AND_NON_PROFIT', 'GOVERNMENT_AND_NON_PROFIT_OTHER_GOVERNMENT_AND_NON_PROFIT', 'Other miscellaneous government and non-profit agencies'),

    ('TRANSPORTATION', 'TRANSPORTATION_BIKES_AND_SCOOTERS', 'Bike and scooter rentals'),
    ('TRANSPORTATION', 'TRANSPORTATION_GAS', 'Purchases at a gas station'),
    ('TRANSPORTATION', 'TRANSPORTATION_PARKING', 'Parking fees and expenses'),
    ('TRANSPORTATION', 'TRANSPORTATION_PUBLIC_TRANSIT', 'Public transportation, including rail and train, buses, and metro'),
    ('TRANSPORTATION', 'TRANSPORTATION_TAXIS_AND_RIDE_SHARES', 'Taxi and ride share services'),
    ('TRANSPORTATION', 'TRANSPORTATION_TOLLS', 'Toll expenses'),
    ('TRANSPORTATION', 'TRANSPORTATION_OTHER_TRANSPORTATION', 'Other miscellaneous transportation expenses'),

    ('TRAVEL', 'TRAVEL_FLIGHTS', 'Airline expenses'),
    ('TRAVEL', 'TRAVEL_LODGING', 'Hotels, motels, and hosted accommodation such as Airbnb'),
    ('TRAVEL', 'TRAVEL_RENTAL_CARS', 'Rental cars, charter buses, and trucks'),
    ('TRAVEL', 'TRAVEL_OTHER_TRAVEL', 'Other miscellaneous travel expenses'),

    ('RENT_AND_UTILITIES', 'RENT_AND_UTILITIES_GAS_AND_ELECTRICITY', 'Gas and electricity bills'),
    ('RENT_AND_UTILITIES', 'RENT_AND_UTILITIES_INTERNET_AND_CABLE', 'Internet and cable bills'),
    ('RENT_AND_UTILITIES', 'RENT_AND_UTILITIES_RENT', 'Rent payment'),
    ('RENT_AND_UTILITIES', 'RENT_AND_UTILITIES_SEWAGE_AND_WASTE_MANAGEMENT', 'Sewage and garbage disposal bills'),
    ('RENT_AND_UTILITIES', 'RENT_AND_UTILITIES_TELEPHONE', 'Cell phone bills'),
    ('RENT_AND_UTILITIES', 'RENT_AND_UTILITIES_WATER', 'Water bills'),
    ('RENT_AND_UTILITIES', 'RENT_AND_UTILITIES_OTHER_UTILITIES', 'Other miscellaneous utility bills'),

    -- Delta values: ours, not Plaid's, and primary-level.
    ('internal_transfer', 'internal_transfer', 'Movement between two accounts the product already tracks — card payments, funding wires, mortgage payments'),
    ('cash_withdrawal', 'cash_withdrawal', 'Cash taken out at an ATM or a counter; what it was then spent on is unobservable'),
    ('other', 'other', 'Spend that no rule, matcher or model could place in another category');

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (40, CAST(epoch(now()) AS BIGINT));
