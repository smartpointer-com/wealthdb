package canonical

// LegalForms are the legal-entity forms a company's name carries,
// upper-cased and without a trailing dot. A name that carries one is a
// firm's, not a person's, and one that ends in one is complete. The
// UBS reader, the bond description reader, the spending signature fence
// and the exposure name matcher read this one set.
var LegalForms = WordSet(
	"AG", "GMBH", "KG", "OHG",
	"SA", "SAS", "SARL", "SE", "SPA", "SRL",
	"BV", "NV",
	"AB", "AS", "ASA", "OY", "OYJ",
	"INC", "INCORPORATED", "LLC", "LP", "PLC", "LTD", "LIMITED",
	"CORP", "CORPORATION", "CO", "COMPANY")

// SharedLegalForms are the legal forms a US municipal issuer's name may
// share with a company's: many a municipal issuer is a corporation, CO
// stands for a county, LTD for a limited obligation, and INCORPORATED
// begins an incorporated village's name.
var SharedLegalForms = WordSet("CORP", "CORPORATION", "CO", "COMPANY", "LTD", "LIMITED", "INCORPORATED")

// NameLikeLegalForms are the legal forms that are also a person's given
// name: ASA is Åsa folded. A reader that tells a person's name from a
// firm's does not take one for a firm (the spending signature fence, the
// UBS payee reader); the name matchers drop it like any other form.
var NameLikeLegalForms = WordSet("ASA")
