package canonical

// LegalForms are the legal-entity forms a company's name carries,
// upper-cased and without a trailing dot. A name that carries one is a
// firm's, not a person's, and one that ends in one is complete. The
// UBS reader, the bond description reader and the spending signature
// fence read this one set.
var LegalForms = WordSet(
	"AG", "GMBH", "KG", "OHG",
	"SA", "SAS", "SARL", "SE", "SPA", "SRL",
	"BV", "NV",
	"AB", "AS", "OY",
	"INC", "LLC", "LP", "PLC", "LTD", "LIMITED",
	"CORP", "CORPORATION", "CO", "COMPANY")

// SharedLegalForms are the legal forms a US municipal issuer's name may
// share with a company's: many a municipal issuer is a corporation, CO
// stands for a county and LTD for a limited obligation.
var SharedLegalForms = WordSet("CORP", "CORPORATION", "CO", "COMPANY", "LTD", "LIMITED")
