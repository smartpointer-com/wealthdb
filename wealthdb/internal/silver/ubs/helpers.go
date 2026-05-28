package ubs

import "database/sql"

// nullStringPtr converts a sql.NullString to *string. Empty
// strings (including SQL NULL) yield nil — matching the canonical
// "the field is not set" semantics throughout the canonical
// types.
func nullStringPtr(n sql.NullString) *string {
	if !n.Valid || n.String == "" {
		return nil
	}
	s := n.String
	return &s
}
