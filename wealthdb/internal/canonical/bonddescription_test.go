package canonical

import "testing"

func TestReadBondDescription(t *testing.T) {
	muni := BondReading{SectorMunicipal, "US", "USD"}
	corp := BondReading{Sector: SectorCorporate}
	for _, c := range []struct {
		desc string
		usID bool
		want BondReading
		ok   bool
	}{
		// A municipal word under a US identifier.
		{"QLAND CNTY ST GO BDS SER. 2041 03.33000% 07/01/2041", true, muni, true},
		{"QLAND ECONOMIC DEV AUTH REV SCH 03.17000% 09/01/2043", true, muni, true},
		{"QLAND INDPT SCH DIST UNLTD TAX 03.21000% 11/01/2044", true, muni, true},
		// A municipal issuer that is a corporation.
		{"QLAND ST URBAN DEV CORP REV 03.17000% 03/15/2042", true, muni, true},
		// Two words a municipal bond often carries; one is not enough.
		{"QLAND ST BRD ED PUB ED CAP OUTLAY 03.33000% 04/01/2046", true, muni, true},
		{"AMERICAN QLAND WTR CAP CORP 3.21% 2043", true, corp, true},
		// A company's legal form rules the municipal reading out.
		{"QLAND PASS THRU CTFS AUTH LLC 3.21% 2043", true, corp, true},
		// A US state or territory under a US identifier.
		{"STATE OF GEORGIA 3.33% 2041", true, muni, true},
		{"PUERTO RICO COMWLTH 3.33% 2041", true, muni, true},
		{"GUAM GOVT LTD OBLIG 3.33% 2041", true, muni, true},
		{"QLAND CNTY LTD TAX GO 3.33% 2041", true, muni, true},
		{"GEORGIA QLAND POWER CO 3.21% 2043", true, corp, true},
		// The same words under another identifier say nothing municipal.
		{"QLAND CNTY GO BDS 03.33000% 07/01/2041", false, BondReading{}, false},
		// A country's name, with nothing but the paper beside it.
		{"FINLAND 3.33% 15.09.2041", false, BondReading{Sector: SectorGovernment, Country: "FI"}, true},
		{"REPUBLIC OF AUSTRIA 3.33% 20.04.2041", false, BondReading{Sector: SectorGovernment, Country: "AT"}, true},
		{"3.33% REPUBLIC OF AUSTRIA 2041", false, BondReading{Sector: SectorGovernment, Country: "AT"}, true},
		{"BUNDESREPUB. DEUTSCHLAND 3.33% 2041", false, BondReading{Sector: SectorGovernment, Country: "DE"}, true},
		{"UK TSY GILT 3.33% 2041", false, BondReading{Sector: SectorGovernment, Country: "GB"}, true},
		{"UNITED STATES TREAS NTS 3.31% 08/31/2041", true, BondReading{Sector: SectorGovernment, Country: "US"}, true},
		{"U.S. TREASURY 3.31% 08/31/2041", true, BondReading{Sector: SectorGovernment, Country: "US"}, true},
		// A foreign sovereign's bond sold in New York stays the sovereign's.
		{"STATE OF ISRAEL 3.33% 2041", true, BondReading{Sector: SectorGovernment, Country: "IL"}, true},
		// A company named after a country is a company.
		{"AUSTRALIA AND QLAND BANKING GROUP LTD 3.21% 2043", false, corp, true},
		{"QORVANT INC 3.21000% 05/31/2039 NTS", true, corp, true},
		// Nothing to read.
		{"QLAND 3.33% 2041", false, BondReading{}, false},
	} {
		got, ok := ReadBondDescription(c.desc, c.usID)
		if got != c.want || ok != c.ok {
			t.Errorf("%q (US id %v) = %+v %v, want %+v %v", c.desc, c.usID, got, ok, c.want, c.ok)
		}
	}
}
