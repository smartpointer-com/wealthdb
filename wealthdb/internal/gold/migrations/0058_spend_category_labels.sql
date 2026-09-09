-- Display labels for the spend taxonomy.
--
-- The vendored values shout in full caps and repeat their primary in the
-- detail (`GENERAL_MERCHANDISE_OTHER_GENERAL_MERCHANDISE`), while the six
-- deltas are lower-case, because one vocabulary is Plaid's and the other
-- is ours. That mix is fine as an identifier and poor as a column header.
--
-- So each row gains what it should READ as, and keeps what it IS. The
-- value stays the join key, the name a rule and a pin write, and what the
-- model gauntlet validates against — a taxonomy refresh still diffs
-- against the vendored spelling. Only presentation changes.
--
-- The labels are mechanical: the primary's prefix taken off, underscores
-- opened out, one capital at the front, and two initialisms the rule
-- would otherwise flatten. They are seeded literally rather than derived
-- in SQL so a label can be corrected by hand later without the
-- correction being computed away, and canonical.SpendLabel is the same
-- rule in Go; TestSpendCategoryLabelsMatchGoTable pins the two together.
ALTER TABLE spend_categories ADD COLUMN IF NOT EXISTS label         TEXT;
ALTER TABLE spend_categories ADD COLUMN IF NOT EXISTS primary_label TEXT;

UPDATE spend_categories SET label = v.label, primary_label = v.primary_label
  FROM (VALUES
    ('BANK_FEES_ATM_FEES', 'ATM fees', 'Bank fees'),
    ('BANK_FEES_FOREIGN_TRANSACTION_FEES', 'Foreign transaction fees', 'Bank fees'),
    ('BANK_FEES_INSUFFICIENT_FUNDS', 'Insufficient funds', 'Bank fees'),
    ('BANK_FEES_INTEREST_CHARGE', 'Interest charge', 'Bank fees'),
    ('BANK_FEES_OVERDRAFT_FEES', 'Overdraft fees', 'Bank fees'),
    ('BANK_FEES_OTHER_BANK_FEES', 'Other bank fees', 'Bank fees'),
    ('ENTERTAINMENT_CASINOS_AND_GAMBLING', 'Casinos and gambling', 'Entertainment'),
    ('ENTERTAINMENT_MUSIC_AND_AUDIO', 'Music and audio', 'Entertainment'),
    ('ENTERTAINMENT_SPORTING_EVENTS_AMUSEMENT_PARKS_AND_MUSEUMS', 'Sporting events amusement parks and museums', 'Entertainment'),
    ('ENTERTAINMENT_TV_AND_MOVIES', 'TV and movies', 'Entertainment'),
    ('ENTERTAINMENT_VIDEO_GAMES', 'Video games', 'Entertainment'),
    ('ENTERTAINMENT_OTHER_ENTERTAINMENT', 'Other entertainment', 'Entertainment'),
    ('FOOD_AND_DRINK_BEER_WINE_AND_LIQUOR', 'Beer wine and liquor', 'Food and drink'),
    ('FOOD_AND_DRINK_COFFEE', 'Coffee', 'Food and drink'),
    ('FOOD_AND_DRINK_FAST_FOOD', 'Fast food', 'Food and drink'),
    ('FOOD_AND_DRINK_GROCERIES', 'Groceries', 'Food and drink'),
    ('FOOD_AND_DRINK_RESTAURANT', 'Restaurant', 'Food and drink'),
    ('FOOD_AND_DRINK_VENDING_MACHINES', 'Vending machines', 'Food and drink'),
    ('FOOD_AND_DRINK_OTHER_FOOD_AND_DRINK', 'Other food and drink', 'Food and drink'),
    ('GENERAL_MERCHANDISE_BOOKSTORES_AND_NEWSSTANDS', 'Bookstores and newsstands', 'General merchandise'),
    ('GENERAL_MERCHANDISE_CLOTHING_AND_ACCESSORIES', 'Clothing and accessories', 'General merchandise'),
    ('GENERAL_MERCHANDISE_CONVENIENCE_STORES', 'Convenience stores', 'General merchandise'),
    ('GENERAL_MERCHANDISE_DEPARTMENT_STORES', 'Department stores', 'General merchandise'),
    ('GENERAL_MERCHANDISE_DISCOUNT_STORES', 'Discount stores', 'General merchandise'),
    ('GENERAL_MERCHANDISE_ELECTRONICS', 'Electronics', 'General merchandise'),
    ('GENERAL_MERCHANDISE_GIFTS_AND_NOVELTIES', 'Gifts and novelties', 'General merchandise'),
    ('GENERAL_MERCHANDISE_OFFICE_SUPPLIES', 'Office supplies', 'General merchandise'),
    ('GENERAL_MERCHANDISE_ONLINE_MARKETPLACES', 'Online marketplaces', 'General merchandise'),
    ('GENERAL_MERCHANDISE_PET_SUPPLIES', 'Pet supplies', 'General merchandise'),
    ('GENERAL_MERCHANDISE_SPORTING_GOODS', 'Sporting goods', 'General merchandise'),
    ('GENERAL_MERCHANDISE_SUPERSTORES', 'Superstores', 'General merchandise'),
    ('GENERAL_MERCHANDISE_TOBACCO_AND_VAPE', 'Tobacco and vape', 'General merchandise'),
    ('GENERAL_MERCHANDISE_OTHER_GENERAL_MERCHANDISE', 'Other general merchandise', 'General merchandise'),
    ('HOME_IMPROVEMENT_FURNITURE', 'Furniture', 'Home improvement'),
    ('HOME_IMPROVEMENT_HARDWARE', 'Hardware', 'Home improvement'),
    ('HOME_IMPROVEMENT_REPAIR_AND_MAINTENANCE', 'Repair and maintenance', 'Home improvement'),
    ('HOME_IMPROVEMENT_SECURITY', 'Security', 'Home improvement'),
    ('HOME_IMPROVEMENT_OTHER_HOME_IMPROVEMENT', 'Other home improvement', 'Home improvement'),
    ('MEDICAL_DENTAL_CARE', 'Dental care', 'Medical'),
    ('MEDICAL_EYE_CARE', 'Eye care', 'Medical'),
    ('MEDICAL_NURSING_CARE', 'Nursing care', 'Medical'),
    ('MEDICAL_PHARMACIES_AND_SUPPLEMENTS', 'Pharmacies and supplements', 'Medical'),
    ('MEDICAL_PRIMARY_CARE', 'Primary care', 'Medical'),
    ('MEDICAL_VETERINARY_SERVICES', 'Veterinary services', 'Medical'),
    ('MEDICAL_OTHER_MEDICAL', 'Other medical', 'Medical'),
    ('PERSONAL_CARE_GYMS_AND_FITNESS_CENTERS', 'Gyms and fitness centers', 'Personal care'),
    ('PERSONAL_CARE_HAIR_AND_BEAUTY', 'Hair and beauty', 'Personal care'),
    ('PERSONAL_CARE_LAUNDRY_AND_DRY_CLEANING', 'Laundry and dry cleaning', 'Personal care'),
    ('PERSONAL_CARE_OTHER_PERSONAL_CARE', 'Other personal care', 'Personal care'),
    ('GENERAL_SERVICES_ACCOUNTING_AND_FINANCIAL_PLANNING', 'Accounting and financial planning', 'General services'),
    ('GENERAL_SERVICES_AUTOMOTIVE', 'Automotive', 'General services'),
    ('GENERAL_SERVICES_CHILDCARE', 'Childcare', 'General services'),
    ('GENERAL_SERVICES_CONSULTING_AND_LEGAL', 'Consulting and legal', 'General services'),
    ('GENERAL_SERVICES_EDUCATION', 'Education', 'General services'),
    ('GENERAL_SERVICES_INSURANCE', 'Insurance', 'General services'),
    ('GENERAL_SERVICES_POSTAGE_AND_SHIPPING', 'Postage and shipping', 'General services'),
    ('GENERAL_SERVICES_STORAGE', 'Storage', 'General services'),
    ('GENERAL_SERVICES_OTHER_GENERAL_SERVICES', 'Other general services', 'General services'),
    ('GOVERNMENT_AND_NON_PROFIT_DONATIONS', 'Donations', 'Government and non profit'),
    ('GOVERNMENT_AND_NON_PROFIT_GOVERNMENT_DEPARTMENTS_AND_AGENCIES', 'Government departments and agencies', 'Government and non profit'),
    ('GOVERNMENT_AND_NON_PROFIT_TAX_PAYMENT', 'Tax payment', 'Government and non profit'),
    ('GOVERNMENT_AND_NON_PROFIT_OTHER_GOVERNMENT_AND_NON_PROFIT', 'Other government and non profit', 'Government and non profit'),
    ('TRANSPORTATION_BIKES_AND_SCOOTERS', 'Bikes and scooters', 'Transportation'),
    ('TRANSPORTATION_GAS', 'Gas', 'Transportation'),
    ('TRANSPORTATION_PARKING', 'Parking', 'Transportation'),
    ('TRANSPORTATION_PUBLIC_TRANSIT', 'Public transit', 'Transportation'),
    ('TRANSPORTATION_TAXIS_AND_RIDE_SHARES', 'Taxis and ride shares', 'Transportation'),
    ('TRANSPORTATION_TOLLS', 'Tolls', 'Transportation'),
    ('TRANSPORTATION_OTHER_TRANSPORTATION', 'Other transportation', 'Transportation'),
    ('TRAVEL_FLIGHTS', 'Flights', 'Travel'),
    ('TRAVEL_LODGING', 'Lodging', 'Travel'),
    ('TRAVEL_RENTAL_CARS', 'Rental cars', 'Travel'),
    ('TRAVEL_OTHER_TRAVEL', 'Other travel', 'Travel'),
    ('RENT_AND_UTILITIES_GAS_AND_ELECTRICITY', 'Gas and electricity', 'Rent and utilities'),
    ('RENT_AND_UTILITIES_INTERNET_AND_CABLE', 'Internet and cable', 'Rent and utilities'),
    ('RENT_AND_UTILITIES_RENT', 'Rent', 'Rent and utilities'),
    ('RENT_AND_UTILITIES_SEWAGE_AND_WASTE_MANAGEMENT', 'Sewage and waste management', 'Rent and utilities'),
    ('RENT_AND_UTILITIES_TELEPHONE', 'Telephone', 'Rent and utilities'),
    ('RENT_AND_UTILITIES_WATER', 'Water', 'Rent and utilities'),
    ('RENT_AND_UTILITIES_OTHER_UTILITIES', 'Other utilities', 'Rent and utilities'),
    ('GENERAL_SERVICES_DIGITAL_SERVICES', 'Digital services', 'General services'),
    ('internal_transfer', 'Internal transfer', 'Internal transfer'),
    ('cash_withdrawal', 'Cash withdrawal', 'Cash withdrawal'),
    ('card_spend', 'Card spend', 'Card spend'),
    ('gift', 'Gift', 'Gift'),
    ('investment', 'Investment', 'Investment'),
    ('other', 'Other', 'Other')
  ) AS v(detailed, label, primary_label)
 WHERE spend_categories.spend_detailed = v.detailed;

INSERT INTO schema_meta (gold_schema_version, applied_at)
    VALUES (58, CAST(epoch(now()) AS BIGINT));
